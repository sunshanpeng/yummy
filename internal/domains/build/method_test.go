package build

import (
	"errors"
	"testing"

	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
)

func fileEntries(paths ...string) []sourcearchive.Entry {
	entries := make([]sourcearchive.Entry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, sourcearchive.Entry{Path: path, Size: 1})
	}
	return entries
}

func dirEntry(path string) sourcearchive.Entry {
	return sourcearchive.Entry{Path: path, Directory: true}
}

// 三条选择规则：显式路径、上下文根目录的 Dockerfile、缺失时交给 Railpack。
func TestResolveMethodSelection(t *testing.T) {
	cases := []struct {
		name       string
		config     Config
		entries    []sourcearchive.Entry
		method     string
		dockerfile string
	}{
		{"根目录 Dockerfile 自动使用", Config{Context: "."}, fileEntries("Dockerfile", "main.go"), MethodDockerfile, "Dockerfile"},
		{"没有 Dockerfile 交给 Railpack", Config{Context: "."}, fileEntries("package.json", "index.js"), MethodRailpack, ""},
		{"显式路径优先", Config{Context: ".", Dockerfile: "deploy/app.Dockerfile"}, fileEntries("Dockerfile", "deploy/app.Dockerfile"), MethodDockerfile, "deploy/app.Dockerfile"},
		{"子目录上下文使用其根目录的 Dockerfile", Config{Context: "svc"}, fileEntries("svc/Dockerfile", "Dockerfile"), MethodDockerfile, "svc/Dockerfile"},
		{"子目录上下文不使用仓库根目录的 Dockerfile", Config{Context: "svc"}, fileEntries("Dockerfile", "svc/app.py"), MethodRailpack, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveMethod(c.config, c.entries)
			if err != nil {
				t.Fatal(err)
			}
			if got.Method != c.method || got.Dockerfile != c.dockerfile {
				t.Fatalf("期望 %s/%q，实际 %s/%q", c.method, c.dockerfile, got.Method, got.Dockerfile)
			}
		})
	}
}

// 显式路径或已存在但不合法的 Dockerfile 是输入错误，不能悄悄退回 Railpack。
func TestResolveMethodRejectsInvalidDockerfileInput(t *testing.T) {
	cases := []struct {
		name    string
		config  Config
		entries []sourcearchive.Entry
	}{
		{"显式路径缺失", Config{Context: ".", Dockerfile: "Dockerfile"}, fileEntries("package.json")},
		{"显式路径是目录", Config{Context: ".", Dockerfile: "docker"}, []sourcearchive.Entry{dirEntry("docker")}},
		{"显式路径在上下文之外", Config{Context: "svc", Dockerfile: "Dockerfile"}, fileEntries("Dockerfile", "svc/app.py")},
		{"根目录 Dockerfile 是目录", Config{Context: "."}, []sourcearchive.Entry{dirEntry("Dockerfile"), {Path: "Dockerfile/x", Size: 1}}},
		{"根目录 Dockerfile 是隐式父目录", Config{Context: "."}, fileEntries("Dockerfile/x")},
		{"上下文内的 Dockerfile 是隐式父目录", Config{Context: "svc"}, fileEntries("svc/Dockerfile/x")},
		{"构建上下文是普通文件", Config{Context: "svc"}, fileEntries("svc")},
		{"构建上下文不在快照内", Config{Context: "missing"}, fileEntries("Dockerfile")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := resolveMethod(c.config, c.entries); !errors.Is(err, ErrSourceInvalid) {
				t.Fatalf("应返回源码输入错误，实际 %v", err)
			}
		})
	}
}

// Railpack 构建的限制：目标阶段只适用于 Dockerfile；参数与密钥名称必须能被 Railpack 解析且互不重名。
func TestResolveMethodRailpackInputConstraints(t *testing.T) {
	entries := fileEntries("package.json")
	bad := []struct {
		name   string
		config Config
	}{
		{"目标阶段", Config{Context: ".", Target: "prod"}},
		{"参数名含点号", Config{Context: ".", Args: map[string]string{"a.b": "1"}}},
		{"参数名以数字开头", Config{Context: ".", Args: map[string]string{"1A": "1"}}},
		{"密钥名含连字符", Config{Context: ".", SecretRefs: []SecretRef{{SecretID: "sec_1", Name: "npm-token", Version: 1}}}},
		{"参数与密钥重名", Config{Context: ".", Args: map[string]string{"TOKEN": "1"}, SecretRefs: []SecretRef{{SecretID: "sec_1", Name: "TOKEN", Version: 1}}}},
		{"保留名称", Config{Context: ".", Args: map[string]string{"RAILPACK_CONFIG_FILE": "../x"}}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := resolveMethod(c.config, entries); err == nil {
				t.Fatal("应拒绝")
			}
		})
	}
	ok := Config{Context: ".", Args: map[string]string{"RAILPACK_START_CMD": "node server.js", "NEXT_PUBLIC_API": "https://api"}, SecretRefs: []SecretRef{{SecretID: "sec_1", Name: "NPM_TOKEN", Version: 1}}}
	got, err := resolveMethod(ok, entries)
	if err != nil || got.Method != MethodRailpack {
		t.Fatalf("合法的 Railpack 输入被拒绝: %+v %v", got, err)
	}
	// Dockerfile 构建不受 Railpack 命名限制：带点号、连字符的参数名沿用原规则。
	docker := Config{Context: ".", Target: "prod", Args: map[string]string{"a.b-c": "1"}}
	got, err = resolveMethod(docker, fileEntries("Dockerfile"))
	if err != nil || got.Method != MethodDockerfile {
		t.Fatalf("Dockerfile 构建被误伤: %+v %v", got, err)
	}
}

func TestEffectiveMethodDefaultsToDockerfileForOldSnapshots(t *testing.T) {
	if (Config{Dockerfile: "Dockerfile"}).EffectiveMethod() != MethodDockerfile {
		t.Fatal("方式字段出现前冻结的快照应按 Dockerfile 处理")
	}
	if (Config{Method: MethodRailpack}).EffectiveMethod() != MethodRailpack {
		t.Fatal("已冻结的方式不应被改写")
	}
}
