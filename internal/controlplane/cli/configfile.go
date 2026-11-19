// 配置文件加载与优先级归并。同一配置项的读取优先级固定为：
// 显式命令行参数 > 配置文件 > 环境变量 > 内置默认值。
// 配置文件承接"启动时加载且运行中不可变"的那一层（AGENTS.md 配置分层），
// 运行中可修改的参数仍走数据库 instance_settings，本文件不涉及。
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// configSearchPaths 是未显式指定 --config 时的默认查找位置（依次命中即止）。
// 工作目录文件便于开发，/etc/yummy 面向 systemd 常驻实例。
var configSearchPaths = []string{"config.yaml", "config.yml", "/etc/yummy/config.yaml"}

// resolveConfigPath 决定本次命令使用的配置文件路径：显式 --config 优先，
// 其次环境变量 YUMMY_CONFIG，都没有则按默认查找路径静默探测。
// explicit 表示路径来自用户显式指定（缺失时必须报错，而不是静默跳过）。
// 返回值为空表示本次不加载配置文件。
func resolveConfigPath(args []string) (path string, explicit bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config" || arg == "-c":
			if i+1 < len(args) {
				return args[i+1], true
			}
		case strings.HasPrefix(arg, "--config="):
			return strings.TrimPrefix(arg, "--config="), true
		case strings.HasPrefix(arg, "-c="):
			return strings.TrimPrefix(arg, "-c="), true
		}
	}
	if fromEnv := os.Getenv("YUMMY_CONFIG"); fromEnv != "" {
		return fromEnv, true
	}
	for _, candidate := range configSearchPaths {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, false
		}
	}
	return "", false
}

// loadConfigSettings 读取配置文件为扁平键值（键与命令行 flag 同名，kebab-case）。
// 支持 yaml/yml/toml/json，按扩展名解析；文件必须存在，解析失败直接报错，
// 不允许带病启动。
func loadConfigSettings(path string) (map[string]any, error) {
	reader := viper.New()
	switch {
	case strings.HasSuffix(path, ".yaml"), strings.HasSuffix(path, ".yml"):
		reader.SetConfigType("yaml")
	case strings.HasSuffix(path, ".toml"):
		reader.SetConfigType("toml")
	case strings.HasSuffix(path, ".json"):
		reader.SetConfigType("json")
	default:
		reader.SetConfigType("yaml")
	}
	reader.SetConfigFile(path)
	if err := reader.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	settings := reader.AllSettings()
	// viper 的 AllSettings 键为小写扁平键，直接归并进主 viper 实例。
	return settings, nil
}

// envKeyReplacer 是统一的 viper 环境变量键映射：kebab-case flag 名转大写下划线
// 环境变量名（session-key → SESSION_KEY）。
func envKeyReplacer() *strings.Replacer {
	return strings.NewReplacer("-", "_")
}

// applyConfigSettings 把配置文件值写入主 viper 实例。显式传入的命令行参数
// （flag.Changed）不动——它保持最高优先级；其余键用 viper.Set 提级，
// 使配置文件压过环境变量。配置文件里的未知键也照常写入，供后续通过
// config.GetString 读取（配置项会持续增长，不受 flag 集合限制）。
func applyConfigSettings(v *viper.Viper, settings map[string]any, flags *pflag.FlagSet) {
	for key, value := range settings {
		if key == "config" {
			continue
		}
		if flags != nil {
			if flag := flags.Lookup(key); flag != nil && flag.Changed {
				continue
			}
		}
		v.Set(key, value)
	}
}

// loadStartupConfig 是 Execute 的入口辅助：解析配置文件路径并加载为扁平键值。
// 显式指定（--config / YUMMY_CONFIG）的路径缺失直接报错；默认查找路径全部
// 未命中时静默返回空（纯环境变量/flag 的部署形态保持原样）。
func loadStartupConfig(args []string) (map[string]any, error) {
	path, explicit := resolveConfigPath(args)
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); err != nil {
		if explicit {
			return nil, fmt.Errorf("配置文件不存在: %s", path)
		}
		return nil, nil
	}
	return loadConfigSettings(path)
}
