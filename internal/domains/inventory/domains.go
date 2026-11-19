package inventory

import (
	"context"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/audit"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"k8s.io/apimachinery/pkg/util/validation"
)

// domainClaimRecord 只读投影 application_domain_claims；域名占用由发布受理
// （deployment.claimDomains）维护，inventory 只管理其 TLS 绑定列，不创建/删除声明。
type domainClaimRecord struct {
	Domain        string `gorm:"primaryKey"`
	WorkspaceID   string
	ApplicationID string
	IsDefault     bool
	TLSSecretName string
	CreatedAt     time.Time
}

func (domainClaimRecord) TableName() string { return "application_domain_claims" }

// DomainTLSView 是域名当前证书绑定的只读视图；空 SecretName 表示未绑定。
type DomainTLSView struct {
	Domain      string `json:"domain"`
	SecretName  string `json:"secretName,omitempty"`
	WorkspaceID string `json:"workspaceId"`
}

// domainTLSView 裁剪为业务可见摘要：不暴露应用内部标识以外的集群事实。
func domainTLSView(v domainClaimRecord) DomainTLSView {
	return DomainTLSView{Domain: v.Domain, SecretName: v.TLSSecretName, WorkspaceID: v.WorkspaceID}
}

// validateTLSSecretName 校验管理员预置证书 Secret 名：空串=解除绑定；
// 非空必须是合法 DNS1123 子域，与 kube.Runtime 装配 Ingress.Spec.TLS 的规则一致。
func validateTLSSecretName(secretName string) (string, error) {
	secretName = strings.TrimSpace(secretName)
	if secretName == "" {
		return "", nil
	}
	if len(validation.IsDNS1123Subdomain(secretName)) != 0 {
		return "", invalid("证书Secret名必须是合法DNS1123子域")
	}
	return secretName, nil
}

// SetDomainTLSSecret 绑定或解除域名证书 Secret；域名声明必须已由发布占用。
// 绑定即时生效于下一次发布领取的汇聚，不回改任何历史发布快照。
func (s *Service) SetDomainTLSSecret(ctx context.Context, p authorization.Principal, domain, secretName string) (DomainTLSView, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return DomainTLSView{}, invalid("域名不能为空")
	}
	secretName, err := validateTLSSecretName(secretName)
	if err != nil {
		return DomainTLSView{}, err
	}
	var result DomainTLSView
	err = s.access.Do(ctx, p, true, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var claim domainClaimRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&claim, "domain = ?", domain).Error; err != nil {
			return missing(err)
		}
		if err := tx.Model(&domainClaimRecord{}).Where("domain = ?", domain).Update("tls_secret_name", secretName).Error; err != nil {
			return err
		}
		if err := audit.Record(tx, current.UserID, "instance.domain.tls.set", "application_domain_claim", domain, &claim.WorkspaceID, &claim.ApplicationID); err != nil {
			return err
		}
		claim.TLSSecretName = secretName
		result = domainTLSView(claim)
		return nil
	})
	return result, err
}

// GetDomainTLSSecret 只读查询域名当前证书绑定；未声明域名失败关闭。
func (s *Service) GetDomainTLSSecret(ctx context.Context, p authorization.Principal, domain string) (DomainTLSView, error) {
	domain = strings.TrimSpace(domain)
	var result DomainTLSView
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if err := s.access.RequireInstance(current); err != nil {
			return err
		}
		var claim domainClaimRecord
		if err := tx.First(&claim, "domain = ?", domain).Error; err != nil {
			return missing(err)
		}
		result = domainTLSView(claim)
		return nil
	})
	return result, err
}
