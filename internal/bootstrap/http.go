package bootstrap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/control"
	transport "github.com/sunshanpeng/yummy/internal/transport/connectrpc"
)

func newPublicHandler(service *control.Service, assets fs.FS) http.Handler {
	api := http.NewServeMux()
	path, handler := transport.NewControlHandler(service)
	api.Handle(path, handler)

	root := http.NewServeMux()
	root.Handle("/api/", http.StripPrefix("/api", api))
	if assets != nil {
		root.Handle("/", spaHandler(assets))
	} else {
		root.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("multi-topology demo control plane\n"))
		})
	}
	return securityHeaders(root)
}

// contentSecurityPolicy 只允许加载同源资源；Vite 构建产物不包含内联脚本和样式。
const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; object-src 'none'; " +
	"frame-ancestors 'none'; form-action 'self'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func spaHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filePath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if info, err := fs.Stat(assets, filePath); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		// 目录和不存在的路径都返回 index.html：既支持前端路由，也不暴露目录列表。
		indexRequest := r.Clone(r.Context())
		indexRequest.URL.Path = "/"
		fileServer.ServeHTTP(w, indexRequest)
	})
}

// newPublicServer 创建页面与公共 API 的 HTTP 服务。公共 API 只有 Unary 调用，
// 可以对整个请求和响应设置超时。
func newPublicServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

// newAgentServer 创建 worker 使用的 mTLS HTTP/2 服务。worker 保持长期双向流，
// 因此不设置 ReadTimeout/WriteTimeout，改用 HTTP/2 ping 检测失效连接。
func newAgentServer(addr string, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: 16,
			SendPingTimeout:      30 * time.Second,
			PingTimeout:          15 * time.Second,
			WriteByteTimeout:     30 * time.Second,
		},
	}
}

type serverProcess struct {
	server *http.Server
	serve  func() error
}

func runServers(ctx context.Context, processes ...serverProcess) error {
	errorsChannel := make(chan error, len(processes))
	for _, process := range processes {
		go func() {
			errorsChannel <- process.serve()
		}()
	}

	var runError error
	select {
	case <-ctx.Done():
	case err := <-errorsChannel:
		if !errors.Is(err, http.ErrServerClosed) {
			runError = err
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, process := range processes {
		if err := process.server.Shutdown(shutdownContext); err != nil {
			_ = process.server.Close()
			if runError == nil {
				runError = fmt.Errorf("关闭 HTTP 服务: %w", err)
			}
		}
	}
	return runError
}
