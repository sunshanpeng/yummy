package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/sunshanpeng/yummy/internal/platform/deployment"
	"github.com/sunshanpeng/yummy/internal/platform/enrollment"
	"github.com/sunshanpeng/yummy/internal/platform/security"
	"github.com/sunshanpeng/yummy/internal/platform/workergateway"
)

// workerGenerationGuard 将每次 Kubernetes 写入前的检查连接到持久化运行意图。
type workerGenerationGuard struct{ service *deployment.Service }

func (g workerGenerationGuard) Check(ctx context.Context, identity deployment.WorkerIdentity, value workergateway.GenerationCheck) error {
	return g.service.CheckGeneration(ctx, identity, value.WorkspaceID, value.ApplicationID, value.OperationID, value.ReleaseID, value.Generation, value.ClusterNodeUID)
}

type workerCertificateRequest struct {
	Token string `json:"token,omitempty"`
	CSR   string `json:"csr"`
}

type workerCertificateResponse struct {
	NodeID      string `json:"nodeId"`
	WorkerID    string `json:"workerId"`
	Serial      string `json:"serial"`
	Certificate string `json:"certificate"`
	CA          string `json:"ca"`
	NotAfter    string `json:"notAfter"`
	RotateAfter string `json:"rotateAfter"`
}

// workerHandler 只能绑定专用 HTTPS 监听器，TLS 握手允许无证书以便使用一次性注册码；
// 注册以外的操作逐次要求已验证的客户端证书，网关再次核对证书登记状态。
func workerHandler(service *enrollment.Service, gateway *workergateway.Gateway) http.Handler {
	path, stream := gateway.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.TLS == nil {
			http.Error(w, "需要 TLS", http.StatusUpgradeRequired)
			return
		}
		if strings.HasPrefix(r.URL.Path, path) {
			stream.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || (r.URL.Path != "/worker/register" && r.URL.Path != "/worker/rotate") {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 96<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var input workerCertificateRequest
		if decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "无效登记请求", http.StatusBadRequest)
			return
		}
		var issued enrollment.IssuedCertificate
		var err error
		if r.URL.Path == "/worker/register" {
			issued, err = service.Register(r.Context(), enrollment.RegisterInput{Token: input.Token, CSRPEM: []byte(input.CSR), Transport: enrollment.Transport{TLS: true}})
		} else {
			if input.Token != "" || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 || !r.TLS.VerifiedChains[0][0].Equal(r.TLS.PeerCertificates[0]) {
				http.Error(w, "需要已验证的客户端证书", http.StatusUnauthorized)
				return
			}
			issued, err = service.Rotate(r.Context(), r.TLS.PeerCertificates[0], []byte(input.CSR))
		}
		if err != nil {
			if errors.Is(err, enrollment.ErrRotationEarly) {
				http.Error(w, "证书尚未进入轮换窗口", http.StatusConflict)
			} else if errors.Is(err, enrollment.ErrCSR) {
				http.Error(w, "证书请求无效", http.StatusBadRequest)
			} else {
				// 不区分不存在、过期或撤销，避免登记凭证枚举。
				http.Error(w, "登记或证书验证失败", http.StatusUnauthorized)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(workerCertificateResponse{NodeID: issued.Identity.NodeID, WorkerID: issued.Identity.WorkerID, Serial: issued.Identity.Serial, Certificate: string(issued.CertificatePEM), CA: string(issued.CACertificatePEM), NotAfter: issued.Identity.NotAfter.Format("2006-01-02T15:04:05Z07:00"), RotateAfter: issued.RotateAfter.Format("2006-01-02T15:04:05Z07:00")})
	})
}

func newWorkerGateway(dbConfig workergateway.Config) (*workergateway.Gateway, error) {
	id, err := security.NewID("srv")
	if err != nil {
		return nil, err
	}
	dbConfig.ServerID = id
	return workergateway.New(dbConfig)
}

// WorkerTLSConfig 不读取用户集群配置。服务端证书由监听器显式加载。
func (s *Server) WorkerTLSConfig() (*tls.Config, error) {
	if s.Enrollment == nil || s.WorkerHandler == nil {
		return nil, errors.New("worker 登记 CA 未配置")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: s.Enrollment.ClientCAPool(), NextProtos: []string{"h2", "http/1.1"}}, nil
}
