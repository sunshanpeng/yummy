// cleanup.go 实现过期上传会话与源码快照的系统级回收。
// 系统维护没有用户主体：不经过 authorization 授权，也不写 audit.Record
// （audit_events.actor_id 外键要求真实用户，不能伪造 "system" 操作者），
// 结果经返回值交由调用方记录与监控。
package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 终态构建列表照抄 internal/domains/build 的构建状态约束（编译期常量拼接，
// 与 build 包状态机同步演进）。仅当引用快照的构建全部处于终态时才允许回收
// 快照文件；在途构建经 source_build_grants 直接读取该文件，绝不删除。
const terminalBuildStatesSQL = "'" + build.Succeeded + "','" + build.Failed + "','" + build.Canceled + "','" + build.TimedOut + "','" + build.Lost + "'"

// cleanupTarget 是待回收条目的最小事实集。
type cleanupTarget struct {
	ID            string
	WorkspaceID   string
	ApplicationID string
	StorageKey    string
}

// Cleanup 回收过期数据，每类最多处理 limit 行（<=0 返回 ErrInvalid），now 由
// 调用方传入；removed 是本轮成功回收的条目数（快照置 EXPIRED 保留行也计为回收）。
//
// 决策表：
//   - 上传 PENDING/UPLOADED 且过期：删 storage_key 文件，再删行；
//   - 上传 COMPLETE：Complete 已把 upload_<id>.tar.gz 改名为快照文件，行自身无文件，
//     且与快照行互相 ON DELETE RESTRICT（snapshot_id / upload_id），只能随快照成对
//     回收，因此不单独选取；
//   - 快照 READY 且过期、无任何 builds 引用：删文件，同事务解除 COMPLETE 上传行的
//     引用（置 EXPIRED、清 snapshot_id/completed_at 以满足行 CHECK）、删快照行、
//     删上传行，成对彻底回收；
//   - 快照 READY 且过期、仅被终态构建引用：删文件，行置 state=EXPIRED 保留
//     （行仍被 builds 与上传行 RESTRICT，保留行即满足全部外键）；
//   - 快照 READY 且过期、被非终态构建引用：本轮完全跳过；
//   - 收包临时 part 文件寿命超限（进程崩溃遗留，正常路径由 defer 删除）：按修改
//     时间有界删除。
//
// 每条先删文件再改行：文件不存在视为已删，行删除失败时下一轮重试仍能先处理文件；
// 文件删除失败则不触碰行，保持整轮可重试。单条失败记录后继续（errors.Join 汇总），
// 不中断整轮。孤儿快照文件（无对应行）不在本轮扫描范围，留待运维工具处理。
func (s *Service) Cleanup(ctx context.Context, now time.Time, limit int) (removed int, err error) {
	if limit <= 0 {
		return 0, authorization.ErrInvalid
	}
	var failures []error
	// 先快照后上传：无引用快照会在同一轮连带回收其 COMPLETE 上传行，随后
	// 上传轮次只面对独立的 PENDING/UPLOADED 会话。
	targets, selectErr := s.selectExpiredSnapshots(ctx, now, limit)
	if selectErr != nil {
		failures = append(failures, fmt.Errorf("选取过期快照: %w", selectErr))
	} else {
		for _, target := range targets {
			if ctx.Err() != nil {
				failures = append(failures, ctx.Err())
				break
			}
			count, targetErr := s.cleanupSnapshot(ctx, target, now)
			removed += count
			if targetErr != nil {
				failures = append(failures, fmt.Errorf("回收快照 %s: %w", target.ID, targetErr))
			}
		}
	}
	targets, selectErr = s.selectExpiredUploads(ctx, now, limit)
	if selectErr != nil {
		failures = append(failures, fmt.Errorf("选取过期上传: %w", selectErr))
	} else {
		for _, target := range targets {
			if ctx.Err() != nil {
				failures = append(failures, ctx.Err())
				break
			}
			count, targetErr := s.cleanupUpload(ctx, target, now)
			removed += count
			if targetErr != nil {
				failures = append(failures, fmt.Errorf("回收上传 %s: %w", target.ID, targetErr))
			}
		}
	}
	// 收包临时文件的兜底回收：收包已移到数据库事务外，进程崩溃遗留的半包
	// 不再有任何行可循，只能按文件名模式与寿命清理。
	count, sweepErr := s.sweepOrphanParts(ctx, now, limit)
	removed += count
	if sweepErr != nil {
		failures = append(failures, fmt.Errorf("回收遗留收包文件: %w", sweepErr))
	}
	return removed, errors.Join(failures...)
}

