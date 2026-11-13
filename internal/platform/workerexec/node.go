package workerexec

// 节点侧 CLI 装配辅助：worker register/run 子命令的参数校验、状态目录、
// 显式 rest.Config 与真实节点事实。网络访问只发生在 Register 显式指定的
// 控制面；绝不读取 kubeconfig、隐式环境默认集群或宿主 Shell。

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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"crypto"

	"github.com/sunshanpeng/yummy/internal/platform/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	stateKeyFile      = "worker-key.pem"
	stateCertFile     = "worker-cert.pem"
	stateCAFile       = "worker-ca.pem"
	stateIdentityFile = "worker-identity.json"
	// registerPath 与控制面 internal/platform/server/worker.go 的登记入口一致。
	registerPath = "/worker/register"
)

// NodeState 是 state 目录中冻结的 worker 身份；run 从这里恢复，不重复登记。
type NodeState struct {
	NodeID      string `json:"nodeId"`
	WorkerID    string `json:"workerId"`
	Serial      string `json:"serial"`
	NotAfter    string `json:"notAfter,omitempty"`
	RotateAfter string `json:"rotateAfter,omitempty"`
	Server      string `json:"server"`
}

// NodeFacts 是来自真实 kube API 的节点事实；心跳与资格上报只使用这些值。
type NodeFacts struct {
	UID, OS, Arch string
}

// RegisterOptions 全部显式注入；ServerCA 为可选的自签/私有 CA PEM。
type RegisterOptions struct {
	Server   string
	Token    string
	StateDir string
	ServerCA []byte
}

// registerResponse 字段对照 server.workerCertificateResponse，只取需要的材料。
type registerResponse struct {
	NodeID      string `json:"nodeId"`
	WorkerID    string `json:"workerId"`
	Serial      string `json:"serial"`
	Certificate string `json:"certificate"`
	CA          string `json:"ca"`
	NotAfter    string `json:"notAfter"`
	RotateAfter string `json:"rotateAfter"`
}

// Register 生成 ECDSA P256 私钥与 CSR，向控制面专用 HTTPS 入口登记并冻结
// 身份材料到 0700/0600 状态目录。证书写盘前先核对该 CA 签发且与私钥匹配。
func Register(ctx context.Context, options RegisterOptions) (NodeState, error) {
	// 与 rotate 共用同一服务端地址校验与"证书由返回CA签发且公钥匹配"核对。
	parsed, err := parseServerURL(options.Server)
	if err != nil {
		return NodeState{}, err
	}
	if options.Token == "" {
		return NodeState{}, errors.New("workerexec: --token 不能为空")
	}
	if options.StateDir == "" {
		return NodeState{}, errors.New("workerexec: --state-dir 不能为空")
	}
	if err := prepareStateDirectory(options.StateDir); err != nil {
		return NodeState{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return NodeState{}, err
	}
	keyPEM, err := privateKeyPEM(key)
	if err != nil {
		return NodeState{}, err
	}
	// 不携带 SAN/KeyUsage/BasicConstraints 扩展：enrollment 只核对签名与密钥强度，
	// 不复制 CSR 身份，显式扩展会被控制面拒绝。
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "yummy-worker"}}, key)
	if err != nil {
		return NodeState{}, err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	rootCAs, err := certPool(options.ServerCA)
	if err != nil {
		return NodeState{}, err
	}
	// 登记只走验证 TLS 的专用入口；绝不关闭证书校验。
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs}, ForceAttemptHTTP2: true, Proxy: nil}, Timeout: 30 * time.Second}
	body, err := json.Marshal(map[string]string{"token": options.Token, "csr": string(csrPEM)})
	if err != nil {
		return NodeState{}, err
	}
	endpoint := *parsed
	endpoint.Path = registerPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return NodeState{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return NodeState{}, fmt.Errorf("workerexec: 登记请求失败: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		return NodeState{}, fmt.Errorf("workerexec: 登记响应读取失败: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return NodeState{}, fmt.Errorf("workerexec: 登记被拒绝: HTTP %d", response.StatusCode)
	}
	var issued registerResponse
	if json.Unmarshal(payload, &issued) != nil || issued.NodeID == "" || issued.WorkerID == "" || issued.Serial == "" || issued.Certificate == "" || issued.CA == "" {
		return NodeState{}, errors.New("workerexec: 登记响应无效")
	}
	certPEM := []byte(issued.Certificate)
	caPEM := []byte(issued.CA)
	if err := verifyIssuedCertificate(certPEM, caPEM, key); err != nil {
		return NodeState{}, err
	}
	state := NodeState{NodeID: issued.NodeID, WorkerID: issued.WorkerID, Serial: issued.Serial, NotAfter: issued.NotAfter, RotateAfter: issued.RotateAfter, Server: options.Server}
	for _, file := range []struct {
		name    string
		content []byte
	}{
		{stateKeyFile, keyPEM}, {stateCertFile, certPEM}, {stateCAFile, caPEM},
	} {
		if err := writeStateFile(options.StateDir, file.name, file.content); err != nil {
			return NodeState{}, err
		}
	}
	identity, err := json.Marshal(state)
	if err != nil {
		return NodeState{}, err
	}
	if err := writeStateFile(options.StateDir, stateIdentityFile, identity); err != nil {
		return NodeState{}, err
	}
	return state, nil
}

