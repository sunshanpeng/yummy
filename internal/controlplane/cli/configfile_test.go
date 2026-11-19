package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func TestResolveConfigPathExplicitForms(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPath string
		wantExpl bool
	}{
		{name: "空格形式", args: []string{"serve", "--config", "a.yaml"}, wantPath: "a.yaml", wantExpl: true},
		{name: "等号形式", args: []string{"--config=b.yaml", "serve"}, wantPath: "b.yaml", wantExpl: true},
		{name: "短旗标空格", args: []string{"-c", "c.yaml"}, wantPath: "c.yaml", wantExpl: true},
		{name: "短旗标等号", args: []string{"migrate", "-c=d.yaml"}, wantPath: "d.yaml", wantExpl: true},
		{name: "无配置参数", args: []string{"serve", "--log-level", "debug"}, wantPath: "", wantExpl: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, explicit := resolveConfigPath(c.args)
			if path != c.wantPath || explicit != c.wantExpl {
				t.Fatalf("resolveConfigPath(%v) = (%q, %v), want (%q, %v)", c.args, path, explicit, c.wantPath, c.wantExpl)
			}
		})
	}
}

func TestResolveConfigPathEnvFallback(t *testing.T) {
	t.Setenv("YUMMY_CONFIG", "from-env.yaml")
	path, explicit := resolveConfigPath([]string{"serve"})
	if path != "from-env.yaml" || !explicit {
		t.Fatalf("YUMMY_CONFIG 未生效: (%q, %v)", path, explicit)
	}
}

func TestResolveConfigPathDefaultSearch(t *testing.T) {
	t.Setenv("YUMMY_CONFIG", "")
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if path, _ := resolveConfigPath(nil); path != "" {
		t.Fatalf("无文件时不应命中: %q", path)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("log-level: debug\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path, explicit := resolveConfigPath(nil)
	if path != "config.yaml" || explicit {
		t.Fatalf("默认查找未命中工作目录文件: (%q, %v)", path, explicit)
	}
}

func TestLoadStartupConfigExplicitMissingFails(t *testing.T) {
	if _, err := loadStartupConfig([]string{"serve", "--config", filepath.Join(t.TempDir(), "nope.yaml")}); err == nil {
		t.Fatal("显式指定的配置文件缺失必须报错")
	}
}

func TestApplyConfigSettingsPrecedence(t *testing.T) {
	t.Setenv("LOG_LEVEL", "warn")
	v := viper.New()
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(envKeyReplacer())
	v.SetDefault("log-level", "info")
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	_ = flags.String("log-level", "", "")
	_ = flags.String("redis-addr", "", "")
	_ = flags.String("config", "", "")

	// 显式 flag 不被配置文件覆盖。
	if err := flags.Set("log-level", "debug"); err != nil {
		t.Fatal(err)
	}
	applyConfigSettings(v, map[string]any{"log-level": "error", "redis-addr": "redis:6379", "future-key": "v1", "config": "ignored"}, flags)
	if got := flags.Lookup("log-level").Value.String(); got != "debug" {
		t.Fatalf("显式 flag 被配置文件覆盖: %q", got)
	}
	if got := v.GetString("redis-addr"); got != "redis:6379" {
		t.Fatalf("配置文件应写入 viper: %q", got)
	}
	if got := v.GetString("future-key"); got != "v1" {
		t.Fatalf("未知键应透传供后续读取: %q", got)
	}
	if got := v.GetString("config"); got != "" {
		t.Fatalf("config 键不应从文件归并: %q", got)
	}
}

func TestConfigFileBeatsEnvWhileFlagStillBeatsFile(t *testing.T) {
	t.Setenv("LOG_LEVEL", "warn")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log-level: error\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := loadConfigSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	// 场景一：未显式传 flag，配置文件压过环境变量。
	v1 := viper.New()
	v1.AutomaticEnv()
	v1.SetEnvKeyReplacer(envKeyReplacer())
	v1.SetDefault("log-level", "info")
	emptyFlags := pflag.NewFlagSet("t", pflag.ContinueOnError)
	_ = emptyFlags.String("log-level", "", "")
	v1.BindPFlag("log-level", emptyFlags.Lookup("log-level"))
	applyConfigSettings(v1, settings, emptyFlags)
	if got := v1.GetString("log-level"); got != "error" {
		t.Fatalf("配置文件应压过环境变量: %q", got)
	}
	// 场景二：显式 flag 压过配置文件（还原 root.go 的 BindPFlag 机制）。
	v2 := viper.New()
	v2.AutomaticEnv()
	v2.SetEnvKeyReplacer(envKeyReplacer())
	v2.SetDefault("log-level", "info")
	flags := pflag.NewFlagSet("t", pflag.ContinueOnError)
	_ = flags.String("log-level", "", "")
	if err := flags.Set("log-level", "debug"); err != nil {
		t.Fatal(err)
	}
	v2.BindPFlag("log-level", flags.Lookup("log-level"))
	applyConfigSettings(v2, settings, flags)
	if got := v2.GetString("log-level"); got != "debug" {
		t.Fatalf("显式 flag 应压过配置文件: %q", got)
	}
}

func TestLoadConfigSettingsRejectsBrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("key: [未闭合"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigSettings(path); err == nil {
		t.Fatal("解析失败的配置文件必须报错")
	}
}
