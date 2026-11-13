package mcp

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/deployment"
)

type expectedInput struct {
	Generation       int64  `json:"generation"`
	ServingReleaseID string `json:"servingReleaseId" jsonschema:"首次发布时显式传空字符串"`
	ConfigRevision   int64  `json:"configRevision"`
}

func (e expectedInput) valid() bool {
	return e.Generation >= 0 && e.ConfigRevision >= 0 && (e.ServingReleaseID == "" || validID(e.ServingReleaseID))
}
func (e expectedInput) domain() deployment.Expected {
	return deployment.Expected{Generation: e.Generation, ServingReleaseID: e.ServingReleaseID, ConfigRevision: e.ConfigRevision}
}

type deployInput struct {
	ArtifactID             string        `json:"artifactId"`
	RuntimeConfigRevision  int64         `json:"runtimeConfigRevision"`
	ExpectedPolicyRevision int64         `json:"expectedPolicyRevision"`
	Expected               expectedInput `json:"expected"`
	IdempotencyKey         string        `json:"idempotencyKey"`
}
type rollbackInput struct {
	TargetReleaseID        string        `json:"targetReleaseId"`
	ExpectedPolicyRevision int64         `json:"expectedPolicyRevision"`
	Expected               expectedInput `json:"expected"`
	IdempotencyKey         string        `json:"idempotencyKey"`
}
type changeInput struct {
	Expected       expectedInput `json:"expected"`
	IdempotencyKey string        `json:"idempotencyKey"`
}