// selectExpiredSnapshots 选取可处理的过期 READY 快照；非终态构建引用的条目在
// 选取阶段即排除，不占用 limit 配额。
func (s *Service) selectExpiredSnapshots(ctx context.Context, now time.Time, limit int) ([]cleanupTarget, error) {
	var targets []cleanupTarget
	err := s.db.WithContext(ctx).Table("source_snapshots").
		Select("id, workspace_id, application_id, storage_key").
		Where("state = ? AND expires_at <= ?", StateReady, now).
		Where("NOT EXISTS (SELECT 1 FROM builds b WHERE b.workspace_id = source_snapshots.workspace_id AND b.application_id = source_snapshots.application_id AND b.source_snapshot_id = source_snapshots.id AND b.state NOT IN (" + terminalBuildStatesSQL + "))").
		Order("expires_at, id").
		Limit(limit).
		Scan(&targets).Error
	return targets, err
}

// selectExpiredUploads 选取独立的过期上传会话；COMPLETE 行随快照回收，不在此列。
func (s *Service) selectExpiredUploads(ctx context.Context, now time.Time, limit int) ([]cleanupTarget, error) {
	var targets []cleanupTarget
	err := s.db.WithContext(ctx).Table("source_uploads").
		Select("id, workspace_id, application_id, storage_key").
		Where("state IN (?, ?) AND expires_at <= ?", StatePending, StateUploaded, now).
		Order("expires_at, id").
		Limit(limit).
		Scan(&targets).Error
	return targets, err
}

// removeStored 删除卷内对象并 fsync 目录，使“文件已删”先于行变更持久化；
// 对象不存在视为已删除。删除一律经 s.root，路径必须通过存储键模式校验。
func (s *Service) removeStored(key string) error {
	if !storageKeyPattern.MatchString(key) {
		return ErrUnavailable
	}
	if err := s.root.Remove(key); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.syncDirectory()
}

