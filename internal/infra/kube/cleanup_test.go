package kube

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"
)

func TestApplicationCleanupPreservesVolumesUntilExplicitConfirmation(t *testing.T) {
	a, client, _ := fixture(t)
	task := CleanupTask{WorkspaceID: "workspace-1", ApplicationID: "app-1", SpaceSlug: "team", AppName: "demo"}
	if err := a.BootstrapBuildNamespace(ctx, task.SpaceSlug, task.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := a.BootstrapRuntimeNamespace(ctx, task.SpaceSlug, task.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	runtimeNS, buildNS := RuntimeNamespaceFor(task.SpaceSlug), BuildNamespaceFor(task.SpaceSlug)
	metadata := func(namespace, name, application string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: namespace, Name: name, ResourceVersion: "42", Labels: labelsFor(task.WorkspaceID, application, a.config.Node.UID)}
	}
	deployment := &appsv1.Deployment{ObjectMeta: metadata(runtimeNS, DeployedName(task.AppName), task.ApplicationID)}
	_, err := client.AppsV1().Deployments(runtimeNS).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.BatchV1().Jobs(buildNS).Create(ctx, &batchv1.Job{ObjectMeta: metadata(buildNS, "build-job", task.ApplicationID)}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CoreV1().Secrets(runtimeNS).Create(ctx, &corev1.Secret{ObjectMeta: metadata(runtimeNS, "other-secret", "other-app")}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metadata(runtimeNS, "demo-default-pvc-data", task.ApplicationID)}
	if _, err := client.CoreV1().PersistentVolumeClaims(runtimeNS).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	endpoints := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "old-endpoints", Labels: map[string]string{discoveryv1.LabelServiceName: deployment.Name}}}
	if _, err := client.DiscoveryV1().EndpointSlices(runtimeNS).Create(ctx, endpoints, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	check := func(context.Context) error { return nil }
	done, volumes, err := a.CleanupApplication(ctx, task, check)
	if err != nil || done || !volumes {
		t.Fatalf("第一次清理：%v %v %v", done, volumes, err)
	}
	for _, action := range client.Actions() {
		if d, ok := action.(ktesting.DeleteAction); ok {
			options := d.GetDeleteOptions()
			if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID == "" || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != "42" {
				t.Fatalf("删除未携带 UID / resourceVersion 前置条件：%v", action)
			}
		}
	}
	done, volumes, err = a.CleanupApplication(ctx, task, check)
	if err != nil || done || !volumes {
		t.Fatalf("端点仍在回收时保留应用和卷：%v %v %v", done, volumes, err)
	}
	if err := client.DiscoveryV1().EndpointSlices(runtimeNS).Delete(ctx, endpoints.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	done, volumes, err = a.CleanupApplication(ctx, task, check)
	if err != nil || !done || !volumes {
		t.Fatalf("保留 PVC：%v %v %v", done, volumes, err)
	}
	if _, err := client.CoreV1().Secrets(runtimeNS).Get(ctx, "other-secret", metav1.GetOptions{}); err != nil {
		t.Fatalf("其他应用被清理：%v", err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(runtimeNS).Get(ctx, claim.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("卷提前删除：%v", err)
	}
	task.DeleteVolumes = true
	done, _, err = a.CleanupApplication(ctx, task, check)
	if err != nil || done {
		t.Fatalf("清理卷仍需等待实际消失：%v %v", done, err)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(runtimeNS).Get(ctx, claim.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("已确认卷未删除：%v", err)
	}
	done, volumes, err = a.CleanupApplication(ctx, task, check)
	if err != nil || !done || volumes {
		t.Fatalf("完成确认：%v %v %v", done, volumes, err)
	}
}

func TestApplicationCleanupRefusesWrongOwnershipOrAbsentLease(t *testing.T) {
	a, client, _ := fixture(t)
	task := CleanupTask{WorkspaceID: "workspace-1", ApplicationID: "app-1", SpaceSlug: "team", AppName: "demo"}
	if err := a.BootstrapRuntimeNamespace(ctx, task.SpaceSlug, task.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	ns := RuntimeNamespaceFor(task.SpaceSlug)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: DeployedName(task.AppName), Namespace: ns, UID: types.UID("app-uid"), ResourceVersion: "1", Labels: labelsFor(task.WorkspaceID, "another-app", a.config.Node.UID)}}
	if _, err := client.AppsV1().Deployments(ns).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CleanupApplication(ctx, task, func(context.Context) error { return nil }); !errors.Is(err, ErrOwnership) {
		t.Fatalf("错误归属没有拦截：%v", err)
	}
	deployment.Labels[LabelApplication] = task.ApplicationID
	if _, err := client.AppsV1().Deployments(ns).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	expired := errors.New("租约失效")
	if _, _, err := a.CleanupApplication(ctx, task, func(context.Context) error { return expired }); !errors.Is(err, expired) {
		t.Fatalf("未检查租约：%v", err)
	}
	if _, err := client.AppsV1().Deployments(ns).Get(ctx, deployment.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("失效租约下删除应用：%v", err)
	}
}
