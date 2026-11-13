package workerexec

// worker 证书轮换：登记证书 30 天有效，到期前轮换窗口（服务端 enrollment.
// RotationWindow，7 天）开始即可经 /worker/rotate 换发新证书。控制面要求
// 请求由当前有效客户端证书的 mTLS 连接携带、请求体只含 {csr}（token 必须
// 为空）、响应与 register 相同 JSON；旧证书在服务端同事务撤销。本文件的
// 流程任何一步失败都保留磁盘旧状态（窗口内旧证书仍有效，可按检查间隔重
// 试），绝不打印私钥、令牌或响应正文。

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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// rotatePath 与控制面 internal/platform/server/worker.go 的轮换入口一致。
	rotatePath = "/worker/rotate"
	// rotationFallbackWindow 对照 enrollment.RotationWindow；身份文件缺失
	// RotateAfter 时用 NotAfter 前推该窗口兜底，与服务端轮换窗口保持一致。
	rotationFallbackWindow = 7 * 24 * time.Hour
)

// CertificateHolder 保存可原子替换的客户端证书。TLS 配置经 GetClientCertificate
// 回调读取证书：workergateway.NewWorkerClient 在构造时 Clone 一次 tls.Config，
// 回调指针被保留，轮换成功后更新 holder 即可让后续握手使用新证书，无需重建
// 客户端（TLS.Certificates 留空但 GetClientCertificate 非空即为合法配置）。
type CertificateHolder struct {
	mu          sync.Mutex
	certificate *tls.Certificate
}

// NewCertificateHolder 从 PEM 材料装配 holder；材料不匹配直接失败，不建立空 holder。
func NewCertificateHolder(certPEM, keyPEM []byte) (*CertificateHolder, error) {
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("workerexec: 证书与私钥不匹配: %w", err)
	}
	return &CertificateHolder{certificate: &certificate}, nil
}

// GetClientCertificate 作为 tls.Config.GetClientCertificate 回调；无证书时失败关闭。
func (h *CertificateHolder) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	h.mu.Lock()
	certificate := h.certificate
	h.mu.Unlock()
	if certificate == nil {
		return nil, errors.New("workerexec: 客户端证书尚未装配")
	}
	return certificate, nil
}

// Update 原子替换持有的证书；新材料解析失败时保持旧证书不变。
func (h *CertificateHolder) Update(certPEM, keyPEM []byte) error {
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("workerexec: 新证书与私钥不匹配: %w", err)
	}
	h.mu.Lock()
	h.certificate = &certificate
	h.mu.Unlock()
	return nil
}

// RotateOptions 全部显式注入；ServerCA 为可选的自签/私有 CA PEM（与 register
// 的 --server-ca-file 同源），服务端校验同时信任状态目录记录的 CA。
type RotateOptions struct {
	Server   string
	StateDir string
	ServerCA []byte
}

