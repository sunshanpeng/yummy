// Package kube 是 worker 内的类型化 Kubernetes 适配器，不读取 kubeconfig，不访问宿主 Shell/SSH/socket。
// 控制面负责授权、不可变快照、操作串行化和结果持久化；本包只返回执行证据。
package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	Platform           = "linux/amd64"
	LabelManaged       = "yummy.dev/managed-by"
	LabelWorkspace     = "yummy.dev/workspace"
	LabelApplication   = "yummy.dev/application"
	LabelAttempt       = "yummy.dev/attempt"
	LabelNodeUID       = "yummy.dev/node-uid"
	LabelRelease       = "yummy.dev/release"
	LabelGeneration    = "yummy.dev/generation"
	LabelOperation     = "yummy.dev/operation"
	annotationSnapshot = "yummy.dev/snapshot-sha256"
)

var (
	ErrInvalid    = errors.New("Kubernetes执行参数无效")
	ErrOwnership  = errors.New("Kubernetes资源身份不匹配，拒绝接管")
	ErrNotReady   = errors.New("节点尚未通过所需能力资格检查")
	ErrGeneration = errors.New("操作已被新的期望generation替代")
	ErrJobLost    = errors.New("已确认的Job消失，禁止在同一attempt补跑")
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type NodeTarget struct {
	Name string
	UID  types.UID
}

// InstalledCapabilities 必须来自安装资格记录，不能由API连通或fake测试自动产生。
// 放宽LSM和no-process-sandbox的开关仅允许显式安装决策；默认不放宽。
type InstalledCapabilities struct {
	Build, Runtime, Ingress, Metrics                        bool
	QualificationID                                         string
	RunnerImage, BuildKitImage                              string
	SeccompUnconfined, AppArmorUnconfined, NoProcessSandbox bool
	// AllowPrivilegeEscalation 允许构建容器 exec setuid 助手（如 rootlesskit 的 newuidmap），
	// 解除 no_new_privs。实机验收发现：嵌套 rootless userns 需要它，由资格记录显式授权。
	AllowPrivilegeEscalation bool
	// HostBuild 构建改由 worker 进程在宿主机直接驱动节点级 rootless BuildKit
	// 守护（实机验收发现：k8s pod 内嵌套 rootless 被 containerd 锁定挂载与
	// 默认 seccomp 双重封锁，privileged 方案违背安全不变量）。仅作为构建
	// 执行路径开关；部署仍走 Kubernetes pod 且不随此开关放宽。
	HostBuild bool
}

type Config struct {
	Node                             NodeTarget
	BuildNamespace, RuntimeNamespace string
	Installed                        InstalledCapabilities
	BuildDeadline                    time.Duration
	StorageClass                     string
	IngressClass                     string
}

type Adapter struct {
	client kubernetes.Interface
	config Config
}

// New 不创建默认fake，也不隐式读取环境或本机认证材料。
func New(client kubernetes.Interface, config Config) (*Adapter, error) {
	if client == nil || config.Node.Name == "" || config.Node.UID == "" {
		return nil, fmt.Errorf("%w: client与节点身份必填", ErrInvalid)
	}
	for _, ns := range []string{config.BuildNamespace, config.RuntimeNamespace} {
		if len(validation.IsDNS1123Label(ns)) != 0 {
			return nil, fmt.Errorf("%w: namespace", ErrInvalid)
		}
	}
	if config.BuildNamespace == config.RuntimeNamespace {
		return nil, fmt.Errorf("%w: 构建与运行namespace必须分离", ErrInvalid)
	}
	if config.BuildDeadline == 0 {
		config.BuildDeadline = 30 * time.Minute
	}
	if config.BuildDeadline < time.Second || config.BuildDeadline > 2*time.Hour {
		return nil, fmt.Errorf("%w: 构建deadline必须为1秒至2小时", ErrInvalid)
	}
	return &Adapter{client: client, config: config}, nil
}

func ptr[T any](v T) *T { return &v }
func shortHash(value string) string {
	s := sha256.Sum256([]byte(value))
	return hex.EncodeToString(s[:])[:32]
}
func JobName(attemptID string) string     { return "yb-" + shortHash(attemptID) }
func AppName(applicationID string) string { return "ya-" + shortHash(applicationID) }
func validIdentity(value string) bool {
	return value != "" && len(validation.IsValidLabelValue(value)) == 0
}
func pinnedImage(image string) bool {
	parts := strings.Split(image, "@")
	return len(parts) == 2 && parts[0] != "" && !strings.ContainsAny(parts[0], " \t\r\n,;") && !strings.HasSuffix(parts[0], ":latest") && digestPattern.MatchString(parts[1])
}
func labelsFor(workspace, app string, node types.UID) map[string]string {
	return map[string]string{LabelManaged: "worker", LabelWorkspace: workspace, LabelApplication: app, LabelNodeUID: string(node)}
}
func copyLabels(source map[string]string) map[string]string {
	result := map[string]string{}
	for k, v := range source {
		result[k] = v
	}
	return result
}
func owns(meta metav1.Object, expected map[string]string, uid types.UID) error {
	if uid != "" && meta.GetUID() != uid {
		return ErrOwnership
	}
	for k, v := range expected {
		if meta.GetLabels()[k] != v {
			return ErrOwnership
		}
	}
	return nil
}
func (a *Adapter) checkNodeIdentity(ctx context.Context, target NodeTarget) error {
	if target != a.config.Node {
		return fmt.Errorf("%w: 不允许迁移到其他节点", ErrOwnership)
	}
	node, err := a.client.CoreV1().Nodes().Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if node.UID != target.UID {
		return ErrOwnership
	}
	return nil
}

func (a *Adapter) checkNode(ctx context.Context, target NodeTarget) error {
	if target != a.config.Node {
		return fmt.Errorf("%w: 不允许迁移到其他节点", ErrOwnership)
	}
	node, err := a.client.CoreV1().Nodes().Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if node.UID != target.UID {
		return ErrOwnership
	}
	if node.Status.NodeInfo.OperatingSystem != "linux" || node.Status.NodeInfo.Architecture != "amd64" {
		return fmt.Errorf("%w: 仅认证原生linux/amd64", ErrNotReady)
	}
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready || node.Spec.Unschedulable {
		return fmt.Errorf("%w: 节点不可调度或未就绪", ErrNotReady)
	}
	return nil
}
func (a *Adapter) capability(name string) error {
	c := a.config.Installed
	ready := false
	switch name {
	case "build":
		ready = c.Build && pinnedImage(c.RunnerImage) && pinnedImage(c.BuildKitImage)
	case "runtime":
		ready = c.Runtime
	case "ingress":
		ready = c.Ingress
	case "metrics":
		ready = c.Metrics
	}
	if !ready || c.QualificationID == "" {
		return fmt.Errorf("%w: %s缺少安装资格证据", ErrNotReady, name)
	}
	return nil
}
func podSecurity() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(1000)), RunAsGroup: ptr(int64(1000)), FSGroup: ptr(int64(1000)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
}
func containerSecurity(installed InstalledCapabilities) *corev1.SecurityContext {
	security := &corev1.SecurityContext{Privileged: ptr(false), AllowPrivilegeEscalation: ptr(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}}
	// 仅在资格记录显式授权时解除 no_new_privs 并补 SETUID/SETGID：
	// 嵌套 rootless userns 的 newuidmap 是 setuid 二进制，Drop ALL 清空 bounding
	// 集后即使 setuid 提权也拿不到这两个能力；其余硬化保持不变。
	if installed.AllowPrivilegeEscalation {
		security.AllowPrivilegeEscalation = ptr(true)
		security.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"SETUID", "SETGID"}}
	}
	return security
}

// CapabilityEvidence 分开报告能力，不把API可达误当作全部ready。
type CapabilityEvidence struct {
	APIReachable                     bool
	NodeUID                          types.UID
	Build, Runtime, Ingress, Metrics bool
	QualificationID                  string
	Reasons                          map[string]string
}

func (a *Adapter) Qualify(ctx context.Context) (CapabilityEvidence, error) {
	e := CapabilityEvidence{Reasons: map[string]string{}, QualificationID: a.config.Installed.QualificationID}
	if err := a.checkNode(ctx, a.config.Node); err != nil {
		return e, err
	}
	e.APIReachable = true
	e.NodeUID = a.config.Node.UID
	for _, name := range []string{"build", "runtime", "ingress", "metrics"} {
		err := a.capability(name)
		if err != nil {
			e.Reasons[name] = err.Error()
			continue
		}
		switch name {
		case "build":
			e.Build = true
		case "runtime":
			e.Runtime = true
		case "ingress":
			e.Ingress = true
		case "metrics":
			e.Metrics = true
		}
	}
	return e, nil
}
