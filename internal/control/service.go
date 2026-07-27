package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/ports"
)

var ErrInvalidInput = errors.New("输入不能为空")

type Service struct {
	repo     ports.CommandRepository
	presence ports.PresenceStore
	bus      ports.CommandBus
	newID    func() string
}

func NewService(
	repo ports.CommandRepository,
	presence ports.PresenceStore,
	bus ports.CommandBus,
	newID func() string,
) *Service {
	return &Service{
		repo:     repo,
		presence: presence,
		bus:      bus,
		newID:    newID,
	}
}

func (s *Service) CreateCommand(
	ctx context.Context,
	workerID string,
	input string,
) (domain.Command, error) {
	workerID = strings.TrimSpace(workerID)
	input = strings.TrimSpace(input)
	if workerID == "" || input == "" {
		return domain.Command{}, ErrInvalidInput
	}

	online, err := s.presence.IsOnline(ctx, workerID)
	if err != nil {
		return domain.Command{}, fmt.Errorf("检查 worker 在线状态: %w", err)
	}

	command := domain.NewCommand(s.newID(), workerID, input)
	if err := s.repo.Create(ctx, command); err != nil {
		return domain.Command{}, fmt.Errorf("创建指令: %w", err)
	}

	if !online {
		if err := s.repo.Transition(
			ctx,
			command.ID,
			domain.StatusCreated,
			domain.StatusDispatchFailed,
			"",
			"worker 不在线",
		); err != nil {
			return domain.Command{}, fmt.Errorf("记录离线下发失败: %w", err)
		}
		return s.repo.Get(ctx, command.ID)
	}

	if err := s.repo.Transition(
		ctx,
		command.ID,
		domain.StatusCreated,
		domain.StatusDispatched,
		"",
		"",
	); err != nil {
		return domain.Command{}, fmt.Errorf("标记指令已下发: %w", err)
	}

	if err := s.bus.Dispatch(ctx, command); err != nil {
		if transitionErr := s.repo.Transition(
			ctx,
			command.ID,
			domain.StatusDispatched,
			domain.StatusDispatchFailed,
			"",
			err.Error(),
		); transitionErr != nil {
			return domain.Command{}, fmt.Errorf(
				"下发失败 %v，记录失败状态: %w",
				err,
				transitionErr,
			)
		}
	}

	return s.repo.Get(ctx, command.ID)
}

func (s *Service) GetCommand(ctx context.Context, id string) (domain.Command, error) {
	return s.repo.Get(ctx, id)
}

func (s *Service) ListWorkers(ctx context.Context) ([]domain.WorkerPresence, error) {
	return s.presence.ListOnline(ctx)
}

func (s *Service) HandleEvent(ctx context.Context, event domain.Event) error {
	switch event.Type {
	case domain.EventAccepted:
		return s.repo.Transition(
			ctx,
			event.CommandID,
			domain.StatusDispatched,
			domain.StatusRunning,
			"",
			"",
		)
	case domain.EventProgress:
		return nil
	case domain.EventSucceeded:
		return s.repo.Transition(
			ctx,
			event.CommandID,
			domain.StatusRunning,
			domain.StatusSucceeded,
			event.Output,
			"",
		)
	case domain.EventFailed:
		return s.repo.Transition(
			ctx,
			event.CommandID,
			domain.StatusRunning,
			domain.StatusFailed,
			"",
			event.Error,
		)
	default:
		return fmt.Errorf("未知事件类型 %q", event.Type)
	}
}
