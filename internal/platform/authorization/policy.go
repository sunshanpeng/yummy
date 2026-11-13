package authorization

import (
	"context"
	"errors"
	"slices"

	"github.com/casbin/casbin/v2"
	casbinmodel "github.com/casbin/casbin/v2/model"
)

type Action string

const (
	ApplicationRead  Action = "application:read"
	ApplicationWrite Action = "application:write"
	SourceRead       Action = "source:read"
	SourceUpload     Action = "source:upload"
	BuildRead        Action = "build:read"
	BuildWrite       Action = "build:write"
	LogsRead         Action = "logs:read"
	DeploymentRead   Action = "deployment:read"
	DeploymentWrite  Action = "deployment:write"
	RuntimeRead      Action = "runtime:read"
	RuntimeWrite     Action = "runtime:write"
	ConfigRead       Action = "config:read"
	ConfigWrite      Action = "config:write"
	SecretWrite      Action = "secret:write"
	MemberWrite      Action = "member:write"
)

var applicationActions = []Action{
	ApplicationRead, ApplicationWrite, SourceRead, SourceUpload, BuildRead, BuildWrite,
	LogsRead, DeploymentRead, DeploymentWrite, RuntimeRead, RuntimeWrite, ConfigRead, ConfigWrite, SecretWrite, MemberWrite,
}

var TokenScopes = []string{
	"application:read", "source:read", "source:upload", "build:read", "build:write", "logs:read",
	"deployment:read", "deployment:write", "runtime:read", "runtime:write", "config:read", "config:write",
}

var (
	ErrUnauthenticated = errors.New("请先登录")
	ErrDenied          = errors.New("没有执行此操作的权限")
	ErrNotFound        = errors.New("资源不存在或无权访问")
	ErrConflict        = errors.New("数据已变化，请刷新后重试")
	ErrInvalid         = errors.New("输入无效")
)

// Principal 由认证层构造；HTTP/MCP 请求不能直接提交角色或可信身份。
type Principal struct {
	UserID              string
	AuthVersion         int64
	InstanceAdmin       bool
	TokenID             string
	AuthKind            string
	OAuthGrantID        string
	OAuthTokenSignature string
	Audience            string
	Scopes              []string
	ApplicationIDs      []string
}

func (p Principal) IsBearer() bool { return p.TokenID != "" || p.AuthKind == "oauth" }

func (p Principal) AllowsScope(applicationID string, action Action) bool {
	if !p.IsBearer() {
		return true
	}
	return slices.Contains(p.ApplicationIDs, applicationID) && slices.Contains(p.Scopes, string(action))
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func FromContext(ctx context.Context) (Principal, error) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok || p.UserID == "" {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}

type Policy struct{ enforcer *casbin.SyncedEnforcer }

// 固定角色的动作交给 Casbin；成员关系每次从事务数据库读取，不保存可能陈旧的角色缓存。
func NewPolicy() (*Policy, error) {
	m, err := casbinmodel.NewModelFromString(`[request_definition]
r = role, action
[policy_definition]
p = role, action
[policy_effect]
e = some(where (p.eft == allow))
[matchers]
m = r.role == p.role && r.action == p.action`)
	if err != nil {
		return nil, err
	}
	e, err := casbin.NewSyncedEnforcer(m)
	if err != nil {
		return nil, err
	}
	roles := map[string][]Action{
		"viewer":          {ApplicationRead, BuildRead, LogsRead, DeploymentRead, RuntimeRead, ConfigRead},
		"developer":       {ApplicationRead, SourceRead, SourceUpload, BuildRead, BuildWrite, LogsRead, DeploymentRead, DeploymentWrite, RuntimeRead, ConfigRead},
		"maintainer":      {ApplicationRead, SourceRead, SourceUpload, BuildRead, BuildWrite, LogsRead, DeploymentRead, DeploymentWrite, RuntimeRead, RuntimeWrite, ConfigRead, ConfigWrite, SecretWrite},
		"workspace_admin": applicationActions,
		"instance_admin":  applicationActions,
	}
	for role, actions := range roles {
		for _, action := range actions {
			if _, err := e.AddPolicy(role, string(action)); err != nil {
				return nil, err
			}
		}
	}
	return &Policy{enforcer: e}, nil
}

func (p *Policy) Allows(role string, action Action) bool {
	allowed, err := p.enforcer.Enforce(role, string(action))
	return err == nil && allowed
}

func (p *Policy) Capabilities(role string, principal Principal, applicationID string) []string {
	result := make([]string, 0)
	for _, action := range applicationActions {
		if p.Allows(role, action) && principal.AllowsScope(applicationID, action) {
			result = append(result, string(action))
		}
	}
	return result
}
