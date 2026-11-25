package clientcli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestDirectoryDeployDryRunAndBuildOnly(t *testing.T) {
	t.Setenv("YUMMY_TOKEN", "test-deploy")
	var uploads, builds, releases, mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-deploy" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		var method protoreflect.MethodDescriptor
		if !strings.HasPrefix(r.URL.Path, "/transfer/") {
			methodName := strings.Replace(strings.TrimPrefix(r.URL.Path, "/api/"), "/", ".", 1)
			desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(methodName))
			if err != nil {
				t.Errorf("未知 RPC %s", methodName)
				http.NotFound(w, r)
				return
			}
			method = desc.(protoreflect.MethodDescriptor)
			req := dynamicpb.NewMessage(method.Input())
			payload, _ := io.ReadAll(r.Body)
			if err := proto.Unmarshal(payload, req); err != nil {
				t.Errorf("解码 RPC %s: %v", methodName, err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			js, _ := protojson.Marshal(req)
			_ = json.Unmarshal(js, &body)
		}
		respond := func(js string) {
			msg := dynamicpb.NewMessage(method.Output())
			if err := protojson.Unmarshal([]byte(js), msg); err != nil {
				t.Errorf("编码 RPC %s: %v", method.Name(), err)
				http.Error(w, "bad response", http.StatusInternalServerError)
				return
			}
			payload, _ := proto.Marshal(msg)
			w.Header().Set("Content-Type", "application/proto")
			_, _ = w.Write(payload)
		}
		switch filepath.Base(r.URL.Path) {
		case "GetWorkspace":
			if body["slug"] != "team" {
				t.Errorf("空间参数 = %v", body)
			}
			respond(`{"workspace":{"id":"w1","slug":"team"}}`)
		case "GetApplication":
			if body["workspaceId"] != "w1" || body["applicationName"] != "app" {
				t.Errorf("应用参数 = %v", body)
			}
			respond(`{"application":{"id":"a1","workspaceId":"w1","name":"app"}}`)
		case "GetBuildConfig":
			if body["workspaceId"] != "w1" || body["applicationId"] != "a1" {
				t.Errorf("目录关联没有解析为资源 ID：%v", body)
			}
			respond(`{"revision":"2","config":{"context":"service","dockerfile":"service/Build.Dockerfile"}}`)
		case "ResolveExecution":
			respond(`{"summary":{"policyRevision":"8","platform":"linux/amd64","imageRepository":"registry.test/w1-a1","ready":true}}`)
		case "CreateSourceUpload":
			uploads.Add(1)
			if body["dockerfile"] != "Build.Dockerfile" {
				t.Errorf("已打包路径未归一到上下文根：%v", body)
			}
			respond(`{"upload":{"id":"up1"},"transferPath":"/transfer/up1"}`)
		case "up1":
			n, _ := io.Copy(io.Discard, r.Body)
			if r.Method != "PUT" || n == 0 {
				t.Error("缺少归档")
			}
			w.WriteHeader(http.StatusNoContent)
		case "CompleteSourceUpload":
			respond(`{"snapshot":{"id":"src1","dockerfile":"Build.Dockerfile"}}`)
		case "GetRuntimeConfig":
			respond(`{"config":{"revision":"9","confirmed":true}}`)
		case "GetRuntimeStatus":
			if body["workspaceId"] != "w1" || body["applicationId"] != "a1" {
				t.Errorf("运行状态目标错误：%v", body)
			}
			respond(`{"status":{"generation":"6","servingReleaseId":"previous-release"}}`)
		case "GetRuntimeLogs":
			if body["workspaceId"] != "w1" || body["applicationId"] != "a1" {
				t.Errorf("运行日志目标错误：%v", body)
			}
			if body["cursor"] == "page-a" {
				respond(`{"chunks":[{"id":"log-2"}],"resumeCursor":"page-b"}`)
			} else {
				respond(`{"chunks":[{"id":"log-1"}],"nextCursor":"page-a","resumeCursor":"page-a"}`)
			}
		case "ReadBuildLogs":
			if body["workspaceId"] != "w1" || body["applicationId"] != "a1" || body["buildId"] != "b1" {
				t.Errorf("构建日志目标错误：%v", body)
			}
			respond(`{"complete":true}`)
		case "GetRuntimeMetrics":
			if body["workspaceId"] != "w1" || body["applicationId"] != "a1" || body["since"] != "2026-11-25T00:00:00Z" {
				t.Errorf("监控查询参数错误：%v", body)
			}
			respond(`{"samples":[{"id":"sample-1","cpuNanoCores":"300"}]}`)
		case "PreviewBuild", "CreateBuild":
			input, _ := body["input"].(map[string]any)
			overrides, _ := input["overrides"].(map[string]any)
			if input["sourceSnapshotId"] != "src1" || input["expectedPolicyRevision"] != "8" || overrides["context"] != "." || overrides["dockerfile"] != "Build.Dockerfile" {
				t.Errorf("构建未按快照和策略冻结：%v", input)
			}
			if mode.Load() == 1 {
				if input["autoDeploy"] != true || input["runtimeConfigRevision"] != "9" || input["expectedServingRelease"] != "previous-release" || input["expectedRuntimeGeneration"] != "6" {
					t.Errorf("异步发布未绑定运行版本：%v", input)
				}
			} else if input["autoDeploy"] == true {
				t.Errorf("意外发起后台发布：%v", input)
			}
			if filepath.Base(r.URL.Path) == "PreviewBuild" {
				respond(`{"preview":{"execution":{"policyRevision":"8","ready":true}}}`)
			} else {
				builds.Add(1)
				if mode.Load() == 1 {
					respond(`{"build":{"id":"b1","state":"QUEUED","autoDeploy":true}}`)
				} else {
					respond(`{"build":{"id":"b1","state":"SUCCEEDED","artifact":{"id":"img1","state":"VERIFIED"}}}`)
				}
			}
		case "CreateDeployment":
			releases.Add(1)
			expected, _ := body["expected"].(map[string]any)
			if mode.Load() != 2 || expected["configRevision"] != "9" || expected["servingReleaseId"] != "previous-release" || expected["generation"] != "6" || body["artifactId"] != "img1" || body["idempotencyKey"] == "" {
				t.Errorf("发布未绑定构建开始前的运行事实：%v", body)
			}
			respond(`{"deployment":{"operation":{"id":"op1","state":"QUEUED"}}}`)
		case "GetDeployment":
			respond(`{"deployment":{"release":{"id":"rel1","domains":["app.example.com"]},"operation":{"id":"op1","state":"SUCCEEDED"}}}`)
		default:
			t.Errorf("请求了不相关接口 %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	project := t.TempDir()
	t.Chdir(project)
	if err := os.Mkdir("service", 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"service/Build.Dockerfile": "FROM scratch\n", "service/data.txt": "data", "outside.txt": "outside"} {
		if err := os.WriteFile(name, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
	defer cancel()
	dir := configDir(t)
	if _, _, err := runCLI(ctx, dir, server.URL, "use", "team/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, ".yummy")); !os.IsNotExist(err) {
		t.Fatalf("目录关联写入了项目：%v", err)
	}
	out, _, err := runCLI(ctx, dir, server.URL, "deploy", ".", "--dry-run", "--build-only", "--output", "json")
	if err != nil || !strings.Contains(out, "Build.Dockerfile") || !strings.Contains(out, "data.txt") || strings.Contains(out, "outside.txt") || uploads.Load() != 0 {
		t.Fatalf("预览错误：%s, %v，上传数 %d", out, err, uploads.Load())
	}
	out, _, err = runCLI(ctx, dir, server.URL, "deploy", project, "--build-only")
	if err != nil || !strings.Contains(out, "VERIFIED") || uploads.Load() != 1 || builds.Load() != 1 || releases.Load() != 0 {
		t.Fatalf("只构建流程错误：%s, %v，上传 %d 构建 %d 发布 %d", out, err, uploads.Load(), builds.Load(), releases.Load())
	}
	mode.Store(1)
	out, _, err = runCLI(ctx, dir, server.URL, "deploy", "--no-wait")
	if err != nil || !strings.Contains(out, "QUEUED") || releases.Load() != 0 {
		t.Fatalf("后台受理返回错误：%s, %v", out, err)
	}
	mode.Store(2)
	out, _, err = runCLI(ctx, dir, server.URL, "deploy")
	if err != nil || !strings.Contains(out, "SUCCEEDED") || !strings.Contains(out, "app.example.com") || releases.Load() != 1 {
		t.Fatalf("等待真实发布结果错误：%s, %v，发布数 %d", out, err, releases.Load())
	}
	out, _, err = runCLI(ctx, dir, server.URL, "status", "--output", "json")
	if err != nil || !strings.Contains(out, "previous-release") {
		t.Fatalf("目录运行状态：%s, %v", out, err)
	}
	out, _, err = runCLI(ctx, dir, server.URL, "logs", "--build", "b1", "--follow")
	if err != nil || !strings.Contains(out, "complete") {
		t.Fatalf("目录构建日志：%s, %v", out, err)
	}
	out, _, err = runCLI(ctx, dir, server.URL, "metrics", "--since", "2026-11-25T00:00:00Z")
	if err != nil || !strings.Contains(out, "sample-1") {
		t.Fatalf("目录运行指标：%s, %v", out, err)
	}
	logCtx, stopLogs := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stopLogs()
	out, _, err = runCLI(logCtx, dir, server.URL, "logs", "--follow")
	if err == nil || strings.Count(out, "log-1") != 1 || strings.Count(out, "log-2") != 1 {
		t.Fatalf("运行日志没有按游标续读：%s, %v", out, err)
	}
}