// RotateCertificate 用状态目录中的当前证书向控制面轮换客户端证书：生成新
// ECDSA P256 私钥与 CSR（与登记同款：CN yummy-worker、不携带扩展）、携带当前
// 证书 POST {server}/worker/rotate、核对响应证书由返回 CA 签发且公钥与新私钥
// 匹配后，以临时文件+rename 原子替换 worker-key.pem/worker-cert.pem/worker-ca.pem/
// worker-identity.json（0600，identity 最后写入作为提交点）。任何一步失败都
// 保留磁盘旧状态并返回错误，由调用方按检查间隔重试。
func RotateCertificate(ctx context.Context, options RotateOptions) (NodeState, error) {
	if options.StateDir == "" {
		return NodeState{}, errors.New("workerexec: 轮换需要显式状态目录")
	}
	// 以磁盘上的材料为准：mTLS 必须携带服务端登记在册的当前证书，重放其他
	// 材料只会被拒绝。
	keyPEM, certPEM, stateCAPEM, current, err := LoadNodeState(options.StateDir)
	if err != nil {
		return NodeState{}, err
	}
	server := options.Server
	if server == "" {
		server = current.Server
	}
	parsed, err := parseServerURL(server)
	if err != nil {
		return NodeState{}, err
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return NodeState{}, fmt.Errorf("workerexec: 证书与私钥不匹配: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return NodeState{}, err
	}
	newKeyPEM, err := privateKeyPEM(key)
	if err != nil {
		return NodeState{}, err
	}
	// 与 Register 相同：不携带 SAN/KeyUsage/BasicConstraints 扩展，显式扩展会被控制面拒绝。
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "yummy-worker"}}, key)
	if err != nil {
		return NodeState{}, err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	rootCAs, err := rotateRootCAs(stateCAPEM, options.ServerCA)
	if err != nil {
		return NodeState{}, err
	}
	// 轮换必须携带当前客户端证书；CA 校验与登记同款，绝不关闭证书校验。
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs, Certificates: []tls.Certificate{certificate}}, ForceAttemptHTTP2: true, Proxy: nil}, Timeout: 30 * time.Second}
	// 服务端 DisallowUnknownFields：请求体只允许 {csr}，不得携带 token。
	body, err := json.Marshal(map[string]string{"csr": string(csrPEM)})
	if err != nil {
		return NodeState{}, err
	}
	endpoint := *parsed
	endpoint.Path = rotatePath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return NodeState{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return NodeState{}, fmt.Errorf("workerexec: 轮换请求失败: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		return NodeState{}, fmt.Errorf("workerexec: 轮换响应读取失败: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		// 只上报状态码：409 表示尚未进入轮换窗口，401/400 由下一间隔重试兜底。
		return NodeState{}, fmt.Errorf("workerexec: 轮换被拒绝: HTTP %d", response.StatusCode)
	}
	var issued registerResponse
	if json.Unmarshal(payload, &issued) != nil || issued.NodeID == "" || issued.WorkerID == "" || issued.Serial == "" || issued.Certificate == "" || issued.CA == "" {
		return NodeState{}, errors.New("workerexec: 轮换响应无效")
	}
	newCertPEM := []byte(issued.Certificate)
	newCAPEM := []byte(issued.CA)
	if err := verifyIssuedCertificate(newCertPEM, newCAPEM, key); err != nil {
		return NodeState{}, err
	}
	state := NodeState{NodeID: issued.NodeID, WorkerID: issued.WorkerID, Serial: issued.Serial, NotAfter: issued.NotAfter, RotateAfter: issued.RotateAfter, Server: server}
	// 写盘顺序 key → cert → ca → identity：identity 最后写入作为提交点，
	// 任何一步失败都保留旧材料，旧证书在窗口内仍有效可重试。
	for _, file := range []struct {
		name    string
		content []byte
	}{
		{stateKeyFile, newKeyPEM}, {stateCertFile, newCertPEM}, {stateCAFile, newCAPEM},
	} {
		if err := replaceStateFile(options.StateDir, file.name, file.content); err != nil {
			return NodeState{}, err
		}
	}
	identity, err := json.Marshal(state)
	if err != nil {
		return NodeState{}, err
	}
	if err := replaceStateFile(options.StateDir, stateIdentityFile, identity); err != nil {
		return NodeState{}, err
	}
	return state, nil
}

// rotateRootCAs 合并状态目录 CA 与显式 ServerCA；两者都缺省时返回 nil 使用系统池，
// 服务端信任来源与 run 期网关客户端（状态目录 CA）保持一致。
func rotateRootCAs(stateCAPEM, serverCAPEM []byte) (*x509.CertPool, error) {
	var pool *x509.CertPool
	for _, caPEM := range [][]byte{stateCAPEM, serverCAPEM} {
		if len(caPEM) == 0 {
			continue
		}
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("workerexec: CA内容不是有效的PEM证书")
		}
	}
	return pool, nil
}

// replaceStateFile 以同目录临时文件+rename 原子替换状态文件，目标始终 0600；
// 写入或校验失败时旧文件保持原样，不留半成品。
func replaceStateFile(directory, name string, content []byte) (err error) {
	file, err := os.CreateTemp(directory, ".replace-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() {
		_ = file.Close()
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	if _, err = file.Write(content); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(path, filepath.Join(directory, name)); err != nil {
		return err
	}
	if err = os.Chmod(filepath.Join(directory, name), 0600); err != nil {
		return err
	}
	info, statErr := os.Lstat(filepath.Join(directory, name))
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		err = fmt.Errorf("workerexec: %s 必须是0600普通文件", name)
		return err
	}
	return nil
}

// RotationManagerConfig 全部显式注入。
type RotationManagerConfig struct {
	// Server 控制面 worker 专用 HTTPS 入口；空则使用状态目录身份文件记录值。
	Server string
	// StateDir register 冻结的状态目录（0700）。
	StateDir string
	// ServerCA 可选控制面 CA PEM（与 register --server-ca-file 同源）。
	ServerCA []byte
	// RotationCheckInterval 到期检查间隔（默认 1 小时）；轮换失败按同一间隔重试。
	RotationCheckInterval time.Duration
	// OnRotationError 轮换失败的观测出口（不影响运行，仅在窗口内重试）。
	OnRotationError func(error)
}

