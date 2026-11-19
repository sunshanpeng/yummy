package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,62}$`)
var ErrCredentials = errors.New("账号或密码错误")

// Service 不依赖 HTTP、Cookie 或 MCP，实现统一身份与凭证生命周期。
type Service struct {
	DB        *gorm.DB
	Access    *authorization.Access
	dummyHash string
}

func NewService(db *gorm.DB, access *authorization.Access) (*Service, error) {
	dummy, err := security.HashPassword("timing-only-invalid-password")
	if err != nil {
		return nil, err
	}
	return &Service{DB: db, Access: access, dummyHash: dummy}, nil
}

func normalizeUsername(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !usernamePattern.MatchString(value) {
		return "", fmt.Errorf("%w: 用户名需以小写字母开头，使用字母、数字、点、下划线或连字符，最长63字符", authorization.ErrInvalid)
	}
	return value, nil
}

func view(user model.User) UserView {
	return UserView{ID: user.ID, Username: user.Username, Status: user.Status, InstanceAdmin: user.InstanceAdmin, AuthVersion: user.AuthVersion}
}

func principal(user model.User) authorization.Principal {
	return authorization.Principal{UserID: user.ID, AuthVersion: user.AuthVersion, InstanceAdmin: user.InstanceAdmin}
}

// InitializeAdmin 只提供给本机管理命令，不得挂载匿名 HTTP 入口。
func (s *Service) InitializeAdmin(ctx context.Context, username, password string) (UserView, error) {
	username, err := normalizeUsername(username)
	if err != nil {
		return UserView{}, err
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return UserView{}, err
	}
	id, err := security.NewID("usr")
	if err != nil {
		return UserView{}, err
	}
	user := model.User{ID: id, Username: username, PasswordHash: hash, Status: model.UserActive, AuthVersion: 1, InstanceAdmin: true}
	err = s.Access.PublicMutation(ctx, func(tx *gorm.DB) error {
		// 所有初始化/全局管理员变更共用事务锁，不能靠先查询后插入保证唯一。
		if err := lockAdminMutation(tx); err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&model.User{}).Where("instance_admin = TRUE").Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("%w: 已初始化全局管理员", authorization.ErrConflict)
		}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		return audit.Record(tx, user.ID, "instance.initialize", "user", user.ID, nil, nil)
	})
	return view(user), err
}

func (s *Service) Authenticate(ctx context.Context, username, password string) (UserView, authorization.Principal, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	var user model.User
	err := s.DB.WithContext(ctx).First(&user, "username = ?", username).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return UserView{}, authorization.Principal{}, err
	}
	hash := user.PasswordHash
	if err != nil {
		hash = s.dummyHash
	}
	valid := security.VerifyPassword(hash, password)
	if err != nil || !valid || user.Status != model.UserActive {
		return UserView{}, authorization.Principal{}, ErrCredentials
	}
	if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return audit.Record(tx, user.ID, "account.login", "user", user.ID, nil, nil) }); err != nil {
		return UserView{}, authorization.Principal{}, err
	}
	return view(user), principal(user), nil
}

func (s *Service) Current(ctx context.Context, supplied authorization.Principal) (UserView, error) {
	var result UserView
	err := s.Access.Do(ctx, supplied, false, func(tx *gorm.DB, current authorization.Principal) error {
		var user model.User
		if err := tx.First(&user, "id = ?", current.UserID).Error; err != nil {
			return err
		}
		result = view(user)
		return nil
	})
	return result, err
}

func (s *Service) AuthenticatePAT(ctx context.Context, plaintext, audience string) (authorization.Principal, error) {
	if !strings.HasPrefix(plaintext, "ym_pat_") || len(plaintext) > 128 {
		return authorization.Principal{}, authorization.ErrUnauthenticated
	}
	var token model.AccessToken
	err := s.DB.WithContext(ctx).First(&token, "token_hash = ?", security.Digest(plaintext)).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return authorization.Principal{}, authorization.ErrUnauthenticated
		}
		return authorization.Principal{}, err
	}
	result := authorization.Principal{}
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.First(&user, "id = ?", token.UserID).Error; err != nil {
			return err
		}
		var err error
		result, err = s.Access.Refresh(tx, authorization.Principal{UserID: user.ID, AuthVersion: user.AuthVersion, TokenID: token.ID, Audience: audience})
		return err
	})
	return result, err
}

func (s *Service) ChangePassword(ctx context.Context, supplied authorization.Principal, oldPassword, newPassword string) error {
	if supplied.IsBearer() {
		return authorization.ErrDenied
	}
	hash, err := security.HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.Access.Do(ctx, supplied, true, func(tx *gorm.DB, p authorization.Principal) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", p.UserID).Error; err != nil {
			return err
		}
		if !security.VerifyPassword(user.PasswordHash, oldPassword) {
			return ErrCredentials
		}
		if err := tx.Model(&user).Updates(map[string]any{"password_hash": hash, "auth_version": gorm.Expr("auth_version + 1")}).Error; err != nil {
			return err
		}
		return audit.Record(tx, p.UserID, "account.password.change", "user", p.UserID, nil, nil)
	})
}

func (s *Service) ListUsers(ctx context.Context, supplied authorization.Principal) ([]UserView, error) {
	result := make([]UserView, 0)
	err := s.Access.Do(ctx, supplied, false, func(tx *gorm.DB, p authorization.Principal) error {
		if err := s.Access.RequireInstance(p); err != nil {
			return err
		}
		var users []model.User
		if err := tx.Order("username ASC").Limit(1000).Find(&users).Error; err != nil {
			return err
		}
		for _, user := range users {
			result = append(result, view(user))
		}
		return nil
	})
	return result, err
}

func (s *Service) SetUserStatus(ctx context.Context, supplied authorization.Principal, userID, status string) error {
	if status != model.UserActive && status != model.UserDisabled {
		return authorization.ErrInvalid
	}
	return s.Access.Do(ctx, supplied, true, func(tx *gorm.DB, p authorization.Principal) error {
		if err := s.Access.RequireInstance(p); err != nil {
			return err
		}
		if err := lockAdminMutation(tx); err != nil {
			return err
		}
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		if status == model.UserDisabled && user.InstanceAdmin && user.Status == model.UserActive {
			var count int64
			if err := tx.Model(&model.User{}).Where("instance_admin = TRUE AND status = ?", model.UserActive).Count(&count).Error; err != nil {
				return err
			}
			if count <= 1 {
				return fmt.Errorf("%w: 不能停用最后一位全局管理员", authorization.ErrConflict)
			}
		}
		if user.Status == status {
			return nil
		}
		if err := tx.Model(&user).Updates(map[string]any{"status": status, "auth_version": gorm.Expr("auth_version + 1")}).Error; err != nil {
			return err
		}
		return audit.Record(tx, p.UserID, "instance.user.status", "user", user.ID, nil, nil)
	})
}

func (s *Service) CreatePasswordReset(ctx context.Context, supplied authorization.Principal, userID string) (string, error) {
	plain, err := security.NewToken("ym_reset_")
	if err != nil {
		return "", err
	}
	id, err := security.NewID("rst")
	if err != nil {
		return "", err
	}
	err = s.Access.Do(ctx, supplied, true, func(tx *gorm.DB, p authorization.Principal) error {
		if err := s.Access.RequireInstance(p); err != nil {
			return err
		}
		var user model.User
		if err := tx.First(&user, "id = ? AND status = ?", userID, model.UserActive).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		if err := tx.Model(&model.PasswordReset{}).Where("user_id = ? AND consumed_at IS NULL", userID).Update("consumed_at", time.Now().UTC()).Error; err != nil {
			return err
		}
		if err := tx.Create(&model.PasswordReset{ID: id, UserID: userID, TokenHash: security.Digest(plain), ExpiresAt: time.Now().UTC().Add(30 * time.Minute)}).Error; err != nil {
			return err
		}
		// 发起重设立即使既有会话与凭证失效，管理员仍无法读取旧密码。
		if err := tx.Model(&user).Update("auth_version", gorm.Expr("auth_version + 1")).Error; err != nil {
			return err
		}
		return audit.Record(tx, p.UserID, "instance.user.password-reset", "user", userID, nil, nil)
	})
	if err != nil {
		return "", err
	}
	return plain, nil
}

func (s *Service) CompletePasswordReset(ctx context.Context, plaintext, password string) error {
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	return s.Access.PublicMutation(ctx, func(tx *gorm.DB) error {
		var reset model.PasswordReset
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&reset, "token_hash = ? AND consumed_at IS NULL AND expires_at > NOW()", security.Digest(plaintext)).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ? AND status = ?", reset.UserID, model.UserActive).Error; err != nil {
			return authorization.ErrNotFound
		}
		if err := tx.Model(&reset).Update("consumed_at", time.Now().UTC()).Error; err != nil {
			return err
		}
		if err := tx.Model(&user).Updates(map[string]any{"password_hash": hash, "auth_version": gorm.Expr("auth_version + 1")}).Error; err != nil {
			return err
		}
		return audit.Record(tx, user.ID, "account.password.reset", "user", user.ID, nil, nil)
	})
}

// WorkspaceSelfCreateEnabled 读取实例级"自由建空间"开关（2026-10-26 设计对齐：
// SaaS 形态允许登录身份自助建空间）。无实例行或查询失败时按关闭处理——该位只是
// 控制台入口的展示依据，授权仍由 CreateWorkspace 在事务内判定。
func (s *Service) WorkspaceSelfCreateEnabled(ctx context.Context) bool {
	var row struct {
		WorkspaceSelfCreate bool `gorm:"column:workspace_self_create"`
	}
	if err := s.DB.WithContext(ctx).Table("instance_settings").Select("workspace_self_create").Where("singleton = TRUE").Take(&row).Error; err != nil {
		return false
	}
	return row.WorkspaceSelfCreate
}
