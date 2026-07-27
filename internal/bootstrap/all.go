package bootstrap

import (
	"context"
	"io/fs"
	"net/http"
	"time"

	"github.com/sunshanpeng/yummy/internal/commandbus"
	"github.com/sunshanpeng/yummy/internal/control"
	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/ports"
	"github.com/sunshanpeng/yummy/internal/storage"
	"github.com/sunshanpeng/yummy/internal/worker"
)

const localWorkerID = "local-worker"

func RunAll(ctx context.Context, config Config, assets fs.FS) error {
	return runAll(ctx, config, func(service *control.Service) http.Handler {
		return newPublicHandler(service, assets)
	})
}

func runAll(ctx context.Context, config Config, publicHandlerFactory func(*control.Service) http.Handler) error {
	dependencies, err := storage.Open(ctx, storage.Config{
		DatabaseURL: config.DatabaseURL,
		RedisAddr:   config.RedisAddr,
		PresenceTTL: config.PresenceTTL,
	})
	if err != nil {
		return err
	}
	defer dependencies.Close()

	runtime := worker.NewRuntime()
	var service *control.Service
	events := ports.EventHandlerFunc(func(ctx context.Context, event domain.Event) error {
		return service.HandleEvent(ctx, event)
	})
	localBus := commandbus.NewLocal(runtime, events)
	service = control.NewService(dependencies.Commands, dependencies.Presence, localBus, newID)

	presence := domain.WorkerPresence{
		WorkerID:   localWorkerID,
		InstanceID: newID(),
		Version:    config.Version,
		LastSeen:   time.Now().UTC(),
	}
	if err := dependencies.Presence.Upsert(ctx, presence); err != nil {
		return err
	}
	go refreshLocalPresence(ctx, dependencies.Presence, presence, config.HeartbeatInterval)

	publicServer := &http.Server{
		Addr:              config.PublicAddr,
		Handler:           publicHandlerFactory(service),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return runServers(
		ctx,
		serverProcess{server: publicServer, serve: publicServer.ListenAndServe},
	)
}

func refreshLocalPresence(
	ctx context.Context,
	store ports.PresenceStore,
	presence domain.WorkerPresence,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			presence.LastSeen = now.UTC()
			_ = store.Upsert(ctx, presence)
		}
	}
}
