package kube

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

type PodEvidence struct {
	UID                             types.UID
	Name, ReleaseID, Phase, ImageID string
	Ready                           bool
	Restarts                        int32
	Deleting                        bool
	Reasons                         []string
}
type RuntimeEvidence struct {
	CollectedAt                                                                       time.Time
	DeploymentUID                                                                     types.UID
	Generation, ObservedGeneration                                                    int64
	DesiredGeneration                                                                 int64
	CorrectGeneration, CorrectDigest, Ready, OldPodsExited, ServiceEndpoints, Stopped bool
	KubernetesReady                                                                   bool
	// HTTP应用必须由控制面另行验证已登记入口/TLS，不能仅凭KubernetesReady最终成功。
	RequiresIngressProbe bool
	Pods                 []PodEvidence
	Reasons              []string
}

func deploymentOwnsRS(d *appsv1.Deployment, refs []metav1.OwnerReference) bool {
	for _, o := range refs {
		if o.Kind == "Deployment" && o.UID == d.UID && o.Name == d.Name && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}
func podOwnerRS(p *corev1.Pod, rs map[types.UID]bool) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "ReplicaSet" && rs[o.UID] && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}
func podReady(p *corev1.Pod) bool {
	for _, condition := range p.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
func terminalPod(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// Observe 仅读资源证据，不更改控制面DB，不宣告真实访问URL。
func (r *Runtime) Observe(ctx context.Context, c RuntimeChange, http bool) (RuntimeEvidence, error) {
	e := RuntimeEvidence{CollectedAt: time.Now().UTC(), DesiredGeneration: c.ExpectedGeneration, RequiresIngressProbe: http}
	if err := c.validate(); err != nil {
		return e, err
	}
	if err := r.adapter.checkNodeIdentity(ctx, c.Release.Placement); err != nil {
		return e, err
	}
	a := r.adapter
	ns := a.runtimeNamespace(c)
	name := a.deployedName(c.Release)
	d, err := a.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return e, err
	}
	if err := runtimeOwned(d, c); err != nil {
		// 归属/代际拒绝必须带资源上下文，否则操作者无从判断是哪个对象冲突。
		return e, fmt.Errorf("%w: deployment %s", err, name)
	}
	actualHTTP := false
	for _, container := range d.Spec.Template.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name == "http" {
				actualHTTP = true
			}
		}
	}
	if actualHTTP != http {
		return e, fmt.Errorf("%w: Observe类型必须与已部署快照匹配", ErrInvalid)
	}
	e.DeploymentUID = d.UID
	e.Generation = d.Generation
	e.ObservedGeneration = d.Status.ObservedGeneration
	generation := strconv.FormatInt(c.ExpectedGeneration, 10)
	e.CorrectGeneration = d.Labels[LabelGeneration] == generation && d.Labels[LabelOperation] == c.OperationID && d.Labels[LabelRelease] == c.Release.ID && d.Status.ObservedGeneration >= d.Generation
	e.CorrectDigest = len(d.Spec.Template.Spec.Containers) == 1 && d.Spec.Template.Spec.Containers[0].Image == c.Release.Image && d.Spec.Template.Annotations[annotationSnapshot] == fingerprint(c.Release)
	// 可运维性：digest 证据缺失时区分具体子条件（镜像引用/快照指纹），便于节点侧定位。
	if !e.CorrectDigest && len(d.Spec.Template.Spec.Containers) == 1 {
		if d.Spec.Template.Spec.Containers[0].Image != c.Release.Image {
			e.Reasons = append(e.Reasons, "deployment镜像引用与命令不一致")
		}
		if d.Spec.Template.Annotations[annotationSnapshot] != fingerprint(c.Release) {
			e.Reasons = append(e.Reasons, "deployment快照指纹与命令不一致: 部署注解="+d.Spec.Template.Annotations[annotationSnapshot]+" 计算="+fingerprint(c.Release))
		}
	}
	selector := metav1.ListOptions{LabelSelector: labels.SelectorFromSet(labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID)).String()}
	replicasets, err := a.client.AppsV1().ReplicaSets(ns).List(ctx, selector)
	if err != nil {
		return e, err
	}
	rs := map[types.UID]bool{}
	for i := range replicasets.Items {
		item := &replicasets.Items[i]
		if err := owns(item, labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID), ""); err != nil {
			return e, fmt.Errorf("%w: replicaset %s", err, item.Name)
		}
		if !deploymentOwnsRS(d, item.OwnerReferences) {
			return e, fmt.Errorf("%w: replicaset %s 不属于deployment %s", ErrOwnership, item.Name, d.Name)
		}
		rs[item.UID] = true
	}
	pods, err := a.client.CoreV1().Pods(ns).List(ctx, selector)
	if err != nil {
		return e, err
	}
	currentReady := 0
	e.OldPodsExited = true
	live := map[types.UID]bool{}
	activeCount := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if err := owns(p, labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID), ""); err != nil {
			return e, fmt.Errorf("%w: pod %s", err, p.Name)
		}
		// Deployment控制器先建ReplicaSet再建Pod；两次List之间出现新Pod是正常
		// 中间态，跳过本轮流数等待收敛，不构成归属冲突。孤儿Pod使本轮无法就绪
		// （OldPodsExited=false），绝不把不受控Pod计入副本或就绪证据。
		if !podOwnerRS(p, rs) {
			e.OldPodsExited = false
			e.Reasons = append(e.Reasons, "pod "+p.Name+" 的ReplicaSet本轮未观察到，等待控制器收敛")
			continue
		}
		if p.Spec.NodeName != c.Release.Placement.Name {
			return e, fmt.Errorf("%w: pod %s 不属于本节点受管ReplicaSet", ErrOwnership, p.Name)
		}
		pe := PodEvidence{UID: p.UID, Name: p.Name, ReleaseID: p.Labels[LabelRelease], Phase: string(p.Status.Phase), Ready: podReady(p), Deleting: p.DeletionTimestamp != nil}
		for _, status := range p.Status.ContainerStatuses {
			pe.Restarts += status.RestartCount
			if status.Name == "app" {
				pe.ImageID = status.ImageID
			}
			if status.State.Waiting != nil {
				pe.Reasons = append(pe.Reasons, status.State.Waiting.Reason)
			}
			if status.State.Terminated != nil {
				pe.Reasons = append(pe.Reasons, status.State.Terminated.Reason)
			}
		}
		e.Pods = append(e.Pods, pe)
		if terminalPod(p) {
			continue
		}
		activeCount++
		current := p.Labels[LabelRelease] == c.Release.ID && p.Labels[LabelGeneration] == generation && p.Labels[LabelOperation] == c.OperationID && p.Annotations[annotationSnapshot] == fingerprint(c.Release)
		if !current {
			e.OldPodsExited = false
			continue
		}
		if len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Image != c.Release.Image || !correctImageID(pe.ImageID, c.Release.Image) {
			// 可运维性：pod 级证据失败时输出三个子条件的事实值（引用/ID 均为公开形态）。
			if p.Spec.Containers[0].Image != c.Release.Image {
				e.Reasons = append(e.Reasons, "pod镜像与命令不一致: pod="+p.Spec.Containers[0].Image+" 命令="+c.Release.Image)
			}
			if !correctImageID(pe.ImageID, c.Release.Image) {
				e.Reasons = append(e.Reasons, "pod imageID证据不足: id="+pe.ImageID+" 命令="+c.Release.Image)
			}
			if p.Annotations[annotationSnapshot] != fingerprint(c.Release) {
				e.Reasons = append(e.Reasons, "pod快照指纹不一致: pod="+p.Annotations[annotationSnapshot]+" 计算="+fingerprint(c.Release))
			}
			e.CorrectDigest = false
			continue
		}
		if pe.Ready && !pe.Deleting {
			currentReady++
			live[p.UID] = true
		}
	}
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	e.Stopped = desired == 0 && activeCount == 0 && e.CorrectGeneration && d.Status.Replicas == 0
	e.Ready = desired == 1 && currentReady == 1 && activeCount == 1 && d.Status.ReadyReplicas == 1 && d.Status.UpdatedReplicas == 1 && d.Status.AvailableReplicas == 1
	// 可运维性：副本未就绪时输出各计数子条件。
	if !e.Ready {
		e.Reasons = append(e.Reasons, fmt.Sprintf("副本计数: desired=%d currentReady=%d active=%d statusReady=%d updated=%d available=%d", desired, currentReady, activeCount, d.Status.ReadyReplicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas))
	}
	e.ServiceEndpoints = !http
	if http && desired > 0 {
		service, err := a.client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return e, err
		}
		if err := owns(service, labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID), ""); err != nil {
			return e, fmt.Errorf("%w: service %s", err, name)
		}
		endpointSlices, err := a.client.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(map[string]string{"kubernetes.io/service-name": name}).String()})
		if err != nil {
			return e, err
		}
		matched := map[types.UID]bool{}
		unexpected := false
		for _, slice := range endpointSlices.Items {
			owned := false
			for _, o := range slice.OwnerReferences {
				if o.Kind == "Service" && o.UID == service.UID {
					owned = true
				}
			}
			if !owned {
				return e, fmt.Errorf("%w: endpointslice %s 不属于service %s", ErrOwnership, slice.Name, name)
			}
			for _, endpoint := range slice.Endpoints {
				if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready || endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
					continue
				}
				ref := endpoint.TargetRef
				if ref == nil || ref.Kind != "Pod" || ref.Namespace != ns || !live[ref.UID] || len(endpoint.Addresses) == 0 {
					unexpected = true
					continue
				}
				matched[ref.UID] = true
			}
		}
		e.ServiceEndpoints = len(matched) == 1 && !unexpected
		ingress, err := a.client.NetworkingV1().Ingresses(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return e, err
		}
		if err := owns(ingress, labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID), ""); err != nil {
			return e, fmt.Errorf("%w: ingress %s", err, name)
		}
	}
	e.KubernetesReady = e.CorrectGeneration && e.CorrectDigest && e.Ready && e.OldPodsExited && e.ServiceEndpoints
	if !e.CorrectGeneration {
		e.Reasons = append(e.Reasons, "目标generation尚未被Deployment控制器观察")
	}
	if !e.CorrectDigest {
		e.Reasons = append(e.Reasons, "目标镜像digest证据不足或不匹配")
	}
	if !e.Ready {
		e.Reasons = append(e.Reasons, "目标唯一副本尚未就绪")
	}
	if !e.OldPodsExited {
		e.Reasons = append(e.Reasons, "旧操作Pod尚未退出")
	}
	if !e.ServiceEndpoints {
		e.Reasons = append(e.Reasons, "Service端点尚未唯一匹配目标Pod")
	}
	return e, nil
}

