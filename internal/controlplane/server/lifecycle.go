package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/deployment"
	"go.uber.org/zap"
)

// 后台循环节奏：产物验证与入口探测是网络型操作，不能沿用 1 秒级的数据库扫描节奏，
// 否则仓库或入口暂不可用时会以每秒多次的频率重试外部依赖。
const (
	reconcileInterval = 5 * time.Second
	verifyInterval    = 30 * time.Second
	sweepInterval     = 30 * time.Second
	publishInterval   = 10 * time.Second
	probeInterval     = 15 * time.Second
	cleanupInterval   = 10 * time.Minute
)

// lifecycle 拥有控制面进程内请求外后台循环的生命周期。
// 这些循环只做数据库收敛与外部验证，任务派发由 worker 网关在已认证连接内完成，
// 不在这里重复调度，避免同一次执行被双份投递。
type lifecycle struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed atomic.Bool
}

func (s *Server) startLifecycle() *lifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	l := &lifecycle{cancel: cancel}
	run := func(interval time.Duration, fn func(context.Context)) {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					fn(ctx)
				}
			}
		}()
	}
	report := func(scope string) func(error) {
		return func(err error) {
			if err != nil {
				s.Log.Error("控制面后台循环失败", zap.String("scope", scope), zap.Error(err))
			}
		}
	}
	// 构建断线/超时恢复与资格失效收敛；LOST 保留占用，不改派。
	run(reconcileInterval, func(ctx context.Context) {
		report("build.reconcile")(s.Builds.Reconcile(ctx, time.Now().UTC()))
	})
	// 产物独立验证：不以 worker 成功结论为事实，网络暂不可用时保留待验证状态重试。
	run(verifyInterval, func(ctx context.Context) {
		report("build.verify")(s.Builds.VerifyPending(ctx, s.artifactVerifier, 16))
	})
	// 自动发布：构建成功且产物 VERIFIED 后按冻结配置受理发布；幂等且单个失败不中断整轮。
	run(publishInterval, func(ctx context.Context) {
		report("deployment.publish")(func() error { _, err := s.Deployments.AutoPublish(ctx, 16); return err }())
	})
	// 入口探测：登记域名以默认根证书做 HTTPS 健康取证，证据齐备后才宣告发布成功。
	run(probeInterval, func(ctx context.Context) {
		report("deployment.probe")(s.Deployments.ProbeOnce(ctx, deployment.HTTPSProber{}, 16))
	})
	// 观察收敛：过期运行观察置为 UNKNOWN，不伪造停止或成功。
	run(sweepInterval, func(ctx context.Context) {
		report("deployment.sweep")(s.Deployments.Sweep(ctx, time.Now()))
	})
	// 源码清理：过期上传与无引用快照回收（在途构建的文件绝不删除）。
	// Cleanup 自身有界且逐条守卫，循环按较慢节奏推进，失败仅记录下次重试。
	run(cleanupInterval, func(ctx context.Context) {
		report("source.cleanup")(func() error {
			_, err := s.Sources.Cleanup(ctx, time.Now().UTC(), 100)
			return err
		}())
	})
	// 授权请求清理：authorize 允许匿名发起，过期 oauth_requests 若不清理会无界增长。
	if s.OAuth != nil {
		run(cleanupInterval, func(ctx context.Context) {
			report("oauth.cleanup")(func() error {
				_, err := s.OAuth.CleanupExpiredRequests(ctx, 500)
				return err
			}())
		})
	}
	return l
}

// Close 先停后台循环再由调用方关闭数据库，保证循环不会使用已关闭的连接。
func (l *lifecycle) Close() {
	if l == nil || !l.closed.CompareAndSwap(false, true) {
		return
	}
	l.cancel()
	l.wg.Wait()
}
