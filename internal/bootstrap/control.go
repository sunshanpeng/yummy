package bootstrap

import (
	"context"
	"io/fs"
	"net/http"
	"time"

	"example.com/multitopology-demo/internal/control"
	"example.com/multitopology-demo/internal/storage"
	transport "example.com/multitopology-demo/internal/transport/connectrpc"
)

func RunControl(ctx context.Context, config Config, assets fs.FS) error {
	return runControl(ctx, config, func(service *control.Service) http.Handler {
		return newPublicHandler(service, assets)
	})
}

func runControl(ctx context.Context, config Config, publicHandlerFactory func(*control.Service) http.Handler) error {
	dependencies, err := storage.Open(ctx, storage.Config{
		DatabaseURL: config.DatabaseURL,
		RedisAddr:   config.RedisAddr,
		PresenceTTL: config.PresenceTTL,
	})
	if err != nil {
		return err
	}
	defer dependencies.Close()

	registry := transport.NewRegistry(5 * time.Second)
	service := control.NewService(dependencies.Commands, dependencies.Presence, registry, newID)
	agentPath, agentHandler := transport.NewAgentHandler(
		registry,
		dependencies.Presence,
		service,
		newID(),
	)
	agentMux := http.NewServeMux()
	agentMux.Handle(agentPath, transport.PeerIdentityMiddleware(agentHandler))

	tlsConfig, err := transport.LoadServerTLSConfig(
		config.ServerCertFile(),
		config.ServerKeyFile(),
		config.CACertFile(),
	)
	if err != nil {
		return err
	}
	publicServer := &http.Server{
		Addr:              config.PublicAddr,
		Handler:           publicHandlerFactory(service),
		ReadHeaderTimeout: 5 * time.Second,
	}
	agentServer := &http.Server{
		Addr:              config.AgentAddr,
		Handler:           agentMux,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return runServers(
		ctx,
		serverProcess{server: publicServer, serve: publicServer.ListenAndServe},
		serverProcess{server: agentServer, serve: func() error {
			return agentServer.ListenAndServeTLS("", "")
		}},
	)
}