// parseServerURL 校验控制面 worker 专用 HTTPS 入口：无路径参数的 HTTPS 地址；
// register 与 rotate 共用同一判定。
func parseServerURL(server string) (*url.URL, error) {
	parsed, err := url.Parse(server)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("workerexec: --server 必须是无路径参数的HTTPS控制面地址")
	}
	return parsed, nil
}

// verifyIssuedCertificate 确认返回证书确实由返回 CA 签发、可用于客户端认证
// 且公钥与本地私钥匹配，避免把不可用材料写入状态目录；register 与 rotate
// 共用该核对。
func verifyIssuedCertificate(certPEM, caPEM []byte, key *ecdsa.PrivateKey) error {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("workerexec: 返回的证书不是PEM格式")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("workerexec: 返回的证书无法解析: %w", err)
	}
	comparable, ok := certificate.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !comparable.Equal(key.Public()) {
		return errors.New("workerexec: 返回证书公钥与本地私钥不匹配")
	}
	roots, err := certPool(caPEM)
	if err != nil {
		return err
	}
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("workerexec: 返回证书未通过返回CA验证: %w", err)
	}
	return nil
}

func certPool(caPEM []byte) (*x509.CertPool, error) {
	if len(caPEM) == 0 {
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("workerexec: CA内容不是有效的PEM证书")
	}
	return pool, nil
}

func privateKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// prepareStateDirectory 确保 0700 普通目录；已存在目录权限过宽直接失败，
// 不静默放宽，避免私钥落盘即泄露。
func prepareStateDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("workerexec: --state-dir 必须是不带链接的普通目录")
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("workerexec: --state-dir 权限必须为0700，当前 %04o", info.Mode().Perm())
	}
	return nil
}

// writeStateFile 以 0600 写入并显式收紧，防止 umask 放宽私钥/证书权限。
func writeStateFile(directory, name string, content []byte) error {
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, content, 0600); err != nil {
		return err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fmt.Errorf("workerexec: %s 必须是0600普通文件", name)
	}
	return nil
}

