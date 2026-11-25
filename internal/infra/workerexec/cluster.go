package workerexec

import (
	"context"
	"slices"
	"strings"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func ReadClusterMachines(ctx context.Context, client kubernetes.Interface) *platformv1.ClusterMachinesReport {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: workerprotocol.ClusterMachineLimit})
	if apierrors.IsForbidden(err) {
		return &platformv1.ClusterMachinesReport{ErrorCode: "FORBIDDEN"}
	}
	if err != nil {
		return &platformv1.ClusterMachinesReport{ErrorCode: "UNAVAILABLE"}
	}
	report := &platformv1.ClusterMachinesReport{Truncated: nodes.Continue != "" || len(nodes.Items) > workerprotocol.ClusterMachineLimit}
	for _, node := range nodes.Items[:min(len(nodes.Items), workerprotocol.ClusterMachineLimit)] {
		ready := slices.ContainsFunc(node.Status.Conditions, func(c corev1.NodeCondition) bool {
			return c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue
		})
		info := node.Status.NodeInfo
		report.Machines = append(report.Machines, &platformv1.ClusterMachine{
			Uid: string(node.UID), Name: node.Name, Os: info.OperatingSystem, Arch: info.Architecture,
			KubeletVersion: info.KubeletVersion, Ready: ready, Schedulable: !node.Spec.Unschedulable,
		})
	}
	slices.SortFunc(report.Machines, func(a, b *platformv1.ClusterMachine) int { return strings.Compare(a.Name, b.Name) })
	return report
}
