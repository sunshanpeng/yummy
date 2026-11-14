package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sunshanpeng/yummy/internal/platform/runtimeconfig"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Release 是控制面已验证、不可变的发布快照。Placement不得由应用输入或当前默认策略重新计算。
type Release struct {
	ID, WorkspaceID, ApplicationID, Image string
	// SpaceSlug/AppName 统一命名派生源：运行命名空间 prod-<space_slug>，
	// Deployment/Service 名称 <app_name>-default（2026-10-26 设计）。
	SpaceSlug, AppName string
	ConfigRevision     int64
	Placement          NodeTarget
	// 拉取凭据只供受限worker内部使用，不参与快照公开序列化。
	RegistryDockerConfig      []byte `json:"-"`
	RegistryCredentialVersion int64
}
type RuntimeChange struct {
	Release            Release
	OperationID        string
	ExpectedGeneration int64
}

// GenerationGuard 必须查询控制面的当前持久期望态；每个写API之前都会调用。
// 控制面仍须串行化同一应用的操作/在途调用，本接口不假装跨数据库和Kubernetes具备分布式事务。
type GenerationGuard interface {
	Check(context.Context, RuntimeChange) error
}
type Runtime struct {
	adapter *Adapter
	guard   GenerationGuard
}

func (a *Adapter) Runtime(guard GenerationGuard) (*Runtime, error) {
	if guard == nil {
		return nil, fmt.Errorf("%w: 缺少控制面generation校验", ErrInvalid)
	}
	return &Runtime{adapter: a, guard: guard}, nil
}
func (c RuntimeChange) validate() error {
	for _, id := range []string{c.Release.ID, c.Release.WorkspaceID, c.Release.ApplicationID, c.OperationID, string(c.Release.Placement.UID)} {
		if !validIdentity(id) {
			return ErrInvalid
		}
	}
	if c.ExpectedGeneration < 1 || c.Release.ConfigRevision < 1 || !pinnedImage(c.Release.Image) {
		return fmt.Errorf("%w: generation/revision/镜像digest", ErrInvalid)
	}
	if len(c.Release.RegistryDockerConfig) > 0 && c.Release.RegistryCredentialVersion < 1 {
		return ErrInvalid
	}
	if len(c.Release.RegistryDockerConfig) > 256<<10 || (len(c.Release.RegistryDockerConfig) > 0 && !json.Valid(c.Release.RegistryDockerConfig)) {
		return ErrInvalid
	}
	return nil
}
func runtimeLabels(c RuntimeChange) map[string]string {
	l := labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID)
	l[LabelRelease] = c.Release.ID
	l[LabelOperation] = c.OperationID
	l[LabelGeneration] = strconv.FormatInt(c.ExpectedGeneration, 10)
	return l
}
func (r *Runtime) before(ctx context.Context, c RuntimeChange) error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := r.guard.Check(ctx, c); err != nil {
		return err
	}
	return r.adapter.checkNodeIdentity(ctx, c.Release.Placement)
}
func runtimeOwned(meta metav1.Object, c RuntimeChange) error {
	if err := owns(meta, labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID), ""); err != nil {
		return err
	}
	gen, err := strconv.ParseInt(meta.GetLabels()[LabelGeneration], 10, 64)
	if err != nil || gen > c.ExpectedGeneration {
		return ErrGeneration
	}
	if gen == c.ExpectedGeneration && (meta.GetLabels()[LabelOperation] != c.OperationID || meta.GetLabels()[LabelRelease] != c.Release.ID) {
		return ErrOwnership
	}
	return nil
}
func runtimeMeta(name, namespace string, c RuntimeChange) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: runtimeLabels(c)}
}

