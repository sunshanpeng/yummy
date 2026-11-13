package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
	"github.com/sunshanpeng/yummy/internal/platform/inventory"
	"github.com/sunshanpeng/yummy/internal/platform/testkit"
)

// 测试正式控制面装配、HTTPS 一次性登记及真实 HTTP/2 mTLS；不启用任何虚假节点资格。
func TestWorkerRegistrationAndGatewayOnProductServer(t *testing.T) {
	redis := os.Getenv("TEST_REDIS_ADDR")
	if redis == "" {
		t.Skip("未设置 TEST_REDIS_ADDR")
	}
	db := testkit.OpenDatabase(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "worker-test-ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	caPath, caKeyPath := filepath.Join(directory, "ca.pem"), filepath.Join(directory, "ca-key.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	secret := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	product, err := openWithDatabase(context.Background(), db, Config{RedisAddress: redis, PublicBaseURL: "http://127.0.0.1:8080", SessionKey: secret(51), CSRFKey: secret(52), MasterKey: secret(53), SourceRoot: t.TempDir(), WorkerCACertificatePath: caPath, WorkerCAPrivateKeyPath: caKeyPath, AllowLocalHTTP: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = product.Close() })
	admin, err := product.Identity.InitializeAdmin(context.Background(), "workeradmin", "worker-integration-password")
	if err != nil {
		t.Fatal(err)
	}
	principal := authorization.Principal{UserID: admin.ID, AuthVersion: admin.AuthVersion}
	node, err := product.Inventory.CreateNode(context.Background(), principal, inventory.NodeInput{Name: "登记测试", WorkerID: "worker-test", Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := product.Enrollment.CreateRegistration(context.Background(), principal, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := product.WorkerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	https := httptest.NewUnstartedServer(product.WorkerHandler)
	https.EnableHTTP2 = true
	https.TLS = serverTLS
	https.StartTLS()
	t.Cleanup(https.Close)
	client := https.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// 公共监听器和明文请求都不能用于换取证书。
	plain := httptest.NewRecorder()
	product.WorkerHandler.ServeHTTP(plain, httptest.NewRequest(http.MethodPost, "http://worker/worker/register", nil))
	if plain.Code != http.StatusUpgradeRequired {
		t.Fatalf("明文登记=%d", plain.Code)
	}
	workerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	requestDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "untrusted-self-claim"}}, workerKey)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(workerCertificateRequest{Token: registration.Token, CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: requestDER}))})
	response, err := client.Post(https.URL+"/worker/register", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	var issued workerCertificateResponse
	err = json.NewDecoder(response.Body).Decode(&issued)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || issued.NodeID != node.ID || issued.WorkerID != node.WorkerID {
		t.Fatalf("登记返回=%d %+v %v", response.StatusCode, issued, err)
	}
	response, err = client.Post(https.URL+"/worker/register", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("注册码可重放=%d", response.StatusCode)
	}
	response, err = client.Post(https.URL+"/worker/rotate", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无证书可轮换=%d", response.StatusCode)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(workerKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair([]byte(issued.Certificate), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	if err != nil {
		t.Fatal(err)
	}
	config := client.Transport.(*http.Transport).TLSClientConfig.Clone()
	config.Certificates = []tls.Certificate{certificate}
	transport := &http.Transport{TLSClientConfig: config, ForceAttemptHTTP2: true}
	t.Cleanup(transport.CloseIdleConnections)
	rpc := platformv1connect.NewWorkerGatewayClient(&http.Client{Transport: transport}, https.URL, connect.WithGRPC())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := rpc.Connect(ctx)
	defer stream.CloseRequest()
	defer stream.CloseResponse()
	if err := stream.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_Hello{Hello: &platformv1.WorkerHello{NodeId: node.ID, WorkerId: node.WorkerID, Version: "test"}}}); err != nil {
		t.Fatal(err)
	}
	welcome, err := stream.Receive()
	if err != nil || welcome.GetWelcome() == nil {
		t.Fatalf("mTLS 握手: %+v %v", welcome, err)
	}
	q := &platformv1.WorkerQualification{QualificationId: "not-verified", Nonce: welcome.GetWelcome().QualificationNonce, ClusterNodeUid: "self-declared", Os: "linux", Arch: "amd64", Build: true, Runtime: true}
	if err := stream.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_Qualification{Qualification: q}}); err != nil {
		t.Fatal(err)
	}
	result, err := stream.Receive()
	if err != nil || result.GetRejection() == nil {
		t.Fatalf("自报资格未拒绝=%+v %v", result, err)
	}
	var state struct {
		BuildReady, RuntimeReady bool
		ClusterNodeUID           string
	}
	if err := db.Table("execution_nodes").Where("id = ?", node.ID).Take(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.BuildReady || state.RuntimeReady || state.ClusterNodeUID != "" {
		t.Fatalf("错误信任自报资格: %+v", state)
	}
	if err := product.Enrollment.RevokeCertificate(context.Background(), principal, issued.Serial); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); err == nil || err == io.EOF {
		t.Fatalf("撤销后连接未中断: %v", err)
	}
}
