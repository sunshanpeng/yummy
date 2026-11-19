package build

import (
	"context"
	"errors"
	"fmt"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Service) Get(ctx context.Context, p authorization.Principal, ws, app, id string) (View, error) {
	var out View
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.BuildRead); err != nil {
			return err
		}
		var b buildRecord
		if err := tx.First(&b, "workspace_id = ? AND application_id = ? AND id = ?", ws, app, id).Error; err != nil {
			return missing(err)
		}
		var err error
		out, err = s.view(tx, b)
		return err
	})
	return out, err
}
func (s *Service) List(ctx context.Context, p authorization.Principal, ws, app string, limit int, cursor string) (Page, error) {
	out := Page{Items: []View{}}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		return out, authorization.ErrInvalid
	}
	limit, afterID, err := store.ParsePage(limit, cursor, ws+":"+app+":builds")
	if err != nil {
		return out, err
	}
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.BuildRead); err != nil {
			return err
		}
		q := tx.Where("workspace_id = ? AND application_id = ?", ws, app)
		if cursor != "" {
			var after buildRecord
			if err := tx.Where("workspace_id = ? AND application_id = ? AND id = ?", ws, app, afterID).Take(&after).Error; err != nil {
				return store.ErrInvalidPage
			}
			q = q.Where("(created_at, id) < (?, ?)", after.CreatedAt, after.ID)
		}
		var records []buildRecord
		if err := q.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&records).Error; err != nil {
			return err
		}
		if len(records) > limit {
			records = records[:limit]
			last := records[len(records)-1]
			out.NextCursor = store.NextCursor(ws+":"+app+":builds", last.ID)
		}
		for _, b := range records {
			v, err := s.view(tx, b)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, v)
		}
		return nil
	})
	return out, err
}
func (s *Service) ReadLogs(ctx context.Context, p authorization.Principal, ws, app, id string, after int64, limit int) (Logs, error) {
	out := Logs{Items: []LogChunk{}, NextSeq: after}
	if after < 0 {
		return out, authorization.ErrInvalid
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		return out, authorization.ErrInvalid
	}
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.LogsRead); err != nil {
			return err
		}
		var b buildRecord
		if err := tx.First(&b, "workspace_id = ? AND application_id = ? AND id = ?", ws, app, id).Error; err != nil {
			return missing(err)
		}
		var records []logRecord
		if err := tx.Where("build_id = ? AND seq > ?", b.ID, after).Order("seq ASC").Limit(limit).Find(&records).Error; err != nil {
			return err
		}
		for _, v := range records {
			out.Items = append(out.Items, LogChunk{Seq: v.Seq, Content: string(v.Content), Truncated: v.Truncated, CreatedAt: v.CreatedAt})
			out.NextSeq = v.Seq
		}
		out.Truncated = b.LogsTruncated
		var remaining int64
		if err := tx.Model(&logRecord{}).Where("build_id = ? AND seq > ?", b.ID, out.NextSeq).Count(&remaining).Error; err != nil {
			return err
		}
		out.Complete = b.LogsComplete && remaining == 0
		return nil
	})
	return out, err
}
func (s *Service) Cancel(ctx context.Context, p authorization.Principal, ws, app, id string) (View, error) {
	var out View
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.BuildWrite); err != nil {
			return err
		}
		var b buildRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&b, "workspace_id = ? AND application_id = ? AND id = ?", ws, app, id).Error; err != nil {
			return missing(err)
		}
		if !terminal(b.State) {
			if err := requestStop(tx, &b, "CANCEL"); err != nil {
				return err
			}
			if err := audit.Record(tx, current.UserID, "build.cancel", "build", id, &ws, &app); err != nil {
				return err
			}
		}
		var err error
		out, err = s.view(tx, b)
		return err
	})
	return out, err
}

// Retry 产生新 build 和标签；新请求仍按当前策略/配置 revision 严格受理，不修改原记录。
func (s *Service) Retry(ctx context.Context, p authorization.Principal, ws, app, id string, in CreateInput) (View, error) {
	var previous buildRecord
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.BuildWrite); err != nil {
			return err
		}
		if err := tx.First(&previous, "workspace_id = ? AND application_id = ? AND id = ?", ws, app, id).Error; err != nil {
			return missing(err)
		}
		if !terminal(previous.State) {
			return fmt.Errorf("%w: 原构建尚未结束", authorization.ErrConflict)
		}
		if previous.State == Lost {
			var a attemptRecord
			err := tx.Where("build_id = ?", id).Take(&a).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if a.Occupied {
				return fmt.Errorf("%w: 失联任务尚未确认停止", authorization.ErrConflict)
			}
		}
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return s.create(ctx, p, ws, app, in, &id)
}
