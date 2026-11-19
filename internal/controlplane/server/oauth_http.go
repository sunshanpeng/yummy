package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/oauth"
)

// consentResponse 收集 OAuth 同意结果。授权回调由 Fosite 根据服务端保存、已校验的请求生成，
// 而非读取表单中的 URL；前端接收后再导航，避免 fetch 默认跟随并泄露授权码。
type consentResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *consentResponse) Header() http.Header { return w.header }
func (w *consentResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *consentResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

// connectionsHandler 只接受浏览器会话；全局管理员也只能管理自己拥有的连接。
func connectionsHandler(service *oauth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		principal, err := authorization.FromContext(r.Context())
		if err != nil || principal.IsBearer() {
			http.Error(w, "请使用浏览器会话登录", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			values, err := service.ListConnections(r.Context(), principal)
			if err != nil {
				http.Error(w, "无法读取个人连接", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"connections": values})
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var input struct {
			ID string `json:"id"`
		}
		if err := decoder.Decode(&input); err != nil || !strings.HasPrefix(input.ID, "grt_") || len(input.ID) > 128 {
			http.Error(w, "连接标识无效", http.StatusBadRequest)
			return
		}
		if err := service.RevokeConnection(r.Context(), principal, input.ID); err != nil {
			if errors.Is(err, authorization.ErrNotFound) {
				http.Error(w, "连接不存在或无权访问", http.StatusNotFound)
			} else {
				http.Error(w, "无法撤销个人连接", http.StatusServiceUnavailable)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func consentHandler(service *oauth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			service.Consent(w, r)
			return
		}
		result := &consentResponse{header: make(http.Header)}
		service.Consent(result, r)
		for key, values := range result.header {
			if key != "Location" && key != "Content-Type" && key != "Content-Length" {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
		}
		if result.status >= 300 && result.status < 400 && result.header.Get("Location") != "" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]string{"redirect": result.header.Get("Location")})
			return
		}
		if contentType := result.header.Get("Content-Type"); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if result.status != 0 {
			w.WriteHeader(result.status)
		}
		_, _ = result.body.WriteTo(w)
	}
}
