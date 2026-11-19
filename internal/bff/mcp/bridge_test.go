package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/transport/clientcli"
)

// stubControlPlane 是对照 clientcli 测试约定的 httptest 假控制面：记录请求与
// Authorization 头，可按 failCode 注入权限或传输错误，不访问真实网络。
type stubControlPlane struct {
	platformv1connect.UnimplementedWorkspaceServiceHandler
	platformv1connect.UnimplementedApplicationServiceHandler
	platformv1connect.UnimplementedBuildServiceHandler
	platformv1connect.UnimplementedDeploymentServiceHandler

	mu               sync.Mutex
	lastAuth         string
	failCode         connect.Code
	listWorkspaces   *platformv1.ListWorkspacesRequest
	listApplications *platformv1.ListApplicationsRequest
	createApp        *platformv1.CreateApplicationRequest
	createBuild      *platformv1.CreateBuildRequest
	getBuild         *platformv1.GetBuildRequest
	listBuilds       *platformv1.ListBuildsRequest
	readLogs         *platformv1.ReadBuildLogsRequest
	cancelBuild      *platformv1.CancelBuildRequest
	status           *platformv1.GetRuntimeStatusRequest
	createDeployment *platformv1.CreateDeploymentRequest
	getDeployment    *platformv1.GetDeploymentRequest
	listDeployments  *platformv1.ListDeploymentsRequest
	rollback         *platformv1.RollbackDeploymentRequest
	changeRuntime    *platformv1.ChangeRuntimeRequest
	runtimeLogs      *platformv1.GetRuntimeLogsRequest
	runtimeMetrics   *platformv1.GetRuntimeMetricsRequest
}

func failOr[T any](s *stubControlPlane, msg *T) (*connect.Response[T], error) {
	if s.failCode != 0 {
		return nil, connect.NewError(s.failCode, errors.New("stub failure"))
	}
	return connect.NewResponse(msg), nil
}

