package source

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var storageKeyPattern = regexp.MustCompile(`^(upload|snapshot)_[0-9a-f]{32}\.tar\.gz$`)
var uploadIDPattern = regexp.MustCompile(`^upl_[0-9a-f]{32}$`)

type Service struct {
	db     *gorm.DB
	access *authorization.Access
	root   *os.Root
	limits sourcearchive.Limits
}

func NewService(db *gorm.DB, access *authorization.Access, config Config) (*Service, error) {
	if db == nil || access == nil || config.Root == "" {
		return nil, authorization.ErrInvalid
	}
	limits, err := effectiveLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(config.Root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, authorization.ErrInvalid
	}
	root, err := os.OpenRoot(config.Root)
	if err != nil {
		return nil, err
	}
	// 构造时验证卷可写和 fsync；不能把“目录存在”当作可持久化资格。
	id, err := security.NewID("part")
	if err != nil {
		root.Close()
		return nil, err
	}
	probe, err := root.OpenFile(id, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		root.Close()
		return nil, err
	}
	err = probe.Sync()
	closeErr := probe.Close()
	removeErr := root.Remove(id)
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = removeErr
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	service := &Service{db: db, access: access, root: root, limits: limits}
	if err := service.syncDirectory(); err != nil {
		root.Close()
		return nil, err
	}
	return service, nil
}

// Close 应在所有在途调用完成后执行，不接管数据库或 Access 的生命周期。
func (s *Service) Close() error { return s.root.Close() }

func effectiveLimits(configured sourcearchive.Limits) (sourcearchive.Limits, error) {
	defaults := sourcearchive.DefaultLimits()
	if configured.MaxCompressedBytes < 0 || configured.MaxExpandedBytes < 0 || configured.MaxEntries < 0 {
		return configured, sourcearchive.ErrLimit
	}
	if configured.MaxCompressedBytes == 0 {
		configured.MaxCompressedBytes = defaults.MaxCompressedBytes
	}
	if configured.MaxExpandedBytes == 0 {
		configured.MaxExpandedBytes = defaults.MaxExpandedBytes
	}
	if configured.MaxEntries == 0 {
		configured.MaxEntries = defaults.MaxEntries
	}
	if configured.MaxCompressedBytes > defaults.MaxCompressedBytes || configured.MaxExpandedBytes > defaults.MaxExpandedBytes || configured.MaxEntries > defaults.MaxEntries {
		return configured, sourcearchive.ErrLimit
	}
	return configured, nil
}

func (s *Service) CreateUpload(ctx context.Context, principal authorization.Principal, workspaceID, applicationID string, input CreateInput) (result Upload, err error) {
	digest, decodeErr := hex.DecodeString(input.SHA256)
	if decodeErr != nil || len(digest) != 32 || input.Size <= 0 || input.Size > s.limits.MaxCompressedBytes {
		return result, authorization.ErrInvalid
	}
	dockerfile := input.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if !validDockerfile(dockerfile) {
		return result, sourcearchive.ErrDockerfile
	}
	err = s.access.Do(ctx, principal, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, workspaceID, applicationID, authorization.SourceUpload); err != nil {
			return err
		}
		id, err := security.NewID("upl")
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		upload := uploadRecord{ID: id, WorkspaceID: workspaceID, ApplicationID: applicationID, CreatedBy: current.UserID, ExpectedSize: input.Size, ExpectedSHA256: strings.ToLower(input.SHA256), Dockerfile: dockerfile, State: StatePending, StorageKey: "upload_" + strings.TrimPrefix(id, "upl_") + ".tar.gz", CreatedAt: now, ExpiresAt: now.Add(UploadLifetime)}
		if err := tx.Create(&upload).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, current.UserID, "source.upload.create", "source_upload", id, &workspaceID, &applicationID); err != nil {
			return err
		}
		result = uploadDTO(upload)
		return nil
	})
	return result, err
}

func validDockerfile(name string) bool {
	if len(name) > 4096 || !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00") || path.IsAbs(name) {
		return false
	}
	return true
}

func (s *Service) authorize(ctx context.Context, principal authorization.Principal, workspaceID, applicationID string, action authorization.Action) error {
	return s.access.Do(ctx, principal, false, func(tx *gorm.DB, current authorization.Principal) error {
		_, _, err := s.access.Application(tx, current, workspaceID, applicationID, action)
		return err
	})
}

func (s *Service) lockUpload(tx *gorm.DB, principal authorization.Principal, workspaceID, applicationID, uploadID string) (uploadRecord, error) {
	var value uploadRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&value, "id = ? AND workspace_id = ? AND application_id = ? AND created_by = ?", uploadID, workspaceID, applicationID, principal.UserID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return value, authorization.ErrNotFound
	}
	if err != nil {
		return value, err
	}
	if !storageKeyPattern.MatchString(value.StorageKey) || !uploadIDPattern.MatchString(value.ID) {
		return value, ErrUnavailable
	}
	return value, nil
}

// commitAuthorized 只在提交时短暂持权限读锁，不让长上传阻塞停用、撤权。
// 文件已先 fsync/rename；提交结果不确定时保留同名文件，后续重试按摘要核实并恢复。
func (s *Service) commitAuthorized(ctx context.Context, principal authorization.Principal, workspaceID, applicationID string, tx *gorm.DB) error {
	return s.access.DoInTx(ctx, principal, tx, func(authTx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(authTx, current, workspaceID, applicationID, authorization.SourceUpload); err != nil {
			return err
		}
		return tx.Commit().Error
	})
}

func (s *Service) syncDirectory() error {
	directory, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func snapshotIdentity(upload uploadRecord) (string, string) {
	suffix := strings.TrimPrefix(upload.ID, "upl_")
	return "src_" + suffix, "snapshot_" + suffix + ".tar.gz"
}
