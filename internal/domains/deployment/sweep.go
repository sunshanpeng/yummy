package deployment

import (
	"context"
	"time"
)

// staleObservationWindow 与 ApplyProbe/GetStatus 的新鲜度窗口一致：观察超过 90 秒未更新即视为过期。
const staleObservationWindow = 90 * time.Second

// Sweep 做服务端观察收敛：把超过新鲜度窗口未更新的运行观察置为 UNKNOWN。
// 阈值与前置校验语义完全沿用 MarkStale 的实现（其自身要求阈值不短于 30 秒），
// 本方法不发明新的状态迁移；操作重投、恢复等收敛分别由 worker 网关与事件路径负责。
func (s *Service) Sweep(ctx context.Context, now time.Time) error {
	return s.MarkStale(ctx, now.UTC().Add(-staleObservationWindow))
}
