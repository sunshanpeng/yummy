package server

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/build"
	"github.com/sunshanpeng/yummy/internal/platform/buildsecret"
	"github.com/sunshanpeng/yummy/internal/platform/deployment"
	"github.com/sunshanpeng/yummy/internal/platform/enrollment"
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/source"
	"github.com/sunshanpeng/yummy/internal/platform/workergateway"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// workerMaterials 只由 mTLS 网关调用；数据库中重新读取冻结的引用，不信任传入命令中的材料。
type workerMaterials struct {
	db        *gorm.DB
	box       *security.SecretBox
	source    *source.Service
	secrets   *buildsecret.Service
	inventory *inventory.Service
	baseURL   string
}

type frozenBuildMaterial struct {
	WorkspaceID, ApplicationID, RegistryID, Repository string
	CredentialVersion                                  int64
}

func (m *workerMaterials) MaterializeBuild(ctx context.Context, identity enrollment.Identity, dispatch build.Dispatch) (result workergateway.BuildMaterial, err error) {
	if dispatch.Command != "RUN" || identity.NodeID != dispatch.NodeID || identity.WorkerID != dispatch.WorkerID {
		return result, authorization.ErrDenied
	}
	err = m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		grant, err := m.source.GrantForBuild(tx, m.box, build.WorkerIdentity{NodeID: identity.NodeID, WorkerID: identity.WorkerID, CertificateSerial: identity.Serial}, dispatch.AttemptID)
		if err != nil {
			return err
		}
		if grant.BuildID != dispatch.BuildID || grant.WorkspaceID != dispatch.WorkspaceID || grant.ApplicationID != dispatch.ApplicationID || grant.SHA256 != dispatch.SourceSHA256 {
			return authorization.ErrDenied
		}
		var frozen frozenBuildMaterial
		if err := tx.Table("builds").Select("workspace_id, application_id, registry_id, credential_version, repository").Where("id = ? AND workspace_id = ? AND application_id = ?", grant.BuildID, grant.WorkspaceID, grant.ApplicationID).Take(&frozen).Error; err != nil {
			return err
		}
		if dispatch.RegistryID != frozen.RegistryID || dispatch.CredentialVersion != frozen.CredentialVersion || dispatch.Repository != frozen.Repository {
			return authorization.ErrDenied
		}
		credential, err := m.inventory.GetCredential(tx, frozen.RegistryID, frozen.CredentialVersion)
		if err != nil {
			return err
		}
		if err = validateCredentialTarget(credential.URL, frozen.Repository); err != nil {
			credential.Clear()
			return err
		}
		result.Registry = &platformv1.WorkerRegistryCredential{Origin: credential.URL, Username: credential.Username, Password: credential.Password, Version: frozen.CredentialVersion}
		var refs []struct {
			SecretID, Name string
			Version        int64
		}
		if err := tx.Table("build_secret_references").Select("secret_id,name,version").Where("workspace_id = ? AND application_id = ? AND build_id = ?", grant.WorkspaceID, grant.ApplicationID, grant.BuildID).Order("secret_id").Find(&refs).Error; err != nil {
			return err
		}
		for _, ref := range refs {
			material, err := m.secrets.ReadVersion(tx, grant.WorkspaceID, grant.ApplicationID, buildsecret.Ref{SecretID: ref.SecretID, Name: ref.Name, Version: ref.Version})
			if err != nil {
				return err
			}
			result.Secrets = append(result.Secrets, &platformv1.WorkerBuildSecret{Name: material.Name, Value: material.Value})
		}
		result.Source = &platformv1.WorkerSourceGrant{Url: m.baseURL + "/transfer/build-grants/" + grant.ID, Token: grant.Token, Sha256: "sha256:" + grant.SHA256, ExpiresUnix: grant.ExpiresAt.Unix()}
		return nil
	})
	if err != nil {
		if result.Registry != nil {
			clear(result.Registry.Password)
		}
		for _, secret := range result.Secrets {
			clear(secret.Value)
		}
		return workergateway.BuildMaterial{}, err
	}
	return result, nil
}

