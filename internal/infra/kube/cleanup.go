package kube

import (
	"context"
	"fmt"

	"github.com/sunshanpeng/yummy/internal/lib/naming"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kruntime "k8s.io/apimachinery/pkg/runtime"
)

type CleanupTask struct {
	WorkspaceID, ApplicationID, SpaceSlug, AppName string
	DeleteVolumes                                  bool
}

type cleanupTarget struct {
	object metav1.Object
	remove func(context.Context, string, metav1.DeleteOptions) error
}

type cleanupList func(context.Context, metav1.ListOptions) ([]cleanupTarget, error)

func cleanupKind[L kruntime.Object](list func(context.Context, metav1.ListOptions) (L, error), remove func(context.Context, string, metav1.DeleteOptions) error) cleanupList {
	return func(ctx context.Context, options metav1.ListOptions) ([]cleanupTarget, error) {
		page, err := list(ctx, options)
		if err != nil {
			return nil, err
		}
		items, err := apimeta.ExtractList(page)
		if err != nil {
			return nil, err
		}
		targets := make([]cleanupTarget, 0, len(items))
		for _, item := range items {
			if secret, ok := item.(*corev1.Secret); ok && secret.Type == corev1.SecretTypeTLS {
				continue
			}
			meta, err := apimeta.Accessor(item)
			if err != nil {
				return nil, err
			}
			targets = append(targets, cleanupTarget{object: meta, remove: remove})
		}
		return targets, nil
	}
}

func (a *Adapter) cleanupNamespace(ctx context.Context, name, workspace, role string) (bool, error) {
	ns, err := a.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, owns(ns, map[string]string{LabelManaged: "worker", LabelWorkspace: workspace, "yummy.dev/role": role}, "")
}

func collectCleanup(ctx context.Context, owner map[string]string, kinds ...cleanupList) ([]cleanupTarget, error) {
	options := metav1.ListOptions{LabelSelector: labels.Set{LabelApplication: owner[LabelApplication]}.String()}
	var targets []cleanupTarget
	for _, kind := range kinds {
		items, err := kind(ctx, options)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if err := owns(item.object, owner, ""); err != nil {
				return nil, err
			}
			if item.object.GetUID() == "" || item.object.GetResourceVersion() == "" {
				return nil, fmt.Errorf("%w: 清理对象缺少 UID 或 resourceVersion", ErrOwnership)
			}
		}
		targets = append(targets, items...)
	}
	return targets, nil
}

func checkCleanupName[T metav1.Object](ctx context.Context, name string, owner map[string]string, get func(context.Context, string, metav1.GetOptions) (T, error)) error {
	object, err := get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return owns(object, owner, "")
}

// CleanupApplication 先核对两个命名空间中的所有目标，再清理应用专属工作负载。
// 持久卷需要单独授权；删除请求成功后仍需下一轮确认对象已消失。
func (a *Adapter) CleanupApplication(ctx context.Context, t CleanupTask, before func(context.Context) error) (done, hasVolumes bool, err error) {
	if !validIdentity(t.WorkspaceID) || !validIdentity(t.ApplicationID) ||
		!naming.ValidWorkspaceSlug(t.SpaceSlug) || !naming.ValidName(t.AppName) || before == nil {
		return false, false, ErrInvalid
	}
	if err := a.checkNodeIdentity(ctx, a.config.Node); err != nil {
		return false, false, err
	}
	build, prod := BuildNamespaceFor(t.SpaceSlug), RuntimeNamespaceFor(t.SpaceSlug)
	buildExists, err := a.cleanupNamespace(ctx, build, t.WorkspaceID, "build")
	if err != nil {
		return false, false, err
	}
	prodExists, err := a.cleanupNamespace(ctx, prod, t.WorkspaceID, "runtime")
	if err != nil {
		return false, false, err
	}

	core := a.client.CoreV1()
	var workload, volumes []cleanupTarget
	pendingEndpoints := false
	if buildExists {
		jobs, pods, secrets := a.client.BatchV1().Jobs(build), core.Pods(build), core.Secrets(build)
		workload, err = collectCleanup(ctx, map[string]string{LabelManaged: "worker", LabelWorkspace: t.WorkspaceID, LabelApplication: t.ApplicationID},
			cleanupKind(jobs.List, jobs.Delete), cleanupKind(pods.List, pods.Delete), cleanupKind(secrets.List, secrets.Delete))
		if err != nil {
			return false, false, err
		}
	}
	if prodExists {
		apps := a.client.AppsV1()
		deployments, replicas, pods := apps.Deployments(prod), apps.ReplicaSets(prod), core.Pods(prod)
		ingresses, services := a.client.NetworkingV1().Ingresses(prod), core.Services(prod)
		configs, secrets := core.ConfigMaps(prod), core.Secrets(prod)
		owner := labelsFor(t.WorkspaceID, t.ApplicationID, a.config.Node.UID)
		for _, name := range []string{DeployedName(t.AppName), AppName(t.ApplicationID)} {
			if err := checkCleanupName(ctx, name, owner, deployments.Get); err != nil {
				return false, false, err
			}
			if err := checkCleanupName(ctx, name, owner, services.Get); err != nil {
				return false, false, err
			}
			if err := checkCleanupName(ctx, name, owner, ingresses.Get); err != nil {
				return false, false, err
			}
			// EndpointSlice 由 Kubernetes 回收。旧入口未消失时继续保留应用名。
			slices, err := a.client.DiscoveryV1().EndpointSlices(prod).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{discoveryv1.LabelServiceName: name}.String()})
			if err != nil {
				return false, false, err
			}
			pendingEndpoints = pendingEndpoints || len(slices.Items) != 0
		}
		runtime, err := collectCleanup(ctx, owner,
			cleanupKind(deployments.List, deployments.Delete), cleanupKind(replicas.List, replicas.Delete),
			cleanupKind(pods.List, pods.Delete), cleanupKind(ingresses.List, ingresses.Delete),
			cleanupKind(services.List, services.Delete), cleanupKind(configs.List, configs.Delete), cleanupKind(secrets.List, secrets.Delete))
		if err != nil {
			return false, false, err
		}
		workload = append(workload, runtime...)
		claims := core.PersistentVolumeClaims(prod)
		volumes, err = collectCleanup(ctx, owner, cleanupKind(claims.List, claims.Delete))
		if err != nil {
			return false, false, err
		}
	}

	hasVolumes = len(volumes) > 0
	if len(workload) > 0 {
		return false, hasVolumes, removeCleanupTargets(ctx, workload, before)
	}
	if pendingEndpoints {
		return false, hasVolumes, nil
	}
	if t.DeleteVolumes && hasVolumes {
		return false, true, removeCleanupTargets(ctx, volumes, before)
	}
	return true, hasVolumes, nil
}

func removeCleanupTargets(ctx context.Context, targets []cleanupTarget, before func(context.Context) error) error {
	for _, target := range targets {
		if target.object.GetDeletionTimestamp() != nil {
			continue
		}
		if err := before(ctx); err != nil {
			return err
		}
		uid, rv := target.object.GetUID(), target.object.GetResourceVersion()
		options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, PropagationPolicy: ptr(metav1.DeletePropagationForeground)}
		if err := target.remove(ctx, target.object.GetName(), options); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
