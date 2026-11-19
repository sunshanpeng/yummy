package deployment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
)

// Prober 由控制面入口探测器实现；目标必须先从 ProbeTargets 取得，探测结果交给 ApplyProbe 落库。
// 真实实现必须以系统根证书完成 TLS 验证，不能用 worker 声称的 HTTP 成功替代。
type Prober interface {
	Probe(ctx context.Context, domain, path string) (ProbeResult, error)
}

// ProbeResult 是一次入口观察。TLSVerified 仅在请求无 TLS 错误完成（握手按默认根证书验证通过）时为 true；
// Healthy 表示入口以 2xx 响应了探测路径。ObservedAt 必须是探测完成时刻，
// ApplyProbe 会校验其不早于该操作最近一次 Kubernetes 观察（LastObservedAt）且足够新鲜。
type ProbeResult struct {
	TLSVerified bool
	Healthy     bool
	ObservedAt  time.Time
}

// ProbeOnce 取 ProbeTargets 并逐个探测、ApplyProbe。
// 单个目标失败（探测器错误或 ApplyProbe 拒绝，含 ErrStaleObservation）记录后继续，不中断整轮；
// 所有目标处理完后以 errors.Join 汇总返回，便于上层记录，但不代表整轮未执行。
func (s *Service) ProbeOnce(ctx context.Context, prober Prober, limit int) error {
	if prober == nil {
		return authorization.ErrInvalid
	}
	targets, err := s.ProbeTargets(ctx, limit)
	if err != nil {
		return err
	}
	var failures []error
	for _, target := range targets {
		result, err := prober.Probe(ctx, target.Domain, target.Path)
		if err != nil {
			failures = append(failures, fmt.Errorf("probe %s%s: %w", target.Domain, target.Path, err))
			continue
		}
		evidence := ProbeEvidence{WorkspaceID: target.WorkspaceID, ApplicationID: target.ApplicationID, OperationID: target.OperationID, Generation: target.Generation, Domain: target.Domain, Path: target.Path, PodUID: target.PodUID, TLSVerified: result.TLSVerified, Healthy: result.Healthy, ObservedAt: result.ObservedAt}
		if err := s.ApplyProbe(ctx, evidence); err != nil {
			failures = append(failures, fmt.Errorf("apply probe %s%s: %w", target.Domain, target.Path, err))
		}
	}
	return errors.Join(failures...)
}

// httpsProbeTimeout 是入口探测的整体超时：覆盖 DNS、TLS 握手、请求与响应。
const httpsProbeTimeout = 10 * time.Second

// HTTPSProber 是默认入口探测器：对 "https://"+domain+path 发起 GET，禁止跟随重定向，
// TLS 一律使用默认根证书验证（InsecureSkipVerify 恒为 false），2xx 判定健康。
// Client 可注入用于测试（例如指向本地测试服务的自定义 Transport）；注入的客户端会被复制后
// 强制禁止重定向，未设置超时时补齐为 10 秒，不会改动调用方传入的实例。
type HTTPSProber struct {
	Client *http.Client
}

func (p HTTPSProber) Probe(ctx context.Context, domain, path string) (ProbeResult, error) {
	if domain == "" || path == "" {
		return ProbeResult{}, authorization.ErrInvalid
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+path, nil)
	if err != nil {
		return ProbeResult{}, err
	}
	client := &http.Client{}
	if p.Client != nil {
		*client = *p.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout <= 0 {
		client.Timeout = httpsProbeTimeout
	}
	response, err := client.Do(request)
	if err != nil {
		// 传输或 TLS 错误不构成一次有效观察：不产生证据，由调用方记录后重试。
		return ProbeResult{}, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	// 走到这里说明请求已完成且无 TLS 错误；ObservedAt 取探测完成时刻。
	return ProbeResult{TLSVerified: true, Healthy: response.StatusCode >= 200 && response.StatusCode < 300, ObservedAt: time.Now().UTC()}, nil
}
