package workerexec

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/platform/kube"
	"github.com/sunshanpeng/yummy/internal/platform/runtimeconfig"
	"github.com/sunshanpeng/yummy/internal/platform/workergateway"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 部署观察 Kind 与控制面 deployment.ApplyObservation 白名单一一对应；
// LOST 只是未知，绝不冒充 STOPPED 或 OBSERVED。
const (
	observationKindApplying     = "APPLYING"
	observationKindWaitingReady = "WAITING_READY"
	observationKindObserved     = "OBSERVED"
	observationKindStopped      = "STOPPED"
	observationKindLost         = "LOST"
	observationKindFailed       = "FAILED"
)

// Deploy 把控制面 WorkerDeploymentCommand 翻译为 kube 运行时变更并驱动到终态证据：
// 每次 Kubernetes 写入前经 generation 守卫核对控制面持久期望态（Adapter.Runtime 已接线）；
// RECONCILE 重投同样执行——kube 适配器按快照指纹与 operation/release/generation 标签
// 幂等核对后原样跳过，不重复创建模板、不触发多余重启。观察循环按 Options.ObserveInterval
// 轮询并 emitter.Observation，字段只映射 kube 适配器实际返回的证据；ctx 取消立即退出，
// 不发终态，journal 保留已持久化观察待重投。
func (e *Executor) Deploy(ctx context.Context, command *platformv1.WorkerDeploymentCommand, reconcile bool, emitter workergateway.Emitter) error {
	if command == nil || emitter == nil {
		return errors.New("workerexec: 部署命令或发射器为空")
	}
	stop := command.Action == "STOP"
	change, material, err := e.runtimeChange(command)
	if err != nil {
		return err
	}
	// DEPLOY/ROLLBACK/RECOVERY/START 走幂等 Apply；RESTART/STOP 只做幂等 scale。
	// STOP 仅缩容 replicas=0，不删除模板与 PVC；HTTP 改后台的路由清理由适配器负责。
	switch command.Action {
	case "DEPLOY", "ROLLBACK", "RECOVERY", "START":
		route := kube.Route{}
		if material.View.Type == runtimeconfig.TypeHTTP {
			// 域名属冻结发布快照；TLS 绑定是实例级运行设施的当下事实，控制面领取时
			// 已按全部域名一致绑定汇聚后经 wire 协议下发（https + tls_secret_name）。
			// 未一致绑定时命令不携带 TLS，入口探测将因无 TLS 证据而无法收敛，
			// 失败关闭，worker 不猜测或伪造证书。
			route.Hosts = slices.Clone(material.View.Domains)
			route.HTTPS = command.GetHttps()
			route.TLSSecretName = command.GetTlsSecretName()
		}
		if err := e.options.Adapter.BootstrapRuntimeNamespace(ctx, command.SpaceSlug, command.WorkspaceId); err != nil {
			return err
		}
		if _, err := e.runtime.Apply(ctx, change, material, route); err != nil {
			return err
		}
	case "RESTART":
		if err := e.runtime.Restart(ctx, change); err != nil {
			return err
		}
	case "STOP":
		e.stopRuntimeCollector(change.Release.ID)
		if err := e.runtime.Stop(ctx, change); err != nil {
			return err
		}
	default:
		return fmt.Errorf("workerexec: 未知部署动作 %q", command.Action)
	}
	return e.observeLoop(ctx, command, change, material.View.Type == runtimeconfig.TypeHTTP, stop, emitter)
}

