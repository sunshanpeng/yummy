package build

import (
	"context"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"gorm.io/gorm"
)

// 本文件是构建域提供给部署域的产物事实：已验证产物定位与自动发布候选。
// 这些是构建域自身事实的导出视图（只读，不承载任何可写语义），部署域经
// 消费方接口依赖（deployment.BuildFacts），不再直查 builds/build_artifacts。

// VerifiedArtifact 是已验证产物定位：构建 SUCCEEDED 且产物 VERIFIED 的
// 仓库坐标，部署域据此受理发布并执行镜像核验。
type VerifiedArtifact struct {
	ID                string
	WorkspaceID       string
	ApplicationID     string
	RegistryID        string
	CredentialVersion int64
	Repository        string
	Digest            string
	Platform          string
}

// AutoDeployCandidate 是一条自动发布候选：auto_deploy 构建已成功且产物
// VERIFIED，携带构建时冻结的发布期望；候选是否已有发布由部署域按自身
// 事实过滤，本视图不做该判定。
type AutoDeployCandidate struct {
	BuildID                string
	WorkspaceID            string
	ApplicationID          string
	ActorID                string
	ActorAuthVersion       int64
	RuntimeConfigRevision  int64
	ExpectedServingRelease string
	Artifact               VerifiedArtifact
}

// AutoDeployCandidates 返回最多 limit 条自动发布候选（auto_deploy=TRUE、构建
// SUCCEEDED、产物 VERIFIED 且 verified_at 非空），按 (created_at, id) 升序；
// 调用方传入自己的 ctx，查询不经授权（调用方为控制面后台循环或装配方）。
func (s *Service) AutoDeployCandidates(ctx context.Context, limit int) ([]AutoDeployCandidate, error) {
	if limit < 1 {
		return nil, authorization.ErrInvalid
	}
	var rows []struct {
		BuildID                string `gorm:"column:build_id"`
		WorkspaceID            string
		ApplicationID          string
		ActorID                string
		ActorAuthVersion       int64  `gorm:"column:actor_auth_version"`
		RuntimeConfigRevision  int64  `gorm:"column:runtime_config_revision"`
		ExpectedServingRelease string `gorm:"column:expected_serving_release"`
		ArtifactID             string `gorm:"column:artifact_id"`
		RegistryID             string `gorm:"column:registry_id"`
		CredentialVersion      int64  `gorm:"column:credential_version"`
		Repository             string
		Digest                 string
		Platform               string
	}
	err := s.db.WithContext(ctx).Table("builds AS b").
		Select("b.id AS build_id, b.workspace_id, b.application_id, b.actor_id, b.actor_auth_version, b.runtime_config_revision, b.expected_serving_release, a.id AS artifact_id, a.registry_id, a.credential_version, a.repository, a.digest, a.platform").
		Joins("JOIN build_artifacts a ON a.build_id = b.id AND a.workspace_id = b.workspace_id AND a.application_id = b.application_id").
		Where("b.auto_deploy = TRUE AND b.state = ? AND a.state = 'VERIFIED' AND a.verified_at IS NOT NULL", Succeeded).
		Order("b.created_at, b.id").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]AutoDeployCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, AutoDeployCandidate{
			BuildID:                row.BuildID,
			WorkspaceID:            row.WorkspaceID,
			ApplicationID:          row.ApplicationID,
			ActorID:                row.ActorID,
			ActorAuthVersion:       row.ActorAuthVersion,
			RuntimeConfigRevision:  row.RuntimeConfigRevision,
			ExpectedServingRelease: row.ExpectedServingRelease,
			Artifact: VerifiedArtifact{
				ID:                row.ArtifactID,
				WorkspaceID:       row.WorkspaceID,
				ApplicationID:     row.ApplicationID,
				RegistryID:        row.RegistryID,
				CredentialVersion: row.CredentialVersion,
				Repository:        row.Repository,
				Digest:            row.Digest,
				Platform:          row.Platform,
			},
		})
	}
	return out, nil
}

// VerifiedArtifact 读取单个已验证产物：要求产物 VERIFIED 且所属构建
// SUCCEEDED，条件与发布受理一致；调用方传入自己的事务，由其负责授权。
func (s *Service) VerifiedArtifact(tx *gorm.DB, workspaceID, applicationID, artifactID string) (VerifiedArtifact, error) {
	var row struct {
		ID                string `gorm:"column:id"`
		WorkspaceID       string
		ApplicationID     string
		RegistryID        string `gorm:"column:registry_id"`
		CredentialVersion int64  `gorm:"column:credential_version"`
		Repository        string
		Digest            string
		Platform          string
	}
	err := tx.Table("build_artifacts AS a").
		Select("a.id, a.workspace_id, a.application_id, a.registry_id, a.credential_version, a.repository, a.digest, a.platform").
		Joins("JOIN builds b ON b.id = a.build_id AND b.workspace_id = a.workspace_id AND b.application_id = a.application_id").
		Where("a.workspace_id = ? AND a.application_id = ? AND a.id = ? AND a.state = 'VERIFIED' AND a.verified_at IS NOT NULL AND b.state = ?", workspaceID, applicationID, artifactID, Succeeded).
		Take(&row).Error
	if err != nil {
		return VerifiedArtifact{}, missing(err)
	}
	return VerifiedArtifact{
		ID:                row.ID,
		WorkspaceID:       row.WorkspaceID,
		ApplicationID:     row.ApplicationID,
		RegistryID:        row.RegistryID,
		CredentialVersion: row.CredentialVersion,
		Repository:        row.Repository,
		Digest:            row.Digest,
		Platform:          row.Platform,
	}, nil
}