// putObject 使用typed client的GET/Create/Update，保留resourceVersion作为Kubernetes并发条件。
// AlreadyExists只重新读取核对，不换名、强制覆盖或跨应用接管。
func putObject[T metav1.Object](ctx context.Context, r *Runtime, c RuntimeChange, want T, get func(context.Context, string, metav1.GetOptions) (T, error), create func(context.Context, T, metav1.CreateOptions) (T, error), update func(context.Context, T, metav1.UpdateOptions) (T, error), same func(T, T) bool) (T, error) {
	var zero T
	if _, ok := any(want).(*corev1.PersistentVolumeClaim); ok {
		want.SetLabels(labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID))
	}
	got, err := get(ctx, want.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if err = r.before(ctx, c); err != nil {
			return zero, err
		}
		got, err = create(ctx, want, metav1.CreateOptions{})
		if !apierrors.IsAlreadyExists(err) {
			return got, err
		}
		got, err = get(ctx, want.GetName(), metav1.GetOptions{})
	}
	if err != nil {
		return zero, err
	}
	switch any(want).(type) {
	case *corev1.PersistentVolumeClaim:
		err = owns(got, labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID), "")
	case *corev1.Secret, *corev1.ConfigMap:
		err = owns(got, labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID), "")
		if err == nil && got.GetLabels()[LabelRelease] != c.Release.ID {
			err = ErrOwnership
		}
	default:
		err = runtimeOwned(got, c)
	}
	if err != nil {
		return zero, err
	}
	if same != nil {
		if !same(got, want) {
			return zero, ErrOwnership
		}
		return got, nil
	}
	want.SetResourceVersion(got.GetResourceVersion())
	want.SetUID(got.GetUID())
	if err = r.before(ctx, c); err != nil {
		return zero, err
	}
	return update(ctx, want, metav1.UpdateOptions{})
}
func validateMaterialized(c RuntimeChange, m runtimeconfig.Materialized) error {
	v := m.View
	if v.WorkspaceID != c.Release.WorkspaceID || v.ApplicationID != c.Release.ApplicationID || v.Revision != c.Release.ConfigRevision || !v.Confirmed {
		return fmt.Errorf("%w: release与配置版本不一致", ErrInvalid)
	}
	input := runtimeconfig.Input{Type: v.Type, Port: v.Port, Probe: v.Probe, Command: slices.Clone(v.Command), Args: slices.Clone(v.Args), Volumes: slices.Clone(v.Volumes), Domains: slices.Clone(v.Domains), Resources: v.Resources, Confirmed: v.Confirmed}
	if len(m.Variables) != len(v.Variables) || len(m.Files) != len(v.Files) {
		return fmt.Errorf("%w: materialized字段数量不一致", ErrInvalid)
	}
	for _, variable := range v.Variables {
		value, ok := m.Variables[variable.Name]
		if !ok || !variable.Configured {
			return ErrInvalid
		}
		input.Variables = append(input.Variables, runtimeconfig.VariableInput{Name: variable.Name, Value: value, Sensitive: variable.Sensitive})
	}
	for _, file := range v.Files {
		value, ok := m.Files[file.Path]
		if !ok || !file.Configured {
			return ErrInvalid
		}
		input.Files = append(input.Files, runtimeconfig.FileInput{Path: file.Path, Content: value, Sensitive: file.Sensitive})
	}
	if _, err := runtimeconfig.Validate(input); err != nil {
		return err
	}
	return nil
}
func runtimeResources(v runtimeconfig.Resources) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(v.CPURequestMilli, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(v.MemoryRequestMiB<<20, resource.BinarySI), corev1.ResourceEphemeralStorage: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: *resource.NewMilliQuantity(v.CPULimitMilli, resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(v.MemoryLimitMiB<<20, resource.BinarySI), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}}
}
func httpProbe(v runtimeconfig.View) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: v.Probe.Path, Port: intstr.FromInt32(v.Port)}}, InitialDelaySeconds: v.Probe.InitialDelaySeconds, PeriodSeconds: v.Probe.PeriodSeconds, TimeoutSeconds: v.Probe.TimeoutSeconds, FailureThreshold: v.Probe.FailureThreshold}
}

type runtimeObjects struct {
	deployment *appsv1.Deployment
	configMap  *corev1.ConfigMap
	secret     *corev1.Secret
	pullSecret *corev1.Secret
	pvcs       []*corev1.PersistentVolumeClaim
	service    *corev1.Service
	ingress    *networkingv1.Ingress
}

// Route 仅接受控制面已核验占用的域名/TLS Secret引用，不允许用户传任意Ingress注解。
type Route struct {
	Hosts         []string
	TLSSecretName string
	HTTPS         bool
}

