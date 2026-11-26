package workergateway

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/domains/application"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/enrollment"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
	"gorm.io/gorm"
)

func (g *Gateway) cleanupWorker(ctx context.Context, identity enrollment.Identity, epoch int64) (application.CleanupWorker, error) {
	var node struct{ ClusterNodeUID string }
	err := g.config.DB.WithContext(ctx).Table("execution_nodes").
		Select("cluster_node_uid").Where("id = ? AND worker_id = ? AND connection_epoch = ? AND connection_owner <> '' AND status IN ('ACTIVE','DRAINING') AND cluster_node_uid <> ''", identity.NodeID, identity.WorkerID, epoch).
		Take(&node).Error
	return application.CleanupWorker{NodeID: identity.NodeID, ClusterUID: node.ClusterNodeUID, ConnectionEpoch: epoch}, err
}

func (g *Gateway) scheduleCleanup(ctx context.Context, s *session) error {
	if g.config.ApplicationCleanup == nil || !s.applicationCleanup {
		return nil
	}
	worker, err := g.cleanupWorker(ctx, s.identity, s.epoch)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	dispatch, err := g.config.ApplicationCleanup.ClaimCleanup(ctx, worker)
	if err != nil || dispatch.Lease.ID == "" {
		return err
	}
	if err := g.check(ctx, s); err != nil {
		return err
	}
	lease := &platformv1.WorkerCleanupLease{CleanupId: dispatch.Lease.ID, Revision: dispatch.Lease.Revision, ConnectionEpoch: s.epoch, ClusterNodeUid: worker.ClusterUID}
	command := &platformv1.WorkerCleanupCommand{Lease: lease, WorkspaceId: dispatch.WorkspaceID, ApplicationId: dispatch.ApplicationID, SpaceSlug: dispatch.SpaceSlug, AppName: dispatch.AppName, DeleteVolumes: dispatch.DeleteVolumes}
	return g.enqueue(s, &platformv1.ControlEnvelope{Payload: &platformv1.ControlEnvelope_ApplicationCleanup{ApplicationCleanup: command}})
}

func validCleanupLease(lease *platformv1.WorkerCleanupLease) bool {
	return lease.GetCleanupId() != "" && len(lease.GetCleanupId()) <= 128 && lease.GetRevision() > 0 && lease.GetConnectionEpoch() > 0 && lease.GetClusterNodeUid() != "" && len(lease.GetClusterNodeUid()) <= 256
}

func (g *Gateway) receiveCleanup(ctx context.Context, s *session, result *platformv1.WorkerCleanupResult) error {
	lease := result.GetLease()
	if g.config.ApplicationCleanup == nil || !s.applicationCleanup || !validCleanupLease(lease) || lease.GetConnectionEpoch() != s.epoch {
		return workerprotocol.ErrProtocol
	}
	worker, err := g.cleanupWorker(ctx, s.identity, s.epoch)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if worker.ClusterUID != lease.ClusterNodeUid {
		return workerprotocol.ErrIdentity
	}
	err = g.config.ApplicationCleanup.CompleteCleanup(ctx, worker, application.CleanupLease{ID: lease.CleanupId, Revision: lease.Revision}, application.CleanupResult{Done: result.Done, HasVolumes: result.HasVolumes, ErrorCode: result.ErrorCode})
	// 结果丢失可在租约到期后重做；迟到的结果不影响同一流上的其他任务。
	if errors.Is(err, authorization.ErrConflict) {
		return nil
	}
	return err
}

func (g *Gateway) CheckApplicationCleanup(ctx context.Context, request *connect.Request[platformv1.WorkerCleanupLease]) (*connect.Response[platformv1.WorkerGenerationResponse], error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, connect.NewError(connect.CodeUnavailable, workerprotocol.ErrIdentity)
	}
	g.streams.Add(1)
	g.mu.Unlock()
	defer g.streams.Done()
	certificate, err := peer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, workerprotocol.ErrIdentity)
	}
	identity, err := g.config.Peers.VerifyPeer(ctx, certificate)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, workerprotocol.ErrIdentity)
	}
	lease := request.Msg
	if !validCleanupLease(lease) {
		return nil, connect.NewError(connect.CodeInvalidArgument, workerprotocol.ErrProtocol)
	}
	if g.config.ApplicationCleanup == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, workerprotocol.ErrQualification)
	}
	worker, err := g.cleanupWorker(ctx, identity, lease.ConnectionEpoch)
	if err != nil || worker.ClusterUID != lease.ClusterNodeUid {
		return nil, connect.NewError(connect.CodeAborted, workerprotocol.ErrIdentity)
	}
	if err := g.config.ApplicationCleanup.CheckCleanup(ctx, worker, application.CleanupLease{ID: lease.CleanupId, Revision: lease.Revision}); err != nil {
		return nil, connect.NewError(connect.CodeAborted, workerprotocol.ErrIdentity)
	}
	return connect.NewResponse(&platformv1.WorkerGenerationResponse{Current: true}), nil
}
