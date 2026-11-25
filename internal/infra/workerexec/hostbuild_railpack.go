package workerexec

import (
	"errors"
	"os"
	"os/user"
	"strconv"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/buildrunner"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
)

// 宿主构建的 Railpack 路径（2026-11-23 构建方式自动选择）：源码没有 Dockerfile 时，
// worker 先以降权身份调用本机固定版本的 Railpack 生成计划，再让 buildctl 通过
// digest 钉住的 Railpack 前端镜像执行该计划。事件、日志与摘要协议和 Dockerfile 路径一致。

// hostRailpackReady 要求宿主 Railpack 二进制与 digest 钉住的前端镜像同时配置；缺一则失败关闭。
func (e *Executor) hostRailpackReady() bool {
	return e.options.RailpackPath != "" && buildrunner.ValidRailpackFrontendImage(e.options.Installed.RailpackFrontendImage)
}

// hostRailpackArgs 生成 Railpack 路径交给固定校验器的 buildctl 参数，以及需要落盘的挂载文件。
func (e *Executor) hostRailpackArgs(task kube.BuildTask) ([]string, []buildrunner.RailpackMount, error) {
	secrets := make([]buildrunner.RailpackSecret, 0, len(task.Secrets))
	for _, secret := range task.Secrets {
		secrets = append(secrets, buildrunner.RailpackSecret{ID: secret.ID, Value: secret.Value})
	}
	return buildrunner.RailpackBuildArgs(buildrunner.RailpackBuildInput{
		FrontendImage: e.options.Installed.RailpackFrontendImage, Context: task.Context, ImageTag: task.ImageTag,
		ApplicationID: task.ApplicationID, Args: task.BuildArgs, Secrets: secrets,
	})
}

// hostStage 上报阶段变化，让页面按实际阶段显示识别与构建。RECONCILE 重放不再发阶段事件：
// 控制面要求阶段只能前进，重放可能晚于已经到达的阶段。
func (e *Executor) hostStage(command *platformv1.WorkerBuildCommand, jobUID string, emitter Emitter, stage string) error {
	if command.Action != "RUN" {
		return nil
	}
	return emitter.BuildEvent(&platformv1.WorkerBuildEvent{AttemptId: command.AttemptId, Kind: "STAGE", JobUid: jobUID, Stage: stage})
}

// railpackFailureCode 把识别阶段的失败映射为构建错误码；空串表示不是 Railpack 给出的结论（宿主环境问题）。
func railpackFailureCode(err error) string {
	switch {
	case errors.Is(err, buildrunner.ErrRailpackPlan):
		return "RAILPACK_PLAN_FAILED"
	case errors.Is(err, buildrunner.ErrRailpackTransient):
		return "RAILPACK_PLAN_TRANSIENT"
	case errors.Is(err, buildrunner.ErrRailpackVersion):
		return "RAILPACK_UNAVAILABLE"
	}
	return ""
}

func secretIDs(secrets []kube.BuildSecret) []string {
	ids := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		ids = append(ids, secret.ID)
	}
	return ids
}

// railpackDropIdentity 在 worker 以 root 运行时返回降权身份（nobody）：Railpack 分析的是不可信源码，
// 不应继承 root。非 root 运行时返回 0，0 表示不降权。
func railpackDropIdentity() (uint32, uint32) {
	if os.Geteuid() != 0 {
		return 0, 0
	}
	if account, err := user.Lookup("nobody"); err == nil {
		uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
		gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
		if uidErr == nil && gidErr == nil && uid != 0 {
			return uint32(uid), uint32(gid)
		}
	}
	return 65534, 65534
}