func (s *stubControlPlane) ListWorkspaces(_ context.Context, r *connect.Request[platformv1.ListWorkspacesRequest]) (*connect.Response[platformv1.ListWorkspacesResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.listWorkspaces = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.ListWorkspacesResponse{Workspaces: []*platformv1.Workspace{{Id: "w1", Name: "space", Role: "ADMIN"}}, NextCursor: "c2"})
}
func (s *stubControlPlane) ListApplications(_ context.Context, r *connect.Request[platformv1.ListApplicationsRequest]) (*connect.Response[platformv1.ListApplicationsResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.listApplications = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.ListApplicationsResponse{Applications: []*platformv1.Application{{Id: "app_a", WorkspaceId: r.Msg.WorkspaceId, Name: "demo"}}})
}
func (s *stubControlPlane) CreateApplication(_ context.Context, r *connect.Request[platformv1.CreateApplicationRequest]) (*connect.Response[platformv1.CreateApplicationResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.createApp = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.CreateApplicationResponse{Application: &platformv1.Application{Id: "app_new", WorkspaceId: r.Msg.WorkspaceId, Name: r.Msg.Name}, ConfigurationPath: "/workspaces/ws_a/apps/app_new"})
}
func (s *stubControlPlane) CreateBuild(_ context.Context, r *connect.Request[platformv1.CreateBuildRequest]) (*connect.Response[platformv1.CreateBuildResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.createBuild = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.CreateBuildResponse{Build: buildStub("RUNNING")})
}
func (s *stubControlPlane) GetBuild(_ context.Context, r *connect.Request[platformv1.GetBuildRequest]) (*connect.Response[platformv1.GetBuildResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.getBuild = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.GetBuildResponse{Build: buildStub("RUNNING")})
}
func (s *stubControlPlane) ListBuilds(_ context.Context, r *connect.Request[platformv1.ListBuildsRequest]) (*connect.Response[platformv1.ListBuildsResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.listBuilds = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.ListBuildsResponse{Builds: []*platformv1.Build{buildStub("SUCCEEDED")}, NextCursor: "b2"})
}
func (s *stubControlPlane) ReadBuildLogs(_ context.Context, r *connect.Request[platformv1.ReadBuildLogsRequest]) (*connect.Response[platformv1.ReadBuildLogsResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.readLogs = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.ReadBuildLogsResponse{Chunks: []*platformv1.BuildLogChunk{{Seq: 7, Content: "step\n", Truncated: true}}, NextSeq: 8, Complete: false, Truncated: true})
}
func (s *stubControlPlane) CancelBuild(_ context.Context, r *connect.Request[platformv1.CancelBuildRequest]) (*connect.Response[platformv1.CancelBuildResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.cancelBuild = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.CancelBuildResponse{Build: buildStub("CANCELING")})
}
func (s *stubControlPlane) GetRuntimeStatus(_ context.Context, r *connect.Request[platformv1.GetRuntimeStatusRequest]) (*connect.Response[platformv1.GetRuntimeStatusResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.status = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.GetRuntimeStatusResponse{Status: &platformv1.RuntimeStatus{WorkspaceId: r.Msg.WorkspaceId, ApplicationId: r.Msg.ApplicationId, Generation: 3, DesiredState: "RUNNING", ServingReleaseId: "rel_1", ServingGeneration: 3, ObservedGeneration: 3, ObservedState: "RUNNING"}})
}
func (s *stubControlPlane) CreateDeployment(_ context.Context, r *connect.Request[platformv1.CreateDeploymentRequest]) (*connect.Response[platformv1.CreateDeploymentResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.createDeployment = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.CreateDeploymentResponse{Deployment: deploymentStub("rel_2", "", "DEPLOY", true)})
}
func (s *stubControlPlane) GetDeployment(_ context.Context, r *connect.Request[platformv1.GetDeploymentRequest]) (*connect.Response[platformv1.GetDeploymentResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.getDeployment = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.GetDeploymentResponse{Deployment: deploymentStub("rel_2", "", "DEPLOY", false)})
}
func (s *stubControlPlane) ListDeployments(_ context.Context, r *connect.Request[platformv1.ListDeploymentsRequest]) (*connect.Response[platformv1.ListDeploymentsResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.listDeployments = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.ListDeploymentsResponse{Deployments: []*platformv1.Deployment{deploymentStub("rel_2", "", "DEPLOY", false)}, NextCursor: "d2"})
}
func (s *stubControlPlane) RollbackDeployment(_ context.Context, r *connect.Request[platformv1.RollbackDeploymentRequest]) (*connect.Response[platformv1.RollbackDeploymentResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.rollback = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.RollbackDeploymentResponse{Deployment: deploymentStub("rel_1", "rel_2", "ROLLBACK", true)})
}
func (s *stubControlPlane) ChangeRuntime(_ context.Context, r *connect.Request[platformv1.ChangeRuntimeRequest]) (*connect.Response[platformv1.ChangeRuntimeResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.changeRuntime = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.ChangeRuntimeResponse{Deployment: deploymentStub("rel_1", "", r.Msg.Action, true)})
}
func (s *stubControlPlane) GetRuntimeLogs(_ context.Context, r *connect.Request[platformv1.GetRuntimeLogsRequest]) (*connect.Response[platformv1.GetRuntimeLogsResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.runtimeLogs = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.GetRuntimeLogsResponse{Chunks: []*platformv1.RuntimeLogChunk{{Id: "lg1", PodUid: "pod1", Container: "app", Seq: 1, Content: []byte("hello\n")}}, NextCursor: "lc2"})
}
func (s *stubControlPlane) GetRuntimeMetrics(_ context.Context, r *connect.Request[platformv1.GetRuntimeMetricsRequest]) (*connect.Response[platformv1.GetRuntimeMetricsResponse], error) {
	s.mu.Lock()
	s.lastAuth, s.runtimeMetrics = r.Header().Get("Authorization"), r.Msg
	s.mu.Unlock()
	return failOr(s, &platformv1.GetRuntimeMetricsResponse{Samples: []*platformv1.RuntimeMetricSample{{Id: "m1", ReleaseId: "rel_1", Generation: 3, CpuNanoCores: 1200, CpuWindowMillis: 1000, MemoryWorkingSetBytes: 2048, CollectedAt: "2026-01-01T00:00:00Z"}}})
}

