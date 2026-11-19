package enrollment

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
	"gorm.io/gorm"
)

func makeCA(t *testing.T) Config {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Yummy测试节点CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	config := Config{CACertificatePath: filepath.Join(root, "node-ca.pem"), CAPrivateKeyPath: filepath.Join(root, "node-ca-key.pem")}
	if err := os.WriteFile(config.CACertificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.CAPrivateKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return config
}

func csr(t *testing.T, signer crypto.Signer, template *x509.CertificateRequest) []byte {
	t.Helper()
	if template == nil {
		template = &x509.CertificateRequest{Subject: pkix.Name{CommonName: "attacker-csr-self-claims-another-worker"}}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, signer)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}
func edCSR(t *testing.T) []byte {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return csr(t, key, nil)
}
func leaf(t *testing.T, issued IssuedCertificate) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(issued.CertificatePEM)
	if block == nil {
		t.Fatal("缺少叶证书")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

type environment struct {
	db      *gorm.DB
	service *Service
	admin   authorization.Principal
	member  authorization.Principal
	config  Config
}

func setup(t *testing.T) *environment {
	t.Helper()
	db := testkit.OpenDatabase(t)
	for _, value := range []any{&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true}, &model.User{ID: "member", Username: "member", PasswordHash: "hash"}} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("INSERT INTO instance_settings(singleton,id) VALUES(TRUE, 'instance')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO execution_nodes(id, instance_id, name, worker_id) VALUES('node-a','instance','节点甲','worker-a'),('node-b','instance','节点乙','worker-b')").Error; err != nil {
		t.Fatal(err)
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	config := makeCA(t)
	service, err := NewService(db, access, config)
	if err != nil {
		t.Fatal(err)
	}
	return &environment{db: db, service: service, admin: authorization.Principal{UserID: "root", AuthVersion: 1}, member: authorization.Principal{UserID: "member", AuthVersion: 1}, config: config}
}
func (e *environment) register(t *testing.T, nodeID string) IssuedCertificate {
	t.Helper()
	token, err := e.service.CreateRegistration(context.Background(), e.admin, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := e.service.Register(context.Background(), RegisterInput{Token: token.Token, CSRPEM: edCSR(t), Transport: Transport{TLS: true}})
	if err != nil {
		t.Fatal(err)
	}
	return issued
}

func TestCAFileProtectionAndIdentity(t *testing.T) {
	for _, name := range []string{"可读私钥", "链接私钥", "硬链接私钥", "公私钥不匹配", "非CA", "证书不足30天", "无路径"} {
		t.Run(name, func(t *testing.T) {
			config := makeCA(t)
			switch name {
			case "可读私钥":
				if err := os.Chmod(config.CAPrivateKeyPath, 0644); err != nil {
					t.Fatal(err)
				}
			case "链接私钥":
				link := filepath.Join(t.TempDir(), "key")
				if err := os.Symlink(config.CAPrivateKeyPath, link); err != nil {
					t.Fatal(err)
				}
				config.CAPrivateKeyPath = link
			case "硬链接私钥":
				if err := os.Link(config.CAPrivateKeyPath, filepath.Join(filepath.Dir(config.CAPrivateKeyPath), "key-link")); err != nil {
					t.Fatal(err)
				}
			case "公私钥不匹配":
				config.CAPrivateKeyPath = makeCA(t).CAPrivateKeyPath
			case "非CA", "证书不足30天":
				certificate, signer, _, err := loadCA(config)
				if err != nil {
					t.Fatal(err)
				}
				if name == "非CA" {
					certificate.IsCA = false
				} else {
					certificate.NotAfter = time.Now().Add(24 * time.Hour)
				}
				der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, signer.Public(), signer)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config.CACertificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
					t.Fatal(err)
				}
			case "无路径":
				config.CAPrivateKeyPath = ""
			}
			if _, _, _, err := loadCA(config); !errors.Is(err, ErrCA) {
				t.Fatalf("接受不安全CA: %v", err)
			}
		})
	}
	config := makeCA(t)
	if _, _, _, err := loadCA(config); err != nil {
		t.Fatalf("合法CA被拒: %v", err)
	}
}

func TestCSRValidationAndFixedUsage(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	badUsage, err := asn1.Marshal(asn1.BitString{Bytes: []byte{0x04}, BitLength: 6})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		input []byte
	}{
		{"空", nil}, {"过大", bytes.Repeat([]byte{'x'}, maxCSRBytes+1)},
		{"RSA不足2048", csr(t, weak, nil)},
		{"请求SAN", csr(t, key, &x509.CertificateRequest{DNSNames: []string{"attacker.example"}})},
		{"请求CA", csr(t, key, &x509.CertificateRequest{ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Value: []byte{0x30, 0}}}})},
		{"请求签发权限", csr(t, key, &x509.CertificateRequest{ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 15}, Value: badUsage}}})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseCSR(test.input); !errors.Is(err, ErrCSR) {
				t.Fatalf("接受无效CSR: %v", err)
			}
		})
	}
	valid := edCSR(t)
	block, _ := pem.Decode(valid)
	block.Bytes[len(block.Bytes)-1] ^= 1
	if _, err := parseCSR(pem.EncodeToMemory(block)); !errors.Is(err, ErrCSR) {
		t.Fatalf("接受被替换签名: %v", err)
	}
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCSR(csr(t, p256, nil)); err != nil {
		t.Fatal(err)
	}
	strong, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCSR(csr(t, strong, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterSingleUseAndAuthoritativeNodeIdentity(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.service.CreateRegistration(ctx, e.member, "node-a"); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员获得注册码: %v", err)
	}
	token, err := e.service.CreateRegistration(ctx, e.admin, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	var stored registrationRecord
	if err := e.db.First(&stored, "id = ?", token.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.TokenHash == token.Token || strings.Contains(stored.TokenHash, "wrk_") || len(stored.TokenHash) != 64 {
		t.Fatal("注册码不是摘要存储")
	}
	request := edCSR(t)
	issued, err := e.service.Register(ctx, RegisterInput{Token: token.Token, CSRPEM: request, Transport: Transport{TLS: true}})
	if err != nil {
		t.Fatal(err)
	}
	certificate := leaf(t, issued)
	if certificate.Subject.CommonName != "worker-a" || certificate.IsCA || certificate.KeyUsage != x509.KeyUsageDigitalSignature || len(certificate.ExtKeyUsage) != 1 || certificate.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("CSR替换身份或权限: %+v", certificate)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(issued.CACertificatePEM)
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("签发链错误: %v", err)
	}
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("worker证书可用作服务端")
	}
	identity, err := e.service.VerifyPeer(ctx, certificate)
	if err != nil || identity.NodeID != "node-a" || identity.WorkerID != "worker-a" {
		t.Fatalf("验证身份: %+v %v", identity, err)
	}
	if _, err := e.service.Register(ctx, RegisterInput{Token: token.Token, CSRPEM: request, Transport: Transport{TLS: true}}); !errors.Is(err, ErrRegistration) {
		t.Fatalf("重复使用token: %v", err)
	}
	if _, err := e.service.CreateRegistration(ctx, e.admin, "node-a"); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("已有证书重复登记: %v", err)
	}
}