// cleanupSnapshot 处理单个过期快照。事务内持行锁重读并复核引用事实，防止
// 选取与执行之间引用关系变化；单条失败由调用方记录后继续。
func (s *Service) cleanupSnapshot(ctx context.Context, target cleanupTarget, now time.Time) (int, error) {
	if err := s.removeStored(target.StorageKey); err != nil {
		return 0, err
	}
	processed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var snapshot snapshotRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&snapshot, "id = ?", target.ID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // 并发清理已回收
		}
		if err != nil {
			return err
		}
		if snapshot.State != StateReady || snapshot.ExpiresAt.After(now) {
			return nil // 状态或有效期已变化，跳过本轮
		}
		var active, referenced int64
		reference := "workspace_id = ? AND application_id = ? AND source_snapshot_id = ?"
		referenceArgs := []any{snapshot.WorkspaceID, snapshot.ApplicationID, snapshot.ID}
		if err := tx.Table("builds").Where(reference+" AND state NOT IN ("+terminalBuildStatesSQL+")", referenceArgs...).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return nil // 在途构建依赖快照文件，本轮完全跳过
		}
		if err := tx.Table("builds").Where(reference, referenceArgs...).Count(&referenced).Error; err != nil {
			return err
		}
		if referenced > 0 {
			// 仅终态构建引用：保留行满足 builds 与上传行的 RESTRICT，只回收文件。
			result := tx.Exec("UPDATE source_snapshots SET state = ? WHERE id = ? AND state = ?", StateExpired, snapshot.ID, StateReady)
			if result.Error != nil {
				return result.Error
			}
			processed = result.RowsAffected > 0
			return nil
		}
		var parents []struct{ ID string }
		if err := tx.Raw("SELECT id FROM source_uploads WHERE workspace_id = ? AND application_id = ? AND snapshot_id = ?", referenceArgs...).Scan(&parents).Error; err != nil {
			return err
		}
		for _, parent := range parents {
			// 上传行与快照行互相 ON DELETE RESTRICT：先解除 COMPLETE 引用并满足
			// 行 CHECK（state 与 snapshot_id/completed_at 成对变化），删除快照行
			// 之后再删除上传行，成对彻底回收。
			if err := tx.Exec("UPDATE source_uploads SET state = ?, snapshot_id = NULL, completed_at = NULL WHERE id = ?", StateExpired, parent.ID).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec("DELETE FROM source_snapshots WHERE id = ?", snapshot.ID).Error; err != nil {
			return err
		}
		for _, parent := range parents {
			if err := tx.Exec("DELETE FROM source_uploads WHERE id = ? AND state = ? AND snapshot_id IS NULL", parent.ID, StateExpired).Error; err != nil {
				return err
			}
		}
		processed = true
		return nil
	})
	if err != nil {
		return 0, err
	}
	if processed {
		return 1, nil
	}
	return 0, nil
}

// cleanupUpload 处理单个独立的过期上传会话；行删除带状态与有效期守卫，
// 并发清理或状态变化时静默跳过。
func (s *Service) cleanupUpload(ctx context.Context, target cleanupTarget, now time.Time) (int, error) {
	if err := s.removeStored(target.StorageKey); err != nil {
		return 0, err
	}
	result := s.db.WithContext(ctx).Exec("DELETE FROM source_uploads WHERE id = ? AND state IN (?, ?) AND expires_at <= ?", target.ID, StatePending, StateUploaded, now)
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected == 0 {
		return 0, nil // 并发清理已处理或状态已变化
	}
	return 1, nil
}

// partFilePattern 匹配收包临时文件（security.NewID("part") 派生），与
// storageKeyPattern 的 upload_/snapshot_ 前缀不重叠，绝不误删已落盘内容。
var partFilePattern = regexp.MustCompile(`^part_[0-9a-f]{32}$`)

// orphanPartLifetime 是收包临时文件的兜底寿命：正常路径 part 文件由 defer
// 删除且收包自带 UploadLifetime 截止，超过寿命的 part 文件只可能是进程崩溃
// 遗留的半包，删除不影响任何活跃上传。
const orphanPartLifetime = time.Hour

// sweepOrphanParts 回收进程崩溃遗留的收包临时文件：只删修改时间早于
// orphanPartLifetime 的 part 文件，每轮最多 limit 个；失败记录后继续。
func (s *Service) sweepOrphanParts(ctx context.Context, now time.Time, limit int) (int, error) {
	directory, err := s.root.Open(".")
	if err != nil {
		return 0, err
	}
	entries, err := directory.ReadDir(-1)
	closeErr := directory.Close()
	if err != nil {
		return 0, err
	}
	if closeErr != nil {
		return 0, closeErr
	}
	cutoff := now.Add(-orphanPartLifetime)
	removed := 0
	var failures []error
	for _, entry := range entries {
		if removed >= limit {
			break
		}
		if ctx.Err() != nil {
			break
		}
		name := entry.Name()
		if entry.IsDir() || !partFilePattern.MatchString(name) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("回收 %s: %w", name, err))
			continue
		}
		removed++
	}
	if removed > 0 {
		if err := s.syncDirectory(); err != nil {
			failures = append(failures, err)
		}
	}
	return removed, errors.Join(failures...)
}
