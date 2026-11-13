package authorization

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/model"
	"gorm.io/gorm"
)

// Access 串行化权限变更与本实例业务授权，避免提交撤权后仍提交使用旧角色的操作。
// 首期仅单控制面；多控制面必须先补数据库授权版本/租约协议。
type Access struct {
	DB            *gorm.DB
	Policy        *Policy
	mu            sync.RWMutex
	verifierMu    sync.RWMutex
	oauthVerifier func(*gorm.DB, Principal) (Principal, error)
}

func NewAccess(db *gorm.DB) (*Access, error) {
	policy, err := NewPolicy()
	if err != nil {
		return nil, err
	}
	return &Access{DB: db, Policy: policy}, nil
}

// SetOAuthVerifier 只在控制面装配时注入可信协议模块，未注入时OAuth身份失败关闭。
func (a *Access) SetOAuthVerifier(verifier func(*gorm.DB, Principal) (Principal, error)) {
	a.verifierMu.Lock()
	defer a.verifierMu.Unlock()
	a.oauthVerifier = verifier
}

// Do 将身份复核、授权、业务写入和审计放在同一事务中。
// exclusive 用于成员、用户、凭证等权限事实的变更。
func (a *Access) Do(ctx context.Context, supplied Principal, exclusive bool, fn func(*gorm.DB, Principal) error) error {
	if exclusive {
		a.mu.Lock()
		defer a.mu.Unlock()
	} else {
		a.mu.RLock()
		defer a.mu.RUnlock()
	}
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := a.Refresh(tx, supplied)
		if err != nil {
			return err
		}
		return fn(tx, current)
	})
}

// DoInTx 在已有业务事务内复核权限；调用方在回调中提交，不额外占用连接。
// 适用于有界文件传输最终提交，长传输阶段不持本实例权限锁。
func (a *Access) DoInTx(ctx context.Context, supplied Principal, tx *gorm.DB, fn func(*gorm.DB, Principal) error) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	current, err := a.Refresh(tx.WithContext(ctx), supplied)
	if err != nil {
		return err
	}
	return fn(tx.WithContext(ctx), current)
}

// PublicMutation 仅供初始化、邀请消费、密码重设等持有专用凭证的入口使用。
// 调用方仍须在事务内检查一次性凭证，不能用它绕开已登录操作的授权。
func (a *Access) PublicMutation(ctx context.Context, fn func(*gorm.DB) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.DB.WithContext(ctx).Transaction(fn)
}

func (a *Access) Refresh(tx *gorm.DB, supplied Principal) (Principal, error) {
	if supplied.UserID == "" {
		return Principal{}, ErrUnauthenticated
	}
	var user model.User
	if err := tx.First(&user, "id = ?", supplied.UserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Principal{}, ErrUnauthenticated
		}
		return Principal{}, err
	}
	if user.Status != model.UserActive || user.AuthVersion != supplied.AuthVersion {
		return Principal{}, ErrUnauthenticated
	}
	current := Principal{UserID: user.ID, AuthVersion: user.AuthVersion, InstanceAdmin: user.InstanceAdmin}
	if supplied.AuthKind == "oauth" {
		a.verifierMu.RLock()
		verifier := a.oauthVerifier
		a.verifierMu.RUnlock()
		if verifier == nil || supplied.OAuthGrantID == "" || supplied.OAuthTokenSignature == "" || supplied.Audience == "" {
			return Principal{}, ErrUnauthenticated
		}
		verified, err := verifier(tx, supplied)
		if err != nil {
			return Principal{}, err
		}
		if verified.UserID != user.ID || verified.AuthVersion != user.AuthVersion || verified.AuthKind != "oauth" || verified.OAuthGrantID != supplied.OAuthGrantID || verified.OAuthTokenSignature != supplied.OAuthTokenSignature || verified.Audience != supplied.Audience {
			return Principal{}, ErrUnauthenticated
		}
		verified.InstanceAdmin = user.InstanceAdmin
		return verified, nil
	}
	if supplied.AuthKind != "" && supplied.AuthKind != "pat" {
		return Principal{}, ErrUnauthenticated
	}
	if supplied.IsBearer() {
		var token model.AccessToken
		if err := tx.First(&token, "id = ? AND user_id = ?", supplied.TokenID, user.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return Principal{}, ErrUnauthenticated
			}
			return Principal{}, err
		}
		if token.RevokedAt != nil || !token.ExpiresAt.After(time.Now()) || token.AuthVersion != user.AuthVersion {
			return Principal{}, ErrUnauthenticated
		}
		var links []model.AccessTokenApplication
		if err := tx.Where("token_id = ?", token.ID).Find(&links).Error; err != nil {
			return Principal{}, err
		}
		current.TokenID, current.Audience, current.Scopes = token.ID, supplied.Audience, token.Scopes
		for _, link := range links {
			current.ApplicationIDs = append(current.ApplicationIDs, link.ApplicationID)
		}
	}
	return current, nil
}

