package registryclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type ociFixture struct {
	manifest, config             []byte
	manifestDigest, configDigest string
	mediaType                    types.MediaType
}

func imageFixture(t *testing.T, os, arch string) ociFixture {
	t.Helper()
	config, _ := json.Marshal(v1.ConfigFile{OS: os, Architecture: arch, RootFS: v1.RootFS{Type: "layers"}})
	configHash, err := v1.NewHash(digestOf(config))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Config: v1.Descriptor{MediaType: types.OCIConfigJSON, Size: int64(len(config)), Digest: configHash}, Layers: []v1.Descriptor{}})
	return ociFixture{manifest: manifest, config: config, manifestDigest: digestOf(manifest), configDigest: digestOf(config), mediaType: types.OCIManifestSchema1}
}
func fixtureHandler(fixture ociFixture) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", string(fixture.mediaType))
			_, _ = w.Write(fixture.manifest)
		case strings.HasSuffix(r.URL.Path, "/blobs/"+fixture.configDigest):
			w.Header().Set("Content-Type", string(types.OCIConfigJSON))
			_, _ = w.Write(fixture.config)
		default:
			http.NotFound(w, r)
		}
	})
}
func newTLSClient(t *testing.T, handler http.Handler, mutate func(*Config)) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport)
	config := Config{URL: server.URL, AllowPrivate: true, TLSConfig: transport.TLSClientConfig}
	if mutate != nil {
		mutate(&config)
	}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, server
}
func TestVerifyReadsActualManifestAndConfigAndRejectsWrongIdentity(t *testing.T) {
	fixture := imageFixture(t, "linux", "amd64")
	client, server := newTLSClient(t, fixtureHandler(fixture), nil)
	reference := strings.TrimPrefix(server.URL, "https://") + "/tenant/app@" + fixture.manifestDigest
	artifact, err := client.Verify(context.Background(), reference, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Digest != fixture.manifestDigest || artifact.ConfigDigest != fixture.configDigest || artifact.Platform != "linux/amd64" {
		t.Fatal(artifact)
	}
	if _, err := client.Verify(context.Background(), strings.TrimPrefix(server.URL, "https://")+"/tenant/app:latest", "linux/amd64"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("可变tag=%v", err)
	}
	if _, err := client.Verify(context.Background(), "evil.example/tenant/app@"+fixture.manifestDigest, "linux/amd64"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("其他仓库=%v", err)
	}
	if _, err := client.Verify(context.Background(), reference, "linux/arm64"); !errors.Is(err, ErrPlatform) {
		t.Fatalf("未认证平台=%v", err)
	}
	wrong := reference[:len(reference)-64] + strings.Repeat("0", 64)
	if _, err := client.Verify(context.Background(), wrong, "linux/amd64"); err == nil {
		t.Fatal("错误digest未拒绝")
	}
}
func TestVerifyRejectsConfigPlatformAndDigestMismatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture ociFixture
	}{{"wrong-platform", imageFixture(t, "linux", "arm64")}, {"tampered-config", imageFixture(t, "linux", "amd64")}} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "tampered-config" {
				test.fixture.config = []byte(`{"os":"linux","architecture":"amd64","tampered":true}`)
			}
			client, server := newTLSClient(t, fixtureHandler(test.fixture), nil)
			reference := strings.TrimPrefix(server.URL, "https://") + "/app@" + test.fixture.manifestDigest
			if _, err := client.Verify(context.Background(), reference, "linux/amd64"); err == nil {
				t.Fatal("无效config未拒绝")
			}
		})
	}
}
func TestIndexSelectsActualImageNotAttestationAndRejectsAmbiguity(t *testing.T) {
	image := imageFixture(t, "linux", "amd64")
	hash, err := v1.NewHash(image.manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	actual := v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: hash, Size: int64(len(image.manifest)), Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}
	attestation := actual
	attestation.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
	for _, test := range []struct {
		name     string
		children []v1.Descriptor
		success  bool
	}{{"image-and-attestation", []v1.Descriptor{attestation, actual}, true}, {"attestation-only", []v1.Descriptor{attestation}, false}, {"ambiguous", []v1.Descriptor{actual, actual}, false}, {"wrong-media", []v1.Descriptor{{MediaType: types.OCIImageIndex, Digest: hash, Size: 123, Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}}, false}} {
		t.Run(test.name, func(t *testing.T) {
			index, _ := json.Marshal(v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: test.children})
			indexDigest := digestOf(index)
			handler := fixtureHandler(image)
			client, server := newTLSClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/manifests/"+indexDigest) {
					w.Header().Set("Content-Type", string(types.OCIImageIndex))
					_, _ = w.Write(index)
					return
				}
				handler.ServeHTTP(w, r)
			}), nil)
			artifact, err := client.Verify(context.Background(), strings.TrimPrefix(server.URL, "https://")+"/app@"+indexDigest, "linux/amd64")
			if test.success {
				if err != nil || artifact.Digest != indexDigest || artifact.ImageDigest != image.manifestDigest {
					t.Fatalf("index result=%#v,%v", artifact, err)
				}
			} else if err == nil {
				t.Fatal("无效index未拒绝")
			}
		})
	}
}
func TestRedirectAndForeignBearerRealmNeverReceiveCredentials(t *testing.T) {
	var hits atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Errorf("未授权目标收到请求: %s, auth=%t", r.URL.Path, r.Header.Get("Authorization") != "")
	}))
	defer target.Close()
	for _, mode := range []string{"redirect", "realm"} {
		t.Run(mode, func(t *testing.T) {
			client, server := newTLSClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test"`, target.URL))
				w.WriteHeader(http.StatusUnauthorized)
			}), func(config *Config) { config.Username = "robot"; config.Password = "super-secret" })
			_, err := client.Probe(context.Background(), "")
			if err == nil {
				t.Fatal("不可信认证目标未拒绝")
			}
			if strings.Contains(err.Error(), "super-secret") {
				t.Fatal("错误泄漏口令")
			}
			_ = server
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("跨目标请求数量=%d", hits.Load())
	}
}
func TestExplicitAuthHostAllowsOnlyConfiguredTokenExchange(t *testing.T) {
	var exchanges atomic.Int64
	authServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "robot" || password != "secret" {
			t.Error("显式认证目标没有获得正确的交换凭据")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"limited-registry-token","expires_in":60}`))
	}))
	defer authServer.Close()
	registryServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer limited-registry-token" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="configured-registry"`, authServer.URL))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registryServer.Close()
	roots := x509.NewCertPool()
	roots.AddCert(authServer.Certificate())
	roots.AddCert(registryServer.Certificate())
	client, err := New(Config{URL: registryServer.URL, Username: "robot", Password: "secret", AllowedAuthHosts: []string{strings.TrimPrefix(authServer.URL, "https://")}, AllowPrivate: true, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.Probe(context.Background(), "")
	if err != nil || !result.AuthenticationAccepted || exchanges.Load() != 1 {
		t.Fatalf("显式认证交换=%#v,count=%d,%v", result, exchanges.Load(), err)
	}
}

func TestMetadataBoundAndTimeout(t *testing.T) {
	fixture := imageFixture(t, "linux", "amd64")
	client, server := newTLSClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			return
		}
		w.Header().Set("Content-Type", string(types.OCIManifestSchema1))
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}), func(config *Config) { config.MaxMetadataBytes = 1024 })
	if _, err := client.Verify(context.Background(), strings.TrimPrefix(server.URL, "https://")+"/app@"+fixture.manifestDigest, "linux/amd64"); err == nil {
		t.Fatal("超限元数据未拒绝")
	}
	slow, _ := newTLSClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }), func(config *Config) { config.Timeout = 50 * time.Millisecond })
	started := time.Now()
	if _, err := slow.Probe(context.Background(), ""); err == nil {
		t.Fatal("超时未拒绝")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("探测超时无界")
	}
}
func TestProbeNeverClaimsPushPullQualification(t *testing.T) {
	client, _ := newTLSClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }), nil)
	result, err := client.Probe(context.Background(), "")
	if err != nil || !result.NetworkReachable || result.AuthenticationAccepted {
		t.Fatalf("probe=%#v,%v", result, err)
	}
	if result.ReadVerified || result.PushVerified || result.WorkerPushPullVerified || !strings.Contains(result.Message, "未验证") {
		t.Fatalf("伪造推拉资格=%#v", result)
	}
}
func TestNetworkPolicyRejectsMetadataAndUntrustedPrivateTargets(t *testing.T) {
	for _, address := range []string{"https://169.254.169.254", "https://100.100.100.200", "https://[fe80::1]"} {
		if _, err := New(Config{URL: address, AllowPrivate: true}); !errors.Is(err, ErrUnsafeTarget) {
			t.Fatalf("metadata目标%s=%v", address, err)
		}
	}
	if _, err := New(Config{URL: "https://127.0.0.1"}); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatal("默认不应允许回环地址")
	}
	if _, err := New(Config{URL: "https://registry.example.com", DeniedHosts: []string{"registry.example.com"}}); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatal("显式拒绝目标未执行")
	}
}
