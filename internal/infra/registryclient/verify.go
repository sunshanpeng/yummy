package registryclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	registrytransport "github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type Artifact struct {
	Repository   string `json:"repository"`
	Digest       string `json:"digest"`
	ImageDigest  string `json:"imageDigest"`
	ConfigDigest string `json:"configDigest"`
	Platform     string `json:"platform"`
	MediaType    string `json:"mediaType"`
}

// Verify 读取 manifest 和 config 并独立验证内容摘要，不把构建器成功日志作为产物事实。
func (c *Client) Verify(ctx context.Context, reference, expectedPlatform string) (Artifact, error) {
	if expectedPlatform != "linux/amd64" {
		return Artifact{}, ErrPlatform
	}
	ref, err := c.reference(reference)
	if err != nil {
		return Artifact{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	descriptor, err := remote.Get(ref, c.options(ctx)...)
	if err != nil {
		return Artifact{}, safeError(err)
	}
	if err := checkHash(descriptor.Manifest, ref.DigestStr()); err != nil {
		return Artifact{}, err
	}
	rootDigest := ref.DigestStr()
	selected := descriptor
	if descriptor.MediaType == types.OCIImageIndex || descriptor.MediaType == types.DockerManifestList {
		var index v1.IndexManifest
		if err := json.Unmarshal(descriptor.Manifest, &index); err != nil || index.SchemaVersion != 2 {
			return Artifact{}, ErrManifest
		}
		var candidates []v1.Descriptor
		for _, child := range index.Manifests {
			if child.Platform == nil || child.Platform.OS != "linux" || child.Platform.Architecture != "amd64" || child.Platform.Variant != "" {
				continue
			}
			if child.MediaType != types.OCIManifestSchema1 && child.MediaType != types.DockerManifestSchema2 {
				continue
			}
			if child.ArtifactType != "" || child.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
				continue
			}
			candidates = append(candidates, child)
		}
		if len(candidates) != 1 {
			return Artifact{}, ErrPlatform
		}
		child := candidates[0]
		if child.Size < 1 || child.Size > c.transport.maxBytes || child.Digest.Algorithm != "sha256" {
			return Artifact{}, ErrManifest
		}
		selected, err = remote.Get(ref.Context().Digest(child.Digest.String()), c.options(ctx)...)
		if err != nil {
			return Artifact{}, safeError(err)
		}
		if int64(len(selected.Manifest)) != child.Size {
			return Artifact{}, ErrManifest
		}
		if err := checkHash(selected.Manifest, child.Digest.String()); err != nil {
			return Artifact{}, err
		}
	}
	if selected.MediaType != types.OCIManifestSchema1 && selected.MediaType != types.DockerManifestSchema2 {
		return Artifact{}, ErrManifest
	}
	var manifest struct {
		v1.Manifest
		ArtifactType string `json:"artifactType"`
	}
	if err := json.Unmarshal(selected.Manifest, &manifest); err != nil || manifest.SchemaVersion != 2 || manifest.ArtifactType != "" || manifest.Subject != nil {
		return Artifact{}, ErrManifest
	}
	if manifest.Config.MediaType != types.OCIConfigJSON && manifest.Config.MediaType != types.DockerConfigJSON {
		return Artifact{}, ErrManifest
	}
	if manifest.Config.Size < 1 || manifest.Config.Size > c.transport.maxBytes || manifest.Config.Digest.Algorithm != "sha256" {
		return Artifact{}, ErrManifest
	}
	image, err := selected.Image()
	if err != nil {
		return Artifact{}, safeError(err)
	}
	config, err := image.RawConfigFile()
	if err != nil {
		return Artifact{}, safeError(err)
	}
	if int64(len(config)) != manifest.Config.Size {
		return Artifact{}, ErrManifest
	}
	if err := checkHash(config, manifest.Config.Digest.String()); err != nil {
		return Artifact{}, err
	}
	var configFile v1.ConfigFile
	if err := json.Unmarshal(config, &configFile); err != nil {
		return Artifact{}, ErrManifest
	}
	if configFile.RootFS.Type != "layers" || len(configFile.RootFS.DiffIDs) != len(manifest.Layers) {
		return Artifact{}, ErrManifest
	}
	for index, layer := range manifest.Layers {
		if !layer.MediaType.IsLayer() || !layer.MediaType.IsDistributable() || len(layer.URLs) != 0 || layer.Size < 0 || layer.Digest.Algorithm != "sha256" || configFile.RootFS.DiffIDs[index].Algorithm != "sha256" {
			return Artifact{}, ErrManifest
		}
	}
	if configFile.OS != "linux" || configFile.Architecture != "amd64" || configFile.Variant != "" {
		return Artifact{}, ErrPlatform
	}
	return Artifact{Repository: ref.Context().Name(), Digest: rootDigest, ImageDigest: digestOf(selected.Manifest), ConfigDigest: manifest.Config.Digest.String(), Platform: expectedPlatform, MediaType: string(selected.MediaType)}, nil
}
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func checkHash(data []byte, expected string) error {
	if digestOf(data) != expected {
		return ErrDigest
	}
	return nil
}

type ProbeResult struct {
	NetworkReachable       bool   `json:"networkReachable"`
	AuthenticationAccepted bool   `json:"authenticationAccepted"`
	ReadVerified           bool   `json:"readVerified"`
	PushVerified           bool   `json:"pushVerified"`
	WorkerPushPullVerified bool   `json:"workerPushPullVerified"`
	Message                string `json:"message"`
}

// Probe 验证 HTTPS 和认证握手，可选校验指定 digest 的读取；绝不执行推送或宣告节点侧可推拉。
func (c *Client) Probe(ctx context.Context, readReference string) (ProbeResult, error) {
	result := ProbeResult{Message: "仅检查控制面网络/认证；未验证worker侧推送与读取资格"}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	registry, err := name.NewRegistry(c.host, name.StrictValidation)
	if err != nil {
		return result, ErrInvalid
	}
	challenge, err := registrytransport.Ping(ctx, registry, c.transport)
	if err != nil {
		return result, safeError(err)
	}
	result.NetworkReachable = true
	authenticationRequired := challenge.Scheme != ""
	authenticated, err := registrytransport.NewWithContext(ctx, registry, &authn.Basic{Username: c.username, Password: c.password}, c.transport, nil)
	if err != nil {
		return result, safeError(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+c.host+"/v2/", nil)
	if err != nil {
		return result, ErrInvalid
	}
	client := http.Client{Transport: authenticated, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrUnsafeTarget }}
	response, err := client.Do(request)
	if err != nil {
		return result, safeError(err)
	}
	defer response.Body.Close()
	result.NetworkReachable = true
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		return result, safeError(err)
	}
	if response.StatusCode != http.StatusOK {
		return result, ErrNetwork
	}
	result.AuthenticationAccepted = authenticationRequired
	if !authenticationRequired {
		result.Message += "；仓库允许匿名访问/v2/，未据此验证所存凭据"
	}
	if strings.TrimSpace(readReference) != "" {
		if _, err := c.Verify(ctx, readReference, "linux/amd64"); err != nil {
			return result, err
		}
		result.ReadVerified = true
	}
	return result, nil
}