// MaterializeDeployment 在应用操作行锁内重新验证 generation、节点和已登记证书；
// 即使调用方传入过期派发或伪造仓库，也只能读取关联 release 的冻结凭据。
func (m *workerMaterials) MaterializeDeployment(ctx context.Context, identity enrollment.Identity, certificate *x509.Certificate, dispatch deployment.Dispatch) (result *platformv1.WorkerRegistryCredential, err error) {
	if certificate == nil || certificate.SerialNumber == nil || certificate.SerialNumber.Text(16) != identity.Serial || len(certificate.Raw) == 0 || dispatch.Action == deployment.Stop {
		return nil, authorization.ErrDenied
	}
	certHash := sha256.Sum256(certificate.Raw)
	err = m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 顺序与 ClaimPending、撤销证书及停止操作一致，提交前不可放开身份锁。
		worker := deployment.WorkerIdentity{NodeID: identity.NodeID, WorkerID: identity.WorkerID, CertificateSerial: identity.Serial, CertificateSHA256: hex.EncodeToString(certHash[:])}
		if err := deployment.AuthenticateWorkerTx(tx, worker); err != nil {
			return err
		}
		var application struct{ ID string }
		if err := tx.Table("applications").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("id = ? AND workspace_id = ? AND status = 'ACTIVE'", dispatch.ApplicationID, dispatch.WorkspaceID).Take(&application).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return authorization.ErrDenied
			}
			return err
		}
		var frozen struct {
			RegistryID, Repository, Digest, Platform, Action string
			CredentialVersion                                int64
		}
		err := tx.Table("application_runtime_states AS s").Select("r.registry_id,r.credential_version,r.repository,r.digest,r.platform,o.action").Joins("JOIN deployment_operations o ON o.id = s.active_operation_id AND o.workspace_id = s.workspace_id AND o.application_id = s.application_id").Joins("JOIN application_releases r ON r.id = o.release_id AND r.workspace_id = o.workspace_id AND r.application_id = o.application_id").Joins("JOIN application_placements p ON p.workspace_id = s.workspace_id AND p.application_id = s.application_id").Joins("JOIN execution_nodes n ON n.id = p.node_id AND n.worker_id = p.worker_id").Joins("JOIN worker_certificates c ON c.node_id = n.id AND c.worker_id = n.worker_id").Where("s.workspace_id = ? AND s.application_id = ? AND s.generation = ? AND s.desired_release_id = ? AND o.id = ? AND o.active = TRUE AND o.generation = s.generation AND o.action <> 'STOP' AND p.node_id = ? AND p.worker_id = ? AND p.cluster_node_uid = ? AND n.cluster_node_uid = p.cluster_node_uid AND n.status IN ('ACTIVE','DRAINING') AND c.serial = ? AND c.certificate_sha256 = ? AND c.revoked_at IS NULL AND c.not_before <= NOW() AND c.not_after > NOW()", dispatch.WorkspaceID, dispatch.ApplicationID, dispatch.Generation, dispatch.ReleaseID, dispatch.OperationID, identity.NodeID, identity.WorkerID, dispatch.Placement.ClusterNodeUID, identity.Serial, hex.EncodeToString(certHash[:])).Take(&frozen).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return authorization.ErrDenied
		}
		if err != nil {
			return err
		}
		if dispatch.Action != frozen.Action || dispatch.Placement.NodeID != identity.NodeID || dispatch.Placement.WorkerID != identity.WorkerID || dispatch.Artifact.RegistryID != frozen.RegistryID || dispatch.Artifact.CredentialVersion != frozen.CredentialVersion || dispatch.Artifact.Repository != frozen.Repository || dispatch.Artifact.Digest != frozen.Digest || dispatch.Artifact.Platform != frozen.Platform {
			return authorization.ErrDenied
		}
		credential, err := m.inventory.GetCredential(tx, frozen.RegistryID, frozen.CredentialVersion)
		if err != nil {
			return err
		}
		if err := validateCredentialTarget(credential.URL, frozen.Repository); err != nil {
			credential.Clear()
			return err
		}
		result = &platformv1.WorkerRegistryCredential{Origin: credential.URL, Username: credential.Username, Password: credential.Password, Version: frozen.CredentialVersion}
		return nil
	})
	if err != nil {
		if result != nil {
			clear(result.Password)
		}
		return nil, err
	}
	return result, nil
}

func validateCredentialTarget(origin, repository string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || !strings.HasPrefix(repository, parsed.Host+"/") {
		return errors.New("仓库凭据与冻结镜像目标不匹配")
	}
	return nil
}

var _ workergateway.BuildMaterializer = (*workerMaterials)(nil)
var _ workergateway.DeploymentMaterializer = (*workerMaterials)(nil)
