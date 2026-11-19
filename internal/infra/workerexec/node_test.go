package workerexec

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCA 生成测试用自签CA，仅用于本地httptest回环验证，不做真实登记。
func testCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestRegisterRejectsInvalidInputs(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	for _, options := range []RegisterOptions{
		{Server: "http://control.example", Token: "token", StateDir: stateDir},
		{Server: "https://control.example/path/base", Token: "token", StateDir: stateDir},
		{Server: "https://control.example", Token: "", StateDir: stateDir},
		{Server: "https://control.example", Token: "token", StateDir: ""},
	} {
		if _, err := Register(context.Background(), options); err == nil {
			t.Fatalf("无效参数必须被拒绝: %+v", options)
		}
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("拒绝路径不得创建状态目录")
	}
}

func TestRegisterWritesFrozenStateFiles(t *testing.T) {
	caKey, caCertificate, caPEM := testCA(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/worker/register" {
			http.NotFound(w, r)
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
		template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "worker-1"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, caCertificate, request.PublicKey, caKey)
		if err != nil {
			http.Error(w, "签发失败", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"nodeId": "node-1", "workerId": "worker-1", "serial": "2", "certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "ca": string(caPEM), "notAfter": "2030-01-01T00:00:00Z", "rotateAfter": "2029-12-01T00:00:00Z"})
	}))
	defer server.Close()
	stateDir := filepath.Join(t.TempDir(), "state")
	serverCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	state, err := Register(context.Background(), RegisterOptions{Server: server.URL, Token: "token-1", StateDir: stateDir, ServerCA: serverCAPEM})
	if err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	if state.NodeID != "node-1" || state.WorkerID != "worker-1" || state.Serial != "2" || state.Server != server.URL {
		t.Fatalf("身份字段不正确: %+v", state)
	}
	info, err := os.Lstat(stateDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("状态目录必须为0700: %v %v", info, err)
	}
	for _, name := range []string{StateKeyFile, StateCertFile, StateCAFile, StateIdentityFile} {
		file, err := os.Lstat(filepath.Join(stateDir, name))
		if err != nil || !file.Mode().IsRegular() || file.Mode().Perm() != 0600 {
			t.Fatalf("%s 必须是0600普通文件: %v %v", name, file, err)
		}
	}
	// run命令依赖同一份冻结材料：LoadNodeState必须能原样恢复并核对证书私钥。
	keyPEM, certPEM, loadedCAPEM, loaded, err := LoadNodeState(stateDir)
	if err != nil || loaded.NodeID != "node-1" || loaded.Serial != "2" {
		t.Fatalf("恢复身份失败: %+v %v", loaded, err)
	}
	if len(keyPEM) == 0 || len(certPEM) == 0 || !strings.Contains(string(loadedCAPEM), "BEGIN CERTIFICATE") {
		t.Fatal("恢复材料不完整")
	}
}

func TestRegisterAndLoadRejectWidePermissions(t *testing.T) {
	_, _, caPEM := testCA(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"nodeId": "n", "workerId": "w", "serial": "1", "certificate": "", "ca": string(caPEM)})
	}))
	defer server.Close()
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(context.Background(), RegisterOptions{Server: server.URL, Token: "t", StateDir: stateDir, ServerCA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("过宽目录权限必须被拒绝: %v", err)
	}
	if _, _, _, _, err := LoadNodeState(stateDir); err == nil {
		t.Fatal("LoadNodeState同样必须拒绝过宽权限")
	}
}

func TestNodeRESTConfigValidatesInputs(t *testing.T) {
	directory := t.TempDir()
	_, _, caPEM := testCA(t)
	caPath := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenPath, []byte("kube-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NodeRESTConfig("http://kube.example:6443", caPath, tokenPath); err == nil {
		t.Fatal("非HTTPS API server必须被拒绝")
	}
	if _, err := NodeRESTConfig("https://kube.example:6443", filepath.Join(directory, "missing.pem"), tokenPath); err == nil {
		t.Fatal("缺失CA文件必须被拒绝")
	}
	if _, err := NodeRESTConfig("https://kube.example:6443", caPath, ""); err == nil {
		t.Fatal("缺失令牌文件必须被拒绝")
	}
	config, err := NodeRESTConfig("https://kube.example:6443", caPath, tokenPath)
	if err != nil {
		t.Fatalf("合法输入不应失败: %v", err)
	}
	if config.Host != "https://kube.example:6443" || config.TLSClientConfig.Insecure || len(config.TLSClientConfig.CAData) == 0 || config.BearerTokenFile != tokenPath {
		t.Fatalf("rest.Config装配不正确: %+v", config)
	}
}

func TestLoadInstalledCapabilitiesDefaultsAndOverrides(t *testing.T) {
	capabilities, err := LoadInstalledCapabilities("", "", "")
	if err != nil || capabilities.Build || capabilities.Runtime || capabilities.Ingress || capabilities.Metrics || capabilities.QualificationID != "" {
		t.Fatalf("无资格文件必须全部关闭: %+v %v", capabilities, err)
	}
	directory := t.TempDir()
	loose := filepath.Join(directory, "loose.json")
	if err := os.WriteFile(loose, []byte(`{"build":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstalledCapabilities(loose, "", ""); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("开启build但镜像未钉住digest必须拒绝: %v", err)
	}
	path := filepath.Join(directory, "qual.json")
	content := `{"qualification_id":"qual-1","build":true,"runtime":true,"ingress":true,"metrics":true,"runner_image":"` + testRunnerImage + `","buildkit_image":"` + testRunnerImage + `"}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	capabilities, err = LoadInstalledCapabilities(path, "", "")
	if err != nil || !capabilities.Build || !capabilities.Runtime || !capabilities.Ingress || !capabilities.Metrics || capabilities.QualificationID != "qual-1" {
		t.Fatalf("资格文件解析不正确: %+v %v", capabilities, err)
	}
	overridden, err := LoadInstalledCapabilities(path, "registry.example/r2@sha256:"+strings.Repeat("e", 64), "")
	if err != nil || overridden.RunnerImage != "registry.example/r2@sha256:"+strings.Repeat("e", 64) {
		t.Fatalf("CLI镜像参数应覆盖资格文件: %+v %v", overridden.RunnerImage, err)
	}
	unknown := filepath.Join(directory, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"bogus":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstalledCapabilities(unknown, "", ""); err == nil {
		t.Fatal("未知字段必须被拒绝")
	}
}

func TestInspectNodeReadsRealNodeFacts(t *testing.T) {
	_, client := newKubeFixture(t)
	facts, err := InspectNode(context.Background(), client, testNodeName)
	if err != nil {
		t.Fatal(err)
	}
	if facts.UID != testNodeUID || facts.OS != "linux" || facts.Arch != "amd64" {
		t.Fatalf("节点事实必须来自真实kube API: %+v", facts)
	}
	if _, err := InspectNode(context.Background(), client, ""); err == nil {
		t.Fatal("缺少节点名必须被拒绝")
	}
	if _, err := InspectNode(context.Background(), client, "unknown-node"); err == nil {
		t.Fatal("未知节点必须失败")
	}
}
