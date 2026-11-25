package transport

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/sunshanpeng/yummy/internal/domains/oauth"
	"github.com/sunshanpeng/yummy/internal/domains/session"
	"github.com/sunshanpeng/yummy/internal/lib/security"
)

const (
	ExternalOIDCProvider = "team"
	OIDCCallbackPath     = "/auth/oidc/callback"
)

var oidcReturnRequest = regexp.MustCompile(`^req_[A-Za-z0-9_-]{20,100}$`)

type OIDCLogin struct {
	service  *oauth.Service
	sessions *session.Manager
	secure   bool
	enabled  bool
}

func NewOIDCLogin(service *oauth.Service, sessions *session.Manager, secure, enabled bool) *OIDCLogin {
	return &OIDCLogin{service, sessions, secure, enabled}
}

func (h *OIDCLogin) Status(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": h.enabled})
}

func (h *OIDCLogin) Start(w http.ResponseWriter, r *http.Request) {
	if !h.enabled {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var input struct {
		ReturnRequest string `json:"returnRequest"`
		Redirect      string `json:"redirect"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := h.sessions.AllowOIDCLogin(r.Context()); err != nil {
		if errors.Is(err, session.ErrRateLimited) {
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	destination := oidcLocalPath(input.Redirect)
	if oidcReturnRequest.MatchString(input.ReturnRequest) {
		destination = "/authorize?request_id=" + input.ReturnRequest
	}
	binding, err := security.NewToken("oidcb_")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	started, err := h.service.StartOIDC(r.Context(), ExternalOIDCProvider, binding)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	value := binding + "." + base64.RawURLEncoding.EncodeToString([]byte(destination))
	http.SetCookie(w, h.cookie(value, 300))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": started.URL})
}

func (h *OIDCLogin) Callback(w http.ResponseWriter, r *http.Request) {
	if !h.enabled {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	failure := func(destination string) {
		http.Redirect(w, r, oidcRetryPath(destination), http.StatusSeeOther)
	}
	cookie, err := r.Cookie(h.cookie("", 0).Name)
	if err != nil {
		failure("")
		return
	}
	http.SetCookie(w, h.cookie("", -1))
	binding, encoded, ok := strings.Cut(cookie.Value, ".")
	if !ok || len(encoded) > 2048 {
		failure("")
		return
	}
	destination, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		failure("")
		return
	}
	query := r.URL.Query()
	principal, err := h.service.CompleteOIDC(r.Context(), ExternalOIDCProvider, query.Get("state"), query.Get("code"), binding)
	if err != nil || h.sessions.Establish(r.Context(), principal) != nil {
		failure(string(destination))
		return
	}
	http.Redirect(w, r, oidcLocalPath(string(destination)), http.StatusSeeOther)
}

func (h *OIDCLogin) cookie(value string, maxAge int) *http.Cookie {
	name := "yummy_oidc_binding"
	if h.secure {
		name = "__Host-" + name
	}
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge}
}

func oidcLocalPath(raw string) string {
	path, err := url.ParseRequestURI(raw)
	if err != nil || len(raw) > 1024 || !strings.HasPrefix(path.Path, "/") || strings.HasPrefix(path.Path, "//") ||
		path.IsAbs() || path.Host != "" || strings.ContainsAny(path.Path, "\\\r\n\t") || path.Path == "/login" {
		return "/"
	}
	return path.String()
}

func oidcRetryPath(destination string) string {
	path := oidcLocalPath(destination)
	values := url.Values{"oidc_failed": {"true"}}
	if path != "/" {
		local, _ := url.ParseRequestURI(path)
		if id := local.Query().Get("request_id"); local.Path == "/authorize" && oidcReturnRequest.MatchString(id) {
			values.Set("return_request", id)
		} else {
			values.Set("redirect", path)
		}
	}
	return "/login?" + values.Encode()
}
