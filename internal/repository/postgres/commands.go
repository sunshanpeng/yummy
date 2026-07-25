package postgres

import (
	"context"
	"errors"
	"fmt"

	"example.com/multitopology-demo/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Create(ctx context.Context, command domain.Command) error {
	_, err := r.pool.Exec(
		ctx,
		`INSERT INTO commands (
			id, worker_id, input, output, error, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		command.ID,
		command.WorkerID,
		command.Input,
		command.Output,
		command.Error,
		command.Status,
		command.CreatedAt,
		command.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("插入指令: %w", err)
	}
	return nil
}

func (r *Repository) Get(ctx context.Context, id string) (domain.Command, error) {
	var command domain.Command
	err := r.pool.QueryRow(
		ctx,
		`SELECT id, worker_id, input, output, error, status, created_at, updated_at
		 FROM commands
		 WHERE id = $1`,
		id,
	).Scan(
		&command.ID,
		&command.WorkerID,
		&command.Input,
		&command.Output,
		&command.Error,
		&command.Status,
		&command.CreatedAt,
		&command.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Command{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Command{}, fmt.Errorf("查询指令: %w", err)
	}
	return command, nil
}

func (r *Repository) Transition(
	ctx context.Context,
	id string,
	from domain.CommandStatus,
	to domain.CommandStatus,
	output string,
	errorText string,
) error {
	if !domain.CanTransition(from, to) {
		return domain.ErrStateConflict
	}
	tag, err := r.pool.Exec(
		ctx,
		`UPDATE commands
		 SET status = $3, output = $4, error = $5, updated_at = NOW()
		 WHERE id = $1 AND status = $2`,
		id,
		from,
		to,
		output,
		errorText,
	)
	if err != nil {
		return fmt.Errorf("更新指令状态: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrStateConflict
	}
	return nil
}
