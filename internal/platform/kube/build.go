package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// SourceGrant 的URI不允许查询串/用户密码；授权令牌只能通过临时Secret文件交给prepare。
// 固定runner须使用sourcearchive安全解包规则，校验归档SHA256与大小后才发布source目录。
type SourceGrant struct {
	URI, ArchiveSHA256 string
	MaxBytes           int64
	Token              []byte `json:"-"`
}
type BuildSecret struct {
	ID, Version string
	Value       []byte `json:"-"`
}
type BuildTask struct {
	AttemptID, JobName, StableJobName, WorkspaceID, ApplicationID string
	// SpaceSlug 空间标识：构建命名空间 build-<space_slug> 按此派生（统一命名规范）。
	SpaceSlug                             string
	Node                                  NodeTarget
	Platform                              string
	Source                                SourceGrant
	ImageTag, Dockerfile, Context, Target string
	BuildArgs                             map[string]string
	Secrets                               []BuildSecret
	RegistryDockerConfig                  []byte `json:"-"`
	// JobUID与Acknowledged来自持久化attempt；重投必须保留，不得根据当前内存清空。
	JobUID       types.UID `json:"-"`
	Acknowledged bool      `json:"-"`
}
type BuildEvidence struct {
	JobName                                                      string
	JobUID                                                       types.UID
	Found, Active, Complete, Failed, MissingAfterAcknowledgement bool
	PodUIDs                                                      []types.UID
	// Digest仅为BuildKit元数据证据，控制面独立验证registry manifest/config后才能成功。
	Digest string
	Reason string
}

var buildIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
var snapshotHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func safeRelative(value string) bool {
	return value != "" && !path.IsAbs(value) && path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\\x00\r\n")
}
func (t BuildTask) validateIdentity() error {
	for _, id := range []string{t.AttemptID, t.WorkspaceID, t.ApplicationID, string(t.Node.UID)} {
		if !validIdentity(id) {
			return fmt.Errorf("%w: 构建身份", ErrInvalid)
		}
	}
	if t.StableJobName == "" || t.JobName != t.StableJobName || len(validation.IsDNS1123Label(t.StableJobName)) != 0 || len(t.StableJobName) > 48 || t.Platform != Platform {
		return fmt.Errorf("%w: Job名或平台未冻结", ErrInvalid)
	}
	return nil
}
func (t BuildTask) validate() error {
	if err := t.validateIdentity(); err != nil {
		return err
	}
	u, err := url.Parse(t.Source.URI)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("%w: 源码URI必须是无查询令牌的HTTPS地址", ErrInvalid)
	}
	if !digestPattern.MatchString(t.Source.ArchiveSHA256) || t.Source.MaxBytes <= 0 || t.Source.MaxBytes > 2<<30 || len(t.Source.Token) == 0 || len(t.Source.Token) > 65536 {
		return fmt.Errorf("%w: 源码授权或归档边界", ErrInvalid)
	}
	if !safeRelative(t.Dockerfile) || !safeRelative(t.Context) || (t.Context != "." && !strings.HasPrefix(t.Dockerfile, t.Context+"/")) || strings.ContainsAny(t.ImageTag, " \t\n\r,@;") || strings.LastIndex(t.ImageTag, ":") <= strings.LastIndex(t.ImageTag, "/") || strings.HasSuffix(t.ImageTag, ":latest") {
		return fmt.Errorf("%w: 构建路径或输出引用", ErrInvalid)
	}
	if len(t.RegistryDockerConfig) == 0 || len(t.RegistryDockerConfig) > 256<<10 || !json.Valid(t.RegistryDockerConfig) {
		return fmt.Errorf("%w: registry credential格式", ErrInvalid)
	}
	if t.Target != "" && !buildIDPattern.MatchString(t.Target) {
		return fmt.Errorf("%w: target", ErrInvalid)
	}
	if len(t.BuildArgs) > 100 || len(t.Secrets) > 50 {
		return ErrInvalid
	}
	for k, v := range t.BuildArgs {
		if !buildIDPattern.MatchString(k) || len(v) > 8192 || strings.ContainsRune(v, 0) {
			return ErrInvalid
		}
	}
	seen := map[string]bool{}
	total := len(t.RegistryDockerConfig) + len(t.Source.Token)
	for _, s := range t.Secrets {
		if !buildIDPattern.MatchString(s.ID) || s.Version == "" || seen[s.ID] || len(s.Value) > 64<<10 {
			return fmt.Errorf("%w: BuildKit secret", ErrInvalid)
		}
		seen[s.ID] = true
		total += len(s.Value)
	}
	if total > 512<<10 {
		return fmt.Errorf("%w: 临时Secret过大", ErrInvalid)
	}
	return nil
}
func buildLabels(t BuildTask) map[string]string {
	l := labelsFor(t.WorkspaceID, t.ApplicationID, t.Node.UID)
	l[LabelAttempt] = t.AttemptID
	return l
}
func fingerprint(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func buildResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("10Gi")}}
}
func (a *Adapter) buildObjects(t BuildTask) (*batchv1.Job, *corev1.Secret) {
	l := buildLabels(t)
	annotations := map[string]string{annotationSnapshot: fingerprint(t)}
	name := t.JobName + "-auth"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.buildNamespace(t), Labels: copyLabels(l), Annotations: annotations}, Type: corev1.SecretTypeOpaque, Immutable: ptr(true), Data: map[string][]byte{"source-token": slices.Clone(t.Source.Token), "config.json": slices.Clone(t.RegistryDockerConfig)}}
	authItems := []corev1.KeyToPath{{Key: "config.json", Path: "config.json"}}
	args := []string{"build", "--metadata-output=/dev/termination-log", "--", "build", "--frontend=dockerfile.v0", "--metadata-file=/workspace/buildkit-metadata.json", "--local=context=/workspace/source/" + t.Context, "--local=dockerfile=/workspace/source/" + path.Dir(t.Dockerfile), "--opt=filename=" + path.Base(t.Dockerfile), "--opt=platform=" + Platform, "--output=type=image,name=" + t.ImageTag + ",push=true"}
	if t.Target != "" {
		args = append(args, "--opt=target="+t.Target)
	}
	keys := make([]string, 0, len(t.BuildArgs))
	for k := range t.BuildArgs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--opt=build-arg:"+k+"="+t.BuildArgs[k])
	}
	for i, s := range t.Secrets {
		key := fmt.Sprintf("secret-%d", i)
		secret.Data[key] = slices.Clone(s.Value)
		authItems = append(authItems, corev1.KeyToPath{Key: key, Path: key})
		args = append(args, "--secret=id="+s.ID+",src=/run/build-auth/"+key)
	}
	sec := podSecurity()
	security := containerSecurity(a.config.Installed)
	if a.config.Installed.SeccompUnconfined {
		sec.SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined
	}
	if a.config.Installed.AppArmorUnconfined {
		security.AppArmorProfile.Type = corev1.AppArmorProfileTypeUnconfined
	}
	if a.config.Installed.NoProcessSandbox {
		args = slices.Insert(args, 1, "--no-process-sandbox")
	}
	workspace := corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse("8Gi"))}}}
	state := corev1.Volume{Name: "state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse("1Gi"))}}}
	auth := corev1.Volume{Name: "auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, DefaultMode: ptr(int32(0440)), Items: authItems}}}
	runtime := corev1.Volume{Name: "runner-runtime", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptr(resource.MustParse("64Mi"))}}}
	mounts := []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "state", MountPath: "/home/user/.local/share/buildkit"}, {Name: "auth", MountPath: "/run/build-auth", ReadOnly: true}, {Name: "runner-runtime", MountPath: "/run/yummy-runtime"}}
	prepare := corev1.Container{Name: "prepare", Image: a.config.Installed.RunnerImage, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"/usr/local/bin/yummy-runner"}, Args: []string{"prepare", "--source-uri=" + t.Source.URI, "--grant-file=/run/source-auth/source-token", "--archive-sha256=" + t.Source.ArchiveSHA256, fmt.Sprintf("--max-bytes=%d", t.Source.MaxBytes), "--destination=/workspace/source", "--dockerfile=" + t.Dockerfile}, SecurityContext: security.DeepCopy(), Resources: buildResources(), VolumeMounts: []corev1.VolumeMount{mounts[0], {Name: "source-auth", MountPath: "/run/source-auth", ReadOnly: true}}}
	build := corev1.Container{Name: "build", Image: a.config.Installed.RunnerImage, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"/usr/local/bin/yummy-runner"}, Args: args, SecurityContext: security, Resources: buildResources(), VolumeMounts: mounts, Env: []corev1.EnvVar{{Name: "DOCKER_CONFIG", Value: "/run/build-auth"}}, TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: t.JobName, Namespace: a.buildNamespace(t), Labels: l, Annotations: annotations}, Spec: batchv1.JobSpec{BackoffLimit: ptr(int32(0)), ActiveDeadlineSeconds: ptr(int64(a.config.BuildDeadline.Seconds())), Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: copyLabels(l)}, Spec: corev1.PodSpec{NodeName: t.Node.Name, AutomountServiceAccountToken: ptr(false), RestartPolicy: corev1.RestartPolicyNever, SecurityContext: sec, EnableServiceLinks: ptr(false), Volumes: []corev1.Volume{workspace, state, auth, runtime, {Name: "source-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, DefaultMode: ptr(int32(0440)), Items: []corev1.KeyToPath{{Key: "source-token", Path: "source-token"}}}}}}, InitContainers: []corev1.Container{prepare}, Containers: []corev1.Container{build}}}}}
	return job, secret
}
func (a *Adapter) verifyJob(t BuildTask, job *batchv1.Job) error {
	if err := owns(job, buildLabels(t), t.JobUID); err != nil {
		return err
	}
	desired, _ := a.buildObjects(t)
	p := job.Spec.Template.Spec
	if p.HostNetwork || p.HostPID || p.HostIPC || len(p.Volumes) != len(desired.Spec.Template.Spec.Volumes) || len(p.Containers) != 1 || len(p.InitContainers) != 1 || len(p.EphemeralContainers) != 0 || p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken {
		return ErrOwnership
	}
	for _, v := range p.Volumes {
		if v.HostPath != nil {
			return ErrOwnership
		}
	}
	if job.DeletionTimestamp != nil || job.Annotations[annotationSnapshot] != desired.Annotations[annotationSnapshot] || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != *desired.Spec.ActiveDeadlineSeconds || !equality.Semantic.DeepDerivative(desired.Spec.Template.Spec, job.Spec.Template.Spec) {
		return ErrOwnership
	}
	return nil
}

// verifyExistingJob 用持久 UID 和已绑定的受管身份恢复只读观察；没有 UID 不能使用此路径。
// 创建时的完整快照校验仍由 verifyJob 完成，恢复时绝不以不完整材料创建或覆盖资源。
func verifyExistingJob(t BuildTask, job *batchv1.Job) error {
	if t.JobUID == "" || job.UID != t.JobUID || job.DeletionTimestamp != nil {
		return ErrOwnership
	}
	if err := owns(job, buildLabels(t), t.JobUID); err != nil {
		return err
	}
	p := job.Spec.Template.Spec
	if p.NodeName != t.Node.Name || p.HostNetwork || p.HostPID || p.HostIPC || p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken || len(p.Containers) != 1 || len(p.InitContainers) != 1 || len(p.EphemeralContainers) != 0 || p.RestartPolicy != corev1.RestartPolicyNever || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		return ErrOwnership
	}
	for _, volume := range p.Volumes {
		if volume.HostPath != nil {
			return ErrOwnership
		}
	}
	if job.Spec.Template.Labels[LabelAttempt] != t.AttemptID || job.Spec.Template.Labels[LabelApplication] != t.ApplicationID || job.Spec.Template.Labels[LabelWorkspace] != t.WorkspaceID || job.Spec.Template.Labels[LabelNodeUID] != string(t.Node.UID) {
		return ErrOwnership
	}
	if !snapshotHashPattern.MatchString(job.Annotations[annotationSnapshot]) {
		return ErrOwnership
	}
	return nil
}

func (a *Adapter) ensureBuildSecret(ctx context.Context, want *corev1.Secret) error {
	got, err := a.client.CoreV1().Secrets(want.Namespace).Get(ctx, want.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		got, err = a.client.CoreV1().Secrets(want.Namespace).Create(ctx, want, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			got, err = a.client.CoreV1().Secrets(want.Namespace).Get(ctx, want.Name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return err
	}
	if err := owns(got, want.Labels, ""); err != nil {
		return err
	}
	if got.Annotations[annotationSnapshot] != want.Annotations[annotationSnapshot] || got.Immutable == nil || !*got.Immutable || !equality.Semantic.DeepEqual(got.Data, want.Data) {
		return ErrOwnership
	}
	return nil
}

// Run 同一attempt永远使用同一Job名，先读后建；不明确的创建结果交给下一次同名查询恢复。
func (a *Adapter) Run(ctx context.Context, t BuildTask) (BuildEvidence, error) {
	if err := t.validate(); err != nil {
		return BuildEvidence{}, err
	}
	if err := a.capability("build"); err != nil {
		return BuildEvidence{}, err
	}
	if err := a.checkNodeIdentity(ctx, t.Node); err != nil {
		return BuildEvidence{}, err
	}
	job, err := a.client.BatchV1().Jobs(a.buildNamespace(t)).Get(ctx, t.JobName, metav1.GetOptions{})
	if err == nil {
		if err = a.verifyJob(t, job); err != nil {
			return BuildEvidence{}, err
		}
		return a.buildEvidence(ctx, t, job)
	}
	if !apierrors.IsNotFound(err) {
		return BuildEvidence{}, err
	}
	if t.Acknowledged || t.JobUID != "" {
		return BuildEvidence{JobName: t.JobName, MissingAfterAcknowledgement: true}, ErrJobLost
	}
	if err := a.checkNode(ctx, t.Node); err != nil {
		return BuildEvidence{}, err
	}
	want, secret := a.buildObjects(t)
	if err := a.ensureBuildSecret(ctx, secret); err != nil {
		return BuildEvidence{}, err
	}
	job, err = a.client.BatchV1().Jobs(a.buildNamespace(t)).Create(ctx, want, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		job, err = a.client.BatchV1().Jobs(a.buildNamespace(t)).Get(ctx, t.JobName, metav1.GetOptions{})
	}
	if err != nil {
		return BuildEvidence{}, err
	}
	if err = a.verifyJob(t, job); err != nil {
		return BuildEvidence{}, err
	}
	return a.buildEvidence(ctx, t, job)
}
func (a *Adapter) Reconcile(ctx context.Context, t BuildTask) (BuildEvidence, error) {
	if err := t.validateIdentity(); err != nil {
		return BuildEvidence{}, err
	}
	if err := a.checkNodeIdentity(ctx, t.Node); err != nil {
		return BuildEvidence{}, err
	}
	job, err := a.client.BatchV1().Jobs(a.buildNamespace(t)).Get(ctx, t.JobName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		e := BuildEvidence{JobName: t.JobName, MissingAfterAcknowledgement: t.Acknowledged || t.JobUID != ""}
		if e.MissingAfterAcknowledgement {
			return e, ErrJobLost
		}
		return e, nil
	}
	if err != nil {
		return BuildEvidence{}, err
	}
	// 重连时控制面只提供已持久化的 Job UID 与任务身份，不重新发送可能已过期的秘密。
	// 首次创建已验证完整 spec；此后必须绑定不可变 UID、受管标签和保守安全约束，
	// 不用缺失的临时源码授权伪造原始快照并误判所有重连。
	if err = verifyExistingJob(t, job); err != nil {
		return BuildEvidence{}, err
	}
	return a.buildEvidence(ctx, t, job)
}
func jobPod(p *corev1.Pod, t BuildTask, uid types.UID) error {
	if err := owns(p, buildLabels(t), ""); err != nil {
		return err
	}
	for _, r := range p.OwnerReferences {
		if r.UID == uid && r.Kind == "Job" && r.Name == t.JobName && r.Controller != nil && *r.Controller {
			return nil
		}
	}
	return ErrOwnership
}
func (a *Adapter) buildEvidence(ctx context.Context, t BuildTask, j *batchv1.Job) (BuildEvidence, error) {
	e := BuildEvidence{JobName: j.Name, JobUID: j.UID, Found: true, Active: j.Status.Active > 0}
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete {
			e.Complete = true
		}
		if c.Type == batchv1.JobFailed {
			e.Failed = true
			e.Reason = c.Reason
		}
	}
	pods, err := a.client.CoreV1().Pods(a.buildNamespace(t)).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(buildLabels(t)).String()})
	if err != nil {
		return e, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if err := jobPod(p, t, j.UID); err != nil {
			return e, err
		}
		e.PodUIDs = append(e.PodUIDs, p.UID)
		if !e.Complete {
			continue
		}
		for _, s := range p.Status.ContainerStatuses {
			if s.Name != "build" || s.State.Terminated == nil || s.State.Terminated.ExitCode != 0 {
				continue
			}
			var metadata struct {
				Digest string `json:"containerimage.digest"`
			}
			message := s.State.Terminated.Message
			if len(message) > 4096 || json.Unmarshal([]byte(message), &metadata) != nil || !digestPattern.MatchString(metadata.Digest) {
				continue
			}
			if e.Digest != "" && e.Digest != metadata.Digest {
				return e, fmt.Errorf("%w: 多个结果digest冲突", ErrOwnership)
			}
			e.Digest = metadata.Digest
		}
	}
	if e.Complete && e.Digest == "" {
		e.Reason = "Job完成但缺少可信格式的BuildKit metadata，禁止视为构建成功"
	}
	return e, nil
}

