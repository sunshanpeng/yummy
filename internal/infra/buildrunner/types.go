// Package buildrunner 是每个构建Job按需运行的固定准备/BuildKit助手，不是常驻服务或通用执行器。
package buildrunner

import (
	"errors"
	"path"
	"regexp"
	"strings"
)

var ErrInvalid = errors.New("固定构建runner参数无效")
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var digestHexPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

// BuildAuthDir 是构建凭据与构建密钥文件的固定目录，参数校验器只认这一个目录。
// Pod 内是只读 Secret 挂载；宿主构建由 worker 在同名路径创建 0700 私有目录，
// 密钥文件名固定为 secret-<序号>。
const BuildAuthDir = "/run/build-auth"

// 路径只在包内测试替换；生产CLI不能选择宿主路径、执行器或metadata结果位置。
type paths struct{ workspace, sourceToken, sourceAuth, buildAuth, runtime, metadata, result, script, plan string }

func fixedPaths() paths {
	return paths{workspace: "/workspace", sourceToken: "/run/source-auth/source-token", sourceAuth: "/run/source-auth", buildAuth: BuildAuthDir, runtime: "/run/yummy-runtime", metadata: "/workspace/buildkit-metadata.json", result: "/dev/termination-log", script: "/usr/local/bin/buildctl-daemonless.sh", plan: RailpackPlanDir}
}
func safeRelative(value string) bool {
	return value != "" && !path.IsAbs(value) && path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\\x00\r\n,")
}
