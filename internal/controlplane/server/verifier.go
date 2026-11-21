package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/domains/deployment"
	"github.com/sunshanpeng/yummy/internal/domains/inventory"
	"github.com/sunshanpeng/yummy/internal/infra/registryclient"
	"gorm.io/gorm"
)

// artifactReader 只按已冻结的凭据版本访问原始仓库，绝不把旧口令发往后来修改的URL。
// 网络访问在数据库事务之外；默认不允许跨主机认证，白名单只能由实例装配配置注入。
type artifactReader struct {
	db          *gorm.DB
	inventory   *inventory.Service
	authHosts   map[string][]string
	deniedHosts []string
}

func (r *artifactReader) read(ctx context.Context, registryID string, version int64, repository, digest, platform string) (registryclient.Artifact, error) {
	var credential inventory.Credential
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		credential, err = r.inventory.GetCredential(tx, registryID, version)
		return err
	})
	if err != nil {
		return registryclient.Artifact{}, err
	}
	defer credential.Clear()
	origin, err := url.Parse(credential.URL)
	if err != nil || origin.Scheme != "https" || !strings.HasPrefix(repository, origin.Host+"/") {
		return registryclient.Artifact{}, registryclient.ErrInvalid
	}
	client, err := registryclient.New(registryclient.Config{URL: credential.URL, Username: credential.Username, Password: string(credential.Password), AllowPrivate: true, AllowedAuthHosts: r.authHosts[registryID], DeniedHosts: r.deniedHosts})
	if err != nil {
		return registryclient.Artifact{}, err
	}
	defer client.Close()
	return client.Verify(ctx, repository+"@"+digest, platform)
}

type buildArtifactVerifier struct{ reader *artifactReader }

func (v buildArtifactVerifier) Verify(ctx context.Context, in build.VerificationRequest) (build.VerificationResult, error) {
	artifact, err := v.reader.read(ctx, in.RegistryID, in.CredentialVersion, in.Repository, in.Digest, in.Platform)
	if err != nil {
		// 只有明确不可运行/摘要错误才永久失败；网络暂不可用保留待验证以供后台重试。
		if errors.Is(err, registryclient.ErrDigest) || errors.Is(err, registryclient.ErrManifest) || errors.Is(err, registryclient.ErrPlatform) || errors.Is(err, registryclient.ErrInvalid) || errors.Is(err, registryclient.ErrUnsafeTarget) {
			return build.VerificationResult{}, build.ErrVerification
		}
		return build.VerificationResult{}, err
	}
	return build.VerificationResult{Repository: artifact.Repository, Digest: artifact.Digest, Platform: artifact.Platform}, nil
}

type deploymentArtifactVerifier struct{ reader *artifactReader }

func (v deploymentArtifactVerifier) Verify(ctx context.Context, in deployment.ArtifactSnapshot) error {
	_, err := v.reader.read(ctx, in.RegistryID, in.CredentialVersion, in.Repository, in.Digest, in.Platform)
	if err != nil {
		// 与构建侧核验同一分类（见 buildArtifactVerifier）：明确不可运行（摘要/
		// manifest/平台不符、目标被拒、配置非法）与凭据版本缺失属永久失效，
		// 仓库暂不可用等网络错误保持暂态；自动发布据此区分终态关停与退避重试，
		// 手动发布统一按不可受理呈现（transport 与 ErrVerification 同映射）。
		if errors.Is(err, registryclient.ErrDigest) || errors.Is(err, registryclient.ErrManifest) || errors.Is(err, registryclient.ErrPlatform) || errors.Is(err, registryclient.ErrInvalid) || errors.Is(err, registryclient.ErrUnsafeTarget) || errors.Is(err, authorization.ErrNotFound) {
			return fmt.Errorf("%w: %v", deployment.ErrArtifactInvalid, err)
		}
		return deployment.ErrVerification
	}
	return nil
}
