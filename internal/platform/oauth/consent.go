package oauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ory/fosite"
	"github.com/sunshanpeng/yummy/internal/platform/audit"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/model"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	platformsession "github.com/sunshanpeng/yummy/internal/platform/session"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func splitScopes(value string) []string { return strings.Fields(value) }
func (s *Service) Authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if len(r.URL.RawQuery) > 16<<10 {
		http.Error(w, "请求过大", 400)
		return
	}
	values := r.URL.Query()
	if len(values["resource"]) != 1 || values.Get("audience") != "" {
		writeAuthorizeFailure(w, fosite.ErrInvalidRequest)
		return
	}
	if _, err := s.resourceApplications(values.Get("resource")); err != nil {
		writeAuthorizeFailure(w, err)
		return
	}
	// scope 缺省只读，写权限必须由客户端明确请求、用户再次同意。
	if values.Get("scope") == "" {
		values.Set("scope", "application:read build:read logs:read runtime:read")
	}
	if err := s.ensureClient(r.Context(), values.Get("client_id"), values.Get("resource")); err != nil {
		writeAuthorizeFailure(w, fosite.ErrInvalidClient)
		return
	}
	copy := r.Clone(r.Context())
	copy.URL = cloneURL(r.URL)
	copy.URL.RawQuery = values.Encode()
	request, err := s.provider.NewAuthorizeRequest(r.Context(), copy)
	if err != nil {
		s.provider.WriteAuthorizeError(r.Context(), w, request, err)
		return
	}
	if !request.GetResponseTypes().ExactOne("code") || values.Get("code_challenge_method") != "S256" || values.Get("code_challenge") == "" {
		s.provider.WriteAuthorizeError(r.Context(), w, request, fosite.ErrInvalidRequest)
		return
	}
	id, err := security.NewToken("req_")
	if err != nil {
		http.Error(w, "授权服务不可用", 500)
		return
	}
	record := consentRecord{ID: id, Parameters: values, ExpiresAt: time.Now().Add(5 * time.Minute)}
	if err := s.db.WithContext(r.Context()).Create(&record).Error; err != nil {
		http.Error(w, "授权服务不可用", 500)
		return
	}
	if _, err := browserPrincipal(r.Context()); err != nil {
		http.Redirect(w, r, "/login?return_request="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/authorize?request_id="+url.QueryEscape(id), http.StatusSeeOther)
}
func cloneURL(value *url.URL) *url.URL { copy := *value; return &copy }

// writeAuthorizeFailure 处理无法安全构造 fosite 授权请求的失败：此时 client_id 或
// redirect_uri 尚未核实，绝不能向未信任的回调重定向，也不得以 nil 请求调用 fosite
// 写错误（其内部解引用会 panic 变成 500）。直接返回标准 OAuth 错误形态的 400。
func writeAuthorizeFailure(w http.ResponseWriter, err error) {
	code := "invalid_request"
	if errors.Is(err, fosite.ErrInvalidClient) {
		code = "invalid_client"
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
func browserPrincipal(ctx context.Context) (authorization.Principal, error) {
	p, err := authorization.FromContext(ctx)
	if err != nil || p.IsBearer() {
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	return p, nil
}

type ConsentView struct {
	RequestID             string              `json:"requestId"`
	CSRFToken             string              `json:"csrfToken"`
	FormCSRFToken         string              `json:"formCsrfToken"`
	ClientID              string              `json:"clientId"`
	ClientName            string              `json:"clientName"`
	ClientSource          string              `json:"clientSource"`
	Resource              string              `json:"resource"`
	Scopes                []string            `json:"scopes"`
	Applications          []string            `json:"applications"`
	OfflineAccess         bool                `json:"offlineAccess"`
	AvailableApplications []ApplicationChoice `json:"availableApplications"`
}
type ApplicationChoice struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	Name        string `json:"name"`
}

// Consent GET 返回审查DTO供正式前端渲染；POST 只接受 requestID/CSRF/decision/offlineAccess/
// app IDs，client/resource/redirect/scopes 均从服务端保存的已验证请求恢复。
func (s *Service) Consent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// fetch 默认会跟随 OAuth 成功后的跨站回调；返回地址前先确认客户端已明确请求手动跳转。
	if r.Method == http.MethodPost && r.Header.Get("X-Yummy-Response-Mode") != "manual" {
		http.Error(w, "必须显式使用手动重定向", http.StatusBadRequest)
		return
	}
	p, err := browserPrincipal(r.Context())
	if err != nil {
		http.Error(w, "请先登录", 401)
		return
	}
	if r.Method == "GET" {
		s.consentView(w, r, p)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	// 同意表单由浏览器 FormData 以 multipart 提交，也兼容传统 urlencoded；
	// 只用 ParseForm 会读不到 multipart 的 csrf_token，导致真实浏览器同意流全部 403。
	if err := r.ParseMultipartForm(16 << 10); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		http.Error(w, "请求无效", 400)
		return
	}
	if r.Header.Get("Origin") != s.base {
		http.Error(w, "请求来源不匹配", 403)
		return
	}
	var authorize fosite.AuthorizeRequester
	var response fosite.AuthorizeResponder
	err = s.access.Do(r.Context(), p, false, func(tx *gorm.DB, current authorization.Principal) error {
		var stored consentRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&stored, "id = ?", r.Form.Get("request_id")).Error; err != nil {
			return authorization.ErrNotFound
		}
		if stored.UserID == nil || *stored.UserID != current.UserID || stored.ConsumedAt != nil || !stored.ExpiresAt.After(time.Now()) || subtle.ConstantTimeCompare([]byte(stored.CSRFHash), []byte(security.Digest(r.Form.Get("csrf_token")))) != 1 {
			return authorization.ErrDenied
		}
		ctx := context.WithValue(r.Context(), txKey{}, &transaction{db: tx})
		req := &http.Request{Method: "GET", URL: &url.URL{Scheme: "https", Host: "placeholder", Path: "/oauth/authorize", RawQuery: stored.Parameters.Encode()}, Header: http.Header{}}
		var err error
		authorize, err = s.provider.NewAuthorizeRequest(ctx, req)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if r.Form.Get("decision") != "allow" {
			if err := tx.Model(&stored).Update("consumed_at", now).Error; err != nil {
				return err
			}
			return nil
		}
		appIDs, err := s.selectApplications(tx, current, stored.Parameters.Get("resource"), r.Form["application_id"])
		if err != nil {
			return err
		}
		scopes := slices.Clone(authorize.GetRequestedScopes())
		if slices.Contains(scopes, OfflineAccess) && r.Form.Get("offline_access") != "true" {
			scopes = slices.DeleteFunc(scopes, func(value string) bool { return value == OfflineAccess })
		}
		if err := s.checkScopePermission(tx, current, appIDs, scopes); err != nil {
			return err
		}
		id, err := security.NewID("grt")
		if err != nil {
			return err
		}
		grant := grantRecord{ID: id, UserID: current.UserID, ClientID: authorize.GetClient().GetID(), Resource: stored.Parameters.Get("resource"), ApplicationIDs: appIDs, Scopes: scopes, AuthVersion: current.AuthVersion, CreatedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour)}
		if err := tx.Create(&grant).Error; err != nil {
			return err
		}
		authorize.SetID(id)
		for _, scope := range scopes {
			authorize.GrantScope(scope)
		}
		authorize.GrantAudience(grant.Resource)
		sess := &session{DefaultSession: fosite.DefaultSession{Subject: current.UserID}, GrantID: id, Resource: grant.Resource, AuthVersion: current.AuthVersion, ExpiresAtAbsolute: grant.ExpiresAt}
		response, err = s.provider.NewAuthorizeResponse(ctx, authorize, sess)
		if err != nil {
			return err
		}
		if err := tx.Model(&stored).Update("consumed_at", now).Error; err != nil {
			return err
		}
		return audit.Record(tx, current.UserID, "account.oauth.authorize", "oauth_grant", id, nil, nil)
	})
	if err != nil {
		if authorize != nil {
			s.provider.WriteAuthorizeError(r.Context(), w, authorize, fosite.ErrAccessDenied)
		} else {
			http.Error(w, "授权请求无效或已过期", 403)
		}
		return
	}
	if response == nil {
		s.provider.WriteAuthorizeError(r.Context(), w, authorize, fosite.ErrAccessDenied)
		return
	}
	// RFC9207 返回显式 issuer，CLI 必须同时核对 state 和 issuer。
	response.GetParameters().Set("iss", s.base)
	s.provider.WriteAuthorizeResponse(r.Context(), w, authorize, response)
}
func (s *Service) consentView(w http.ResponseWriter, r *http.Request, p authorization.Principal) {
	var view ConsentView
	err := s.access.Do(r.Context(), p, false, func(tx *gorm.DB, current authorization.Principal) error {
		var stored consentRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&stored, "id = ?", r.URL.Query().Get("request_id")).Error; err != nil {
			return authorization.ErrNotFound
		}
		if stored.ConsumedAt != nil || !stored.ExpiresAt.After(time.Now()) || stored.UserID != nil && *stored.UserID != current.UserID {
			return authorization.ErrDenied
		}
		csrf, err := security.NewToken("csrf_")
		if err != nil {
			return err
		}
		if err := tx.Model(&stored).Updates(map[string]any{"user_id": current.UserID, "csrf_hash": security.Digest(csrf)}).Error; err != nil {
			return err
		}
		var client clientRecord
		if err := tx.First(&client, "id = ? AND active = TRUE", stored.Parameters.Get("client_id")).Error; err != nil {
			return err
		}
		apps, err := s.selectApplications(tx, current, stored.Parameters.Get("resource"), nil)
		if err != nil && stored.Parameters.Get("resource") != s.base+"/api" {
			return err
		}
		view = ConsentView{RequestID: stored.ID, CSRFToken: csrf, ClientID: client.ID, ClientName: client.Name, ClientSource: client.Source, Resource: stored.Parameters.Get("resource"), Scopes: splitScopes(stored.Parameters.Get("scope")), Applications: apps, OfflineAccess: slices.Contains(splitScopes(stored.Parameters.Get("scope")), OfflineAccess), FormCSRFToken: platformsession.CSRFToken(r.Context())}
		if stored.Parameters.Get("resource") == s.base+"/api" {
			query := tx.Model(&model.Application{}).Where("applications.status = ?", model.ResourceActive)
			if !current.InstanceAdmin {
				query = query.Where("EXISTS (SELECT 1 FROM workspace_members wm WHERE wm.workspace_id=applications.workspace_id AND wm.user_id=? AND (wm.role='admin' OR EXISTS(SELECT 1 FROM application_members am WHERE am.workspace_id=applications.workspace_id AND am.application_id=applications.id AND am.user_id=wm.user_id)))", current.UserID)
			}
			var values []model.Application
			if err := query.Order("applications.id").Limit(100).Find(&values).Error; err != nil {
				return err
			}
			for _, app := range values {
				view.AvailableApplications = append(view.AvailableApplications, ApplicationChoice{ID: app.ID, WorkspaceID: app.WorkspaceID, Name: app.Name})
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, "授权请求无效或已过期", 403)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}
func (s *Service) selectApplications(tx *gorm.DB, p authorization.Principal, resource string, selected []string) ([]string, error) {
	fixed, err := s.resourceApplications(resource)
	if err != nil {
		return nil, err
	}
	if len(fixed) > 0 {
		if len(selected) > 0 && !slices.Equal(selected, fixed) {
			return nil, authorization.ErrDenied
		}
		selected = fixed
	}
	if len(selected) == 0 || len(selected) > 100 {
		return nil, authorization.ErrInvalid
	}
	result := []string{}
	for _, id := range selected {
		var app model.Application
		if err := tx.First(&app, "id = ?", id).Error; err != nil {
			return nil, authorization.ErrNotFound
		}
		if _, _, err := s.access.Application(tx, p, app.WorkspaceID, id, authorization.ApplicationRead); err != nil {
			return nil, err
		}
		if !slices.Contains(result, id) {
			result = append(result, id)
		}
	}
	return result, nil
}
func (s *Service) checkScopePermission(tx *gorm.DB, p authorization.Principal, apps, scopes []string) error {
	for _, scope := range scopes {
		if scope == OfflineAccess {
			continue
		}
		if !slices.Contains(authorization.TokenScopes, scope) {
			return authorization.ErrDenied
		}
		for _, id := range apps {
			var app model.Application
			if err := tx.First(&app, "id = ?", id).Error; err != nil {
				return err
			}
			if _, _, err := s.access.Application(tx, p, app.WorkspaceID, id, authorization.Action(scope)); err != nil {
				return err
			}
		}
	}
	return nil
}
