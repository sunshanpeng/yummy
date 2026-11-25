package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
// PLANNING 只出现在 Railpack 构建：识别源码并生成构建计划，位于准备源码与构建镜像之间。
var stages = map[string]int{"PREPARING_SOURCE": 1, "PLANNING": 2, "BUILDING": 3, "PUSHING": 4, "VERIFYING_ARTIFACT": 5}

func validateEvent(e Event) error {
	if e.AttemptID == "" || e.Seq < 1 || len(e.JobUID) > 200 || len(e.ErrorCode) > 128 || !utf8.ValidString(e.JobUID) || !utf8.ValidString(e.ErrorCode) || strings.ContainsAny(e.JobUID, "\x00\r\n") || strings.ContainsRune(e.ErrorCode, 0) {
		return authorization.ErrInvalid
	}
	switch e.Kind {
	case "ACCEPTED", "RUNNING", "STAGE", "SUCCEEDED", "FAILED", "STOPPED":
	default:
		return invalid("未知构建事件")
	}
	if e.Kind != "STOPPED" && e.JobUID == "" {
		return invalid("构建事件缺少 Job UID")
	}
	if e.Kind == "SUCCEEDED" && !digestRE.MatchString(e.Digest) {
		return invalid("产物 digest 非法")
	}
	if e.Digest != "" && !digestRE.MatchString(e.Digest) {
		return invalid("产物 digest 非法")
	}
	if e.Stage != "" && stages[e.Stage] == 0 {
		return invalid("未知构建阶段")
	}
	return nil
}
func releaseAttempt(a *attemptRecord, now time.Time) {
	a.Occupied = false
	a.StoppedAt = &now
	a.ReconcilingAt = nil
}
func (s *Service) recordArtifact(tx *gorm.DB, b buildRecord, a attemptRecord, digest, state string) error {
	if digest == "" {
		return nil
	}
	var old artifactRecord
	err := tx.Where("build_id = ?", b.ID).Take(&old).Error
	if err == nil {
		if old.Digest != digest {
			return authorization.ErrConflict
		}
		if state == "RESIDUAL" && old.State == "VERIFYING" {
			return tx.Model(&old).Update("state", state).Error
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	id, err := security.NewID("art")
	if err != nil {
		return err
	}
	return tx.Create(&artifactRecord{ID: id, WorkspaceID: b.WorkspaceID, ApplicationID: b.ApplicationID, BuildID: b.ID, AttemptID: a.ID, RegistryID: b.RegistryID, CredentialVersion: b.CredentialVersion, Repository: b.Repository, Tag: b.Tag, Digest: digest, Platform: b.Platform, State: state}).Error
}

// ApplyEvent 的 SUCCEEDED/FAILED/STOPPED 表示 worker 已核实原 Job 及 Pod 停止。
// 网络失联或仅 buildctl 退出不构成这些确认，应继续 RECONCILING。
func (s *Service) ApplyEvent(ctx context.Context, w WorkerIdentity, e Event) (Ack, error) {
	var ack Ack
	if err := validateEvent(e); err != nil {
		return ack, err
	}
	raw, _ := json.Marshal(e)
	payloadHash := hash(raw)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		b, a, err := lockAttempt(tx, w, e.AttemptID)
		if err != nil {
			return err
		}
		var previous eventRecord
		err = tx.Where("attempt_id = ? AND seq = ?", a.ID, e.Seq).Take(&previous).Error
		if err == nil {
			if previous.PayloadHash != payloadHash {
				return authorization.ErrConflict
			}
			ack = Ack{Seq: e.Seq, Duplicate: true, State: b.State}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if e.Seq != a.EventSeq+1 {
			return ErrSequence
		}
		if a.JobUID != "" && e.JobUID != a.JobUID {
			return authorization.ErrConflict
		}
		if a.JobUID == "" && e.JobUID != "" {
			a.JobUID = e.JobUID
		}
		// 已观察执行的 Job 后来消失只能 FAILED/STOPPED，绝不在此生成新 UID。
		now := time.Now().UTC()
		a.EventSeq = e.Seq
		a.LastEventAt = &now
		if terminal(b.State) {
			// LOST 的迟到停止证明可释放原节点占用，但不能把历史终态改写为成功。
			if e.Kind == "STOPPED" || e.Kind == "FAILED" || e.Kind == "SUCCEEDED" {
				releaseAttempt(&a, now)
			}
			if err := s.recordArtifact(tx, b, a, e.Digest, "RESIDUAL"); err != nil {
				return err
			}
		} else {
			switch e.Kind {
			case "ACCEPTED":
				if b.State == Reconciling && b.DesiredState == "RUN" {
					b.State = Starting
				}
				a.ReconcilingAt = nil
			case "RUNNING", "STAGE":
				if b.Stage == "VERIFYING_ARTIFACT" {
					return authorization.ErrConflict
				}
				if b.DesiredState == "CANCEL" {
					b.State = Canceling
				} else {
					b.State = Running
				}
				if e.Stage != "" {
					if stages[e.Stage] < stages[b.Stage] {
						return authorization.ErrConflict
					}
					b.Stage = e.Stage
				}
				a.ReconcilingAt = nil
			case "SUCCEEDED":
				releaseAttempt(&a, now)
				b.LogsComplete = e.LogsComplete
				if b.DesiredState == "CANCEL" {
					if err := s.recordArtifact(tx, b, a, e.Digest, "RESIDUAL"); err != nil {
						return err
					}
					state := Canceled
					if b.StopReason == "TIMEOUT" {
						state = TimedOut
					}
					b.State = state
					b.FinishedAt = &now
				} else {
					if err := s.recordArtifact(tx, b, a, e.Digest, "VERIFYING"); err != nil {
						return err
					}
					b.State = Running
					b.Stage = "VERIFYING_ARTIFACT"
				}
			case "FAILED", "STOPPED":
				releaseAttempt(&a, now)
				b.LogsComplete = e.LogsComplete
				if err := s.recordArtifact(tx, b, a, e.Digest, "RESIDUAL"); err != nil {
					return err
				}
				if b.DesiredState == "CANCEL" {
					b.State = Canceled
					if b.StopReason == "TIMEOUT" {
						b.State = TimedOut
					}
				} else {
					b.State = Failed
					b.ErrorCode = e.ErrorCode
					if b.ErrorCode == "" {
						b.ErrorCode = "EXECUTION_FAILED"
					}
				}
				b.FinishedAt = &now
			}
			if err := saveBuild(tx, &b); err != nil {
				return err
			}
		}
		if err := tx.Save(&a).Error; err != nil {
			return err
		}
		if err := tx.Create(&eventRecord{WorkspaceID: b.WorkspaceID, ApplicationID: b.ApplicationID, BuildID: b.ID, AttemptID: a.ID, Seq: e.Seq, PayloadHash: payloadHash, Payload: e}).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, b.ActorID, "build.event."+e.Kind, "build", b.ID, &b.WorkspaceID, &b.ApplicationID); err != nil {
			return err
		}
		ack = Ack{Seq: e.Seq, State: b.State}
		return nil
	})
	return ack, err
}
func (s *Service) AppendLogs(ctx context.Context, w WorkerIdentity, input LogInput) (Ack, error) {
	var ack Ack
	if input.Seq < 1 || len(input.Content) > MaxLogChunk {
		return ack, authorization.ErrInvalid
	}
	payloadHash := hash(input.Content)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		b, a, err := lockAttempt(tx, w, input.AttemptID)
		if err != nil {
			return err
		}
		var previous logRecord
		err = tx.Where("attempt_id = ? AND seq = ?", a.ID, input.Seq).Take(&previous).Error
		if err == nil {
			if previous.PayloadHash != payloadHash {
				return authorization.ErrConflict
			}
			ack = Ack{Seq: input.Seq, Duplicate: true, State: b.State}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if input.Seq != a.LogSeq+1 {
			return ErrSequence
		}
		// 到上限后仍保存有界的序号/摘要和空块，以便重放去重，不阻塞状态事件。
		content := input.Content
		remaining := int64(MaxLogBytes) - b.LogsBytes
		truncated := int64(len(content)) > remaining
		if truncated {
			content = content[:remaining]
			b.LogsTruncated = true
		}
		b.LogsBytes += int64(len(content))
		a.LogSeq = input.Seq
		if err := tx.Create(&logRecord{WorkspaceID: b.WorkspaceID, ApplicationID: b.ApplicationID, BuildID: b.ID, AttemptID: a.ID, Seq: input.Seq, PayloadHash: payloadHash, Content: append([]byte{}, content...), Truncated: truncated}).Error; err != nil {
			return err
		}
		if err := tx.Save(&a).Error; err != nil {
			return err
		}
		if err := saveBuild(tx, &b); err != nil {
			return err
		}
		ack = Ack{Seq: input.Seq, State: b.State}
		return nil
	})
	return ack, err
}

