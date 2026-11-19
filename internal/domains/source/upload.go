package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
)

// Upload 仅接受上传会话既定的一份完整内容。中断不会修改会话状态，可以整包重传。
// reader 必须遵守外层传输 deadline/关闭语义：此处仅在 Read 边界检查 context，
// 不通过无法取消的 goroutine 读取阻塞 reader。HTTP 层应设置绝对读截止时间。
func (s *Service) Upload(ctx context.Context, principal authorization.Principal, workspaceID, applicationID, uploadID string, reader io.Reader) (result Upload, err error) {
	if reader == nil {
		return result, authorization.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, UploadLifetime)
	defer cancel()
	if err := s.authorize(ctx, principal, workspaceID, applicationID, authorization.SourceUpload); err != nil {
		return result, err
	}
	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return result, tx.Error
	}
	defer tx.Rollback()
	upload, err := s.lockUpload(tx, principal, workspaceID, applicationID, uploadID)
	if err != nil {
		return result, err
	}
	if upload.State != StatePending {
		return result, authorization.ErrConflict
	}
	if !upload.ExpiresAt.After(time.Now()) {
		return result, ErrExpired
	}
	id, err := security.NewID("part")
	if err != nil {
		return result, err
	}
	file, err := s.root.OpenFile(id, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, ErrUnavailable
	}
	// 路径来自已校验的随机 ID，并且只在服务配置的 root 句柄下删除。
	defer func() { _ = s.root.Remove(id) }()
	digest := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(file, digest), io.LimitReader(readWithContext{ctx, reader}, upload.ExpectedSize+1))
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return result, copyErr
	}
	if count != upload.ExpectedSize || hex.EncodeToString(digest.Sum(nil)) != upload.ExpectedSHA256 {
		return result, ErrContent
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// 进程在上次 rename 后、事务提交前崩溃时只复用相同内容，不覆盖异物。
	if _, statErr := s.root.Lstat(upload.StorageKey); statErr == nil {
		if err := s.checkContent(ctx, upload.StorageKey, upload); err != nil {
			return result, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, ErrUnavailable
	} else {
		if err := s.root.Rename(id, upload.StorageKey); err != nil {
			return result, ErrUnavailable
		}
		if err := s.syncDirectory(); err != nil {
			return result, ErrUnavailable
		}
	}
	now := time.Now().UTC()
	if !upload.ExpiresAt.After(now) {
		return result, ErrExpired
	}
	if err := tx.Model(&upload).Updates(map[string]any{"state": StateUploaded, "uploaded_at": now}).Error; err != nil {
		return result, err
	}
	if err := audit.Record(tx, principal.UserID, "source.upload.receive", "source_upload", upload.ID, &workspaceID, &applicationID); err != nil {
		return result, err
	}
	if err := s.commitAuthorized(ctx, principal, workspaceID, applicationID, tx); err != nil {
		return result, err
	}
	upload.State, upload.UploadedAt = StateUploaded, &now
	return uploadDTO(upload), nil
}

func (s *Service) checkContent(ctx context.Context, key string, upload uploadRecord) error {
	file, err := s.openStored(key)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(readWithContext{ctx, file}, upload.ExpectedSize+1))
	if err != nil {
		return err
	}
	if count != upload.ExpectedSize || hex.EncodeToString(digest.Sum(nil)) != upload.ExpectedSHA256 {
		return ErrContent
	}
	return nil
}

// Complete 仅校验归档，不解包或执行源码。落盘与目录 fsync 先于 READY 数据库提交；
// 重试使用由随机 upload ID 派生的同一快照路径，因此能恢复 rename 已成功但事务未提交。
func (s *Service) Complete(ctx context.Context, principal authorization.Principal, workspaceID, applicationID, uploadID string) (result Snapshot, err error) {
	ctx, cancel := context.WithTimeout(ctx, UploadLifetime)
	defer cancel()
	if err := s.authorize(ctx, principal, workspaceID, applicationID, authorization.SourceUpload); err != nil {
		return result, err
	}
	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return result, tx.Error
	}
	defer tx.Rollback()
	upload, err := s.lockUpload(tx, principal, workspaceID, applicationID, uploadID)
	if err != nil {
		return result, err
	}
	if upload.State == StateComplete && upload.SnapshotID != nil {
		var snapshot snapshotRecord
		if err := tx.First(&snapshot, "id = ? AND workspace_id = ? AND application_id = ?", *upload.SnapshotID, workspaceID, applicationID).Error; err != nil {
			return result, err
		}
		if err := s.commitAuthorized(ctx, principal, workspaceID, applicationID, tx); err != nil {
			return result, err
		}
		return snapshotDTO(snapshot, time.Now()), nil
	}
	if !upload.ExpiresAt.After(time.Now()) {
		return result, ErrExpired
	}
	if upload.State != StateUploaded {
		return result, ErrNotReady
	}
	snapshotID, target := snapshotIdentity(upload)
	key := upload.StorageKey
	if _, err := s.root.Lstat(target); err == nil {
		key = target
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, ErrUnavailable
	}
	file, err := s.openStored(key)
	if err != nil {
		return result, err
	}
	inspected, inspectErr := sourcearchive.Inspect(ctx, file, sourcearchive.ArchiveOptions{Dockerfile: upload.Dockerfile, ExpectedSHA256: upload.ExpectedSHA256, Limits: s.limits})
	closeErr := file.Close()
	if inspectErr != nil {
		return result, inspectErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	if inspected.CompressedBytes != upload.ExpectedSize {
		return result, ErrContent
	}
	if key != target {
		if err := s.root.Rename(key, target); err != nil {
			return result, ErrUnavailable
		}
		if err := s.syncDirectory(); err != nil {
			return result, ErrUnavailable
		}
	}
	now := time.Now().UTC()
	if !upload.ExpiresAt.After(now) {
		return result, ErrExpired
	}
	snapshot := snapshotRecord{ID: snapshotID, WorkspaceID: workspaceID, ApplicationID: applicationID, UploadID: upload.ID, CreatedBy: principal.UserID, State: StateReady, StorageKey: target, SHA256: inspected.SHA256, CompressedBytes: inspected.CompressedBytes, ExpandedBytes: inspected.ExpandedBytes, FileBytes: inspected.FileBytes, EntryCount: int64(len(inspected.Entries)), Dockerfile: inspected.Dockerfile, Entries: inspected.Entries, CreatedAt: now, ExpiresAt: now.Add(SnapshotRetention)}
	if err := tx.Create(&snapshot).Error; err != nil {
		return result, err
	}
	if err := tx.Model(&upload).Updates(map[string]any{"state": StateComplete, "snapshot_id": snapshotID, "completed_at": now}).Error; err != nil {
		return result, err
	}
	if err := audit.Record(tx, principal.UserID, "source.snapshot.complete", "source_snapshot", snapshotID, &workspaceID, &applicationID); err != nil {
		return result, err
	}
	if err := s.commitAuthorized(ctx, principal, workspaceID, applicationID, tx); err != nil {
		return result, err
	}
	return snapshotDTO(snapshot, now), nil
}

type readWithContext struct {
	ctx    context.Context
	reader io.Reader
}

func (r readWithContext) Read(bytes []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(bytes)
}
