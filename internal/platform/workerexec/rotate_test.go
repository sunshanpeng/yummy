package workerexec

// 证书轮换测试：全部走 httptest TLS 回环，禁止真实网络。回环模式对照
// node_test.go 的 register 测试：自签 CA + 服务端证书自身作为受信根。

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/platform/workergateway"
)

// rotateFixture 是一个已登记完成的状态目录与模拟控制面的回环服务。
type rotateFixture struct {
	Server      *httptest.Server
	ServerCAPEM []byte
	StateDir    string
	State       NodeState
}

// newRotateFixture 完成一次登记（serial=2）并附带 /worker/rotate 模拟：分支按
// 控制面语义核对请求体只允许 {csr}（DisallowUnknownFields，token 字段必须不
// 存在）且必须携带已验证客户端证书；rotateOn 决定签发行为（nil 时固定 500）。
func newRotateFixture(t *testing.T, rotateAfter string, rotateOn func(w http.ResponseWriter, request *x509.CertificateRequest, oldCertificate *x509.Certificate)) rotateFixture {
	t.Helper()
	caKey, caCertificate, caPEM := testCA(t)
	fixture := rotateFixture{StateDir: filepath.Join(t.TempDir(), "state")}
	// 对照 server.WorkerTLSConfig：服务端要求客户端证书时（VerifyClientCertIfGiven）
	// mTLS 客户端证书才会随握手发送，PeerCertificates 才非空。
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker/register":
			if r.Method != http.MethodPost {
				http.Error(w, "需要POST", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Token string `json:"token"`
				CSR   string `json:"csr"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Token != "token-1" || body.CSR == "" {
				http.Error(w, "无效登记请求", http.StatusBadRequest)
				return
			}
			block, _ := pem.Decode([]byte(body.CSR))
			request, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil {
				http.Error(w, "证书请求无效", http.StatusBadRequest)
				return
			}
			respondIssuedCertificate(w, caKey, caCertificate, caPEM, request, 2, "2030-01-01T00:00:00Z", rotateAfter)
		case "/worker/rotate":
			if r.Method != http.MethodPost {
				http.Error(w, "需要POST", http.StatusMethodNotAllowed)
				return
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			var body struct {
				CSR string `json:"csr"`
			}
			// 与 server.workerHandler 同款：多余的 token 字段或非单一JSON值直接拒绝。
			if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF || body.CSR == "" {
				http.Error(w, "无效轮换请求", http.StatusBadRequest)
				return
			}
			if len(r.TLS.PeerCertificates) == 0 {
				http.Error(w, "需要已验证的客户端证书", http.StatusUnauthorized)
				return
			}
			block, _ := pem.Decode([]byte(body.CSR))
			request, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil {
				http.Error(w, "证书请求无效", http.StatusBadRequest)
				return
			}
			if rotateOn == nil {
				http.Error(w, "轮换失败", http.StatusInternalServerError)
				return
			}
			rotateOn(w, request, r.TLS.PeerCertificates[0])
		default:
			http.NotFound(w, r)
		}
	}))
	// 对照 server.WorkerTLSConfig：ClientAuth 要求客户端证书、ClientCAs 为
	// 登记 CA 池，mTLS 客户端证书才随握手发送并通过验证。
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		t.Fatal("测试CA无效")
	}
	server.TLS = &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: clientCAs, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	fixture.Server = server
	fixture.ServerCAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	registered, err := Register(context.Background(), RegisterOptions{Server: server.URL, Token: "token-1", StateDir: fixture.StateDir, ServerCA: fixture.ServerCAPEM})
	if err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	fixture.State = registered
	return fixture
}

// respondIssuedCertificate 用 CSR 公钥经测试 CA 签发新证书并回写控制面格式响应。
func respondIssuedCertificate(w http.ResponseWriter, caKey *ecdsa.PrivateKey, caCertificate *x509.Certificate, caPEM []byte, request *x509.CertificateRequest, serial int64, notAfter, rotateAfter string) {
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "yummy-worker"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(30 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, caCertificate, request.PublicKey, caKey)
	if err != nil {
		http.Error(w, "签发失败", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"nodeId": "node-1", "workerId": "worker-1", "serial": fmt.Sprintf("%d", serial), "certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "ca": string(caPEM), "notAfter": notAfter, "rotateAfter": rotateAfter})
}

// clientCertificatePEM 用测试 CA 为给定私钥签发客户端证书，返回 cert/key PEM。
func clientCertificatePEM(t *testing.T, caKey *ecdsa.PrivateKey, caCertificate *x509.Certificate, key *ecdsa.PrivateKey, serial int64) ([]byte, []byte) {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "yummy-worker"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, caCertificate, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := privateKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM
}

func stateFileSnapshot(t *testing.T, stateDir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range []string{stateKeyFile, stateCertFile, stateCAFile, stateIdentityFile} {
		data, err := os.ReadFile(filepath.Join(stateDir, name))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		files[name] = data
	}
	return files
}

func requireStateFilePermissions(t *testing.T, stateDir string) {
	t.Helper()
	for _, name := range []string{stateKeyFile, stateCertFile, stateCAFile, stateIdentityFile} {
		file, err := os.Lstat(filepath.Join(stateDir, name))
		if err != nil || !file.Mode().IsRegular() || file.Mode().Perm() != 0600 {
			t.Fatalf("%s 必须是0600普通文件: %v %v", name, file, err)
		}
	}
}

func TestRotateCertificateReplacesStateFiles(t *testing.T) {
	caKey, caCertificate, caPEM := testCA(t)
	fixture := newRotateFixture(t, "2029-12-01T00:00:00Z", func(w http.ResponseWriter, request *x509.CertificateRequest, oldCertificate *x509.Certificate) {
		// mTLS 必须携带登记在册的当前证书（serial 2）。
		if oldCertificate.SerialNumber.Int64() != 2 {
			http.Error(w, "轮换必须携带当前证书", http.StatusUnauthorized)
			return
		}
		respondIssuedCertificate(w, caKey, caCertificate, caPEM, request, 3, "2031-01-01T00:00:00Z", "2030-12-01T00:00:00Z")
	})
	rotated, err := RotateCertificate(context.Background(), RotateOptions{Server: fixture.Server.URL, StateDir: fixture.StateDir, ServerCA: fixture.ServerCAPEM})
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if rotated.NodeID != fixture.State.NodeID || rotated.WorkerID != fixture.State.WorkerID || rotated.Serial != "3" || rotated.Server != fixture.Server.URL {
		t.Fatalf("轮换身份不正确: %+v", rotated)
	}
	if rotated.NotAfter != "2031-01-01T00:00:00Z" || rotated.RotateAfter != "2030-12-01T00:00:00Z" {
		t.Fatalf("轮换窗口字段不正确: %+v", rotated)
	}
	// 状态目录原子更新：新私钥/新证书/新身份生效，全部保持0600普通文件。
	keyPEM, certPEM, loadedCAPEM, loaded, err := LoadNodeState(fixture.StateDir)
	if err != nil || loaded.Serial != "3" || loaded.RotateAfter != "2030-12-01T00:00:00Z" {
		t.Fatalf("恢复轮换后身份失败: %+v %v", loaded, err)
	}
	if len(keyPEM) == 0 || len(certPEM) == 0 || !strings.Contains(string(loadedCAPEM), "BEGIN CERTIFICATE") {
		t.Fatal("轮换后材料不完整")
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("轮换后证书与私钥必须匹配: %v", err)
	}
	info, err := os.Lstat(fixture.StateDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("状态目录必须保持0700: %v %v", info, err)
	}
	requireStateFilePermissions(t, fixture.StateDir)
	entries, err := os.ReadDir(fixture.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".replace-") {
			t.Fatalf("轮换不得遗留临时文件: %s", entry.Name())
		}
	}
}

func TestRotateCertificateServerFailureKeepsOldState(t *testing.T) {
	// rotateOn 为 nil：服务端固定 500。
	fixture := newRotateFixture(t, "2029-12-01T00:00:00Z", nil)
	before := stateFileSnapshot(t, fixture.StateDir)
	if _, err := RotateCertificate(context.Background(), RotateOptions{Server: fixture.Server.URL, StateDir: fixture.StateDir, ServerCA: fixture.ServerCAPEM}); err == nil {
		t.Fatal("服务端失败必须返回错误")
	}
	after := stateFileSnapshot(t, fixture.StateDir)
	for name, data := range before {
		if string(after[name]) != string(data) {
			t.Fatalf("失败路径不得改动 %s", name)
		}
	}
	_, _, _, loaded, err := LoadNodeState(fixture.StateDir)
	if err != nil || loaded.Serial != fixture.State.Serial {
		t.Fatalf("失败路径旧状态必须完整保留: %+v %v", loaded, err)
	}
	requireStateFilePermissions(t, fixture.StateDir)
}

func TestRotateCertificateRejectsInvalidInputs(t *testing.T) {
	fixture := newRotateFixture(t, "2029-12-01T00:00:00Z", nil)
	if _, err := RotateCertificate(context.Background(), RotateOptions{Server: fixture.Server.URL, StateDir: ""}); err == nil {
		t.Fatal("缺少状态目录必须被拒绝")
	}
	if _, err := RotateCertificate(context.Background(), RotateOptions{Server: fixture.Server.URL, StateDir: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("不存在的状态目录必须被拒绝")
	}
	if _, err := RotateCertificate(context.Background(), RotateOptions{Server: "http://control.example", StateDir: fixture.StateDir}); err == nil {
		t.Fatal("非HTTPS控制面地址必须被拒绝")
	}
}

func TestCertificateHolderUpdatesClientCertificate(t *testing.T) {
	caKey, caCertificate, _ := testCA(t)
	firstKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	secondKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	firstCertPEM, firstKeyPEM := clientCertificatePEM(t, caKey, caCertificate, firstKey, 4)
	secondCertPEM, secondKeyPEM := clientCertificatePEM(t, caKey, caCertificate, secondKey, 5)
	holder, err := NewCertificateHolder(firstCertPEM, firstKeyPEM)
	if err != nil {
		t.Fatalf("装配holder失败: %v", err)
	}
	current, err := holder.GetClientCertificate(nil)
	if err != nil || !firstCertContains(current, firstCertPEM) {
		t.Fatalf("初始证书不正确: %v %v", current, err)
	}
	if err := holder.Update(secondCertPEM, secondKeyPEM); err != nil {
		t.Fatalf("更新holder失败: %v", err)
	}
	updated, err := holder.GetClientCertificate(nil)
	if err != nil || !firstCertContains(updated, secondCertPEM) {
		t.Fatalf("更新后必须返回新证书: %v %v", updated, err)
	}
	// 坏材料必须保持旧证书不变（失败关闭，不让握手拿到不匹配材料）。
	if err := holder.Update(secondCertPEM, firstKeyPEM); err == nil {
		t.Fatal("不匹配材料必须被拒绝")
	}
	kept, err := holder.GetClientCertificate(nil)
	if err != nil || !firstCertContains(kept, secondCertPEM) {
		t.Fatalf("更新失败必须保持旧证书: %v %v", kept, err)
	}
	if _, err := NewCertificateHolder(firstCertPEM, secondKeyPEM); err == nil {
		t.Fatal("装配时不匹配材料必须被拒绝")
	}
}

// firstCertContains 比较 holder 返回证书的叶子 DER 与期望 PEM。
func firstCertContains(certificate *tls.Certificate, certPEM []byte) bool {
	if certificate == nil || len(certificate.Certificate) == 0 {
		return false
	}
	block, _ := pem.Decode(certPEM)
	return block != nil && bytes.Equal(certificate.Certificate[0], block.Bytes)
}

// nopExecutor 满足 workergateway.Executor，仅用于装配校验。
type nopExecutor struct{}

func (nopExecutor) Build(context.Context, *platformv1.WorkerBuildCommand, bool, workergateway.Emitter) error {
	return nil
}

func (nopExecutor) Deploy(context.Context, *platformv1.WorkerDeploymentCommand, bool, workergateway.Emitter) error {
	return nil
}

// TestCertificateHolderSatisfiesWorkerClientTLS 固定轮换与网关客户端的 TLS
// 契约：TLS.Certificates 为空但 GetClientCertificate 非空的配置合法
// （workergateway.NewWorkerClient 的校验条件），轮换回填 holder 无需重建客户端。
func TestCertificateHolderSatisfiesWorkerClientTLS(t *testing.T) {
	caKey, caCertificate, _ := testCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	certPEM, keyPEM := clientCertificatePEM(t, caKey, caCertificate, key, 6)
	holder, err := NewCertificateHolder(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := workergateway.OpenJournal(filepath.Join(t.TempDir(), "journal"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client, err := workergateway.NewWorkerClient(workergateway.WorkerConfig{
		URL: "https://control.example", NodeID: "node-1", WorkerID: "worker-1",
		TLS:      &tls.Config{GetClientCertificate: holder.GetClientCertificate},
		Journal:  journal,
		Executor: nopExecutor{},
	})
	if err != nil {
		t.Fatalf("holder回调配置必须被网关客户端接受: %v", err)
	}
	_ = client
	if _, err := workergateway.NewWorkerClient(workergateway.WorkerConfig{
		URL: "https://control.example", NodeID: "node-1", WorkerID: "worker-1",
		TLS:      &tls.Config{MinVersion: tls.VersionTLS12},
		Journal:  journal,
		Executor: nopExecutor{},
	}); err == nil {
		t.Fatal("证书与回调都缺失必须被网关客户端拒绝")
	}
}

func TestRotationManagerRotatesOnDeadlineAndStops(t *testing.T) {
	caKey, caCertificate, caPEM := testCA(t)
	rotations := make(chan *x509.Certificate, 4)
	fixture := newRotateFixture(t, "2020-01-01T00:00:00Z", func(w http.ResponseWriter, request *x509.CertificateRequest, oldCertificate *x509.Certificate) {
		rotations <- oldCertificate
		respondIssuedCertificate(w, caKey, caCertificate, caPEM, request, 3, "2031-01-01T00:00:00Z", "2030-12-01T00:00:00Z")
	})
	holder, err := NewCertificateHolder(mustRead(t, fixture.StateDir, stateCertFile), mustRead(t, fixture.StateDir, stateKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewRotationManager(RotationManagerConfig{Server: fixture.Server.URL, StateDir: fixture.StateDir, ServerCA: fixture.ServerCAPEM, RotationCheckInterval: 20 * time.Millisecond, OnRotationError: func(err error) {
		t.Errorf("轮换不应报错: %v", err)
	}}, holder)
	if err != nil {
		t.Fatalf("装配轮换管理器失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	// 启动即检查：RotateAfter 已过期，第一tick内完成轮换。
	select {
	case old := <-rotations:
		if old.SerialNumber.Int64() != 2 {
			t.Fatalf("轮换必须携带当前证书: serial %s", old.SerialNumber)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("到期后必须触发轮换")
	}
	updated, err := holder.GetClientCertificate(nil)
	if err != nil || !firstCertContains(updated, mustRead(t, fixture.StateDir, stateCertFile)) {
		t.Fatalf("轮换后holder必须返回新证书: %v %v", updated, err)
	}
	// 管理器内存状态在 holder 回填之后的微小窗口内回写；轮询等待收敛。
	serialDeadline := time.Now().Add(2 * time.Second)
	for manager.State().Serial != "3" && time.Now().Before(serialDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if manager.State().Serial != "3" {
		t.Fatalf("管理器状态必须更新: %+v", manager.State())
	}
	_, _, _, loaded, err := LoadNodeState(fixture.StateDir)
	if err != nil || loaded.Serial != "3" {
		t.Fatalf("状态目录必须原子更新: %+v %v", loaded, err)
	}
	requireStateFilePermissions(t, fixture.StateDir)
	// 新 RotateAfter 在未来：下一检查间隔不得再次轮换。
	time.Sleep(80 * time.Millisecond)
	if len(rotations) != 0 {
		t.Fatalf("未来轮换点不得重复轮换: %d", len(rotations))
	}
	// ctx 取消后 Run 必须返回且不泄漏定时器。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run退出必须无错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx取消后Run必须退出")
	}
}

func TestRotationManagerFailureRetriesAndKeepsState(t *testing.T) {
	fixture := newRotateFixture(t, "2020-01-01T00:00:00Z", nil)
	holder, err := NewCertificateHolder(mustRead(t, fixture.StateDir, stateCertFile), mustRead(t, fixture.StateDir, stateKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	failures := make(chan error, 8)
	manager, err := NewRotationManager(RotationManagerConfig{Server: fixture.Server.URL, StateDir: fixture.StateDir, ServerCA: fixture.ServerCAPEM, RotationCheckInterval: 20 * time.Millisecond, OnRotationError: func(err error) {
		failures <- err
	}}, holder)
	if err != nil {
		t.Fatal(err)
	}
	before := stateFileSnapshot(t, fixture.StateDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-failures:
		case <-time.After(5 * time.Second):
			t.Fatal("轮换失败必须按检查间隔重试并上报")
		}
	}
	after := stateFileSnapshot(t, fixture.StateDir)
	for name, data := range before {
		if string(after[name]) != string(data) {
			t.Fatalf("重试失败期间不得改动 %s", name)
		}
	}
	if manager.State().Serial != fixture.State.Serial {
		t.Fatalf("失败不得更新管理器状态: %+v", manager.State())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run退出必须无错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx取消后Run必须退出")
	}
}

func TestRotationManagerRejectsInvalidConfigs(t *testing.T) {
	fixture := newRotateFixture(t, "2029-12-01T00:00:00Z", nil)
	holder, err := NewCertificateHolder(mustRead(t, fixture.StateDir, stateCertFile), mustRead(t, fixture.StateDir, stateKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRotationManager(RotationManagerConfig{StateDir: fixture.StateDir}, nil); err == nil {
		t.Fatal("缺少holder必须被拒绝")
	}
	if _, err := NewRotationManager(RotationManagerConfig{}, holder); err == nil {
		t.Fatal("缺少状态目录必须被拒绝")
	}
	if _, err := NewRotationManager(RotationManagerConfig{StateDir: fixture.StateDir, RotationCheckInterval: -time.Second}, holder); err == nil {
		t.Fatal("负检查间隔必须被拒绝")
	}
}

func mustRead(t *testing.T, stateDir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, name))
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return data
}
