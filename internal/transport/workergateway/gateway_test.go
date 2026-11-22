package workergateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/domains/build"
	"github.com/sunshanpeng/yummy/internal/domains/enrollment"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
	"gorm.io/gorm"
)

type tlsEnvironment struct {
	db        *gorm.DB
	gateway   *Gateway
	enroll    *enrollment.Service
	principal authorization.Principal
	identity  enrollment.Identity
	client    *http.Client
	tlsConfig *tls.Config
	url       string
}

func setupTLS(t *testing.T, mutate func(*Config)) tlsEnvironment {
	t.Helper()
	db := testkit.OpenDatabase(t)
	if err := db.Create(&model.User{ID: "root", Username: "root", PasswordHash: "hash", InstanceAdmin: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO instance_settings(singleton,id) VALUES(TRUE,'instance'); INSERT INTO execution_nodes(id,instance_id,name,worker_id) VALUES('node','instance','节点','worker-1')`).Error; err != nil {
		t.Fatal(err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gateway-test-ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	caPath, keyPath := filepath.Join(directory, "ca.pem"), filepath.Join(directory, "ca-key.pem")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	var gateway *Gateway
	enroll, err := enrollment.NewService(db, access, enrollment.Config{CACertificatePath: caPath, CAPrivateKeyPath: keyPath, OnRevoke: func(identity enrollment.Identity) {
		if gateway != nil {
			gateway.Revoke(identity.NodeID, identity.Serial)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	principal := authorization.Principal{UserID: "root", AuthVersion: 1}
	registration, err := enroll.CreateRegistration(context.Background(), principal, "node")
	if err != nil {
		t.Fatal(err)
	}
	workerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "worker-1"}}, workerKey)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := enroll.Register(context.Background(), enrollment.RegisterInput{Token: registration.Token, CSRPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), Transport: enrollment.Transport{TLS: true}})
	if err != nil {
		t.Fatal(err)
	}
	workerDER, err := x509.MarshalPKCS8PrivateKey(workerKey)
	if err != nil {
		t.Fatal(err)
	}
	workerCertificate, err := tls.X509KeyPair(issued.CertificatePEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: workerDER}))
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverPrivate, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverPrivate}))
	if err != nil {
		t.Fatal(err)
	}
	config := Config{DB: db, Peers: enroll, ServerID: "server", ScheduleInterval: 50 * time.Millisecond}
	if mutate != nil {
		mutate(&config)
	}
	gateway, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gateway.Close)
	path, handler := gateway.Handler()
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverCertificate}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	tlsConfig := &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{workerCertificate}, MinVersion: tls.VersionTLS12}
	transport := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}
	client := &http.Client{Transport: transport}
	t.Cleanup(transport.CloseIdleConnections)
	return tlsEnvironment{db: db, gateway: gateway, enroll: enroll, principal: principal, identity: issued.Identity, client: client, tlsConfig: tlsConfig, url: server.URL}
}
func openStream(t *testing.T, env tlsEnvironment, node, worker string) (*connect.BidiStreamForClient[platformv1.WorkerEnvelope, platformv1.ControlEnvelope], context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	client := platformv1connect.NewWorkerGatewayClient(env.client, env.url, connect.WithGRPC())
	stream := client.Connect(ctx)
	if err := stream.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_Hello{Hello: &platformv1.WorkerHello{NodeId: node, WorkerId: worker, Version: "test"}}}); err != nil {
		cancel()
		t.Fatal(err)
	}
	return stream, func() { cancel(); _ = stream.CloseRequest(); _ = stream.CloseResponse() }
}
func waitUntil(t *testing.T, fn func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("等待条件超时")
		case <-ticker.C:
		}
	}
}
func TestTLSBidiRejectsForgedHelloAndMissingVerifiedChain(t *testing.T) {
	env := setupTLS(t, nil)
	stream, cancel := openStream(t, env, "forged-node", "worker-1")
	defer cancel()
	if _, err := stream.Receive(); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("伪造Hello=%v", err)
	}
	_, handler := env.gateway.Handler()
	request := httptest.NewRequest(http.MethodPost, "https://localhost/platform.v1.WorkerGateway/Connect", nil)
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("缺verifiedChains=%d", response.Code)
	}
}
func TestGatewayCloseWaitsForStreamsAndClearsPresence(t *testing.T) {
	env := setupTLS(t, nil)
	stream, cancel := openStream(t, env, "node", "worker-1")
	defer cancel()
	if _, err := stream.Receive(); err != nil {
		t.Fatal(err)
	}
	env.gateway.Close()
	waited := make(chan struct{})
	go func() { env.gateway.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("关闭网关未等待流清理完成")
	}
	var state struct{ ConnectedAt *time.Time }
	if err := env.db.Table("execution_nodes").Where("id='node'").Take(&state).Error; err != nil || state.ConnectedAt != nil {
		t.Fatalf("关闭后仍残留在线事实: %+v %v", state, err)
	}
	if _, err := stream.Receive(); err == nil {
		t.Fatal("已关闭网关仍保持连接")
	}
	client := platformv1connect.NewWorkerGatewayClient(env.client, env.url, connect.WithGRPC())
	if _, err := client.CheckGeneration(context.Background(), connect.NewRequest(&platformv1.WorkerGenerationRequest{})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("关闭后仍接受业务请求: %v", err)
	}
}

// TLS 已认证但迟迟不发送 Hello 的流也必须在关闭时退出。
func TestGatewayCloseInterruptsPreHelloStream(t *testing.T) {
	env := setupTLS(t, nil)
	verified := make(chan struct{})
	env.gateway.config.Peers = signalingPeerVerifier{PeerVerifier: env.enroll, verified: verified}
	reader, writer := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, env.url+"/platform.v1.WorkerGateway/Connect", reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	result := make(chan struct{})
	go func() {
		response, _ := env.client.Do(req)
		if response != nil {
			_ = response.Body.Close()
		}
		close(result)
	}()
	select {
	case <-verified:
	case <-time.After(5 * time.Second):
		_ = writer.Close()
		t.Fatal("请求未进入已认证流")
	}
	env.gateway.Close()
	done := make(chan struct{})
	go func() { env.gateway.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = writer.Close()
		t.Fatal("关闭没有中断首包前的流")
	}
	_ = writer.Close()
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端请求未退出")
	}
}

type signalingPeerVerifier struct {
	PeerVerifier
	verified chan struct{}
}

func (v signalingPeerVerifier) VerifyPeer(ctx context.Context, certificate *x509.Certificate) (enrollment.Identity, error) {
	identity, err := v.PeerVerifier.VerifyPeer(ctx, certificate)
	if err == nil {
		close(v.verified)
	}
	return identity, err
}

func TestDuplicateConnectionEpochPreventsOldDisconnectOverwritingNewPresence(t *testing.T) {
	env := setupTLS(t, nil)
	first, cancelFirst := openStream(t, env, "node", "worker-1")
	defer cancelFirst()
	welcome1, err := first.Receive()
	if err != nil {
		t.Fatal(err)
	}
	second, cancelSecond := openStream(t, env, "node", "worker-1")
	defer cancelSecond()
	welcome2, err := second.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if welcome2.GetWelcome().ConnectionEpoch <= welcome1.GetWelcome().ConnectionEpoch || welcome1.GetWelcome().QualificationNonce == welcome2.GetWelcome().QualificationNonce {
		t.Fatal("连接epoch/nonce没有轮换")
	}
	if _, err := first.Receive(); err == nil {
		t.Fatal("旧流未关闭")
	}
	waitUntil(t, func() bool {
		var state struct {
			ConnectionEpoch int64
			ConnectionOwner string
			ConnectedAt     *time.Time
		}
		if err := env.db.Table("execution_nodes").Where("id='node'").Take(&state).Error; err != nil {
			return false
		}
		return state.ConnectionEpoch == welcome2.GetWelcome().ConnectionEpoch && state.ConnectionOwner != "" && state.ConnectedAt != nil
	})
	if err := second.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_Heartbeat{Heartbeat: &platformv1.WorkerHeartbeat{ClusterNodeUid: "unverified-uid", Os: "linux", Arch: "amd64"}}}); err != nil {
		t.Fatal(err)
	}
	if err := second.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_Qualification{Qualification: &platformv1.WorkerQualification{QualificationId: "fake", Nonce: welcome2.GetWelcome().QualificationNonce, ClusterNodeUid: "unverified-uid", Os: "linux", Arch: "amd64", Build: true}}}); err != nil {
		t.Fatal(err)
	}
	rejection, err := second.Receive()
	if err != nil || rejection.GetRejection() == nil {
		t.Fatalf("缺Verifier不应通过=%#v,%v", rejection, err)
	}
	var state struct {
		BuildReady     bool
		ClusterNodeUID string
		LastSeen       *time.Time
	}
	if err := env.db.Table("execution_nodes").Where("id='node'").Take(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.BuildReady || state.ClusterNodeUID != "" || state.LastSeen == nil {
		t.Fatalf("自报资格被信任=%#v", state)
	}
}
func TestRevocationClosesExistingBidiAndRejectsFurtherMessages(t *testing.T) {
	env := setupTLS(t, nil)
	stream, cancel := openStream(t, env, "node", "worker-1")
	defer cancel()
	if _, err := stream.Receive(); err != nil {
		t.Fatal(err)
	}
	if err := env.enroll.RevokeCertificate(context.Background(), env.principal, env.identity.Serial); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); err == nil {
		t.Fatal("撤销未关闭流")
	}
	next, cancelNext := openStream(t, env, "node", "worker-1")
	defer cancelNext()
	if _, err := next.Receive(); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("撤销证书重新连接=%v", err)
	}
}

type fakeBuild struct {
	mu       sync.Mutex
	dispatch build.Dispatch
	sent     bool
	events   map[int64]build.Event
	logs     map[int64]string
}

func (f *fakeBuild) Claim(context.Context, build.WorkerIdentity) (build.Dispatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent {
		return build.Dispatch{}, build.ErrNoCapacity
	}
	f.sent = true
	return f.dispatch, nil
}
func (f *fakeBuild) PendingDispatch(context.Context, build.WorkerIdentity, int) ([]build.Dispatch, error) {
	return nil, nil
}
func (f *fakeBuild) ApplyEvent(_ context.Context, _ build.WorkerIdentity, value build.Event) (build.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.events == nil {
		f.events = map[int64]build.Event{}
	}
	old, duplicate := f.events[value.Seq]
	if duplicate && old != value {
		return build.Ack{}, workerprotocol.ErrProtocol
	}
	f.events[value.Seq] = value
	return build.Ack{Seq: value.Seq, Duplicate: duplicate, State: "RUNNING"}, nil
}
func (f *fakeBuild) AppendLogs(_ context.Context, _ build.WorkerIdentity, value build.LogInput) (build.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logs == nil {
		f.logs = map[int64]string{}
	}
	old, duplicate := f.logs[value.Seq]
	if duplicate && old != string(value.Content) {
		return build.Ack{}, workerprotocol.ErrProtocol
	}
	f.logs[value.Seq] = string(value.Content)
	return build.Ack{Seq: value.Seq, Duplicate: duplicate}, nil
}
func TestBuildLogAcknowledgesIdenticalRetransmissionAndRejectsConflictingPayload(t *testing.T) {
	backend := &fakeBuild{sent: true}
	env := setupTLS(t, func(config *Config) { config.Builds = backend })
	stream, cancel := openStream(t, env, "node", "worker-1")
	defer cancel()
	if _, err := stream.Receive(); err != nil {
		t.Fatal(err)
	}
	for index, content := range []string{"first", "first", "changed"} {
		if err := stream.Send(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_BuildLog{BuildLog: &platformv1.WorkerBuildLog{AttemptId: "attempt", Seq: 1, Content: []byte(content)}}}); err != nil {
			t.Fatal(err)
		}
		message, err := stream.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 && (message.GetAck() == nil || message.GetAck().Duplicate) {
			t.Fatal(message)
		}
		if index == 1 && (message.GetAck() == nil || !message.GetAck().Duplicate) {
			t.Fatal(message)
		}
		if index == 2 && message.GetRejection() == nil {
			t.Fatal("冲突内容未拒绝")
		}
	}
}
