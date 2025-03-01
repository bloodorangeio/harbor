// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docker/distribution"
	"github.com/docker/distribution/manifest/schema1"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/garyburd/redigo/redis"
	"github.com/goharbor/harbor/src/common/dao"
	"github.com/goharbor/harbor/src/common/models"
	"github.com/goharbor/harbor/src/common/utils"
	"github.com/goharbor/harbor/src/common/utils/clair"
	"github.com/goharbor/harbor/src/common/utils/log"
	"github.com/goharbor/harbor/src/core/config"
	"github.com/goharbor/harbor/src/core/promgr"
	"github.com/goharbor/harbor/src/pkg/scan/whitelist"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type contextKey string

const (
	// ImageInfoCtxKey the context key for image information
	ImageInfoCtxKey = contextKey("ImageInfo")
	// TokenUsername ...
	// TODO: temp solution, remove after vmware/harbor#2242 is resolved.
	TokenUsername = "harbor-core"

	// blobInfoKey the context key for blob info
	blobInfoKey = contextKey("BlobInfo")
	// chartVersionInfoKey the context key for chart version info
	chartVersionInfoKey = contextKey("ChartVersionInfo")
	// manifestInfoKey the context key for manifest info
	manifestInfoKey = contextKey("ManifestInfo")

	// DialConnectionTimeout ...
	DialConnectionTimeout = 30 * time.Second
	// DialReadTimeout ...
	DialReadTimeout = time.Minute + 10*time.Second
	// DialWriteTimeout ...
	DialWriteTimeout = 10 * time.Second
)

var (
	manifestURLRe = regexp.MustCompile(`^/v2/((?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)+)manifests/([\w][\w.:-]{0,127})`)
)

// ChartVersionInfo ...
type ChartVersionInfo struct {
	ProjectID int64
	Namespace string
	ChartName string
	Version   string
}

// MutexKey returns mutex key of the chart version
func (info *ChartVersionInfo) MutexKey(suffix ...string) string {
	a := []string{"quota", info.Namespace, "chart", info.ChartName, "version", info.Version}

	return strings.Join(append(a, suffix...), ":")
}

// ImageInfo ...
type ImageInfo struct {
	Repository  string
	Reference   string
	ProjectName string
	Digest      string
}

// BlobInfo ...
type BlobInfo struct {
	ProjectID   int64
	ContentType string
	Size        int64
	Repository  string
	Digest      string

	blobExist     bool
	blobExistErr  error
	blobExistOnce sync.Once
}

// BlobExists returns true when blob exists in the project
func (info *BlobInfo) BlobExists() (bool, error) {
	info.blobExistOnce.Do(func() {
		info.blobExist, info.blobExistErr = dao.HasBlobInProject(info.ProjectID, info.Digest)
	})

	return info.blobExist, info.blobExistErr
}

// MutexKey returns mutex key of the blob
func (info *BlobInfo) MutexKey(suffix ...string) string {
	projectName, _ := utils.ParseRepository(info.Repository)
	a := []string{"quota", projectName, "blob", info.Digest}

	return strings.Join(append(a, suffix...), ":")
}

// ManifestInfo ...
type ManifestInfo struct {
	// basic information of a manifest
	ProjectID  int64
	Repository string
	Tag        string
	Digest     string

	References []distribution.Descriptor
	Descriptor distribution.Descriptor

	// manifestExist is to index the existing of the manifest in DB by (repository, digest)
	manifestExist     bool
	manifestExistErr  error
	manifestExistOnce sync.Once

	// artifact the artifact indexed by (repository, tag) in DB
	artifact     *models.Artifact
	artifactErr  error
	artifactOnce sync.Once

	// ExclusiveBlobs include the blobs that belong to the manifest only
	// and exclude the blobs that shared by other manifests in the same repo(project/repository).
	ExclusiveBlobs []*models.Blob
}

// MutexKey returns mutex key of the manifest
func (info *ManifestInfo) MutexKey(suffix ...string) string {
	projectName, _ := utils.ParseRepository(info.Repository)
	var a []string

	if info.Tag != "" {
		// tag not empty happened in PUT /v2/<name>/manifests/<reference>
		// lock by to tag to compute the count resource required by quota
		a = []string{"quota", projectName, "manifest", info.Tag}
	} else {
		a = []string{"quota", projectName, "manifest", info.Digest}
	}

	return strings.Join(append(a, suffix...), ":")
}

