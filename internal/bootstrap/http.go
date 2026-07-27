package bootstrap

import (
	"context"
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
	return root
}

func spaHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filePath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if _, err := fs.Stat(assets, filePath); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		indexRequest := r.Clone(r.Context())
		indexRequest.URL.Path = "/"
		fileServer.ServeHTTP(w, indexRequest)
	})
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