// VerifyArtifact 网络验证在事务外；提交时再次锁 build，保证取消先提交时不能成功。
func (s *Service) VerifyArtifact(ctx context.Context, id string, v ArtifactVerifier) error {
	if v == nil {
		return authorization.ErrInvalid
	}
	var a artifactRecord
	if err := s.db.WithContext(ctx).Where("build_id = ?", id).Take(&a).Error; err != nil {
		return missing(err)
	}
	if a.State != "VERIFYING" {
		return nil
	}
	req := VerificationRequest{BuildID: a.BuildID, ArtifactID: a.ID, RegistryID: a.RegistryID, CredentialVersion: a.CredentialVersion, Repository: a.Repository, Tag: a.Tag, Digest: a.Digest, Platform: a.Platform}
	result, verifyErr := v.Verify(ctx, req)
	if verifyErr != nil && !errors.Is(verifyErr, ErrVerification) {
		return verifyErr
	}
	valid := verifyErr == nil && result.Repository == req.Repository && result.Digest == req.Digest && result.Platform == Platform
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var b buildRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&b, "id = ?", id).Error; err != nil {
			return err
		}
		var current artifactRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", a.ID).Error; err != nil {
			return err
		}
		if current.State != "VERIFYING" {
			return nil
		}
		if terminal(b.State) || b.DesiredState == "CANCEL" {
			if err := tx.Model(&current).Update("state", "RESIDUAL").Error; err != nil {
				return err
			}
			return nil
		}
		now := time.Now().UTC()
		current.State = "INVALID"
		if valid {
			current.State = "VERIFIED"
			current.VerifiedAt = &now
			b.State = Succeeded
			b.ErrorCode = ""
		} else {
			b.State = Failed
			b.ErrorCode = "ARTIFACT_VERIFICATION_FAILED"
		}
		b.FinishedAt = &now
		if err := tx.Save(&current).Error; err != nil {
			return err
		}
		if err := saveBuild(tx, &b); err != nil {
			return err
		}
		return audit.Record(tx, b.ActorID, "build.artifact.verify", "build", b.ID, &b.WorkspaceID, &b.ApplicationID)
	})
}

// VerifyPending 逐条核验待验证产物。单个产物失败（仓库暂不可用、manifest 缺失等）
// 只记入汇总继续下一条，不结束整轮：失败记录保留 VERIFYING 由下一轮重试，
// 其余产物照常推进，避免一条坏记录阻塞整个核验批次。
func (s *Service) VerifyPending(ctx context.Context, v ArtifactVerifier, limit int) error {
	if limit < 1 || limit > 100 {
		return authorization.ErrInvalid
	}
	var records []artifactRecord
	if err := s.db.WithContext(ctx).Where("state = 'VERIFYING'").Order("created_at, id").Limit(limit).Find(&records).Error; err != nil {
		return err
	}
	var failures []error
	for _, a := range records {
		if err := s.VerifyArtifact(ctx, a.BuildID, v); err != nil {
			failures = append(failures, fmt.Errorf("build %s: %w", a.BuildID, err))
		}
	}
	return errors.Join(failures...)
}