// BlobMutexKey returns mutex key of the blob in manifest
func (info *ManifestInfo) BlobMutexKey(blob *models.Blob, suffix ...string) string {
	projectName, _ := utils.ParseRepository(info.Repository)
	a := []string{"quota", projectName, "blob", blob.Digest}

	return strings.Join(append(a, suffix...), ":")
}

// SyncBlobs sync layers of manifest to blobs
func (info *ManifestInfo) SyncBlobs() error {
	err := dao.SyncBlobs(info.References)
	if err == dao.ErrDupRows {
		log.Warning("Some blobs created by others, ignore this error")
		return nil
	}

	return err
}

// GetBlobsNotInProject returns blobs of the manifest which not in the project
func (info *ManifestInfo) GetBlobsNotInProject() ([]*models.Blob, error) {
	var digests []string
	for _, reference := range info.References {
		digests = append(digests, reference.Digest.String())
	}

	blobs, err := dao.GetBlobsNotInProject(info.ProjectID, digests...)
	if err != nil {
		return nil, err
	}

	return blobs, nil
}

func (info *ManifestInfo) fetchArtifact() (*models.Artifact, error) {
	info.artifactOnce.Do(func() {
		info.artifact, info.artifactErr = dao.GetArtifact(info.Repository, info.Tag)
	})

	return info.artifact, info.artifactErr
}

// IsNewTag returns true if the tag of the manifest not exists in project
func (info *ManifestInfo) IsNewTag() bool {
	artifact, _ := info.fetchArtifact()

	return artifact == nil
}

// Artifact returns artifact of the manifest
func (info *ManifestInfo) Artifact() *models.Artifact {
	result := &models.Artifact{
		PID:    info.ProjectID,
		Repo:   info.Repository,
		Tag:    info.Tag,
		Digest: info.Digest,
		Kind:   "Docker-Image",
	}

	if artifact, _ := info.fetchArtifact(); artifact != nil {
		result.ID = artifact.ID
		result.CreationTime = artifact.CreationTime
		result.PushTime = time.Now()
	}

	return result
}

// ManifestExists returns true if manifest exist in repository
func (info *ManifestInfo) ManifestExists() (bool, error) {
	info.manifestExistOnce.Do(func() {
		total, err := dao.GetTotalOfArtifacts(&models.ArtifactQuery{
			PID:    info.ProjectID,
			Digest: info.Digest,
		})

		info.manifestExist = total > 0
		info.manifestExistErr = err
	})

	return info.manifestExist, info.manifestExistErr
}

// JSONError wraps a concrete Code and Message, it's readable for docker deamon.
type JSONError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// MarshalError ...
func MarshalError(code, msg string) string {
	var tmpErrs struct {
		Errors []JSONError `json:"errors,omitempty"`
	}
	tmpErrs.Errors = append(tmpErrs.Errors, JSONError{
		Code:    code,
		Message: msg,
		Detail:  msg,
	})
	str, err := json.Marshal(tmpErrs)
	if err != nil {
		log.Debugf("failed to marshal json error, %v", err)
		return msg
	}
	return string(str)
}

// MatchManifestURL ...
func MatchManifestURL(req *http.Request) (bool, string, string) {
	s := manifestURLRe.FindStringSubmatch(req.URL.Path)
	if len(s) == 3 {
		s[1] = strings.TrimSuffix(s[1], "/")
		return true, s[1], s[2]
	}
	return false, "", ""
}

// MatchPullManifest checks if the request looks like a request to pull manifest.  If it is returns the image and tag/sha256 digest as 2nd and 3rd return values
func MatchPullManifest(req *http.Request) (bool, string, string) {
	if req.Method != http.MethodGet {
		return false, "", ""
	}
	return MatchManifestURL(req)
}

// MatchPushManifest checks if the request looks like a request to push manifest.  If it is returns the image and tag/sha256 digest as 2nd and 3rd return values
func MatchPushManifest(req *http.Request) (bool, string, string) {
	if req.Method != http.MethodPut {
		return false, "", ""
	}
	return MatchManifestURL(req)
}

// MatchDeleteManifest checks if the request
func MatchDeleteManifest(req *http.Request) (match bool, repository string, reference string) {
	if req.Method != http.MethodDelete {
		return
	}

	match, repository, reference = MatchManifestURL(req)
	if _, err := digest.Parse(reference); err != nil {
		// Delete manifest only accept digest as reference
		match = false

		return
	}

	return
}

