package bootstrap

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sunshanpeng/yummy/internal/domain"
	transport "github.com/sunshanpeng/yummy/internal/transport/connectrpc"
	"github.com/sunshanpeng/yummy/internal/worker"
)

func runWorker(ctx context.Context, config Config) error {
	if err := validateWorkerConfig(config); err != nil {
		return err
	}
	tlsConfig, err := transport.LoadClientTLSConfig(
		config.WorkerCertFile(),
		config.WorkerKeyFile(),
		config.CACertFile(),
		config.TLSServerName,
	)
	if err != nil {
		return err
	}
	httpTransport := &http.Transport{
		TLSClientConfig:   tlsConfig,
		ForceAttemptHTTP2: true,
	}
	defer httpTransport.CloseIdleConnections()

	client := transport.NewWorkerClient(transport.WorkerClientConfig{
		HTTPClient:        &http.Client{Transport: httpTransport},
		ControlURL:        config.ControlURL,
		WorkerID:          config.WorkerID,
		Version:           config.Version,
		HeartbeatInterval: config.HeartbeatInterval,
	}, worker.NewRuntime())
	return client.Run(ctx)
}

// validateWorkerConfig 在启动时按控制面的规则检查 worker 身份和版本号，
// 配置不合规时直接退出，而不是连接后被拒绝并无限重连。
func validateWorkerConfig(config Config) error {
	if !domain.ValidWorkerID(config.WorkerID) {
		return fmt.Errorf(
			"worker ID %q 无效：只能包含字母、数字、点、下划线和连字符，且不超过 %d 个字符",
			config.WorkerID,
			domain.MaxWorkerIDLength,
		)
	}
	if len(config.Version) > domain.MaxWorkerVersionLength {
		return fmt.Errorf("worker 版本号不能超过 %d 字节", domain.MaxWorkerVersionLength)
	}
	return nil
}

func RunWorker(ctx context.Context, config Config) error {
	return runWorker(ctx, config)
}
