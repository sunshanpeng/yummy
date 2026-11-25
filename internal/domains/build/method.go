package build

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
)

// railpackEnvName 是 Railpack 能正确解析的变量名：它按 [A-Za-z0-9_+-] 切分 --env，
// 带点号的名称会被截断，所以比 Dockerfile 的 build-arg 名称更严格。
var railpackEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// railpackReservedNames 不允许由用户配置：RAILPACK_CONFIG_FILE 可以把配置文件路径指到源码目录之外。
var railpackReservedNames = map[string]bool{"RAILPACK_CONFIG_FILE": true}

// resolveMethod 按“构建方式自动选择”确定并冻结构建方式，返回的配置带 Method 与实际 Dockerfile：
//
//   - 显式指定 Dockerfile：必须是上下文内的普通文件，否则输入错误，不退回 Railpack；
//   - 未指定且上下文根目录有 Dockerfile：使用它；路径存在但不是文件同样是输入错误；
//   - 未指定且上下文根目录没有 Dockerfile：交给 Railpack。
//
// entries 是已验证快照的条目清单，路径相对源码归档根。
func resolveMethod(c Config, entries []sourcearchive.Entry) (Config, error) {
	files := make(map[string]bool, len(entries))
	dirs := map[string]bool{".": true}
	for _, entry := range entries {
		if entry.Directory {
			dirs[entry.Path] = true
		} else {
			files[entry.Path] = true
		}
		// 归档允许省略父目录条目，解包时仍会创建这些目录。
		for parent := path.Dir(entry.Path); !dirs[parent]; parent = path.Dir(parent) {
			dirs[parent] = true
		}
	}
	if !dirs[c.Context] {
		return c, fmt.Errorf("%w: 构建上下文 %q 不是源码快照内的目录", ErrSourceInvalid, c.Context)
	}
	candidate := "Dockerfile"
	if c.Context != "." {
		candidate = c.Context + "/Dockerfile"
	}
	switch {
	case c.Dockerfile != "":
		if !files[c.Dockerfile] || (c.Context != "." && !strings.HasPrefix(c.Dockerfile, c.Context+"/")) {
			return c, fmt.Errorf("%w: 指定的 Dockerfile %q 不存在或不在构建上下文内", ErrSourceInvalid, c.Dockerfile)
		}
		c.Method = MethodDockerfile
	case files[candidate]:
		c.Dockerfile, c.Method = candidate, MethodDockerfile
	case dirs[candidate]:
		return c, fmt.Errorf("%w: %q 存在但不是普通文件", ErrSourceInvalid, candidate)
	default:
		c.Dockerfile, c.Method = "", MethodRailpack
		if err := validateRailpackInputs(c); err != nil {
			return c, err
		}
	}
	return c, nil
}

// validateRailpackInputs 校验只对 Railpack 构建生效的约束：目标阶段属于 Dockerfile 多阶段构建；
// 构建参数与密钥引用都以环境变量形式交给 Railpack，名称必须能被它解析且互不重名。
func validateRailpackInputs(c Config) error {
	if c.Target != "" {
		return invalid("目标阶段只适用于 Dockerfile 构建，当前源码没有 Dockerfile，将由 Railpack 构建")
	}
	seen := make(map[string]bool, len(c.Args)+len(c.SecretRefs))
	check := func(kind, name string) error {
		if !railpackEnvName.MatchString(name) || railpackReservedNames[name] {
			return invalid(fmt.Sprintf("Railpack 构建的%s名称 %q 不合法，只能使用字母、数字和下划线且不能以数字开头", kind, name))
		}
		if seen[name] {
			return invalid(fmt.Sprintf("Railpack 构建的参数与密钥名称不能重复: %q", name))
		}
		seen[name] = true
		return nil
	}
	for name := range c.Args {
		if err := check("构建参数", name); err != nil {
			return err
		}
	}
	for _, ref := range c.SecretRefs {
		if err := check("构建密钥", ref.Name); err != nil {
			return err
		}
	}
	return nil
}