// CopyResp ...
func CopyResp(rec *httptest.ResponseRecorder, rw http.ResponseWriter) {
	for k, v := range rec.Header() {
		rw.Header()[k] = v
	}
	rw.WriteHeader(rec.Result().StatusCode)
	rw.Write(rec.Body.Bytes())
}

// PolicyChecker checks the policy of a project by project name, to determine if it's needed to check the image's status under this project.
type PolicyChecker interface {
	// contentTrustEnabled returns whether a project has enabled content trust.
	ContentTrustEnabled(name string) bool
	// vulnerablePolicy  returns whether a project has enabled vulnerable, and the project's severity.
	VulnerablePolicy(name string) (bool, models.Severity, models.CVEWhitelist)
}

// PmsPolicyChecker ...
type PmsPolicyChecker struct {
	pm promgr.ProjectManager
}

// ContentTrustEnabled ...
func (pc PmsPolicyChecker) ContentTrustEnabled(name string) bool {
	project, err := pc.pm.Get(name)
	if err != nil {
		log.Errorf("Unexpected error when getting the project, error: %v", err)
		return true
	}
	return project.ContentTrustEnabled()
}

// VulnerablePolicy ...
func (pc PmsPolicyChecker) VulnerablePolicy(name string) (bool, models.Severity, models.CVEWhitelist) {
	project, err := pc.pm.Get(name)
	wl := models.CVEWhitelist{}
	if err != nil {
		log.Errorf("Unexpected error when getting the project, error: %v", err)
		return true, models.SevUnknown, wl
	}
	mgr := whitelist.NewDefaultManager()
	if project.ReuseSysCVEWhitelist() {
		w, err := mgr.GetSys()
		if err != nil {
			return project.VulPrevented(), clair.ParseClairSev(project.Severity()), wl
		}
		wl = *w
	} else {
		w, err := mgr.Get(project.ProjectID)
		if err != nil {
			return project.VulPrevented(), clair.ParseClairSev(project.Severity()), wl
		}
		wl = *w
	}
	return project.VulPrevented(), clair.ParseClairSev(project.Severity()), wl

}

// NewPMSPolicyChecker returns an instance of an pmsPolicyChecker
func NewPMSPolicyChecker(pm promgr.ProjectManager) PolicyChecker {
	return &PmsPolicyChecker{
		pm: pm,
	}
}

// GetPolicyChecker ...
func GetPolicyChecker() PolicyChecker {
	return NewPMSPolicyChecker(config.GlobalProjectMgr)
}

// GetRegRedisCon ...
func GetRegRedisCon() (redis.Conn, error) {
	// FOR UT
	if os.Getenv("UTTEST") == "true" {
		return redis.Dial(
			"tcp",
			fmt.Sprintf("%s:%d", os.Getenv("REDIS_HOST"), 6379),
			redis.DialConnectTimeout(DialConnectionTimeout),
			redis.DialReadTimeout(DialReadTimeout),
			redis.DialWriteTimeout(DialWriteTimeout),
		)
	}
	return redis.DialURL(
		config.GetRedisOfRegURL(),
		redis.DialConnectTimeout(DialConnectionTimeout),
		redis.DialReadTimeout(DialReadTimeout),
		redis.DialWriteTimeout(DialWriteTimeout),
	)
}

// BlobInfoFromContext returns blob info from context
func BlobInfoFromContext(ctx context.Context) (*BlobInfo, bool) {
	info, ok := ctx.Value(blobInfoKey).(*BlobInfo)
	return info, ok
}

// ChartVersionInfoFromContext returns chart info from context
func ChartVersionInfoFromContext(ctx context.Context) (*ChartVersionInfo, bool) {
	info, ok := ctx.Value(chartVersionInfoKey).(*ChartVersionInfo)
	return info, ok
}

// ImageInfoFromContext returns image info from context
func ImageInfoFromContext(ctx context.Context) (*ImageInfo, bool) {
	info, ok := ctx.Value(ImageInfoCtxKey).(*ImageInfo)
	return info, ok
}

// ManifestInfoFromContext returns manifest info from context
func ManifestInfoFromContext(ctx context.Context) (*ManifestInfo, bool) {
	info, ok := ctx.Value(manifestInfoKey).(*ManifestInfo)
	return info, ok
}

// NewBlobInfoContext returns context with blob info
func NewBlobInfoContext(ctx context.Context, info *BlobInfo) context.Context {
	return context.WithValue(ctx, blobInfoKey, info)
}

