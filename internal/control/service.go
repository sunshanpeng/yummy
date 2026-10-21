package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sunshanpeng/yummy/internal/domain"
	"github.com/sunshanpeng/yummy/internal/ports"
)

// MaxInputBytes 限制单条指令文本的大小，避免超大请求占用内存和数据库空间。
const MaxInputBytes = 16 << 10

var ErrInvalidInput = errors.New("输入无效")

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
	if err := validateCommandInput(workerID, input); err != nil {
		return domain.Command{}, err
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

func validateCommandInput(workerID, input string) error {
	if workerID == "" || input == "" {
		return fmt.Errorf("%w: worker ID 和任务文本不能为空", ErrInvalidInput)
	}
	if !domain.ValidWorkerID(workerID) {
		return fmt.Errorf(
			"%w: worker ID 只能包含字母、数字、点、下划线和连字符，且不超过 %d 个字符",
			ErrInvalidInput,
			domain.MaxWorkerIDLength,
		)
	}
	if len(input) > MaxInputBytes {
		return fmt.Errorf("%w: 任务文本不能超过 %d 字节", ErrInvalidInput, MaxInputBytes)
	}
	return nil
}

func (s *Service) GetCommand(ctx context.Context, id string) (domain.Command, error) {
	return s.repo.Get(ctx, id)
}

func (s *Service) ListWorkers(ctx context.Context) ([]domain.WorkerPresence, error) {
	return s.presence.ListOnline(ctx)
}

func (s *Service) HandleEvent(ctx context.Context, event domain.Event) error {
	if event.Type == domain.EventProgress {
		// 进度事件不改变指令状态，不需要查询数据库，也就不涉及归属校验。
		return nil
	}

	// 只接受指令目标 worker 上报的事件，防止持有有效证书的 worker 篡改其他 worker 的指令。
	// 指令的 worker_id 创建后不会变化，因此先查询再转换状态不存在竞态。
	command, err := s.repo.Get(ctx, event.CommandID)
	if err != nil {
		return err
	}
	if event.WorkerID == "" || command.WorkerID != event.WorkerID {
		// 与指令不存在返回相同错误，不向其他 worker 暴露指令是否存在。
		return domain.ErrNotFound
	}

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