func (r *Runtime) objects(c RuntimeChange, m runtimeconfig.Materialized, route Route) (runtimeObjects, error) {
	a := r.adapter
	v := m.View
	ns := a.runtimeNamespace(c)
	name := a.deployedName(c.Release)
	configName := name + "-" + shortHash(c.Release.ID)[:12]
	objects := runtimeObjects{}
	cm := &corev1.ConfigMap{ObjectMeta: runtimeMeta(configName, ns, c), Immutable: ptr(true), Data: map[string]string{}}
	secret := &corev1.Secret{ObjectMeta: runtimeMeta(configName, ns, c), Immutable: ptr(true), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
	cm.Annotations = map[string]string{annotationSnapshot: fingerprint(c.Release)}
	secret.Annotations = map[string]string{annotationSnapshot: fingerprint(c.Release)}
	container := corev1.Container{Name: "app", Image: c.Release.Image, ImagePullPolicy: corev1.PullIfNotPresent, Command: slices.Clone(v.Command), Args: slices.Clone(v.Args), // 应用容器不随构建资格放宽：始终保持 no_new_privs 与默认硬化。
		SecurityContext: containerSecurity(InstalledCapabilities{}), Resources: runtimeResources(v.Resources)}
	pod := corev1.PodSpec{NodeName: c.Release.Placement.Name, AutomountServiceAccountToken: ptr(false), SecurityContext: podSecurity(), EnableServiceLinks: ptr(false), RestartPolicy: corev1.RestartPolicyAlways}
	if len(c.Release.RegistryDockerConfig) > 0 {
		pullName := configName + "-pull"
		objects.pullSecret = &corev1.Secret{ObjectMeta: runtimeMeta(pullName, ns, c), Type: corev1.SecretTypeDockerConfigJson, Immutable: ptr(true), Data: map[string][]byte{corev1.DockerConfigJsonKey: slices.Clone(c.Release.RegistryDockerConfig)}}
		objects.pullSecret.Annotations = map[string]string{annotationSnapshot: fingerprint(c.Release)}
		pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullName}}
	}
	for _, variable := range v.Variables {
		env := corev1.EnvVar{Name: variable.Name}
		if variable.Sensitive {
			key := "env-" + variable.Name
			secret.Data[key] = []byte(m.Variables[variable.Name])
			env.ValueFrom = &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: configName}, Key: key}}
		} else {
			env.Value = m.Variables[variable.Name]
		}
		container.Env = append(container.Env, env)
	}
	for i, file := range v.Files {
		key := fmt.Sprintf("file-%d", i)
		volName := key
		volume := corev1.Volume{Name: volName}
		if file.Sensitive {
			secret.Data[key] = []byte(m.Files[file.Path])
			volume.Secret = &corev1.SecretVolumeSource{SecretName: configName, DefaultMode: ptr(int32(0440)), Items: []corev1.KeyToPath{{Key: key, Path: "content"}}}
		} else {
			cm.Data[key] = m.Files[file.Path]
			volume.ConfigMap = &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configName}, DefaultMode: ptr(int32(0444)), Items: []corev1.KeyToPath{{Key: key, Path: "content"}}}
		}
		pod.Volumes = append(pod.Volumes, volume)
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: volName, MountPath: file.Path, SubPath: "content", ReadOnly: true})
	}
	if len(secret.Data) > 0 {
		objects.secret = secret
	}
	if len(cm.Data) > 0 {
		objects.configMap = cm
	}
	// Kubernetes单个Secret/ConfigMap有1MiB上限；提前拒绝而不是在变更一半后失败。
	size := 0
	for k, val := range secret.Data {
		size += len(k) + len(val)
	}
	if size > 900<<10 {
		return objects, fmt.Errorf("%w: Secret配置过大", ErrInvalid)
	}
	size = 0
	for k, val := range cm.Data {
		size += len(k) + len(val)
	}
	if size > 900<<10 {
		return objects, fmt.Errorf("%w: ConfigMap配置过大", ErrInvalid)
	}
	for _, volume := range v.Volumes {
		if a.config.StorageClass == "" {
			return objects, fmt.Errorf("%w: 未配置固定StorageClass", ErrNotReady)
		}
		claimName := name + "-pvc-" + volume.Name
		claim := &corev1.PersistentVolumeClaim{ObjectMeta: runtimeMeta(claimName, ns, c), Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: ptr(a.config.StorageClass), Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", volume.SizeGiB))}}}}
		objects.pvcs = append(objects.pvcs, claim)
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "pvc-" + volume.Name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "pvc-" + volume.Name, MountPath: volume.MountPath})
	}
	selector := labelsFor(c.Release.WorkspaceID, c.Release.ApplicationID, c.Release.Placement.UID)
	strategy := appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: ptr(intstr.FromInt32(1)), MaxUnavailable: ptr(intstr.FromInt32(0))}}
	if len(v.Volumes) > 0 {
		strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}
	if v.Type == runtimeconfig.TypeHTTP {
		container.Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: v.Port}}
		container.ReadinessProbe = httpProbe(v)
		container.LivenessProbe = httpProbe(v)
		container.StartupProbe = httpProbe(v)
		container.StartupProbe.FailureThreshold = 30
		objects.service = &corev1.Service{ObjectMeta: runtimeMeta(name, ns, c), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: selector, Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromString("http")}}}}
		if len(route.Hosts) == 0 {
			return objects, fmt.Errorf("%w: HTTP缺少控制面已分配入口", ErrInvalid)
		}
		ingress := &networkingv1.Ingress{ObjectMeta: runtimeMeta(name, ns, c)}
		if a.config.IngressClass != "" {
			ingress.Spec.IngressClassName = ptr(a.config.IngressClass)
		}
		hosts := slices.Clone(route.Hosts)
		slices.Sort(hosts)
		for i, host := range hosts {
			normalized, err := runtimeconfig.CanonicalDomain(host)
			if err != nil || normalized != host || (i > 0 && hosts[i-1] == host) {
				return objects, ErrInvalid
			}
			ingress.Spec.Rules = append(ingress.Spec.Rules, networkingv1.IngressRule{Host: host, IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: ptr(networkingv1.PathTypePrefix), Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: name, Port: networkingv1.ServiceBackendPort{Name: "http"}}}}}}}})
		}
		for _, domain := range v.Domains {
			if !slices.Contains(hosts, domain) {
				return objects, fmt.Errorf("%w: 已绑定自定义域名未分配路由", ErrInvalid)
			}
		}
		if route.HTTPS {
			if len(validation.IsDNS1123Subdomain(route.TLSSecretName)) != 0 {
				return objects, fmt.Errorf("%w: HTTPS必须有受管预置证书Secret", ErrInvalid)
			}
			ingress.Spec.TLS = []networkingv1.IngressTLS{{Hosts: hosts, SecretName: route.TLSSecretName}}
		} else if route.TLSSecretName != "" {
			return objects, ErrInvalid
		}
		objects.ingress = ingress
	} else if len(route.Hosts) > 0 || route.HTTPS || route.TLSSecretName != "" {
		return objects, fmt.Errorf("%w: 后台进程不创建入口", ErrInvalid)
	}
	pod.Containers = []corev1.Container{container}
	objects.deployment = &appsv1.Deployment{ObjectMeta: runtimeMeta(name, ns, c), Spec: appsv1.DeploymentSpec{Replicas: ptr(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: selector}, Strategy: strategy, RevisionHistoryLimit: ptr(int32(2)), ProgressDeadlineSeconds: ptr(int32(600)), Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: runtimeLabels(c), Annotations: map[string]string{"yummy.dev/operation": c.OperationID, annotationSnapshot: fingerprint(c.Release)}}, Spec: pod}}}
	return objects, nil
}

