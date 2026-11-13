package source

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"gorm.io/gorm"
)

func (s *Service) GetSnapshot(ctx context.Context, principal authorization.Principal, workspaceID, applicationID, snapshotID string) (result Snapshot, err error) {
	err = s.access.Do(ctx, principal, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, workspaceID, applicationID, authorization.SourceRead); err != nil {
			return err
		}
		var snapshot snapshotRecord
		if err := tx.First(&snapshot, "id = ? AND workspace_id = ? AND application_id = ?", snapshotID, workspaceID, applicationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		result = snapshotDTO(snapshot, time.Now())
		return nil
	})
	return result, err
}

// OpenSnapshot 返回的读取句柄由调用者关闭；授权在打开时复核，传输层仍负责截止时间。
// 过期元数据可查询，过期原始内容不可读取。物理回收由系统级 Cleanup 执行：确认无
// 在途构建引用后才删除文件；保留文件不代表延长源快照的可用有效期。
func (s *Service) OpenSnapshot(ctx context.Context, principal authorization.Principal, workspaceID, applicationID, snapshotID string) (reader io.ReadCloser, result Snapshot, err error) {
	err = s.access.Do(ctx, principal, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, workspaceID, applicationID, authorization.SourceRead); err != nil {
			return err
		}
		var snapshot snapshotRecord
		if err := tx.First(&snapshot, "id = ? AND workspace_id = ? AND application_id = ?", snapshotID, workspaceID, applicationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrNotFound
			}
			return err
		}
		if snapshot.State != StateReady {
			return ErrNotReady
		}
		if !snapshot.ExpiresAt.After(time.Now()) {
			return ErrExpired
		}
		file, err := s.openStored(snapshot.StorageKey)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil || info.Size() != snapshot.CompressedBytes {
			file.Close()
			return ErrUnavailable
		}
		reader, result = file, snapshotDTO(snapshot, time.Now())
		return nil
	})
	if err != nil && reader != nil {
		reader.Close()
		reader = nil
	}
	return reader, result, err
}

func (s *Service) openStored(key string) (*os.File, error) {
	if !storageKeyPattern.MatchString(key) {
		return nil, ErrUnavailable
	}
	file, err := openRegular(s.root, key)
	if err != nil {
		return nil, ErrUnavailable
	}
	return file, nil
}