type StopEvidence struct {
	Stopped          bool
	JobPresent       bool
	RemainingPodUIDs []types.UID
}

// Cancel 每次只推进删除并重新查证，API失败/删除中均不确认Stopped；调用者按持久取消意图再次协调。
func (a *Adapter) Cancel(ctx context.Context, t BuildTask) (StopEvidence, error) {
	e := StopEvidence{}
	// 取消不能依赖已经到期/销毁的源码授权或仓库凭据，也不能因节点NotReady而拒绝删除。
	if err := t.validateIdentity(); err != nil {
		return e, err
	}
	if err := a.checkNodeIdentity(ctx, t.Node); err != nil {
		return e, err
	}
	jobs := a.client.BatchV1().Jobs(a.buildNamespace(t))
	podsAPI := a.client.CoreV1().Pods(a.buildNamespace(t))
	j, err := jobs.Get(ctx, t.JobName, metav1.GetOptions{})
	uid := t.JobUID
	if err == nil {
		if err := owns(j, buildLabels(t), t.JobUID); err != nil {
			return e, err
		}
		uid = j.UID
		if uid == "" {
			return e, ErrOwnership
		}
		err = jobs.Delete(ctx, j.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: ptr(metav1.DeletePropagationForeground)})
		if err != nil && !apierrors.IsNotFound(err) {
			return e, err
		}
	} else if !apierrors.IsNotFound(err) {
		return e, err
	}
	selector := metav1.ListOptions{LabelSelector: labels.SelectorFromSet(buildLabels(t)).String()}
	pods, err := podsAPI.List(ctx, selector)
	if err != nil {
		return e, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if uid == "" {
			return e, ErrOwnership
		}
		if err := jobPod(p, t, uid); err != nil {
			return e, err
		}
		err = podsAPI.Delete(ctx, p.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.UID}})
		if err != nil && !apierrors.IsNotFound(err) {
			return e, err
		}
	}
	remainingJob, getErr := jobs.Get(ctx, t.JobName, metav1.GetOptions{})
	err = getErr
	if err == nil {
		if err := owns(remainingJob, buildLabels(t), uid); err != nil {
			return e, err
		}
		e.JobPresent = true
	} else if !apierrors.IsNotFound(err) {
		return e, err
	}
	pods, err = podsAPI.List(ctx, selector)
	if err != nil {
		return e, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if uid == "" || jobPod(p, t, uid) != nil {
			return e, ErrOwnership
		}
		e.RemainingPodUIDs = append(e.RemainingPodUIDs, p.UID)
	}
	e.Stopped = !e.JobPresent && len(e.RemainingPodUIDs) == 0
	if e.Stopped {
		secret, err := a.client.CoreV1().Secrets(a.buildNamespace(t)).Get(ctx, t.JobName+"-auth", metav1.GetOptions{})
		if err == nil {
			if err := owns(secret, buildLabels(t), ""); err != nil {
				return e, err
			}
			err = a.client.CoreV1().Secrets(secret.Namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &secret.UID}})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return e, err
		}
	}
	return e, nil
}
