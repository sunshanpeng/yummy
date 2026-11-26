package workerclient

import (
	"context"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
)

func (c *WorkerClient) startCleanup(ctx context.Context, client platformv1connect.WorkerGatewayClient, epoch int64, command *platformv1.WorkerCleanupCommand, outbound chan<- *platformv1.WorkerEnvelope) error {
	lease := command.GetLease()
	if c.config.Cleanup == nil || lease.GetCleanupId() == "" || lease.GetRevision() < 1 || lease.GetConnectionEpoch() != epoch || lease.GetClusterNodeUid() == "" || command.GetWorkspaceId() == "" || command.GetApplicationId() == "" {
		return workerprotocol.ErrProtocol
	}
	key := "application-cleanup:" + lease.CleanupId
	c.mu.Lock()
	if c.active[key] || len(c.active) >= c.config.MaxExecutions {
		c.mu.Unlock()
		return nil
	}
	c.active[key] = true
	c.mu.Unlock()
	// 清理操作可重复执行。连接中断或结果未送达时，由控制面在租约到期后重新投递。
	go func() {
		defer func() { c.mu.Lock(); delete(c.active, key); c.mu.Unlock() }()
		work, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		check := func(checkCtx context.Context) error {
			response, err := client.CheckApplicationCleanup(checkCtx, connect.NewRequest(lease))
			if err != nil {
				return err
			}
			if !response.Msg.GetCurrent() {
				return workerprotocol.ErrIdentity
			}
			return nil
		}
		if err := check(work); err != nil {
			return
		}
		done, volumes, code := c.config.Cleanup(work, command, check)
		if work.Err() != nil {
			return
		}
		result := &platformv1.WorkerCleanupResult{Lease: lease, Done: done, HasVolumes: volumes, ErrorCode: code}
		select {
		case <-ctx.Done():
		case outbound <- &platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_ApplicationCleanupResult{ApplicationCleanupResult: result}}:
		}
	}()
	return nil
}
