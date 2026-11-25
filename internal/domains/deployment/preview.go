package deployment

import (
	"context"
	"errors"
	"fmt"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/lib/runtimeconfig"
	"gorm.io/gorm"
)

type PreviewResult struct {
	Artifact              ArtifactSnapshot
	RuntimeConfigRevision int64
	Domains               []string
	TLSStatus             string
}

func (s *Service) prepareRelease(tx *gorm.DB, ws, app string, in *CreateInput, target string) (releaseRecord, build.VerifiedArtifact, runtimeconfig.View, error) {
	var selected releaseRecord
	var artifact build.VerifiedArtifact
	state, err := readState(tx, ws, app)
	if err != nil {
		return selected, artifact, runtimeconfig.View{}, err
	}
	if err := s.checkExpected(tx, ws, app, state, in.Expected); err != nil {
		return selected, artifact, runtimeconfig.View{}, err
	}
	if state.ActiveOperationID != nil {
		return selected, artifact, runtimeconfig.View{}, authorization.ErrConflict
	}
	if target != "" {
		selected, err = readRelease(tx, ws, app, target)
		if err != nil {
			return selected, artifact, runtimeconfig.View{}, err
		}
		var successful int64
		if err := tx.Model(&operationRecord{}).Where("workspace_id = ? AND application_id = ? AND release_id = ? AND state = ?", ws, app, target, Succeeded).Count(&successful).Error; err != nil {
			return selected, artifact, runtimeconfig.View{}, err
		}
		if successful == 0 {
			return selected, artifact, runtimeconfig.View{}, invalid("只能回滚到已成功的历史发布")
		}
		in.ArtifactID = selected.ArtifactID
		in.RuntimeConfigRevision = selected.RuntimeConfigRevision
	}
	artifact, err = s.builds.VerifiedArtifact(tx, ws, app, in.ArtifactID)
	if err != nil {
		return selected, artifact, runtimeconfig.View{}, err
	}
	material, err := s.materialize(tx, ws, app, in.RuntimeConfigRevision)
	return selected, artifact, material.View, err
}

func (s *Service) verifyArtifact(ctx context.Context, artifact build.VerifiedArtifact) error {
	if s.verifier == nil {
		return ErrVerification
	}
	if err := s.verifier.Verify(ctx, verifiedArtifactSnapshot(artifact)); err != nil {
		// 保留终态分类，暂时无法读取的仓库仍可重试。
		if errors.Is(err, ErrArtifactInvalid) {
			return err
		}
		return ErrVerification
	}
	return nil
}

// Preview 使用与发布受理相同的检查；仓库核实在事务外，预检不占用运行位置和域名。
func (s *Service) Preview(ctx context.Context, p authorization.Principal, ws, app string, in CreateInput, target string) (PreviewResult, error) {
	if in.Expected.Generation < 0 || in.Expected.ConfigRevision < 0 || in.ExpectedPolicyRevision < 1 {
		return PreviewResult{}, authorization.ErrInvalid
	}
	if target == "" && (in.ArtifactID == "" || in.RuntimeConfigRevision < 1 || in.RuntimeConfigRevision != in.Expected.ConfigRevision) {
		return PreviewResult{}, authorization.ErrInvalid
	}
	var artifact build.VerifiedArtifact
	var result PreviewResult
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.DeploymentWrite); err != nil {
			return err
		}
		selected, snapshot, config, err := s.prepareRelease(tx, ws, app, &in, target)
		if err != nil {
			return err
		}
		placement, _, err := s.placement(tx, ws, app, in.ExpectedPolicyRevision, config)
		if err != nil {
			return err
		}
		if target != "" && selected.PlacementSnapshot != placement {
			return invalid("历史发布运行位置与已固定位置不一致")
		}
		artifact = snapshot
		result = PreviewResult{Artifact: verifiedArtifactSnapshot(snapshot), RuntimeConfigRevision: in.RuntimeConfigRevision, Domains: releaseDomains(config, placement)}
		result.TLSStatus, err = previewDomainClaims(tx, ws, app, result.Domains)
		return err
	})
	if err != nil {
		return PreviewResult{}, err
	}
	if err := s.verifyArtifact(ctx, artifact); err != nil {
		return PreviewResult{}, err
	}
	return result, nil
}

func previewDomainClaims(tx *gorm.DB, ws, app string, domains []string) (string, error) {
	if len(domains) == 0 {
		return "NOT_APPLICABLE", nil
	}
	var claims []domainRecord
	if err := tx.Where("domain IN ?", domains).Find(&claims).Error; err != nil {
		return "", err
	}
	status := "BOUND"
	if len(claims) != len(domains) {
		status = "PENDING"
	}
	secret := ""
	for _, claim := range claims {
		if claim.WorkspaceID != ws || claim.ApplicationID != app {
			return "", fmt.Errorf("%w: 域名 %s 已被其他应用占用", authorization.ErrConflict, claim.Domain)
		}
		if claim.TLSSecretName == "" || (secret != "" && secret != claim.TLSSecretName) {
			status = "PENDING"
		}
		if claim.TLSSecretName != "" {
			secret = claim.TLSSecretName
		}
	}
	return status, nil
}