// NewChartVersionInfoContext returns context with blob info
func NewChartVersionInfoContext(ctx context.Context, info *ChartVersionInfo) context.Context {
	return context.WithValue(ctx, chartVersionInfoKey, info)
}

// NewImageInfoContext returns context with image info
func NewImageInfoContext(ctx context.Context, info *ImageInfo) context.Context {
	return context.WithValue(ctx, ImageInfoCtxKey, info)
}

// NewManifestInfoContext returns context with manifest info
func NewManifestInfoContext(ctx context.Context, info *ManifestInfo) context.Context {
	return context.WithValue(ctx, manifestInfoKey, info)
}

// ParseManifestInfoFromReq parse manifest from request
func ParseManifestInfoFromReq(req *http.Request) (*ManifestInfo, error) {
	match, repository, reference := MatchManifestURL(req)
	if !match {
		return nil, fmt.Errorf("not match url %s for manifest", req.URL.Path)
	}

	var tag string
	if _, err := digest.Parse(reference); err != nil {
		tag = reference
	}

	mediaType := req.Header.Get("Content-Type")
	if mediaType != schema1.MediaTypeManifest &&
		mediaType != schema1.MediaTypeSignedManifest &&
		mediaType != schema2.MediaTypeManifest &&
		mediaType != ocispec.MediaTypeImageManifest {
		return nil, fmt.Errorf("unsupported content type for manifest: %s", mediaType)
	}

	if req.Body == nil {
		return nil, fmt.Errorf("body missing")
	}

	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		log.Warningf("Error occurred when to copy manifest body %v", err)
		return nil, err
	}
	req.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	var manifest ocispec.Manifest
	err = json.Unmarshal(body, &manifest)
	if err != nil {
		log.Warningf("Error occurred when to Unmarshal OCI Manifest %v", err)
		return nil, err
	}

	body, err = ioutil.ReadAll(req.Body)
	if err != nil {
		log.Warningf("Error occurred when to copy manifest body 2 %v", err)
		return nil, err
	}
	req.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	var desc ocispec.Descriptor
	err = json.Unmarshal(body, &desc)
	if err != nil {
		log.Warningf("Error occurred when to Unmarshal OCI Descriptor %v", err)
		return nil, err
	}

	/*
		manifest, desc, err := distribution.UnmarshalManifest(mediaType, body)
		if err != nil {
			log.Warningf("Error occurred when to Unmarshal Manifest %v", err)
			return nil, err
		}
	*/

	projectName, _ := utils.ParseRepository(repository)
	project, err := dao.GetProjectByName(projectName)
	if err != nil {
		return nil, fmt.Errorf("failed to get project %s, error: %v", projectName, err)
	}
	if project == nil {
		return nil, fmt.Errorf("project %s not found", projectName)
	}

	references := []distribution.Descriptor{}
	for _, layer := range manifest.Layers {
		d := distribution.Descriptor{
			MediaType:   layer.MediaType,
			Size:        layer.Size,
			Digest:      layer.Digest,
			URLs:        layer.URLs,
			Annotations: layer.Annotations,
			Platform:    layer.Platform,
		}
		references = append(references, d)
	}

	return &ManifestInfo{
		ProjectID:  project.ProjectID,
		Repository: repository,
		Tag:        tag,
		Digest:     desc.Digest.String(),
		References: references,
		Descriptor: distribution.Descriptor{
			MediaType:   desc.MediaType,
			Size:        desc.Size,
			Digest:      desc.Digest,
			URLs:        desc.URLs,
			Annotations: desc.Annotations,
			Platform:    desc.Platform,
		},
	}, nil
}

// ParseManifestInfoFromPath parse manifest from request path
func ParseManifestInfoFromPath(req *http.Request) (*ManifestInfo, error) {
	match, repository, reference := MatchManifestURL(req)
	if !match {
		return nil, fmt.Errorf("not match url %s for manifest", req.URL.Path)
	}

	projectName, _ := utils.ParseRepository(repository)
	project, err := dao.GetProjectByName(projectName)
	if err != nil {
		return nil, fmt.Errorf("failed to get project %s, error: %v", projectName, err)
	}
	if project == nil {
		return nil, fmt.Errorf("project %s not found", projectName)
	}

	info := &ManifestInfo{
		ProjectID:  project.ProjectID,
		Repository: repository,
	}

	dgt, err := digest.Parse(reference)
	if err != nil {
		info.Tag = reference
	} else {
		info.Digest = dgt.String()
	}

	return info, nil
}
