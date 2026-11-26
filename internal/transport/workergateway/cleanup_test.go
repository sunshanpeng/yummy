package workergateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/application"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"google.golang.org/protobuf/proto"
)

type cleanupBackend struct {
	mu     sync.Mutex
	worker application.CleanupWorker
	result chan application.CleanupResult
}

func (b *cleanupBackend) ClaimCleanup(_ context.Context, worker application.CleanupWorker) (application.CleanupDispatch, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.worker.NodeID != "" {
		return application.CleanupDispatch{}, nil
	}
	b.worker = worker
	return application.CleanupDispatch{
		Lease:       application.CleanupLease{ID: "cleanup", Revision: 2},
		WorkspaceID: "space", ApplicationID: "app", SpaceSlug: "space", AppName: "app",
	}, nil
}

func (b *cleanupBackend) CheckCleanup(_ context.Context, worker application.CleanupWorker, lease application.CleanupLease) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if worker != b.worker || lease.ID != "cleanup" || lease.Revision != 2 {
		return authorization.ErrConflict
	}
	return nil
}

func (b *cleanupBackend) CompleteCleanup(ctx context.Context, worker application.CleanupWorker, lease application.CleanupLease, result application.CleanupResult) error {
	if err := b.CheckCleanup(ctx, worker, lease); err != nil {
		return err
	}
	b.result <- result
	return nil
}

func TestCleanupGatewayBindsDestructiveActionToCurrentLease(t *testing.T) {
	backend := &cleanupBackend{result: make(chan application.CleanupResult, 1)}
	env := setupTLS(t, func(config *Config) { config.ApplicationCleanup = backend })
	if err := env.db.Exec("UPDATE execution_nodes SET cluster_node_uid = 'kube-node' WHERE id = ?", env.identity.NodeID).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := platformv1connect.NewWorkerGatewayClient(env.client, env.url, connect.WithGRPC())
	stream := client.Connect(ctx)
	defer stream.CloseResponse()
	if err := stream.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_Hello{Hello: &platformv1.WorkerHello{NodeId: env.identity.NodeID, WorkerId: env.identity.WorkerID, Version: "test", ApplicationCleanup: true}}}); err != nil {
		t.Fatal(err)
	}
	welcome, err := stream.Receive()
	if err != nil || welcome.GetWelcome() == nil {
		t.Fatalf("握手：%v %v", welcome, err)
	}
	message, err := stream.Receive()
	command := message.GetApplicationCleanup()
	if err != nil || command.GetApplicationId() != "app" || command.GetDeleteVolumes() {
		t.Fatalf("派发：%v %v", message, err)
	}
	lease := command.Lease
	check, err := client.CheckApplicationCleanup(ctx, connect.NewRequest(lease))
	if err != nil || !check.Msg.GetCurrent() {
		t.Fatalf("当前租约：%v %v", check, err)
	}
	stale := proto.CloneOf(lease)
	stale.Revision--
	if _, err := client.CheckApplicationCleanup(ctx, connect.NewRequest(stale)); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("拒绝旧租约：%v", err)
	}
	if err := stream.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_ApplicationCleanupResult{ApplicationCleanupResult: &platformv1.WorkerCleanupResult{Lease: lease, Done: true, HasVolumes: true}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-backend.result:
		if !got.Done || !got.HasVolumes {
			t.Fatalf("清理结果：%+v", got)
		}
	case <-ctx.Done():
		t.Fatal("清理结果未交付")
	}
}