// LoadNodeState 读取 register 冻结的身份材料；目录或文件权限不达标直接失败。
func LoadNodeState(directory string) ([]byte, []byte, []byte, NodeState, error) {
	if err := prepareStateDirectory(directory); err != nil {
		return nil, nil, nil, NodeState{}, err
	}
	keyPEM, err := readStateFile(directory, stateKeyFile, 0600)
	if err != nil {
		return nil, nil, nil, NodeState{}, err
	}
	certPEM, err := readStateFile(directory, stateCertFile, 0600)
	if err != nil {
		return nil, nil, nil, NodeState{}, err
	}
	caPEM, err := readStateFile(directory, stateCAFile, 0644)
	if err != nil {
		return nil, nil, nil, NodeState{}, err
	}
	identityData, err := readStateFile(directory, stateIdentityFile, 0600)
	if err != nil {
		return nil, nil, nil, NodeState{}, err
	}
	var state NodeState
	if json.Unmarshal(identityData, &state) != nil || state.NodeID == "" || state.WorkerID == "" || state.Serial == "" || state.Server == "" {
		return nil, nil, nil, NodeState{}, errors.New("workerexec: 身份文件无效，请重新执行 worker register")
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil, nil, nil, NodeState{}, fmt.Errorf("workerexec: 证书与私钥不匹配: %w", err)
	}
	return keyPEM, certPEM, caPEM, state, nil
}

func readStateFile(directory, name string, maxPerm os.FileMode) ([]byte, error) {
	path := filepath.Join(directory, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("workerexec: %s 必须是普通文件", name)
	}
	if info.Mode().Perm()&^maxPerm != 0 {
		return nil, fmt.Errorf("workerexec: %s 权限过宽，当前 %04o", name, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > 256<<10 {
		return nil, fmt.Errorf("workerexec: %s 大小越界", name)
	}
	return data, nil
}

// NodeRESTConfig 显式拼接受限 rest.Config：只接受验证 TLS 的 HTTPS API server，
// 令牌来自显式文件；绝不读取 kubeconfig、环境默认集群或 exec/auth 插件
// （kube.NewFromRESTConfig 亦会再次强制这些约束）。
func NodeRESTConfig(apiserver, caFile, tokenFile string) (*rest.Config, error) {
	parsed, err := url.Parse(apiserver)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("workerexec: --kube-apiserver 必须是HTTPS API server地址")
	}
	if caFile == "" || tokenFile == "" {
		return nil, errors.New("workerexec: --kube-ca-file 与 --kube-token-file 必须显式提供")
	}
	caPEM, err := readBoundedFile(caFile, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("workerexec: 读取 --kube-ca-file 失败: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("workerexec: --kube-ca-file 不是有效的PEM证书")
	}
	if _, err := readBoundedFile(tokenFile, 64<<10); err != nil {
		return nil, fmt.Errorf("workerexec: 读取 --kube-token-file 失败: %w", err)
	}
	return &rest.Config{Host: apiserver, TLSClientConfig: rest.TLSClientConfig{Insecure: false, CAData: caPEM}, BearerTokenFile: tokenFile}, nil
}

// readBoundedFile 只接受普通文件；拒绝链接与超限内容，避免TOCTOU式滥用。
func readBoundedFile(name string, limit int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("必须是普通文件")
	}
	if info.Size() > limit || info.Size() == 0 {
		return nil, errors.New("文件为空或超过大小上限")
	}
	return os.ReadFile(name)
}

// InspectNode 从真实 kube API 读取节点 UID/OS/arch；不读取本机缓存或推断值。
func InspectNode(ctx context.Context, client kubernetes.Interface, nodeName string) (NodeFacts, error) {
	if nodeName == "" {
		return NodeFacts{}, errors.New("workerexec: --node-name 不能为空")
	}
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return NodeFacts{}, fmt.Errorf("workerexec: 读取节点 %s 失败: %w", nodeName, err)
	}
	if node.UID == "" {
		return NodeFacts{}, errors.New("workerexec: 节点缺少UID事实")
	}
	return NodeFacts{UID: string(node.UID), OS: node.Status.NodeInfo.OperatingSystem, Arch: node.Status.NodeInfo.Architecture}, nil
}

