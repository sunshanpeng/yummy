package workercli

import (
	"context"
	"errors"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/infra/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func cleanupApplication(ctx context.Context, adapter *kube.Adapter, node kube.NodeTarget, command *platformv1.WorkerCleanupCommand, check func(context.Context) error) (bool, bool, string) {
	if command.GetLease().GetClusterNodeUid() != string(node.UID) {
		return false, false, "OWNERSHIP_CONFLICT"
	}
	var leaseErr error
	done, volumes, err := adapter.CleanupApplication(ctx, kube.CleanupTask{
		WorkspaceID: command.WorkspaceId, ApplicationID: command.ApplicationId,
		SpaceSlug: command.SpaceSlug, AppName: command.AppName, DeleteVolumes: command.DeleteVolumes,
	}, func(ctx context.Context) error {
		leaseErr = check(ctx)
		return leaseErr
	})
	if leaseErr != nil || apierrors.IsConflict(err) {
		return false, false, ""
	}
	return done, volumes, cleanupErrorCode(err)
}

func cleanupErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, kube.ErrOwnership):
		return "OWNERSHIP_CONFLICT"
	case apierrors.IsForbidden(err):
		return "PERMISSION_DENIED"
	default:
		return "KUBERNETES_UNAVAILABLE"
	}
}
