package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/domains/deployment"
	"github.com/sunshanpeng/yummy/internal/domains/enrollment"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
)

// 使用正式装配与真实 PG/Redis/文件卷；只有节点资格门禁的单独分支显式置测试事实。
// 本测试没有执行 BuildKit 或 Kubernetes，不证明真实构建、发布与HTTPS资格。
func TestProductHTTPSourceConfigurationBuildAndDeploymentBoundaries(t *testing.T) {
	redis := os.Getenv("TEST_REDIS_ADDR")
	if redis == "" {
		t.Skip("未设置TEST_REDIS_ADDR")
	}
	db := testkit.OpenDatabase(t)
	web := httptest.NewUnstartedServer(nil)
	base := "http://" + web.Listener.Addr().String()
	key := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	product, err := openWithDatabase(context.Background(), db, Config{RedisAddress: redis, PublicBaseURL: base, SessionKey: key(21), CSRFKey: key(22), MasterKey: key(23), SourceRoot: t.TempDir(), AllowLocalHTTP: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = product.Close() })
	if _, err = product.Identity.InitializeAdmin(context.Background(), "admin", "safe-integration-password"); err != nil {
		t.Fatal(err)
	}
	web.Config.Handler = product.Handler
	web.Start()
	t.Cleanup(web.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	response, err := client.Get(base + "/auth/csrf")
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err = json.NewDecoder(response.Body).Decode(&bootstrap); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	csrf := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			r.Header().Set("X-CSRF-Token", bootstrap.CSRFToken)
			return next(ctx, r)
		}
	}))
	identity := platformv1connect.NewIdentityServiceClient(client, base+"/api", csrf)
	if _, err = identity.Login(context.Background(), connect.NewRequest(&platformv1.LoginRequest{Username: "admin", Password: "safe-integration-password"})); err != nil {
		t.Fatal(err)
	}
	workspaces := platformv1connect.NewWorkspaceServiceClient(client, base+"/api", csrf)
	createdSpace, err := workspaces.CreateWorkspace(context.Background(), connect.NewRequest(&platformv1.CreateWorkspaceRequest{Name: "集成空间", Slug: "integration"}))
	if err != nil {
		t.Fatal(err)
	}
	ws := createdSpace.Msg.Workspace.Id
	applications := platformv1connect.NewApplicationServiceClient(client, base+"/api", csrf)
	createApp := &platformv1.CreateApplicationRequest{WorkspaceId: ws, Name: "gateway", IdempotencyKey: "create-gateway"}
	createdApp, err := applications.CreateApplication(context.Background(), connect.NewRequest(createApp))
	if err != nil {
		t.Fatal(err)
	}
	app := createdApp.Msg.Application.Id
	duplicate, err := applications.CreateApplication(context.Background(), connect.NewRequest(createApp))
	if err != nil || duplicate.Msg.Application.Id != app {
		t.Fatalf("应用幂等: %v", err)
	}
	builds := platformv1connect.NewBuildServiceClient(client, base+"/api", csrf)
	deployments := platformv1connect.NewDeploymentServiceClient(client, base+"/api", csrf)
	configs := platformv1connect.NewRuntimeConfigServiceClient(client, base+"/api", csrf)
	secrets := platformv1connect.NewBuildSecretServiceClient(client, base+"/api", csrf)
	inventory := platformv1connect.NewInventoryServiceClient(client, base+"/api", csrf)
	sources := platformv1connect.NewSourceServiceClient(client, base+"/api", csrf)
	status, err := deployments.GetRuntimeStatus(context.Background(), connect.NewRequest(&platformv1.GetRuntimeStatusRequest{WorkspaceId: ws, ApplicationId: app}))
	if err != nil || status.Msg.Status.ObservedState != "NOT_DEPLOYED" {
		t.Fatalf("新应用假运行: %v %v", status, err)
	}
	list, err := builds.ListBuilds(context.Background(), connect.NewRequest(&platformv1.ListBuildsRequest{WorkspaceId: ws, ApplicationId: app}))
	if err != nil || len(list.Msg.Builds) != 0 {
		t.Fatalf("创建应用触发构建: %v", err)
	}
	suggested, err := configs.GetRuntimeConfig(context.Background(), connect.NewRequest(&platformv1.GetRuntimeConfigRequest{WorkspaceId: ws, ApplicationId: app}))
	if err != nil || suggested.Msg.Config.Revision != 0 {
		t.Fatalf("建议配置: %v", err)
	}
	cfg := suggested.Msg.Config
	save := &platformv1.SaveRuntimeConfigRequest{WorkspaceId: ws, ApplicationId: app, Type: cfg.Type, Port: cfg.Port, Probe: cfg.Probe, Resources: cfg.Resources, Confirmed: true, IdempotencyKey: "config-first", Variables: []*platformv1.RuntimeVariableInput{{Name: "TOKEN", Value: "integration-private-value", Sensitive: true}}}
	saved, err := configs.SaveRuntimeConfig(context.Background(), connect.NewRequest(save))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Msg.Config.Variables) != 1 || saved.Msg.Config.Variables[0].Value != "" || !saved.Msg.Config.Variables[0].Configured {
		t.Fatal("敏感变量泄露或配置状态丢失")
	}
	repeat, err := configs.SaveRuntimeConfig(context.Background(), connect.NewRequest(save))
	if err != nil || repeat.Msg.Config.Revision != 1 {
		t.Fatalf("配置幂等重试失败: %v", err)
	}
	save.Port = 9000
	if _, err = configs.SaveRuntimeConfig(context.Background(), connect.NewRequest(save)); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("同key不同配置未冲突: %v", err)
	}
	savedSecret, err := secrets.CreateBuildSecret(context.Background(), connect.NewRequest(&platformv1.CreateBuildSecretRequest{WorkspaceId: ws, ApplicationId: app, Name: "npm_token", Value: "sensitive-build-token"}))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(savedSecret.Msg)
	if bytes.Contains(encoded, []byte("sensitive-build-token")) {
		t.Fatal("密钥响应返回明文")
	}
	buildConfig, err := builds.SaveBuildConfig(context.Background(), connect.NewRequest(&platformv1.SaveBuildConfigRequest{WorkspaceId: ws, ApplicationId: app, Config: &platformv1.BuildConfig{Dockerfile: "Dockerfile", Context: ".", SecretRefs: []*platformv1.BuildSecretRef{{SecretId: savedSecret.Msg.Secret.Id}}}}))
	if err != nil || buildConfig.Msg.Revision != 1 {
		t.Fatalf("构建设置接线: %v", err)
	}
	root := t.TempDir()
	if err = os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\nCOPY index.html /index.html\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "index.html"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	packed, err := sourcearchive.Archive(context.Background(), &archive, sourcearchive.Options{Root: root, AllowRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := sources.CreateSourceUpload(context.Background(), connect.NewRequest(&platformv1.CreateSourceUploadRequest{WorkspaceId: ws, ApplicationId: app, Size: int64(archive.Len()), Sha256: packed.SHA256, Dockerfile: "Dockerfile"}))
	if err != nil {
		t.Fatal(err)
	}
	put, err := http.NewRequest(http.MethodPut, base+upload.Msg.TransferPath, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	put.Header.Set("X-CSRF-Token", bootstrap.CSRFToken)
	putResponse, err := client.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(putResponse.Body)
	_ = putResponse.Body.Close()
	if putResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("上传状态%d: %s", putResponse.StatusCode, body)
	}
	snapshot, err := sources.CompleteSourceUpload(context.Background(), connect.NewRequest(&platformv1.CompleteSourceUploadRequest{WorkspaceId: ws, ApplicationId: app, UploadId: upload.Msg.Upload.Id}))
	if err != nil || snapshot.Msg.Snapshot.State != "READY" {
		t.Fatalf("源码验证: %v", err)
	}
	in := &platformv1.BuildInput{SourceSnapshotId: snapshot.Msg.Snapshot.Id, ConfigRevision: 1, ExpectedPolicyRevision: 1, IdempotencyKey: "build-gateway"}
	// 未配置的实例不能因为页面/API已连通就接受构建。
	if _, err = builds.CreateBuild(context.Background(), connect.NewRequest(&platformv1.CreateBuildRequest{WorkspaceId: ws, ApplicationId: app, Input: in})); err == nil {
		t.Fatal("无节点资格仍受理构建")
	}
	node, err := inventory.CreateNode(context.Background(), connect.NewRequest(&platformv1.CreateNodeRequest{Name: "测试节点", WorkerId: "integration-worker", Capacity: 1}))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := inventory.CreateRegistry(context.Background(), connect.NewRequest(&platformv1.CreateRegistryRequest{Name: "测试仓库", Url: "https://registry.example.com", Username: "robot", Password: "registry-password"}))
	if err != nil {
		t.Fatal(err)
	}
	if node.Msg.Node.BuildReady || registry.Msg.Registry.VerifiedAt != "" {
		t.Fatal("新建节点/仓库伪造资格")
	}
	_, err = inventory.SaveExecutionPolicy(context.Background(), connect.NewRequest(&platformv1.SaveExecutionPolicyRequest{BuildNodeId: node.Msg.Node.Id, RuntimeNodeId: node.Msg.Node.Id, RegistryId: registry.Msg.Registry.Id, ResourceDefault: &platformv1.ExecutionResources{CpuMillis: 100, MemoryMib: 128}, ResourceLimits: &platformv1.ExecutionResources{CpuMillis: 1000, MemoryMib: 1024}}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = inventory.SetWorkspaceAllocation(context.Background(), connect.NewRequest(&platformv1.SetWorkspaceAllocationRequest{WorkspaceId: ws, AllowedNodeIds: []string{node.Msg.Node.Id}, MaxApplications: 10, BuildConcurrency: 1, CpuMillis: 2000, MemoryMib: 2048}))
	if err != nil {
		t.Fatal(err)
	}
	summary, err := inventory.ResolveExecution(context.Background(), connect.NewRequest(&platformv1.ResolveExecutionRequest{WorkspaceId: ws, ApplicationId: app}))
	if err != nil || summary.Msg.Summary.PolicyRevision != 1 || summary.Msg.Summary.Ready || summary.Msg.Summary.Reason == "" {
		t.Fatalf("未就绪摘要错误: %v %v", summary, err)
	}
	if _, err = builds.CreateBuild(context.Background(), connect.NewRequest(&platformv1.CreateBuildRequest{WorkspaceId: ws, ApplicationId: app, Input: in})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("未就绪应拒绝: %v", err)
	}
	// 下两行只给队列/取消API测试设置资格fixture；绝不代表真实推拉或Linux原生认证。
	if err = db.Exec("UPDATE execution_nodes SET cluster_node_uid='test-node-uid',os='linux',arch='amd64',last_seen=NOW(),build_ready=TRUE WHERE id=?", node.Msg.Node.Id).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("UPDATE registries SET verified_at=NOW(),verified_revision=revision WHERE id=?", registry.Msg.Registry.Id).Error; err != nil {
		t.Fatal(err)
	}
	preview, err := builds.PreviewBuild(context.Background(), connect.NewRequest(&platformv1.PreviewBuildRequest{WorkspaceId: ws, ApplicationId: app, Input: in}))
	if err != nil || preview.Msg.Preview.Config.SecretRefs[0].Version != 1 {
		t.Fatalf("构建预览未冻结密钥: %v", err)
	}
	queued, err := builds.CreateBuild(context.Background(), connect.NewRequest(&platformv1.CreateBuildRequest{WorkspaceId: ws, ApplicationId: app, Input: in}))
	if err != nil || queued.Msg.Build.State != "QUEUED" {
		t.Fatalf("排队: %v", err)
	}
	if err := db.Exec("INSERT INTO worker_certificates(serial,node_id,worker_id,certificate_sha256,public_key_sha256,not_before,not_after) VALUES ('a1',?,?,?,?,NOW()-INTERVAL '1 minute',NOW()+INTERVAL '1 day')", node.Msg.Node.Id, node.Msg.Node.WorkerId, strings.Repeat("1", 64), strings.Repeat("2", 64)).Error; err != nil {
		t.Fatal(err)
	}
	worker := build.WorkerIdentity{NodeID: node.Msg.Node.Id, WorkerID: node.Msg.Node.WorkerId, CertificateSerial: "a1"}
	dispatch, err := product.Builds.Claim(context.Background(), worker)
	if err != nil || dispatch.BuildID != queued.Msg.Build.Id {
		t.Fatalf("领取构建: %+v %v", dispatch, err)
	}
	box, err := security.NewSecretBox(key(23))
	if err != nil {
		t.Fatal(err)
	}
	materials := workerMaterials{db: db, box: box, source: product.Sources, secrets: product.BuildSecrets, inventory: product.Inventory, baseURL: base}
	identityWorker := enrollment.Identity{NodeID: worker.NodeID, WorkerID: worker.WorkerID, Serial: worker.CertificateSerial}
	material, err := materials.MaterializeBuild(context.Background(), identityWorker, dispatch)
	if err != nil || material.Source == nil || material.Registry == nil || len(material.Secrets) != 1 {
		t.Fatalf("冻结材料不完整: %v", err)
	}
	if material.Secrets[0].Name != "npm_token" || string(material.Secrets[0].Value) != "sensitive-build-token" || string(material.Registry.Password) != "registry-password" {
		t.Fatal("材料版本或应用归属不符")
	}
	forgedBuild := dispatch
	forgedBuild.Repository = "attacker.example.com/leak"
	if _, err = materials.MaterializeBuild(context.Background(), identityWorker, forgedBuild); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("伪造构建镜像目标可读取凭据: %v", err)
	}
	_, err = secrets.ReplaceBuildSecret(context.Background(), connect.NewRequest(&platformv1.ReplaceBuildSecretRequest{WorkspaceId: ws, ApplicationId: app, SecretId: savedSecret.Msg.Secret.Id, ExpectedVersion: 1, Value: "rotated-value"}))
	if err != nil {
		t.Fatal(err)
	}
	again, err := materials.MaterializeBuild(context.Background(), identityWorker, dispatch)
	if err != nil || again.Source.Token != material.Source.Token || again.Source.Url != material.Source.Url || len(again.Secrets) != 1 || string(again.Secrets[0].Value) != "sensitive-build-token" {
		t.Fatalf("重投源码凭证必须稳定: %v", err)
	}
	get := func(target, token string) *http.Response {
		t.Helper()
		r, requestErr := http.NewRequest(http.MethodGet, target, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		response, requestErr := http.DefaultClient.Do(r)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		return response
	}
	download := get(material.Source.Url, material.Source.Token)
	got, _ := io.ReadAll(download.Body)
	_ = download.Body.Close()
	if download.StatusCode != http.StatusOK || !bytes.Equal(got, archive.Bytes()) || download.Header.Get("X-Source-SHA256") != packed.SHA256 {
		t.Fatalf("受限源码下载: %d", download.StatusCode)
	}
	download = get(material.Source.Url+"?token=leak", material.Source.Token)
	_ = download.Body.Close()
	if download.StatusCode != http.StatusUnauthorized {
		t.Fatalf("查询串不能携带令牌: %d", download.StatusCode)
	}
	canceled, err := builds.CancelBuild(context.Background(), connect.NewRequest(&platformv1.CancelBuildRequest{WorkspaceId: ws, ApplicationId: app, BuildId: queued.Msg.Build.Id}))
	if err != nil || canceled.Msg.Build.State != "CANCELING" {
		t.Fatalf("执行中取消必须等待停止回执: %v", err)
	}
	download = get(material.Source.Url, material.Source.Token)
	_ = download.Body.Close()
	if download.StatusCode != http.StatusUnauthorized {
		t.Fatalf("取消后源码仍可下载: %d", download.StatusCode)
	}
	if _, err = deployments.ChangeRuntime(context.Background(), connect.NewRequest(&platformv1.ChangeRuntimeRequest{WorkspaceId: ws, ApplicationId: app, Action: "ERASE", Expected: &platformv1.ExpectedRuntime{ConfigRevision: 1}, IdempotencyKey: "bad-action"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("任意运行命令未拒绝: %v", err)
	}
	if _, err = deployments.CreateDeployment(context.Background(), connect.NewRequest(&platformv1.CreateDeploymentRequest{WorkspaceId: ws, ApplicationId: app})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("省略CAS快照未拒绝: %v", err)
	}
	var releases int64
	if err = db.Table("application_releases").Where("application_id=?", app).Count(&releases).Error; err != nil || releases != 0 {
		t.Fatalf("保存配置/构建取消误发布: %d %v", releases, err)
	}
	// 显式插入测试发布事实，仅验证材料读取权限；不代表真实仓库或 Kubernetes 已验收。
	fakeCertificate := &x509.Certificate{SerialNumber: big.NewInt(0xa1), Raw: []byte("test-certificate")}
	certificateHash := sha256.Sum256(fakeCertificate.Raw)
	fixture := []struct {
		query string
		args  []any
	}{
		{"UPDATE worker_certificates SET certificate_sha256=? WHERE serial='a1'", []any{hex.EncodeToString(certificateHash[:])}},
		{"UPDATE builds SET state='SUCCEEDED', desired_state='RUN', finished_at=NOW() WHERE id=?", []any{queued.Msg.Build.Id}},
		{"INSERT INTO build_artifacts(id,workspace_id,application_id,build_id,attempt_id,registry_id,credential_version,repository,tag,digest,platform,state,verified_at) SELECT 'artifact-test',workspace_id,application_id,id,?,registry_id,credential_version,repository,tag,?,'linux/amd64','VERIFIED',NOW() FROM builds WHERE id=?", []any{dispatch.AttemptID, "sha256:" + strings.Repeat("b", 64), queued.Msg.Build.Id}},
		{"INSERT INTO application_placements(application_id,workspace_id,instance_id,node_id,worker_id,cluster_node_uid,policy_revision,runtime_type,default_domain) SELECT ?,?,instance_id,?,?, 'test-node-uid',revision,'HTTP','test.apps.example.com' FROM instance_execution_policies WHERE revision=1", []any{app, ws, node.Msg.Node.Id, node.Msg.Node.WorkerId}},
		{"INSERT INTO application_releases(id,workspace_id,application_id,artifact_id,runtime_config_revision,registry_id,credential_version,repository,digest,platform,placement_snapshot,domains,source,actor_id) SELECT 'release-test',workspace_id,application_id,'artifact-test',1,registry_id,credential_version,repository,?,'linux/amd64','{}','[]','DEPLOY',actor_id FROM builds WHERE id=?", []any{"sha256:" + strings.Repeat("b", 64), queued.Msg.Build.Id}},
		{"INSERT INTO deployment_operations(id,workspace_id,application_id,release_id,generation,action,state,actor_id,idempotency_key,fingerprint) SELECT 'operation-test',workspace_id,application_id,'release-test',1,'DEPLOY','QUEUED',actor_id,'test',? FROM builds WHERE id=?", []any{strings.Repeat("c", 64), queued.Msg.Build.Id}},
		{"INSERT INTO application_runtime_states(application_id,workspace_id,generation,desired_state,desired_release_id,active_operation_id,observed_state) VALUES (?,?,1,'RUN','release-test','operation-test','PENDING')", []any{app, ws}},
	}
	for _, item := range fixture {
		if err := db.Exec(item.query, item.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	delivery := deployment.Dispatch{WorkspaceID: ws, ApplicationID: app, OperationID: "operation-test", ReleaseID: "release-test", Generation: 1, Action: deployment.Deploy, Placement: deployment.Placement{NodeID: worker.NodeID, WorkerID: worker.WorkerID, ClusterNodeUID: "test-node-uid"}, Artifact: deployment.ArtifactSnapshot{RegistryID: dispatch.RegistryID, CredentialVersion: dispatch.CredentialVersion, Repository: dispatch.Repository, Digest: "sha256:" + strings.Repeat("b", 64), Platform: "linux/amd64"}}
	credential, err := materials.MaterializeDeployment(context.Background(), identityWorker, fakeCertificate, delivery)
	if err != nil || credential.Version != 1 || string(credential.Password) != "registry-password" {
		t.Fatalf("部署凭据: %v", err)
	}
	wrongCert := &x509.Certificate{SerialNumber: fakeCertificate.SerialNumber, Raw: []byte("other-certificate")}
	if _, err = materials.MaterializeDeployment(context.Background(), identityWorker, wrongCert, delivery); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("伪造证书读取部署凭据: %v", err)
	}
	wrongTarget := delivery
	wrongTarget.Artifact.Repository = "attacker.example.com/leak"
	if _, err = materials.MaterializeDeployment(context.Background(), identityWorker, fakeCertificate, wrongTarget); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("伪造镜像目标可读取凭据: %v", err)
	}
	stale := delivery
	stale.Generation = 2
	if _, err = materials.MaterializeDeployment(context.Background(), identityWorker, fakeCertificate, stale); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("陈旧操作读取部署凭据: %v", err)
	}
	if err := db.Exec("UPDATE worker_certificates SET revoked_at=NOW() WHERE serial='a1'").Error; err != nil {
		t.Fatal(err)
	}
	if _, err = materials.MaterializeDeployment(context.Background(), identityWorker, fakeCertificate, delivery); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("已撤销证书读取部署凭据: %v", err)
	}
	pat, err := identity.CreateAccessToken(context.Background(), connect.NewRequest(&platformv1.CreateAccessTokenRequest{Name: "readonly", ApplicationIds: []string{app}, Scopes: []string{"application:read", "config:read"}}))
	if err != nil {
		t.Fatal(err)
	}
	bearer := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			r.Header().Set("Authorization", "Bearer "+pat.Msg.Plaintext)
			return next(ctx, r)
		}
	}))
	patSecrets := platformv1connect.NewBuildSecretServiceClient(http.DefaultClient, base+"/api", bearer)
	if _, err = patSecrets.ListBuildSecrets(context.Background(), connect.NewRequest(&platformv1.ListBuildSecretsRequest{WorkspaceId: ws, ApplicationId: app})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("PAT管理密钥未拒绝: %v", err)
	}
	var hashes []model.AuditEvent
	if err = db.Find(&hashes).Error; err != nil {
		t.Fatal(err)
	}
	auditJSON, _ := json.Marshal(hashes)
	if bytes.Contains(auditJSON, []byte("sensitive-build-token")) || bytes.Contains(auditJSON, []byte("registry-password")) {
		t.Fatal("审计泄露秘密")
	}
}