// Apply 只推进Kubernetes期望资源，绝不宣告业务URL或发布成功；PVC永不删除。
func (r *Runtime) Apply(ctx context.Context, c RuntimeChange, m runtimeconfig.Materialized, route Route) (types.UID, error) {
	if err := r.before(ctx, c); err != nil {
		return "", err
	}
	if err := r.adapter.capability("runtime"); err != nil {
		return "", err
	}
	if err := r.adapter.checkNode(ctx, c.Release.Placement); err != nil {
		return "", err
	}
	if m.View.Type == runtimeconfig.TypeHTTP {
		if err := r.adapter.capability("ingress"); err != nil {
			return "", err
		}
	}
	if err := validateMaterialized(c, m); err != nil {
		return "", err
	}
	objects, err := r.objects(c, m, route)
	if err != nil {
		return "", err
	}
	a := r.adapter
	ns := a.runtimeNamespace(c)
	// 在创建配置/路由之前预检稳定Deployment，避免跨app冲突后先改动其他运行资源。
	current, checkErr := a.client.AppsV1().Deployments(ns).Get(ctx, objects.deployment.Name, metav1.GetOptions{})
	if checkErr == nil {
		if err := runtimeOwned(current, c); err != nil {
			return "", err
		}
	} else if !apierrors.IsNotFound(checkErr) {
		return "", checkErr
	}
	if objects.configMap != nil {
		api := a.client.CoreV1().ConfigMaps(ns)
		_, err = putObject(ctx, r, c, objects.configMap, api.Get, api.Create, api.Update, func(g, w *corev1.ConfigMap) bool {
			return g.Immutable != nil && *g.Immutable && equality.Semantic.DeepEqual(g.Data, w.Data) && g.Annotations[annotationSnapshot] == w.Annotations[annotationSnapshot]
		})
		if err != nil {
			return "", err
		}
	}
	for _, desiredSecret := range []*corev1.Secret{objects.secret, objects.pullSecret} {
		if desiredSecret == nil {
			continue
		}
		api := a.client.CoreV1().Secrets(ns)
		_, err = putObject(ctx, r, c, desiredSecret, api.Get, api.Create, api.Update, func(g, w *corev1.Secret) bool {
			return g.Immutable != nil && *g.Immutable && equality.Semantic.DeepEqual(g.Data, w.Data) && g.Annotations[annotationSnapshot] == w.Annotations[annotationSnapshot]
		})
		if err != nil {
			return "", err
		}
	}
	for _, pvc := range objects.pvcs {
		api := a.client.CoreV1().PersistentVolumeClaims(ns)
		_, err = putObject(ctx, r, c, pvc, api.Get, api.Create, api.Update, func(g, w *corev1.PersistentVolumeClaim) bool {
			return g.Spec.StorageClassName != nil && *g.Spec.StorageClassName == *w.Spec.StorageClassName && equality.Semantic.DeepEqual(g.Spec.AccessModes, w.Spec.AccessModes) && g.Spec.Resources.Requests.Storage().Cmp(*w.Spec.Resources.Requests.Storage()) == 0
		})
		if err != nil {
			return "", err
		}
	}
	if objects.service != nil {
		api := a.client.CoreV1().Services(ns)
		update := func(ctx context.Context, w *corev1.Service, opts metav1.UpdateOptions) (*corev1.Service, error) {
			g, err := api.Get(ctx, w.Name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			if err := runtimeOwned(g, c); err != nil {
				return nil, err
			}
			if g.ResourceVersion != w.ResourceVersion {
				return nil, ErrGeneration
			}
			w.Spec.ClusterIP = g.Spec.ClusterIP
			w.Spec.ClusterIPs = g.Spec.ClusterIPs
			w.Spec.IPFamilies = g.Spec.IPFamilies
			w.Spec.IPFamilyPolicy = g.Spec.IPFamilyPolicy
			if err := r.before(ctx, c); err != nil {
				return nil, err
			}
			return api.Update(ctx, w, opts)
		}
		if _, err = putObject(ctx, r, c, objects.service, api.Get, api.Create, update, nil); err != nil {
			return "", err
		}
		apiIngress := a.client.NetworkingV1().Ingresses(ns)
		if _, err = putObject(ctx, r, c, objects.ingress, apiIngress.Get, apiIngress.Create, apiIngress.Update, nil); err != nil {
			return "", err
		}
	} else {
		// HTTP改为后台类型只允许控制面在首次成功发布前做；删除前仍核对app归属与generation。
		if err = r.removeRoute(ctx, c); err != nil {
			return "", err
		}
	}
	api := a.client.AppsV1().Deployments(ns)
	dep, err := putObject(ctx, r, c, objects.deployment, api.Get, api.Create, api.Update, nil)
	if err != nil {
		return "", err
	}
	return dep.UID, nil
}
func (r *Runtime) removeRoute(ctx context.Context, c RuntimeChange) error {
	ns := r.adapter.runtimeNamespace(c)
	name := r.adapter.deployedName(c.Release)
	apiI := r.adapter.client.NetworkingV1().Ingresses(ns)
	i, err := apiI.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if err := runtimeOwned(i, c); err != nil {
			return err
		}
		if err := r.before(ctx, c); err != nil {
			return err
		}
		err = apiI.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &i.UID, ResourceVersion: &i.ResourceVersion}})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	apiS := r.adapter.client.CoreV1().Services(ns)
	s, err := apiS.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if err := runtimeOwned(s, c); err != nil {
			return err
		}
		if err := r.before(ctx, c); err != nil {
			return err
		}
		err = apiS.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &s.UID, ResourceVersion: &s.ResourceVersion}})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