func buildStub(state string) *platformv1.Build {
	return &platformv1.Build{Id: "bld_1", SourceSnapshotId: "src_1", State: state, Stage: "build", DesiredState: "RUNNING", ConfigRevision: 2, PolicyRevision: 1, RuntimeConfigRevision: protoInt64(2), CreatedAt: "2026-01-01T00:00:00Z", Artifact: &platformv1.Artifact{Id: "art_1", Repository: "reg/app", Digest: "sha256:x", Platform: "linux/arm64", State: "VERIFIED"}}
}
func deploymentStub(releaseID, rollbackOf, action string, active bool) *platformv1.Deployment {
	return &platformv1.Deployment{Release: &platformv1.Release{Id: releaseID, RollbackOf: rollbackOf, RuntimeConfigRevision: 1, Artifact: &platformv1.ReleaseArtifact{Id: "art_1", Repository: "reg/app", Digest: "sha256:x", Platform: "linux/arm64"}, Domains: []string{"a.example.com"}, CreatedAt: "2026-01-01T00:00:00Z"}, Operation: &platformv1.DeploymentOperation{Id: "op_1", State: "RUNNING", Action: action, Generation: 4, Active: active, CreatedAt: "2026-01-01T00:00:00Z"}}
}
func protoInt64(v int64) *int64 { return &v }

