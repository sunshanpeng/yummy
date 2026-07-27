package bootstrap

import (
	"context"
	"net/http"

	transport "github.com/sunshanpeng/yummy/internal/transport/connectrpc"
	"github.com/sunshanpeng/yummy/internal/worker"
)

func runWorker(ctx context.Context, config Config) error {
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

func RunWorker(ctx context.Context, config Config) error {
	return runWorker(ctx, config)
}