type releaseView struct {
	ID                 string    `json:"id"`
	ArtifactID         string    `json:"artifactId"`
	ArtifactDigest     string    `json:"artifactDigest"`
	ArtifactRepository string    `json:"artifactRepository"`
	Platform           string    `json:"platform"`
	ConfigRevision     int64     `json:"configRevision"`
	PreviousServingID  string    `json:"previousServingId"`
	RollbackOf         string    `json:"rollbackOf,omitempty"`
	Domains            []string  `json:"domains"`
	CreatedAt          time.Time `json:"createdAt"`
}
type deploymentView struct {
	Release     releaseView `json:"release"`
	OperationID string      `json:"operationId"`
	Status      string      `json:"status"`
	Action      string      `json:"action"`
	Generation  int64       `json:"generation"`
	Active      bool        `json:"active"`
	ErrorCode   string      `json:"errorCode,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
	FinishedAt  *time.Time  `json:"finishedAt,omitempty"`
}

func safeDeployment(v deployment.Result) deploymentView {
	return deploymentView{Release: releaseView{ID: v.Release.ID, ArtifactID: v.Release.Artifact.ID, ArtifactDigest: v.Release.Artifact.Digest, ArtifactRepository: v.Release.Artifact.Repository, Platform: v.Release.Artifact.Platform, ConfigRevision: v.Release.RuntimeConfigRevision, PreviousServingID: v.Release.PreviousServingID, RollbackOf: v.Release.RollbackOf, Domains: v.Release.Domains, CreatedAt: v.Release.CreatedAt}, OperationID: v.Operation.ID, Status: v.Operation.State, Action: v.Operation.Action, Generation: v.Operation.Generation, Active: v.Operation.Active, ErrorCode: v.Operation.ErrorCode, CreatedAt: v.Operation.CreatedAt, FinishedAt: v.Operation.FinishedAt}
}

func registerDeploymentTools(server *sdk.Server, s scope) {
	status := func(ctx context.Context, _ emptyInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.RuntimeRead); err != nil {
			return nil, err
		}
		v, err := s.services.Deployments.GetStatus(ctx, s.principal, s.workspaceID, s.applicationID)
		if err != nil {
			return nil, failure(err)
		}
		return v, nil
	}
	typedTool(server, "app_status", "读取应用当前期望状态、实际观察与正在服务的 release；缺测不会当作正常。", annotations(true, false, true, false), status)
	typedTool(server, "runtime_status", "读取应用运行状态及已生效版本。", annotations(true, false, true, false), status)
	typedTool(server, "deployment_create", "使用已验证产物和固定配置版本创建发布；返回操作状态而非提前宣告上线。", annotations(false, true, true, true), func(ctx context.Context, in deployInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.DeploymentWrite); err != nil {
			return nil, err
		}
		if !validID(in.ArtifactID) || in.RuntimeConfigRevision < 1 || in.ExpectedPolicyRevision < 1 || !in.Expected.valid() || in.Expected.ConfigRevision != in.RuntimeConfigRevision || !validKey(in.IdempotencyKey) {
			return nil, toolError("INVALID_ARGUMENT", "产物、版本或幂等键无效")
		}
		v, err := s.services.Deployments.CreateDeployment(ctx, s.principal, s.workspaceID, s.applicationID, deployment.CreateInput{ArtifactID: in.ArtifactID, RuntimeConfigRevision: in.RuntimeConfigRevision, ExpectedPolicyRevision: in.ExpectedPolicyRevision, Expected: in.Expected.domain(), IdempotencyKey: in.IdempotencyKey})
		if err != nil {
			return nil, failure(err)
		}
		return safeDeployment(v), nil
	})
	typedTool(server, "deployment_get", "读取当前应用的发布操作及其不可变 release。", annotations(true, false, true, false), func(ctx context.Context, in idInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.DeploymentRead); err != nil {
			return nil, err
		}
		if !validID(in.ID) {
			return nil, toolError("INVALID_ARGUMENT", "操作 ID 无效")
		}
		v, err := s.services.Deployments.Get(ctx, s.principal, s.workspaceID, s.applicationID, in.ID)
		if err != nil {
			return nil, failure(err)
		}
		return safeDeployment(v), nil
	})
	typedTool(server, "deployment_list", "分页列出当前应用发布操作和 release。", annotations(true, false, true, false), func(ctx context.Context, in pageInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.DeploymentRead); err != nil {
			return nil, err
		}
		if !validPage(in) {
			return nil, toolError("INVALID_ARGUMENT", "分页参数无效")
		}
		v, err := s.services.Deployments.List(ctx, s.principal, s.workspaceID, s.applicationID, in.Limit, in.Cursor)
		if err != nil {
			return nil, failure(err)
		}
		items := make([]deploymentView, 0, len(v.Items))
		for _, item := range v.Items {
			items = append(items, safeDeployment(item))
		}
		return struct {
			Items      []deploymentView `json:"items"`
			NextCursor string           `json:"nextCursor,omitempty"`
		}{items, v.NextCursor}, nil
	})
	typedTool(server, "deployment_rollback", "回滚到当前应用的历史成功 release；不会回退持久数据。", annotations(false, true, true, true), func(ctx context.Context, in rollbackInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.DeploymentWrite); err != nil {
			return nil, err
		}
		if !validID(in.TargetReleaseID) || in.ExpectedPolicyRevision < 1 || !in.Expected.valid() || !validKey(in.IdempotencyKey) {
			return nil, toolError("INVALID_ARGUMENT", "目标、版本或幂等键无效")
		}
		v, err := s.services.Deployments.Rollback(ctx, s.principal, s.workspaceID, s.applicationID, deployment.RollbackInput{TargetReleaseID: in.TargetReleaseID, ExpectedPolicyRevision: in.ExpectedPolicyRevision, Expected: in.Expected.domain(), IdempotencyKey: in.IdempotencyKey})
		if err != nil {
			return nil, failure(err)
		}
		return safeDeployment(v), nil
	})
	registerChange := func(name, description string, action authorization.Action, call func(context.Context, authorization.Principal, string, string, deployment.ChangeInput) (deployment.Result, error)) {
		typedTool(server, name, description, annotations(false, true, true, true), func(ctx context.Context, in changeInput) (any, *sdk.CallToolResult) {
			if err := s.check(action); err != nil {
				return nil, err
			}
			if !in.Expected.valid() || !validKey(in.IdempotencyKey) {
				return nil, toolError("INVALID_ARGUMENT", "预期版本或幂等键无效")
			}
			v, err := call(ctx, s.principal, s.workspaceID, s.applicationID, deployment.ChangeInput{Expected: in.Expected.domain(), IdempotencyKey: in.IdempotencyKey})
			if err != nil {
				return nil, failure(err)
			}
			return safeDeployment(v), nil
		})
	}
	registerChange("runtime_start", "启动当前应用服务。", authorization.RuntimeWrite, s.services.Deployments.Start)
	registerChange("runtime_stop", "停止当前应用服务；保留配置和持久数据。", authorization.RuntimeWrite, s.services.Deployments.Stop)
	registerChange("runtime_restart", "重启当前服务版本。", authorization.DeploymentWrite, s.services.Deployments.Restart)
}