// qualificationFile 字段对齐控制面 inventory 资格登记（node_qualifications）；
// 缺失能力一律关闭，任何布尔都不能凭 API 连通自证。
type qualificationFile struct {
	QualificationID    string `json:"qualification_id"`
	Build              bool   `json:"build"`
	Runtime            bool   `json:"runtime"`
	Ingress            bool   `json:"ingress"`
	Metrics            bool   `json:"metrics"`
	RunnerImage        string `json:"runner_image"`
	BuildKitImage      string `json:"buildkit_image"`
	SeccompUnconfined  bool   `json:"seccomp_unconfined"`
	AppArmorUnconfined bool   `json:"apparmor_unconfined"`
	NoProcessSandbox   bool   `json:"no_process_sandbox"`
	// AllowPrivilegeEscalation 解除构建容器 no_new_privs，供 setuid 的 newuidmap
	// 完成嵌套 userns 的 UID 映射；仅安装资格记录可显式授权。
	AllowPrivilegeEscalation bool `json:"allow_privilege_escalation"`
	// HostBuild 构建改由 worker 在宿主机直接驱动节点级 rootless BuildKit 守护
	// （不再创建 k8s Job）；部署仍走 Kubernetes pod。仅安装资格记录可显式授权。
	HostBuild bool `json:"host_build"`
}

// digestPattern 与 kube.digestPattern 保持一致（kube 包未导出该判定）。
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func pinnedDigest(image string) bool {
	parts := strings.Split(image, "@")
	return len(parts) == 2 && parts[0] != "" && !strings.ContainsAny(parts[0], " \t\r\n,;") && !strings.HasSuffix(parts[0], ":latest") && digestPattern.MatchString(parts[1])
}

// LoadInstalledCapabilities 读取安装资格文件并叠加 CLI 镜像参数；
// 缺失能力即关闭。无资格文件时全部关闭——TODO(资格发放流程)：当前由安装者
// 线下验收后手写资格文件；后续应改为 run 启动时经控制面 inventory 读取 ACTIVE
// 资格记录并核对其下发 nonce，见 internal/platform/inventory/qualifications.go
// 与 internal/platform/qualification/verifier.go 的发放-验证闭环。
func LoadInstalledCapabilities(path, runnerImage, buildKitImage string) (kube.InstalledCapabilities, error) {
	capabilities := kube.InstalledCapabilities{}
	if path != "" {
		data, err := readBoundedFile(path, 64<<10)
		if err != nil {
			return capabilities, fmt.Errorf("workerexec: 读取 --qualification-file 失败: %w", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var parsed qualificationFile
		if err := decoder.Decode(&parsed); err != nil {
			return capabilities, fmt.Errorf("workerexec: --qualification-file 格式无效: %w", err)
		}
		capabilities = kube.InstalledCapabilities{
			Build:                    parsed.Build,
			Runtime:                  parsed.Runtime,
			Ingress:                  parsed.Ingress,
			Metrics:                  parsed.Metrics,
			QualificationID:          parsed.QualificationID,
			RunnerImage:              parsed.RunnerImage,
			BuildKitImage:            parsed.BuildKitImage,
			SeccompUnconfined:        parsed.SeccompUnconfined,
			AppArmorUnconfined:       parsed.AppArmorUnconfined,
			NoProcessSandbox:         parsed.NoProcessSandbox,
			AllowPrivilegeEscalation: parsed.AllowPrivilegeEscalation,
			HostBuild:                parsed.HostBuild,
		}
	}
	// CLI 镜像参数优先于资格文件记录，便于修复镜像引用而不改动资格登记。
	if runnerImage != "" {
		capabilities.RunnerImage = runnerImage
	}
	if buildKitImage != "" {
		capabilities.BuildKitImage = buildKitImage
	}
	if capabilities.Build && (!pinnedDigest(capabilities.RunnerImage) || !pinnedDigest(capabilities.BuildKitImage)) {
		return kube.InstalledCapabilities{}, errors.New("workerexec: 开启build能力必须提供digest钉住的--runner-image与--buildkit-image")
	}
	return capabilities, nil
}
