// Package enrollment 管理实例节点的一次性登记与受数据库撤销约束的客户端证书。
// 本模块不装配 HTTP/TLS；对端证书的链、ClientAuth 和私钥持有证明由 mTLS 层先验证。
package enrollment

import (
	"errors"
	"time"
)

var (
	ErrCA            = errors.New("节点签发 CA 配置不安全或无效")
	ErrCSR           = errors.New("节点证书请求无效")
	ErrRegistration  = errors.New("节点登记凭证无效或已消费")
	ErrPeer          = errors.New("节点证书身份无效或已撤销")
	ErrRotationEarly = errors.New("节点证书尚未进入轮换窗口")
	ErrTLSRequired   = errors.New("节点登记必须使用 TLS")
)

const (
	RegistrationLifetime = 15 * time.Minute
	CertificateLifetime  = 30 * 24 * time.Hour
	RotationWindow       = 7 * 24 * time.Hour
	maxCSRBytes          = 64 << 10
)

type Config struct {
	CACertificatePath string
	CAPrivateKeyPath  string
	// 仅允许明确标记的本地回环测试，生产适配器不得把此配置暴露为请求参数。
	AllowInsecureLoopback bool
	// OnRevoke 在数据库提交成功后通知连接层关闭 worker 旧证书对应的流。
	// 身份已先在数据库失效；hook 不应执行会阻塞请求的网络操作。
	OnRevoke func(Identity)
}

// Transport 由可信 HTTP 适配器按 r.TLS 和远端地址构造，不能从用户 JSON 绑定。
type Transport struct {
	TLS      bool `json:"-"`
	Loopback bool `json:"-"`
}

type RegisterInput struct {
	Token     string
	CSRPEM    []byte
	Transport Transport `json:"-"`
}

type Registration struct {
	ID        string
	NodeID    string
	Token     string
	ExpiresAt time.Time
}

type Identity struct {
	NodeID   string
	WorkerID string
	Serial   string
	NotAfter time.Time
}

type IssuedCertificate struct {
	Identity         Identity
	CertificatePEM   []byte
	CACertificatePEM []byte
	NotBefore        time.Time
	RotateAfter      time.Time
}
