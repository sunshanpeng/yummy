package oauth

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/ory/fosite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Token 把 Fosite 的请求处理和响应生成放进同一 PG 事务，避免 PKCE 消费、
// authorization code 消费、refresh 轮换分散在不同事务；同 grant 行锁串行刷新族。
func (s *Service) Token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		s.provider.WriteAccessError(r.Context(), w, nil, fosite.ErrInvalidRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.provider.WriteAccessError(r.Context(), w, nil, fosite.ErrInvalidRequest)
		return
	}
	ctx, err := s.store.BeginTX(r.Context())
	if err != nil {
		s.provider.WriteAccessError(r.Context(), w, nil, fosite.ErrServerError)
		return
	}
	tx := s.store.database(ctx)
	defer tx.Rollback()
	// 内层 Fosite Transactional 调用复用事务，只有本 handler 负责最终提交。
	state := &transaction{db: tx}
	ctx = context.WithValue(ctx, txKey{}, state)
	grant, lookupErr := s.lockTokenGrant(ctx, r)
	if lookupErr != nil {
		s.provider.WriteAccessError(ctx, w, nil, lookupErr)
		return
	}
	if len(r.Form["resource"]) != 1 || r.Form.Get("resource") != grant.Resource {
		s.provider.WriteAccessError(ctx, w, nil, fosite.ErrInvalidRequest.WithHint("resource 与授权不匹配"))
		return
	}
	if requested := r.Form.Get("scope"); requested != "" {
		for _, scope := range slices.Compact(slices.Clone(splitScopes(requested))) {
			if !slices.Contains(grant.Scopes, scope) {
				s.provider.WriteAccessError(ctx, w, nil, fosite.ErrInvalidScope)
				return
			}
		}
	}
	if err := s.checkGrant(tx, grant); err != nil {
		s.provider.WriteAccessError(ctx, w, nil, fosite.ErrInvalidGrant)
		return
	}
	request, protocolErr := s.provider.NewAccessRequest(ctx, r, &session{})
	if protocolErr != nil {
		if state.securityMutation {
			if err := tx.Commit().Error; err != nil {
				protocolErr = fosite.ErrServerError
			}
		}
		s.provider.WriteAccessError(ctx, w, request, protocolErr)
		return
	}
	response, protocolErr := s.provider.NewAccessResponse(ctx, request)
	if protocolErr != nil {
		s.provider.WriteAccessError(ctx, w, request, protocolErr)
		return
	}
	if err := tx.Commit().Error; err != nil {
		s.provider.WriteAccessError(ctx, w, request, fosite.ErrServerError)
		return
	}
	s.provider.WriteAccessResponse(ctx, w, request, response)
}
func (s *Service) lockTokenGrant(ctx context.Context, r *http.Request) (grantRecord, error) {
	kind := "code"
	value := r.Form.Get("code")
	signature := s.strategy.AuthorizeCodeSignature(ctx, value)
	if r.Form.Get("grant_type") == "refresh_token" {
		kind = "refresh"
		value = r.Form.Get("refresh_token")
		signature = s.strategy.RefreshTokenSignature(ctx, value)
	} else if r.Form.Get("grant_type") != "authorization_code" {
		return grantRecord{}, fosite.ErrUnsupportedGrantType
	}
	if value == "" || len(value) > 4096 {
		return grantRecord{}, fosite.ErrInvalidGrant
	}
	var record protocolRecord
	if err := s.store.database(ctx).First(&record, "kind = ? AND signature = ?", kind, signature).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return grantRecord{}, fosite.ErrInvalidGrant
		}
		return grantRecord{}, fosite.ErrServerError
	}
	var grant grantRecord
	err := s.store.database(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&grant, "id = ?", record.RequestID).Error
	if err != nil {
		return grant, fosite.ErrInvalidGrant
	}
	// 仅凭随机签名后缀不允许触发刷新族撤销；先由 Fosite 策略验证完整 HMAC。
	req, err := s.store.decode(ctx, record.Payload)
	if err != nil {
		return grant, fosite.ErrInvalidGrant
	}
	if kind == "refresh" {
		err = s.strategy.ValidateRefreshToken(ctx, req, value)
	} else {
		err = s.strategy.ValidateAuthorizeCode(ctx, req, value)
	}
	if err != nil {
		return grant, fosite.ErrInvalidGrant
	}
	if r.Form.Get("client_id") != grant.ClientID {
		return grant, fosite.ErrInvalidClient
	}
	return grant, nil
}
func (s *Service) Revoke(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		s.provider.WriteRevocationResponse(r.Context(), w, fosite.ErrInvalidRequest)
		return
	}
	ctx, err := s.store.BeginTX(r.Context())
	if err != nil {
		s.provider.WriteRevocationResponse(r.Context(), w, fosite.ErrServerError)
		return
	}
	tx := s.store.database(ctx)
	defer tx.Rollback()
	ctx = context.WithValue(ctx, txKey{}, &transaction{db: tx})
	value := r.Form.Get("token")
	if value == "" || len(value) > 4096 {
		s.provider.WriteRevocationResponse(ctx, w, nil)
		return
	}
	// Fosite 的撤销 handler 使用签名定位会话；先校验完整 token HMAC，防仅知道
	// 数据库签名的人通过拼接随机 token 内容触发合法授权的撤销。
	valid := false
	for _, kind := range []string{"access", "refresh"} {
		var signature string
		if kind == "access" {
			signature = s.strategy.AccessTokenSignature(ctx, value)
		} else {
			signature = s.strategy.RefreshTokenSignature(ctx, value)
		}
		request, lookupErr := s.store.get(ctx, kind, signature)
		if request == nil || lookupErr != nil && !errors.Is(lookupErr, fosite.ErrInactiveToken) {
			continue
		}
		if kind == "access" {
			err = s.strategy.ValidateAccessToken(ctx, request, value)
		} else {
			err = s.strategy.ValidateRefreshToken(ctx, request, value)
		}
		if err == nil {
			valid = true
			break
		}
	}
	if !valid {
		s.provider.WriteRevocationResponse(ctx, w, nil)
		return
	}
	err = s.provider.NewRevocationRequest(ctx, r.WithContext(ctx))
	if err == nil {
		err = tx.Commit().Error
		if err != nil {
			err = fosite.ErrServerError
		}
	}
	s.provider.WriteRevocationResponse(ctx, w, err)
}
