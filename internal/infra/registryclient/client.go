// Package registryclient 仅访问管理员固定配置的 OCI 仓库，不接收用户提供的任意 URL。
package registryclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

var (
	ErrInvalid      = errors.New("镜像引用或仓库配置无效")
	ErrNetwork      = errors.New("仓库网络或认证失败")
	ErrUnsafeTarget = errors.New("拒绝访问未授权的仓库或认证目标")
	ErrTooLarge     = errors.New("仓库元数据超过大小限制")
	ErrDigest       = errors.New("镜像内容与声明的digest不匹配")
	ErrPlatform     = errors.New("镜像平台不匹配或缺少唯一的可运行镜像")
	ErrManifest     = errors.New("仓库返回无效或不支持的镜像元数据")
)

type Config struct {
	URL      string
	Username string
	Password string
	// AllowedAuthHosts 只由实例配置注入，精确匹配 host[:port]，不接受域名通配。
	AllowedAuthHosts []string
	// AllowPrivate 适用于管理员明确登记的内网仓库；链路本地/云元数据地址始终拒绝。
	AllowPrivate     bool
	DeniedHosts      []string
	TLSConfig        *tls.Config
	Timeout          time.Duration
	MaxMetadataBytes int64
}

type Client struct {
	host               string
	username, password string
	timeout            time.Duration
	transport          *guardTransport
	base               *http.Transport
}

func New(config Config) (*Client, error) {
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, ErrInvalid
	}
	if config.Timeout == 0 {
		config.Timeout = 20 * time.Second
	}
	if config.Timeout < time.Millisecond || config.Timeout > time.Minute {
		return nil, ErrInvalid
	}
	if config.MaxMetadataBytes == 0 {
		config.MaxMetadataBytes = 4 << 20
	}
	if config.MaxMetadataBytes < 1024 || config.MaxMetadataBytes > 16<<20 {
		return nil, ErrInvalid
	}
	host := strings.ToLower(parsed.Host)
	allowed := map[string]bool{host: true}
	for _, authHost := range config.AllowedAuthHosts {
		value, err := url.Parse("https://" + authHost)
		if err != nil || value.Host != authHost || value.Hostname() == "" || value.Path != "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
			return nil, ErrInvalid
		}
		allowed[strings.ToLower(authHost)] = true
	}
	denied := make(map[string]bool, len(config.DeniedHosts))
	for _, value := range config.DeniedHosts {
		denied[strings.ToLower(value)] = true
	}
	guard := &guardTransport{registry: host, allowed: allowed, denied: denied, allowPrivate: config.AllowPrivate, maxBytes: config.MaxMetadataBytes}
	for candidate := range allowed {
		value, _ := url.Parse("https://" + candidate)
		if denied[candidate] || denied[strings.ToLower(value.Hostname())] {
			return nil, ErrUnsafeTarget
		}
		if ip := net.ParseIP(value.Hostname()); ip != nil && !guard.safeIP(ip) {
			return nil, ErrUnsafeTarget
		}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if config.TLSConfig != nil {
		tlsConfig = config.TLSConfig.Clone()
		if tlsConfig.InsecureSkipVerify {
			return nil, ErrInvalid
		}
		if tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	base := &http.Transport{Proxy: nil, DialContext: guard.dialContext, TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 4, MaxIdleConnsPerHost: 2, ForceAttemptHTTP2: true}
	guard.inner = base
	return &Client{host: host, username: config.Username, password: config.Password, timeout: config.Timeout, transport: guard, base: base}, nil
}
func (c *Client) Close() { c.base.CloseIdleConnections() }
func (c *Client) options(ctx context.Context) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithAuth(&authn.Basic{Username: c.username, Password: c.password}), remote.WithTransport(c.transport), remote.WithRetryBackoff(remote.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 1})}
}
func (c *Client) reference(value string) (name.Digest, error) {
	if !strings.HasPrefix(value, c.host+"/") || !strings.Contains(value, "@sha256:") {
		return name.Digest{}, ErrInvalid
	}
	reference, err := name.NewDigest(value, name.StrictValidation)
	if err != nil || reference.RegistryStr() != c.host || !strings.HasPrefix(reference.DigestStr(), "sha256:") || len(reference.DigestStr()) != 71 {
		return name.Digest{}, ErrInvalid
	}
	return reference, nil
}
func safeError(err error) error {
	for _, known := range []error{ErrUnsafeTarget, ErrTooLarge, ErrDigest, ErrPlatform, ErrManifest, ErrInvalid} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return ErrNetwork
}