func (r *Runtime) scale(ctx context.Context, c RuntimeChange, replicas int32, restart bool) error {
	if err := r.before(ctx, c); err != nil {
		return err
	}
	if replicas > 0 {
		if err := r.adapter.capability("runtime"); err != nil {
			return err
		}
		if err := r.adapter.checkNode(ctx, c.Release.Placement); err != nil {
			return err
		}
	}
	api := r.adapter.client.AppsV1().Deployments(r.adapter.runtimeNamespace(c))
	d, err := api.Get(ctx, r.adapter.deployedName(c.Release), metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := runtimeOwned(d, c); err != nil {
		return err
	}
	// 停止命令有意不包含历史仓库凭据版本；已持久化的发布 ID、部署镜像和受管标签
	// 足以安全缩容，不能因仓库已撤销而使停止失效。启动/重启仍要求完整冻结快照。
	if d.Spec.Template.Labels[LabelRelease] != c.Release.ID || len(d.Spec.Template.Spec.Containers) != 1 || d.Spec.Template.Spec.Containers[0].Image != c.Release.Image {
		return ErrOwnership
	}
	if replicas > 0 && d.Spec.Template.Annotations[annotationSnapshot] != fingerprint(c.Release) {
		return ErrOwnership
	}
	// 相同operation的重投只核对，不重复修改Pod模板触发重启。
	marker := "yummy.dev/restart-operation"
	if d.Labels[LabelOperation] == c.OperationID && d.Spec.Replicas != nil && *d.Spec.Replicas == replicas && (!restart || d.Spec.Template.Annotations[marker] == c.OperationID) {
		return nil
	}
	d.Labels = runtimeLabels(c)
	d.Spec.Replicas = ptr(replicas)
	if replicas > 0 {
		d.Spec.Template.Labels = runtimeLabels(c)
		if d.Spec.Template.Annotations == nil {
			d.Spec.Template.Annotations = map[string]string{}
		}
		d.Spec.Template.Annotations["yummy.dev/operation"] = c.OperationID
		if restart {
			d.Spec.Template.Annotations[marker] = c.OperationID
		}
	}
	if err := r.before(ctx, c); err != nil {
		return err
	}
	_, err = api.Update(ctx, d, metav1.UpdateOptions{})
	return err
}

// Stop仅缩容为0；返回nil只是写入成功，停止确认仍必须Observe检查Pod退出，PVC保留。
func (r *Runtime) Stop(ctx context.Context, c RuntimeChange) error  { return r.scale(ctx, c, 0, false) }
func (r *Runtime) Start(ctx context.Context, c RuntimeChange) error { return r.scale(ctx, c, 1, false) }
func (r *Runtime) Restart(ctx context.Context, c RuntimeChange) error {
	return r.scale(ctx, c, 1, true)
}

func correctImageID(id, image string) bool {
	parts := strings.Split(image, "@")
	if len(parts) != 2 {
		return false
	}
	if id == image || id == "docker-pullable://"+image || id == "containerd://"+parts[1] || id == "docker://"+parts[1] {
		return true
	}
	// 实机发现⑦（2026-11-14）：内容寻址构建使不同应用的镜像 digest 相同，而
	// containerd 的 ImageID 引用路径取决于先拉取者（如 repo@app_a@digest vs
	// repo@app_b@digest），整串精确比较会把同 digest 的合法运行证据误判为
	// 不匹配。此处回退为摘要等价：两边 @sha256 摘要一致即视为同一镜像证据。
	if i := strings.LastIndex(id, "@sha256:"); i >= 0 {
		return id[i+1:] == parts[1]
	}
	return false
}