// RotationManager 周期检查身份文件的 RotateAfter：到达轮换点即执行
// RotateCertificate 并回填证书 holder，使后续握手使用新证书；失败只经
// OnRotationError 观测并按检查间隔重试（服务端 7 天窗口提供充足兜底）。
// ctx 取消后 Run 返回，不留下未退出的定时器。
type RotationManager struct {
	options  RotateOptions
	interval time.Duration
	holder   *CertificateHolder
	onError  func(error)
	mu       sync.Mutex
	state    NodeState
}

// NewRotationManager 装配轮换管理器：启动即读取状态目录身份文件（拿到
// RotateAfter 与缺省 Server），材料缺失或非法直接失败。
func NewRotationManager(config RotationManagerConfig, holder *CertificateHolder) (*RotationManager, error) {
	if holder == nil {
		return nil, errors.New("workerexec: 轮换管理器需要证书holder")
	}
	if config.StateDir == "" {
		return nil, errors.New("workerexec: 轮换管理器需要显式状态目录")
	}
	if config.RotationCheckInterval < 0 {
		return nil, errors.New("workerexec: 轮换检查间隔不能为负")
	}
	_, _, _, state, err := LoadNodeState(config.StateDir)
	if err != nil {
		return nil, err
	}
	server := config.Server
	if server == "" {
		server = state.Server
	}
	interval := config.RotationCheckInterval
	if interval == 0 {
		interval = time.Hour
	}
	return &RotationManager{
		options:  RotateOptions{Server: server, StateDir: config.StateDir, ServerCA: config.ServerCA},
		interval: interval,
		holder:   holder,
		onError:  config.OnRotationError,
		state:    state,
	}, nil
}

// State 返回最近一次已知的身份状态（轮换成功后为新证书身份）。
func (m *RotationManager) State() NodeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Run 周期执行轮换检查；启动时立即检查一次，覆盖 run 重启时证书已进入
// 轮换窗口的情况。ctx 取消后返回 nil。
func (m *RotationManager) Run(ctx context.Context) error {
	m.rotateOnce(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.rotateOnce(ctx)
		}
	}
}

func (m *RotationManager) rotateOnce(ctx context.Context) {
	if ctx.Err() != nil || !m.shouldRotate(time.Now()) {
		return
	}
	state, err := RotateCertificate(ctx, m.options)
	if err != nil {
		m.report(fmt.Errorf("workerexec: 证书轮换失败，将在下一检查间隔重试: %w", err))
		return
	}
	// holder 与磁盘保持同源：材料刚通过完整校验并原子落盘，此处失败只可能
	// 是本地读回异常，保持旧证书并由下一间隔重试。
	keyPEM, certPEM, _, _, err := LoadNodeState(m.options.StateDir)
	if err != nil {
		m.report(fmt.Errorf("workerexec: 读取轮换后的身份材料失败: %w", err))
		return
	}
	if err := m.holder.Update(certPEM, keyPEM); err != nil {
		m.report(err)
		return
	}
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
}

// shouldRotate 判断当前时间是否已到轮换点。
func (m *RotationManager) shouldRotate(now time.Time) bool {
	deadline, ok := rotationDeadline(m.State())
	return ok && !now.Before(deadline)
}

func (m *RotationManager) report(err error) {
	if err != nil && m.onError != nil {
		m.onError(err)
	}
}

// rotationDeadline 以身份文件记录的 RotateAfter 为准；缺失或无法解析时用
// NotAfter 前推 rotationFallbackWindow 兜底（与 enrollment.RotationWindow 对
// 齐），两者都缺失则放弃自动轮换，维持重新登记的人工兜底路径。
func rotationDeadline(state NodeState) (time.Time, bool) {
	if state.RotateAfter != "" {
		if deadline, err := time.Parse(time.RFC3339, state.RotateAfter); err == nil {
			return deadline, true
		}
	}
	if state.NotAfter != "" {
		if expiry, err := time.Parse(time.RFC3339, state.NotAfter); err == nil {
			return expiry.Add(-rotationFallbackWindow), true
		}
	}
	return time.Time{}, false
}