type LogRequest struct {
	PodReleaseID       string
	Namespace, PodName string
	PodUID             types.UID
	Container          string
	RestartCount       int32
	Previous           bool
	SinceTime          *metav1.Time
	TailLines          int64
}

// LogStream 必须由调用者Close；PodUID/container/restart是持久日志去重身份的一部分。
type LogStream struct {
	PodReleaseID string
	io.ReadCloser
	PodUID       types.UID
	Container    string
	RestartCount int32
	Previous     bool
}

func (a *Adapter) followPodLogs(ctx context.Context, p *corev1.Pod, request LogRequest) (*LogStream, error) {
	if p.UID != request.PodUID || p.UID == "" || request.TailLines < 0 || request.TailLines > 10000 {
		return nil, ErrOwnership
	}
	statusIndex := slices.IndexFunc(p.Status.ContainerStatuses, func(s corev1.ContainerStatus) bool { return s.Name == request.Container })
	if statusIndex < 0 {
		return nil, fmt.Errorf("%w: 容器尚无日志身份", ErrInvalid)
	}
	status := p.Status.ContainerStatuses[statusIndex]
	if status.RestartCount != request.RestartCount || request.Previous && status.RestartCount < 1 {
		return nil, ErrOwnership
	}
	stream, err := a.client.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: request.Container, Follow: true, Previous: request.Previous, Timestamps: true, SinceTime: request.SinceTime, TailLines: ptr(request.TailLines)}).Stream(ctx)
	if err != nil {
		return nil, err
	}
	// 日志请求不能携带UID前置条件，流打开后再次核对，防止同名Pod替换污染日志。
	after, err := a.client.CoreV1().Pods(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	validRestart := false
	if err == nil {
		for _, s := range after.Status.ContainerStatuses {
			if s.Name == request.Container && s.RestartCount == request.RestartCount {
				validRestart = true
			}
		}
	}
	if err != nil || after.UID != p.UID || after.Labels[LabelRelease] != p.Labels[LabelRelease] || !validRestart {
		_ = stream.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrOwnership
	}
	count := request.RestartCount
	if request.Previous {
		count--
	}
	return &LogStream{ReadCloser: stream, PodUID: p.UID, PodReleaseID: p.Labels[LabelRelease], Container: request.Container, RestartCount: count, Previous: request.Previous}, nil
}
func (a *Adapter) FollowLogs(ctx context.Context, t BuildTask, request LogRequest) (*LogStream, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	if request.Namespace != a.config.BuildNamespace || request.Container != "build" {
		return nil, ErrOwnership
	}
	j, err := a.client.BatchV1().Jobs(request.Namespace).Get(ctx, t.JobName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := owns(j, buildLabels(t), t.JobUID); err != nil {
		return nil, err
	}
	p, err := a.client.CoreV1().Pods(request.Namespace).Get(ctx, request.PodName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := jobPod(p, t, j.UID); err != nil {
		return nil, err
	}
	return a.followPodLogs(ctx, p, request)
}
func (r *Runtime) FollowLogs(ctx context.Context, c RuntimeChange, request LogRequest) (*LogStream, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	a := r.adapter
	if request.Namespace != a.runtimeNamespace(c) || request.Container != "app" {
		return nil, ErrOwnership
	}
	p, err := a.client.CoreV1().Pods(request.Namespace).Get(ctx, request.PodName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := owns(p, labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID), request.PodUID); err != nil {
		return nil, err
	}
	expectedRelease := request.PodReleaseID
	if expectedRelease == "" {
		expectedRelease = c.Release.ID
	}
	if p.Labels[LabelRelease] != expectedRelease || p.Spec.NodeName != c.Release.Placement.Name {
		return nil, ErrOwnership
	}
	return a.followPodLogs(ctx, p, request)
}

// buildNamespace 返回构建任务的命名空间：命令携带空间标识时按统一命名规范
// 派生 build-<space_slug>，否则回退到安装配置的固定命名空间（旧控制面兼容）。
func (a *Adapter) buildNamespace(t BuildTask) string {
	if t.SpaceSlug != "" {
		return BuildNamespaceFor(t.SpaceSlug)
	}
	return a.config.BuildNamespace
}

// runtimeNamespace 返回运行变更的命名空间：命令携带空间标识时按统一命名规范
// 派生 prod-<space_slug>，否则回退到安装配置的固定命名空间。
func (a *Adapter) runtimeNamespace(c RuntimeChange) string {
	if c.Release.SpaceSlug != "" {
		return RuntimeNamespaceFor(c.Release.SpaceSlug)
	}
	return a.config.RuntimeNamespace
}

// deployedName 返回 Deployment/Service/Ingress 的规范名称：<应用名>-default；
// 命令未携带应用名（旧控制面）时回退到 ID 派生的历史名称。
func (a *Adapter) deployedName(r Release) string {
	if r.AppName != "" {
		return DeployedName(r.AppName)
	}
	return AppName(r.ApplicationID)
}

// BootstrapBuildNamespace 确保 build-<space_slug> 存在并打平台归属标签
// （2026-10-26 设计 §5：构建命名空间归属 workspace_id，不加网络策略）。
func (a *Adapter) BootstrapBuildNamespace(ctx context.Context, spaceSlug, workspaceID string) error {
	name := BuildNamespaceFor(spaceSlug)
	labels := map[string]string{
		LabelManaged:     "worker",
		LabelWorkspace:   workspaceID,
		"yummy.dev/role": "build",
	}
	return a.ensureNamespace(ctx, name, labels)
}

// BootstrapRuntimeNamespace 确保 prod-<space_slug> 存在，打归属标签并下发
// 空间隔离 NetworkPolicy（同命名空间与 Traefik 可入站，2026-10-26 设计 §6）。
func (a *Adapter) BootstrapRuntimeNamespace(ctx context.Context, spaceSlug, workspaceID string) error {
	name := RuntimeNamespaceFor(spaceSlug)
	labels := map[string]string{
		LabelManaged:     "worker",
		LabelWorkspace:   workspaceID,
		"yummy.dev/env":  EnvProd,
		"yummy.dev/role": "runtime",
	}
	if err := a.ensureNamespace(ctx, name, labels); err != nil {
		return err
	}
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name:      "yummy-workspace-isolation",
		Namespace: name,
		Labels:    map[string]string{LabelManaged: "worker", LabelWorkspace: workspaceID},
	}}
	// 期望 Spec 在 Get 之前构造完整：Update 必须下发同一份完整规则——用空 Spec
	// 的对象 Update 会把已存在的放行规则抹成"拒绝全部入站"（实机 2026-11-17：
	// 第二次部署起 Traefik 全部 502，正是此缺陷）。
	policy.Spec = networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{
				{PodSelector: &metav1.LabelSelector{}},
				{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "traefik"}}},
			},
		}},
	}
	_, err := a.client.NetworkingV1().NetworkPolicies(name).Get(ctx, policy.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = a.client.NetworkingV1().NetworkPolicies(name).Create(ctx, policy, metav1.CreateOptions{})
	} else if err == nil {
		_, err = a.client.NetworkingV1().NetworkPolicies(name).Update(ctx, policy, metav1.UpdateOptions{})
	}
	return err
}

// ensureNamespace 幂等创建命名空间并设置平台归属标签；同名但归属不符的对象
// 一律报错不接管（2026-10-26 设计 §5 归属核对）。
func (a *Adapter) ensureNamespace(ctx context.Context, name string, labels map[string]string) error {
	ns, err := a.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		for key, value := range labels {
			if ns.Labels[key] != value {
				return fmt.Errorf("%w: 命名空间 %s 的平台标签 %s 归属不符", ErrOwnership, name, key)
			}
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	_, err = a.client.CoreV1().Namespaces().Create(ctx, created, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
