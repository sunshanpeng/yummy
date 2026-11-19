package transport

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/source"
)

// BuildGrantHandler 仅挂载于同一 HTTPS 站点的传输入口，不经过浏览器会话或普通 API Bearer。
// 只读取 Authorization 中的受限令牌，拒绝查询串和 Range，不泄漏任务或主机路径。
func BuildGrantHandler(service *source.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /transfer/build-grants/{grant}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		auth := r.Header.Values("Authorization")
		if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") || r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Range") != "" {
			http.Error(w, "源码授权无效", http.StatusUnauthorized)
			return
		}
		reader, grant, err := service.OpenBuildGrant(r.Context(), r.PathValue("grant"), strings.TrimPrefix(auth[0], "Bearer "))
		if err != nil {
			http.Error(w, "源码授权无效或不可用", http.StatusUnauthorized)
			return
		}
		defer reader.Close()
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute)); err != nil {
			http.Error(w, "传输服务不可用", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.FormatInt(grant.Size, 10))
		w.Header().Set("X-Source-SHA256", grant.SHA256)
		_, _ = io.CopyN(w, reader, grant.Size)
	})
	return mux
}