// runtimeChange 只做字段翻译；授权、钉住镜像与快照一致性校验由 kube 适配器和
// 控制面守卫完成。STOP 不携带仓库凭据，scale 路径也不消费 materialized 配置。
func (e *Executor) runtimeChange(command *platformv1.WorkerDeploymentCommand) (kube.RuntimeChange, runtimeconfig.Materialized, error) {
	if command.Config == nil {
		return kube.RuntimeChange{}, runtimeconfig.Materialized{}, errors.New("workerexec: 部署命令缺少运行配置快照")
	}
	clusterNodeUID := command.ClusterNodeUid
	if clusterNodeUID == "" {
		clusterNodeUID = string(e.options.Node.UID)
	}
	if clusterNodeUID != string(e.options.Node.UID) {
		return kube.RuntimeChange{}, runtimeconfig.Materialized{}, fmt.Errorf("%w: 部署目标节点不是本节点", kube.ErrOwnership)
	}
	config := command.Config
	if config.GetWorkspaceId() != command.WorkspaceId || config.GetApplicationId() != command.ApplicationId || config.GetRevision() < 1 {
		return kube.RuntimeChange{}, runtimeconfig.Materialized{}, errors.New("workerexec: 运行配置与应用身份或版本不一致")
	}
	registry, credentialVersion := []byte(nil), int64(0)
	if command.Registry != nil {
		var err error
		if registry, err = dockerConfigJSON(command.Registry); err != nil {
			return kube.RuntimeChange{}, runtimeconfig.Materialized{}, err
		}
		credentialVersion = command.Registry.GetVersion()
	}
	change := kube.RuntimeChange{
		Release: kube.Release{
			ID:            command.ReleaseId,
			WorkspaceID:   command.WorkspaceId,
			ApplicationID: command.ApplicationId,
			SpaceSlug:     command.SpaceSlug,
			AppName:       command.AppName,
			// 控制面只发布 digest 钉住镜像；repository@digest 形态由 kube 适配器复核。
			Image:                     command.Repository + "@" + command.Digest,
			ConfigRevision:            config.GetRevision(),
			Placement:                 e.options.Node,
			RegistryDockerConfig:      registry,
			RegistryCredentialVersion: credentialVersion,
		},
		OperationID:        command.OperationId,
		ExpectedGeneration: command.Generation,
	}
	material := runtimeconfig.Materialized{
		Variables: maps.Clone(command.MaterializedVariables),
		Files:     maps.Clone(command.MaterializedFiles),
	}
	view := runtimeconfig.View{
		WorkspaceID:   command.WorkspaceId,
		ApplicationID: command.ApplicationId,
		Revision:      config.GetRevision(),
		Confirmed:     config.GetConfirmed(),
		Type:          config.GetType(),
		Port:          config.GetPort(),
		Command:       slices.Clone(config.GetCommand()),
		Args:          slices.Clone(config.GetArgs()),
		Domains:       slices.Clone(config.GetDomains()),
	}
	if probe := config.GetProbe(); probe != nil {
		view.Probe = runtimeconfig.Probe{Path: probe.GetPath(), InitialDelaySeconds: probe.GetInitialDelaySeconds(), PeriodSeconds: probe.GetPeriodSeconds(), TimeoutSeconds: probe.GetTimeoutSeconds(), FailureThreshold: probe.GetFailureThreshold()}
	}
	if resources := config.GetResources(); resources != nil {
		view.Resources = runtimeconfig.Resources{CPURequestMilli: resources.GetCpuRequestMilli(), CPULimitMilli: resources.GetCpuLimitMilli(), MemoryRequestMiB: resources.GetMemoryRequestMib(), MemoryLimitMiB: resources.GetMemoryLimitMib()}
	}
	for _, variable := range config.GetVariables() {
		view.Variables = append(view.Variables, runtimeconfig.Variable{Name: variable.GetName(), Sensitive: variable.GetSensitive(), Configured: variable.GetConfigured()})
	}
	for _, file := range config.GetFiles() {
		view.Files = append(view.Files, runtimeconfig.ConfigFile{Path: file.GetPath(), Sensitive: file.GetSensitive(), Configured: file.GetConfigured()})
	}
	for _, volume := range config.GetVolumes() {
		view.Volumes = append(view.Volumes, runtimeconfig.Volume{Name: volume.GetName(), MountPath: volume.GetMountPath(), SizeGiB: volume.GetSizeGib()})
	}
	material.View = view
	return change, material, nil
}