// bridgeFixture 启动假控制面并连接桥接 server，返回已建立的客户端会话。
func bridgeFixture(t *testing.T) (*sdk.ClientSession, *stubControlPlane) {
	t.Helper()
	stubSvc := &stubControlPlane{}
	mux := http.NewServeMux()
	for _, mount := range []func() (string, http.Handler){
		func() (string, http.Handler) {
			p, h := platformv1connect.NewWorkspaceServiceHandler(stubSvc)
			return p, h
		},
		func() (string, http.Handler) {
			p, h := platformv1connect.NewApplicationServiceHandler(stubSvc)
			return p, h
		},
		func() (string, http.Handler) { p, h := platformv1connect.NewBuildServiceHandler(stubSvc); return p, h },
		func() (string, http.Handler) {
			p, h := platformv1connect.NewDeploymentServiceHandler(stubSvc)
			return p, h
		},
	} {
		path, handler := mount()
		mux.Handle("/api"+path, http.StripPrefix("/api", handler))
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &clientcli.APIClient{
		Base: srv.URL, Token: "pat-token", DefaultWorkspace: "ws_a", DefaultApplication: "app_a",
		Workspace:   platformv1connect.NewWorkspaceServiceClient(srv.Client(), srv.URL+"/api"),
		Application: platformv1connect.NewApplicationServiceClient(srv.Client(), srv.URL+"/api"),
		Build:       platformv1connect.NewBuildServiceClient(srv.Client(), srv.URL+"/api"),
		Deployment:  platformv1connect.NewDeploymentServiceClient(srv.Client(), srv.URL+"/api"),
	}
	server, err := NewBridgeServer(BridgeConfig{Client: client}, LocalConfig{})
	if err != nil {
		t.Fatal(err)
	}
	a, b := sdk.NewInMemoryTransports()
	if _, err = server.Connect(context.Background(), a, nil); err != nil {
		t.Fatal(err)
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, stubSvc
}

func bridgeTools(t *testing.T, s *sdk.ClientSession) map[string]*sdk.Tool {
	t.Helper()
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]*sdk.Tool{}
	for _, tool := range list.Tools {
		seen[tool.Name] = tool
	}
	return seen
}

func errorCode(t *testing.T, v *sdk.CallToolResult) string {
	t.Helper()
	if !v.IsError {
		t.Fatalf("期望错误结果: %+v", v)
	}
	// 本进程内直接断言；经传输层 JSON 往返后 StructuredContent 是 map。
	if body, ok := v.StructuredContent.(errorBody); ok {
		return body.Code
	}
	if body, ok := v.StructuredContent.(map[string]any); ok {
		if code, _ := body["code"].(string); code != "" {
			return code
		}
	}
	data, _ := json.Marshal(v)
	t.Fatalf("非结构化错误: %s", data)
	return ""
}

func TestBridgeWorkspaceApplicationTools(t *testing.T) {
	s, stubSvc := bridgeFixture(t)
	if got := call(t, s, "workspace_list", map[string]any{"limit": 5, "cursor": "c1"}); got.IsError {
		t.Fatalf("workspace_list: %s", contentJSON(t, got))
	} else if out := contentJSON(t, got); !strings.Contains(out, `"id":"w1"`) || !strings.Contains(out, `"nextCursor":"c2"`) {
		t.Fatalf("workspace_list 输出形状错误: %s", out)
	}
	stubSvc.mu.Lock()
	auth, req := stubSvc.lastAuth, stubSvc.listWorkspaces
	stubSvc.mu.Unlock()
	if auth != "Bearer pat-token" {
		t.Fatalf("凭证未随请求发送: %q", auth)
	}
	if req.Limit != 5 || req.Cursor != "c1" {
		t.Fatalf("分页参数未传递: %+v", req)
	}
	// 空目标回退 profile 默认工作空间。
	if got := call(t, s, "app_list", map[string]any{}); got.IsError {
		t.Fatalf("app_list: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	workspaceID := stubSvc.listApplications.WorkspaceId
	stubSvc.mu.Unlock()
	if workspaceID != "ws_a" {
		t.Fatalf("默认工作空间未生效: %q", workspaceID)
	}
	got := call(t, s, "app_create", map[string]any{"name": "demo", "idempotencyKey": "app-key-1"})
	if got.IsError || !strings.Contains(contentJSON(t, got), `"id":"app_new"`) || !strings.Contains(contentJSON(t, got), `"configurationPath":"/workspaces/ws_a/apps/app_new"`) {
		t.Fatalf("app_create: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	createApp := stubSvc.createApp
	stubSvc.mu.Unlock()
	if createApp.WorkspaceId != "ws_a" || createApp.Name != "demo" || createApp.IdempotencyKey != "app-key-1" {
		t.Fatalf("创建应用请求错误: %+v", createApp)
	}
	// 空幂等键通过 schema 但被工具校验拒绝。
	if got = call(t, s, "app_create", map[string]any{"name": "demo", "idempotencyKey": ""}); !got.IsError || errorCode(t, got) != "INVALID_ARGUMENT" {
		t.Fatalf("缺幂等键必须拒绝: %s", contentJSON(t, got))
	}
	if got = call(t, s, "workspace_list", map[string]any{"limit": 101}); !got.IsError || errorCode(t, got) != "INVALID_ARGUMENT" {
		t.Fatalf("超限分页必须拒绝: %s", contentJSON(t, got))
	}
}

func TestBridgeBuildToolsMapping(t *testing.T) {
	s, stubSvc := bridgeFixture(t)
	in := map[string]any{
		"workspaceId": "ws_b", "applicationId": "app_b",
		"sourceSnapshotId": "src_1", "configRevision": 2, "expectedPolicyRevision": 1,
		"overrides":      map[string]any{"dockerfile": "Dockerfile", "args": map[string]any{"GOOS": "linux"}, "secretRefs": []any{map[string]any{"secretId": "sec_1", "name": "token", "version": 3}}},
		"idempotencyKey": "key-1", "autoDeploy": true, "runtimeConfigRevision": 2, "expectedServingRelease": "rel_1",
	}
	if got := call(t, s, "build_create", in); got.IsError || !strings.Contains(contentJSON(t, got), `"status":"RUNNING"`) {
		t.Fatalf("build_create: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	create := stubSvc.createBuild
	stubSvc.mu.Unlock()
	if create.WorkspaceId != "ws_b" || create.ApplicationId != "app_b" || create.Input.SourceSnapshotId != "src_1" || create.Input.IdempotencyKey != "key-1" || !create.Input.AutoDeploy || create.Input.RuntimeConfigRevision != 2 || create.Input.ExpectedServingRelease != "rel_1" {
		t.Fatalf("创建构建请求错误: %+v", create)
	}
	if create.Input.Overrides == nil || create.Input.Overrides.Dockerfile == nil || *create.Input.Overrides.Dockerfile != "Dockerfile" || create.Input.Overrides.Args["GOOS"] != "linux" || len(create.Input.Overrides.SecretRefs) != 1 || create.Input.Overrides.SecretRefs[0].SecretId != "sec_1" {
		t.Fatalf("overrides 未正确传递: %+v", create.Input.Overrides)
	}
	// autoDeploy 为真但缺运行配置版本：通过 schema 但被工具校验拒绝。
	if got := call(t, s, "build_create", map[string]any{"sourceSnapshotId": "src_1", "configRevision": 0, "expectedPolicyRevision": 1, "overrides": map[string]any{}, "idempotencyKey": "k", "autoDeploy": true, "runtimeConfigRevision": 0, "expectedServingRelease": ""}); !got.IsError || errorCode(t, got) != "INVALID_ARGUMENT" {
		t.Fatalf("autoDeploy 缺运行版本必须拒绝: %s", contentJSON(t, got))
	}
	if got, err := s.CallTool(context.Background(), &sdk.CallToolParams{Name: "build_get", Arguments: map[string]any{"id": "bld_1", "extra": 1}}); err != nil || !got.IsError {
		t.Fatal("未知字段必须拒绝")
	}
	if got := call(t, s, "build_get", map[string]any{"id": "bld_1"}); got.IsError || !strings.Contains(contentJSON(t, got), `"sourceSnapshotId":"src_1"`) || !strings.Contains(contentJSON(t, got), `"runtimeConfigRevision":2`) {
		t.Fatalf("build_get: %s", contentJSON(t, got))
	}
	if got := call(t, s, "build_list", map[string]any{"limit": 20, "cursor": "b1"}); got.IsError || !strings.Contains(contentJSON(t, got), `"nextCursor":"b2"`) {
		t.Fatalf("build_list: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	list := stubSvc.listBuilds
	stubSvc.mu.Unlock()
	if list.Limit != 20 || list.Cursor != "b1" {
		t.Fatalf("构建分页参数未传递: %+v", list)
	}
	if got := call(t, s, "build_logs", map[string]any{"id": "bld_1", "afterSeq": 7, "limit": 3}); got.IsError || !strings.Contains(contentJSON(t, got), `"nextSeq":8`) || !strings.Contains(contentJSON(t, got), `"complete":false`) {
		t.Fatalf("build_logs: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	logs := stubSvc.readLogs
	stubSvc.mu.Unlock()
	if logs.AfterSeq != 7 || logs.Limit != 3 {
		t.Fatalf("日志游标未传递: %+v", logs)
	}
	if got := call(t, s, "build_logs", map[string]any{"id": "bld_1", "afterSeq": 0}); got.IsError {
		t.Fatalf("默认日志参数失败: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	logs = stubSvc.readLogs
	stubSvc.mu.Unlock()
	if logs.Limit != 16 {
		t.Fatalf("日志默认块数应为 16: %+v", logs)
	}
	if got := call(t, s, "build_logs", map[string]any{"id": "bld_1", "afterSeq": 0, "limit": 17}); !got.IsError || errorCode(t, got) != "INVALID_ARGUMENT" {
		t.Fatalf("日志超限必须拒绝: %s", contentJSON(t, got))
	}
	if got := call(t, s, "build_cancel", map[string]any{"id": "bld_1"}); got.IsError || !strings.Contains(contentJSON(t, got), `"status":"CANCELING"`) {
		t.Fatalf("build_cancel: %s", contentJSON(t, got))
	}
}

func TestBridgeDeploymentToolsMapping(t *testing.T) {
	s, stubSvc := bridgeFixture(t)
	for _, name := range []string{"app_status", "runtime_status"} {
		if got := call(t, s, name, map[string]any{}); got.IsError || !strings.Contains(contentJSON(t, got), `"observedState":"RUNNING"`) {
			t.Fatalf("%s: %s", name, contentJSON(t, got))
		}
	}
	stubSvc.mu.Lock()
	status := stubSvc.status
	stubSvc.mu.Unlock()
	if status.WorkspaceId != "ws_a" || status.ApplicationId != "app_a" {
		t.Fatalf("默认目标未生效: %+v", status)
	}
	deployIn := map[string]any{"artifactId": "art_1", "runtimeConfigRevision": 2, "expectedPolicyRevision": 1, "expected": map[string]any{"generation": 3, "servingReleaseId": "rel_1", "configRevision": 2}, "idempotencyKey": "dep-key-1"}
	if got := call(t, s, "deployment_create", deployIn); got.IsError || !strings.Contains(contentJSON(t, got), `"artifactDigest":"sha256:x"`) || !strings.Contains(contentJSON(t, got), `"domains":["a.example.com"]`) {
		t.Fatalf("deployment_create: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	create := stubSvc.createDeployment
	stubSvc.mu.Unlock()
	if create.ArtifactId != "art_1" || create.RuntimeConfigRevision != 2 || create.ExpectedPolicyRevision != 1 || create.Expected.GetGeneration() != 3 || create.Expected.GetServingReleaseId() != "rel_1" || create.IdempotencyKey != "dep-key-1" {
		t.Fatalf("发布请求错误: %+v", create)
	}
	if got := call(t, s, "deployment_create", map[string]any{"artifactId": "art_1", "runtimeConfigRevision": 2, "expectedPolicyRevision": 1, "expected": map[string]any{"generation": 3, "servingReleaseId": "", "configRevision": 1}, "idempotencyKey": "k"}); !got.IsError || errorCode(t, got) != "INVALID_ARGUMENT" {
		t.Fatalf("expected.configRevision 不一致必须拒绝: %s", contentJSON(t, got))
	}
	if got := call(t, s, "deployment_get", map[string]any{"id": "op_1"}); got.IsError || !strings.Contains(contentJSON(t, got), `"operationId":"op_1"`) {
		t.Fatalf("deployment_get: %s", contentJSON(t, got))
	}
	if got := call(t, s, "deployment_list", map[string]any{"limit": 10, "cursor": "d1"}); got.IsError || !strings.Contains(contentJSON(t, got), `"nextCursor":"d2"`) {
		t.Fatalf("deployment_list: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	listD := stubSvc.listDeployments
	stubSvc.mu.Unlock()
	if listD.Limit != 10 || listD.Cursor != "d1" {
		t.Fatalf("部署分页参数未传递: %+v", listD)
	}
	if got := call(t, s, "deployment_rollback", map[string]any{"targetReleaseId": "rel_1", "expectedPolicyRevision": 1, "expected": map[string]any{"generation": 3, "servingReleaseId": "rel_2", "configRevision": 2}, "idempotencyKey": "rb-1"}); got.IsError || !strings.Contains(contentJSON(t, got), `"rollbackOf":"rel_2"`) {
		t.Fatalf("deployment_rollback: %s", contentJSON(t, got))
	}
	if got := call(t, s, "runtime_stop", map[string]any{"expected": map[string]any{"generation": 3, "servingReleaseId": "rel_1", "configRevision": 2}, "idempotencyKey": "stop-1"}); got.IsError {
		t.Fatalf("runtime_stop: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	change := stubSvc.changeRuntime
	stubSvc.mu.Unlock()
	if change.Action != "STOP" || change.Expected.GetGeneration() != 3 || change.IdempotencyKey != "stop-1" {
		t.Fatalf("运行变更请求错误: %+v", change)
	}
	if got := call(t, s, "runtime_logs", map[string]any{"limit": 5, "cursor": "lc1", "podUid": "pod1", "container": "app"}); got.IsError || !strings.Contains(contentJSON(t, got), `"content":"hello\n"`) || !strings.Contains(contentJSON(t, got), `"nextCursor":"lc2"`) {
		t.Fatalf("runtime_logs: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	rlogs := stubSvc.runtimeLogs
	stubSvc.mu.Unlock()
	if rlogs.Limit != 5 || rlogs.Cursor != "lc1" || rlogs.PodUid != "pod1" || rlogs.Container != "app" {
		t.Fatalf("运行日志参数未传递: %+v", rlogs)
	}
	if got := call(t, s, "runtime_logs", map[string]any{"limit": 51}); !got.IsError || errorCode(t, got) != "INVALID_ARGUMENT" {
		t.Fatalf("运行日志超限必须拒绝: %s", contentJSON(t, got))
	}
	if got := call(t, s, "runtime_metrics", map[string]any{"since": "2026-01-01T00:00:00Z", "until": "2026-01-02T00:00:00Z"}); got.IsError || !strings.Contains(contentJSON(t, got), `"cpuNanoCores":1200`) {
		t.Fatalf("runtime_metrics: %s", contentJSON(t, got))
	}
	stubSvc.mu.Lock()
	metrics := stubSvc.runtimeMetrics
	stubSvc.mu.Unlock()
	if metrics.Since == "" || metrics.Until == "" {
		t.Fatalf("指标时间范围未传递: %+v", metrics)
	}
}

func TestBridgeErrorMapping(t *testing.T) {
	s, stubSvc := bridgeFixture(t)
	for _, tc := range []struct {
		code connect.Code
		want string
	}{
		{connect.CodePermissionDenied, "INSUFFICIENT_SCOPE"},
		{connect.CodeNotFound, "NOT_FOUND_OR_DENIED"},
		{connect.CodeUnauthenticated, "UNAUTHENTICATED"},
		{connect.CodeInvalidArgument, "INVALID_ARGUMENT"},
		{connect.CodeFailedPrecondition, "CONFLICT"},
		{connect.CodeUnavailable, "NOT_READY"},
		{connect.CodeInternal, "INTERNAL"},
	} {
		stubSvc.mu.Lock()
		stubSvc.failCode = tc.code
		stubSvc.mu.Unlock()
		got := call(t, s, "workspace_list", map[string]any{})
		if code := errorCode(t, got); code != tc.want {
			t.Fatalf("%v: got %s want %s (%s)", tc.code, code, tc.want, contentJSON(t, got))
		}
	}
	stubSvc.mu.Lock()
	stubSvc.failCode = 0
	stubSvc.mu.Unlock()
	if got := call(t, s, "workspace_list", map[string]any{}); got.IsError {
		t.Fatalf("恢复后应成功: %s", contentJSON(t, got))
	}
}

func TestBridgeUnauthenticatedOnlyPreview(t *testing.T) {
	if _, err := NewBridgeServer(BridgeConfig{}, LocalConfig{}); err == nil {
		t.Fatal("无授权根且无凭证必须报错")
	}
	root := t.TempDir()
	server, err := NewBridgeServer(BridgeConfig{}, LocalConfig{AllowRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := sdk.NewInMemoryTransports()
	if _, err = server.Connect(context.Background(), a, nil); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	s, err := client.Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.Contains(s.InitializeResult().Instructions, "未登录") {
		t.Fatalf("未登录说明缺失: %q", s.InitializeResult().Instructions)
	}
	seen := bridgeTools(t, s)
	if len(seen) != 1 || seen["source_preview"] == nil {
		t.Fatalf("未登录必须只有 source_preview: %v", keys(seen))
	}
	// 已登录但未授权本地目录：只有业务工具，无本地预览。
	s2, _ := bridgeFixture(t)
	seen = bridgeTools(t, s2)
	if seen["source_preview"] != nil {
		t.Fatal("未提供 allow-root 不应有本地预览")
	}
	for _, name := range []string{"workspace_list", "app_list", "app_create", "build_create", "build_get", "build_list", "build_logs", "build_cancel", "app_status", "runtime_status", "deployment_create", "deployment_get", "deployment_list", "deployment_rollback", "runtime_start", "runtime_stop", "runtime_restart", "runtime_logs", "runtime_metrics"} {
		if seen[name] == nil {
			t.Errorf("缺少业务工具 %s", name)
		}
	}
	for _, tool := range seen {
		if !strings.Contains(tool.Description, "PAT") {
			t.Errorf("%s 缺少鉴权语义说明: %q", tool.Name, tool.Description)
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