func (a *Access) RequireInstance(principal Principal) error {
	// 应用范围凭证不继承实例管理权限，即便其所有者是实例管理员。
	if !principal.InstanceAdmin || principal.IsBearer() {
		return ErrDenied
	}
	return nil
}

func (a *Access) WorkspaceRole(tx *gorm.DB, principal Principal, workspaceID string) (string, error) {
	var workspace model.Workspace
	if err := tx.First(&workspace, "id = ? AND status = ?", workspaceID, model.ResourceActive).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	if principal.InstanceAdmin {
		return "instance_admin", nil
	}
	var member model.WorkspaceMember
	if err := tx.First(&member, "workspace_id = ? AND user_id = ?", workspaceID, principal.UserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	if member.Role == model.WorkspaceAdmin {
		return "workspace_admin", nil
	}
	return "member", nil
}

func (a *Access) RequireWorkspaceAdmin(tx *gorm.DB, principal Principal, workspaceID string) error {
	if principal.IsBearer() {
		return ErrDenied
	}
	role, err := a.WorkspaceRole(tx, principal, workspaceID)
	if err != nil {
		return err
	}
	if role != "workspace_admin" && role != "instance_admin" {
		return ErrDenied
	}
	return nil
}

func (a *Access) Application(tx *gorm.DB, principal Principal, workspaceID, applicationID string, action Action) (model.Application, string, error) {
	var application model.Application
	if err := tx.First(&application, "id = ? AND workspace_id = ? AND status = ?", applicationID, workspaceID, model.ResourceActive).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return application, "", ErrNotFound
		}
		return application, "", err
	}
	role, err := a.WorkspaceRole(tx, principal, workspaceID)
	if err != nil {
		return application, "", err
	}
	if role == "member" {
		var member model.ApplicationMember
		if err := tx.First(&member, "workspace_id = ? AND application_id = ? AND user_id = ?", workspaceID, applicationID, principal.UserID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application, "", ErrNotFound
			}
			return application, "", err
		}
		role = member.Role
	}
	if !a.Policy.Allows(role, action) || !principal.AllowsScope(applicationID, action) {
		return application, role, ErrNotFound
	}
	return application, role, nil
}

// ApplicationQuery 在数据库查询阶段过滤归属；不能查询全部业务数据后在前端隐藏。
func (a *Access) ApplicationQuery(tx *gorm.DB, principal Principal, workspaceID string) (*gorm.DB, error) {
	role, err := a.WorkspaceRole(tx, principal, workspaceID)
	if err != nil {
		return nil, err
	}
	query := tx.Model(&model.Application{}).Where("applications.workspace_id = ? AND applications.status = ?", workspaceID, model.ResourceActive)
	if role == "member" {
		query = query.Where("EXISTS (SELECT 1 FROM application_members am WHERE am.application_id = applications.id AND am.workspace_id = applications.workspace_id AND am.user_id = ?)", principal.UserID)
	}
	if principal.IsBearer() {
		if !principal.AllowsAnyApplicationRead() {
			return query.Where("FALSE"), nil
		}
		query = query.Where("applications.id IN ?", principal.ApplicationIDs)
	}
	return query, nil
}

func (p Principal) AllowsAnyApplicationRead() bool {
	for _, scope := range p.Scopes {
		if scope == string(ApplicationRead) {
			return true
		}
	}
	return false
}
