package registryclient

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type guardTransport struct {
	registry     string
	allowed      map[string]bool
	denied       map[string]bool
	allowPrivate bool
	maxBytes     int64
	inner        http.RoundTripper
}

func (g *guardTransport) safeIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip.Equal(net.ParseIP("100.100.100.200")) {
		return false
	}
	if !g.allowPrivate && (ip.IsPrivate() || ip.IsLoopback()) {
		return false
	}
	return true
}
func (g *guardTransport) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrUnsafeTarget
	}
	if !g.allowed[strings.ToLower(address)] && !g.allowed[strings.ToLower(host)] {
		return nil, ErrUnsafeTarget
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, ErrNetwork
	}
	if len(ips) == 0 {
		return nil, ErrNetwork
	}
	for _, ip := range ips {
		if !g.safeIP(ip.IP) || g.denied[ip.IP.String()] {
			return nil, ErrUnsafeTarget
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	// 直接拨打已验证 IP，避免检查后再由传输层重新解析 DNS。
	var last error
	for _, ip := range ips {
		connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return connection, nil
		}
		last = err
	}
	return nil, last
}
func (g *guardTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	host := strings.ToLower(request.URL.Host)
	if request.URL.Scheme != "https" || request.URL.User != nil || !g.allowed[host] || g.denied[host] || g.denied[strings.ToLower(request.URL.Hostname())] {
		return nil, ErrUnsafeTarget
	}
	if request.Host != "" && !strings.EqualFold(request.Host, request.URL.Host) {
		return nil, ErrUnsafeTarget
	}
	response, err := g.inner.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	// 禁止所有重定向，避免认证 realm 或 blob redirect 携凭据跳到不同服务。
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		_ = response.Body.Close()
		return nil, ErrUnsafeTarget
	}
	if response.ContentLength > g.maxBytes {
		_ = response.Body.Close()
		return nil, ErrTooLarge
	}
	response.Body = &boundedBody{inner: response.Body, remaining: g.maxBytes}
	return response, nil
}

type boundedBody struct {
	inner     io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(buffer []byte) (int, error) {
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.inner.Read(probe[:])
		if n > 0 {
			return 0, ErrTooLarge
		}
		return 0, err
	}
	if int64(len(buffer)) > b.remaining {
		buffer = buffer[:b.remaining]
	}
	n, err := b.inner.Read(buffer)
	b.remaining -= int64(n)
	return n, err
}
func (b *boundedBody) Close() error { return b.inner.Close() }