func TestConcurrentRegistrationOnlyOneConsumesToken(t *testing.T) {
	e := setup(t)
	token, err := e.service.CreateRegistration(context.Background(), e.admin, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		request := edCSR(t)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := e.service.Register(context.Background(), RegisterInput{Token: token.Token, CSRPEM: request, Transport: Transport{TLS: true}})
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	var good, denied int
	for err := range results {
		if err == nil {
			good++
		} else if errors.Is(err, ErrRegistration) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if good != 1 || denied != 1 {
		t.Fatalf("token竞态: good%d denied%d", good, denied)
	}
	var count int64
	if err := e.db.Model(&certificateRecord{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("重复证书: %d %v", count, err)
	}
}

func TestTLSExpiryAndRegistrationReplacement(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	first, err := e.service.CreateRegistration(ctx, e.admin, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.service.CreateRegistration(ctx, e.admin, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	request := edCSR(t)
	if _, err := e.service.Register(ctx, RegisterInput{Token: second.Token, CSRPEM: request}); !errors.Is(err, ErrTLSRequired) {
		t.Fatalf("允许明文生产登记: %v", err)
	}
	if _, err := e.service.Register(ctx, RegisterInput{Token: first.Token, CSRPEM: request, Transport: Transport{TLS: true}}); !errors.Is(err, ErrRegistration) {
		t.Fatalf("旧token未撤销: %v", err)
	}
	e.service.allowLoopback = true
	if _, err := e.service.Register(ctx, RegisterInput{Token: second.Token, CSRPEM: request, Transport: Transport{Loopback: true}}); err != nil {
		t.Fatalf("显式本地测试被拒: %v", err)
	}
	third, err := e.service.CreateRegistration(ctx, e.admin, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	e.service.now = func() time.Time { return third.ExpiresAt.Add(time.Second) }
	if _, err := e.service.Register(ctx, RegisterInput{Token: third.Token, CSRPEM: request, Transport: Transport{TLS: true}}); !errors.Is(err, ErrRegistration) {
		t.Fatalf("过期token有效: %v", err)
	}
}

func TestRotateAtomicRevocationAndWindow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	original := e.register(t, "node-a")
	certificate := leaf(t, original)
	if _, err := e.service.Rotate(ctx, certificate, edCSR(t)); !errors.Is(err, ErrRotationEarly) {
		t.Fatalf("允许过早轮换: %v", err)
	}
	rotationTime := certificate.NotAfter.Add(-RotationWindow)
	e.service.now = func() time.Time { return rotationTime }
	var notified []Identity
	e.service.onRevoke = func(value Identity) { notified = append(notified, value) }
	rotated, err := e.service.Rotate(ctx, certificate, edCSR(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 1 || notified[0].Serial != original.Identity.Serial {
		t.Fatalf("未通知关闭旧流: %+v", notified)
	}
	if _, err := e.service.VerifyPeer(ctx, certificate); !errors.Is(err, ErrPeer) {
		t.Fatalf("旧证仍有效: %v", err)
	}
	if _, err := e.service.VerifyPeer(ctx, leaf(t, rotated)); err != nil {
		t.Fatalf("新证无效: %v", err)
	}
	if _, err := e.service.Rotate(ctx, certificate, edCSR(t)); !errors.Is(err, ErrPeer) {
		t.Fatalf("旧证可重放轮换: %v", err)
	}
	var record certificateRecord
	if err := e.db.First(&record, "serial = ?", rotated.Identity.Serial).Error; err != nil || record.RotatedFrom == nil || *record.RotatedFrom != original.Identity.Serial {
		t.Fatalf("缺失轮换关联: %+v %v", record, err)
	}
}

func TestRevokeDisableAndCrossIdentityRejection(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	issued := e.register(t, "node-a")
	certificate := leaf(t, issued)
	for _, forged := range []*x509.Certificate{nil, {SerialNumber: certificate.SerialNumber}, func() *x509.Certificate { value := *certificate; value.Subject.CommonName = "worker-b"; return &value }(), func() *x509.Certificate {
		value := *certificate
		value.Raw = bytes.Clone(certificate.Raw)
		value.Raw[len(value.Raw)-1] ^= 1
		return &value
	}()} {
		if _, err := e.service.VerifyPeer(ctx, forged); !errors.Is(err, ErrPeer) {
			t.Fatalf("伪造跨身份通过: %v", err)
		}
	}
	if err := e.service.RevokeCertificate(ctx, e.member, issued.Identity.Serial); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员可吊证: %v", err)
	}
	if err := e.service.RevokeCertificate(ctx, e.admin, issued.Identity.Serial); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.VerifyPeer(ctx, certificate); !errors.Is(err, ErrPeer) {
		t.Fatalf("吊证无效: %v", err)
	}
	if err := e.service.RevokeCertificate(ctx, e.admin, issued.Identity.Serial); err != nil {
		t.Fatalf("重复吊证失败: %v", err)
	}
	issued = e.register(t, "node-a")
	if err := e.service.DisableNode(ctx, e.admin, "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.VerifyPeer(ctx, leaf(t, issued)); !errors.Is(err, ErrPeer) {
		t.Fatalf("停用节点仍有效: %v", err)
	}
	if _, err := e.service.CreateRegistration(ctx, e.admin, "node-a"); !errors.Is(err, ErrRegistration) {
		t.Fatalf("停用可登记: %v", err)
	}
	if err := e.service.EnableNode(ctx, e.member, "node-a"); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("普通成员可启用: %v", err)
	}
	if err := e.service.EnableNode(ctx, e.admin, "node-a"); err != nil {
		t.Fatal(err)
	}
	if err := e.service.EnableNode(ctx, e.admin, "node-a"); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("非停用节点也启用: %v", err)
	}
	if _, err := e.service.VerifyPeer(ctx, leaf(t, issued)); !errors.Is(err, ErrPeer) {
		t.Fatalf("重新启用复活旧证: %v", err)
	}
}

func TestEnablePreservesNodeIdentityAndRequiresFreshRegistration(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.db.Table("execution_nodes").Where("id = ?", "node-a").Update("cluster_node_uid", "original-node-uid").Error; err != nil {
		t.Fatal(err)
	}
	issued := e.register(t, "node-a")
	if err := e.service.DisableNode(ctx, e.admin, "node-a"); err != nil {
		t.Fatal(err)
	}
	if err := e.service.EnableNode(ctx, e.admin, "node-a"); err != nil {
		t.Fatal(err)
	}
	var stored struct {
		ClusterNodeUID string
		BuildReady     bool
		RuntimeReady   bool
	}
	if err := e.db.Table("execution_nodes").Where("id = ?", "node-a").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ClusterNodeUID != "original-node-uid" || stored.BuildReady || stored.RuntimeReady {
		t.Fatalf("启用更改身份或伪造就绪: %+v", stored)
	}
	if _, err := e.service.VerifyPeer(ctx, leaf(t, issued)); !errors.Is(err, ErrPeer) {
		t.Fatalf("启用复活旧证: %v", err)
	}
	fresh := e.register(t, "node-a")
	if _, err := e.service.VerifyPeer(ctx, leaf(t, fresh)); err != nil {
		t.Fatalf("新登记无效: %v", err)
	}
}

func TestConcurrentRotationAndPostCommitHook(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	issued := e.register(t, "node-a")
	certificate := leaf(t, issued)
	e.service.now = func() time.Time { return issued.Identity.NotAfter.Add(-RotationWindow) }
	var hookMu sync.Mutex
	var hookCalls int
	e.service.onRevoke = func(value Identity) {
		var stored certificateRecord
		if err := e.db.First(&stored, "serial = ?", value.Serial).Error; err != nil || stored.RevokedAt == nil {
			t.Errorf("hook在撤销提交前执行: %+v %v", stored, err)
		}
		hookMu.Lock()
		hookCalls++
		hookMu.Unlock()
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		request := edCSR(t)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := e.service.Rotate(ctx, certificate, request)
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	var good, denied int
	for err := range results {
		if err == nil {
			good++
		} else if errors.Is(err, ErrPeer) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if good != 1 || denied != 1 || hookCalls != 1 {
		t.Fatalf("轮换竞态: good=%d denied=%d hooks=%d", good, denied, hookCalls)
	}
	var count int64
	if err := e.db.Model(&credentialEvent{}).Where("action = ?", "ROTATE").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("worker轮换审计重复: %d %v", count, err)
	}
}

func TestExpiredCertificateAndUnusedTokenDisabled(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	issued := e.register(t, "node-a")
	e.service.now = func() time.Time { return issued.Identity.NotAfter.Add(time.Second) }
	if _, err := e.service.VerifyPeer(ctx, leaf(t, issued)); !errors.Is(err, ErrPeer) {
		t.Fatalf("过期证书有效: %v", err)
	}
	e.service.now = func() time.Time { return time.Now().UTC() }
	token, err := e.service.CreateRegistration(ctx, e.admin, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.service.DisableNode(ctx, e.admin, "node-b"); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Model(&nodeRecord{}).Where("id = ?", "node-b").Update("status", "ACTIVE").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Register(ctx, RegisterInput{Token: token.Token, CSRPEM: edCSR(t), Transport: Transport{TLS: true}}); !errors.Is(err, ErrRegistration) {
		t.Fatalf("停用再启用复活旧注册码: %v", err)
	}
}

func TestSingleConnectionPoolEnrollment(t *testing.T) {
	e := setup(t)
	pool, err := e.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	token, err := e.service.CreateRegistration(ctx, e.admin, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := e.service.Register(ctx, RegisterInput{Token: token.Token, CSRPEM: edCSR(t), Transport: Transport{TLS: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.VerifyPeer(ctx, leaf(t, issued)); err != nil {
		t.Fatal(err)
	}
	if err := e.service.RevokeCertificate(ctx, e.admin, issued.Identity.Serial); err != nil {
		t.Fatal(err)
	}
}

func TestDrainingVerifyRotateButCannotRegister(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	issued := e.register(t, "node-a")
	if err := e.db.Model(&nodeRecord{}).Where("id IN ?", []string{"node-a", "node-b"}).Update("status", "DRAINING").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.VerifyPeer(ctx, leaf(t, issued)); err != nil {
		t.Fatalf("排空断开有效证书: %v", err)
	}
	if _, err := e.service.CreateRegistration(ctx, e.admin, "node-b"); !errors.Is(err, ErrRegistration) {
		t.Fatalf("排空可新登记: %v", err)
	}
	e.service.now = func() time.Time { return issued.Identity.NotAfter.Add(-RotationWindow) }
	if _, err := e.service.Rotate(ctx, leaf(t, issued), edCSR(t)); err != nil {
		t.Fatalf("排空无法轮换: %v", err)
	}
}