// observeLoop 观察运行证据并按间隔发射观察事件，直到终态或期望资源消失。
// 终态：非停止动作为 OBSERVED/FAILED，停止动作为 STOPPED；Deployment 期望资源
// NotFound 时如实上报 LOST。其余错误原样返回保留 journal 待重投。运行日志跟随
// 与指标采样挂在同生命周期的尽力而为旁路上（见 runtimecollect.go）：每轮证据
// 推给旁路，Deploy 返回或 ctx 取消时旁路统一停止并等待退出，失败绝不影响状态机。
func (e *Executor) observeLoop(ctx context.Context, command *platformv1.WorkerDeploymentCommand, change kube.RuntimeChange, http, stop bool, emitter workergateway.Emitter) error {
	// 运行日志/指标采集生命周期改挂部署本体（见 startRuntimeCollector）：Apply 成功
	// 即启动持久采集器，部署消失（LOST）或 STOP 时停止；不再随操作跟踪窗口结束。
	if stop {
		e.stopRuntimeCollector(change.Release.ID)
	} else {
		if err := e.startRuntimeCollector(command, change, http, emitter); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(e.options.ObserveInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		evidence, err := e.runtime.Observe(ctx, change, http)
		if err != nil {
			if apierrors.IsNotFound(err) {
				e.stopRuntimeCollector(change.Release.ID)
				return emitter.Observation(e.deploymentObservation(command, kube.RuntimeEvidence{CollectedAt: time.Now().UTC()}, observationKindLost, ""))
			}
			return err
		}
		// 旁路证据由持久采集器独立观察（collectors.go），跟踪循环不再喂第二套。
		kind, code := classifyRuntime(evidence, stop)
		// KubernetesReady 为假且已过发布进度期限时按真实状态条件上报 FAILED，
		// 让控制面走失败收尾/自动恢复；证据不足时继续保持 WAITING_READY。
		if kind == observationKindWaitingReady && e.progressDeadlineExceeded(ctx, change, evidence) {
			kind, code = observationKindFailed, "PROGRESS_DEADLINE_EXCEEDED"
		}
		if err := emitter.Observation(e.deploymentObservation(command, evidence, kind, code)); err != nil {
			return err
		}
		// 可运维性：观察不收敛时把证据缺失原因写入节点日志（不含敏感值），
		// 运维无需进集群即可判断卡在哪一项。
		if kind == observationKindWaitingReady && len(evidence.Reasons) > 0 {
			for _, reason := range evidence.Reasons {
				fmt.Fprintln(os.Stderr, "worker观察未收敛:", reason)
			}
		}
		switch kind {
		case observationKindObserved, observationKindStopped, observationKindFailed:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// classifyRuntime 把 kube 运行证据映射为控制面观察 Kind；不发明证据之外的终态。
func classifyRuntime(evidence kube.RuntimeEvidence, stop bool) (string, string) {
	if stop {
		if evidence.Stopped {
			return observationKindStopped, ""
		}
		return observationKindApplying, ""
	}
	switch {
	case !evidence.CorrectGeneration:
		return observationKindApplying, ""
	case evidence.KubernetesReady:
		return observationKindObserved, ""
	default:
		return observationKindWaitingReady, ""
	}
}

// deploymentObservation 只映射 kube 证据：Seq 固定 0 由 workergateway.Journal
// 递增；digest 仅在镜像 ID 证据匹配钉住 digest 时如实回传；PodUID 只取当前
// 发布且就绪的 Pod；副本数按 Pod 终止态如实统计。
func (e *Executor) deploymentObservation(command *platformv1.WorkerDeploymentCommand, evidence kube.RuntimeEvidence, kind, code string) *platformv1.WorkerDeploymentObservation {
	clusterNodeUID := command.ClusterNodeUid
	if clusterNodeUID == "" {
		clusterNodeUID = string(e.options.Node.UID)
	}
	var replicas, readyReplicas int32
	podUID := ""
	for _, pod := range evidence.Pods {
		if pod.Phase != string(corev1.PodSucceeded) && pod.Phase != string(corev1.PodFailed) {
			replicas++
		}
		if pod.Ready && !pod.Deleting {
			readyReplicas++
		}
		if podUID == "" && pod.ReleaseID == command.ReleaseId && pod.Ready && !pod.Deleting {
			podUID = string(pod.UID)
		}
	}
	digest := ""
	if evidence.CorrectDigest {
		digest = command.Digest
	}
	return &platformv1.WorkerDeploymentObservation{
		WorkspaceId:        command.WorkspaceId,
		ApplicationId:      command.ApplicationId,
		OperationId:        command.OperationId,
		ReleaseId:          command.ReleaseId,
		Generation:         command.Generation,
		Seq:                0,
		ClusterNodeUid:     clusterNodeUID,
		DeploymentUid:      string(evidence.DeploymentUID),
		PodUid:             podUID,
		Digest:             digest,
		CorrectGeneration:  evidence.CorrectGeneration,
		ReadyReplicas:      readyReplicas,
		Replicas:           replicas,
		OldPodsGone:        evidence.OldPodsExited,
		ServiceEndpoints:   evidence.ServiceEndpoints,
		Kind:               kind,
		ErrorCode:          code,
		CollectedUnixMilli: evidence.CollectedAt.UnixMilli(),
	}
}

// progressDeadlineExceeded 用真实 Deployment 状态条件判定发布失败。只信任与本
// 操作 generation/operation/release 标签及 UID 完全一致的 Deployment，避免把
// 他人资源或旧代际误判为失败；读取失败一律不构成 FAILED。
func (e *Executor) progressDeadlineExceeded(ctx context.Context, change kube.RuntimeChange, evidence kube.RuntimeEvidence) bool {
	if evidence.DeploymentUID == "" {
		return false
	}
	deployment, err := e.options.Client.AppsV1().Deployments(e.options.RuntimeNamespace).Get(ctx, kube.AppName(change.Release.ApplicationID), metav1.GetOptions{})
	if err != nil || deployment.UID != evidence.DeploymentUID {
		return false
	}
	labels := deployment.Labels
	if labels[kube.LabelManaged] != "worker" || labels[kube.LabelGeneration] != strconv.FormatInt(change.ExpectedGeneration, 10) || labels[kube.LabelOperation] != change.OperationID || labels[kube.LabelRelease] != change.Release.ID {
		return false
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse && condition.Reason == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}
