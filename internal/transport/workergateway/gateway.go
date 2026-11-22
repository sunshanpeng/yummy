package workergateway

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/domains/enrollment"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
)

type peerKey struct{}
type transportKey struct{}
type streamTransport struct {
	stop func()
}
type session struct {
	identity     enrollment.Identity
	certificate  *x509.Certificate
	epoch        int64
	owner, nonce string
	ctx          context.Context
	cancel       context.CancelFunc
	queue        chan *platformv1.ControlEnvelope
}
type Gateway struct {
	config   Config
	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
	streams  sync.WaitGroup
	closing  chan struct{}
}

func New(config Config) (*Gateway, error) {
	if config.DB == nil || config.Peers == nil || config.ServerID == "" {
		return nil, workerprotocol.ErrProtocol
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = 256
	}
	if config.MaxSessions < 1 || config.MaxSessions > 10000 {
		return nil, workerprotocol.ErrProtocol
	}
	if config.QueueSize == 0 {
		config.QueueSize = 64
	}
	if config.QueueSize < 4 || config.QueueSize > 1024 {
		return nil, workerprotocol.ErrProtocol
	}
	if config.ScheduleInterval == 0 {
		config.ScheduleInterval = 5 * time.Second
	}
	if config.ScheduleInterval < 10*time.Millisecond || config.ScheduleInterval > time.Minute {
		return nil, workerprotocol.ErrProtocol
	}
	return &Gateway{config: config, sessions: make(map[string]*session), closing: make(chan struct{})}, nil
}
func (g *Gateway) Handler() (string, http.Handler) {
	path, handler := platformv1connect.NewWorkerGatewayHandler(g, connect.WithReadMaxBytes(1<<20), connect.WithSendMaxBytes(1<<20))
	return path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "需要已验证的客户端证书", http.StatusUnauthorized)
			return
		}
		leaf := r.TLS.PeerCertificates[0]
		verified := false
		for _, chain := range r.TLS.VerifiedChains {
			if len(chain) > 0 && chain[0].Equal(leaf) {
				verified = true
				break
			}
		}
		if !verified {
			http.Error(w, "客户端证书链无效", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), peerKey{}, leaf)
		controller := http.NewResponseController(w)
		// 所有停止路径共享同一完成信号；handler 返回前等待回调完成，
		// 避免 AfterFunc 在 net/http 已回收 ResponseWriter 后仍设置 deadline。
		var stopOnce sync.Once
		stopped := make(chan struct{})
		stop := func() {
			stopOnce.Do(func() {
				defer close(stopped)
				_ = controller.SetWriteDeadline(time.Now())
				_ = controller.SetReadDeadline(time.Now())
				_ = r.Body.Close()
			})
			<-stopped
		}
		ctx = context.WithValue(ctx, transportKey{}, streamTransport{stop: stop})
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
}
func peer(ctx context.Context) (*x509.Certificate, error) {
	certificate, ok := ctx.Value(peerKey{}).(*x509.Certificate)
	if !ok || certificate == nil {
		return nil, workerprotocol.ErrIdentity
	}
	return certificate, nil
}
func (g *Gateway) check(ctx context.Context, s *session) error {
	identity, err := g.config.Peers.VerifyPeer(ctx, s.certificate)
	if err != nil {
		return workerprotocol.ErrIdentity
	}
	if identity.NodeID != s.identity.NodeID || identity.WorkerID != s.identity.WorkerID || identity.Serial != s.identity.Serial {
		return workerprotocol.ErrIdentity
	}
	var count int64
	if err := g.config.DB.WithContext(ctx).Table("execution_nodes").Where("id = ? AND worker_id = ? AND connection_epoch = ? AND connection_owner = ? AND status IN ('ACTIVE','DRAINING')", identity.NodeID, identity.WorkerID, s.epoch, s.owner).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return workerprotocol.ErrIdentity
	}
	return nil
}
func (g *Gateway) enqueue(s *session, message *platformv1.ControlEnvelope) error {
	select {
	case <-s.ctx.Done():
		return workerprotocol.ErrIdentity
	case s.queue <- message:
		return nil
	default:
		s.cancel()
		return workerprotocol.ErrSpoolFull
	}
}
func (g *Gateway) report(err error) {
	if err != nil && g.config.OnError != nil {
		g.config.OnError(err)
	}
}

// Revoke可直接桥接enrollment.OnRevoke；只关闭匹配证书的流，不会误关已轮换的新证书。
func (g *Gateway) Revoke(nodeID, serial string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if current := g.sessions[nodeID]; current != nil && (serial == "" || current.identity.Serial == serial) {
		current.cancel()
	}
}

// Close 阻止后续认证流并取消现有流；调用 Wait 等待异步数据库清理完成。
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	close(g.closing)
	for _, value := range g.sessions {
		value.cancel()
	}
}

// Wait 必须在 Close 后调用，使关闭数据库不会与流的最终 presence 清理并发。
func (g *Gateway) Wait() { g.streams.Wait() }
func (g *Gateway) Connect(ctx context.Context, stream *connect.BidiStream[platformv1.WorkerEnvelope, platformv1.ControlEnvelope]) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return connect.NewError(connect.CodeUnavailable, workerprotocol.ErrIdentity)
	}
	g.streams.Add(1)
	g.mu.Unlock()
	defer g.streams.Done()
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	transport, _ := ctx.Value(transportKey{}).(streamTransport)
	// 首条 Hello 之前也要能中断阻塞读；Close 不应被未完成握手的客户端拖住。
	stopClosing := context.AfterFunc(connectionCtx, func() {
		if transport.stop != nil {
			transport.stop()
		}
	})
	defer func() {
		// 正常返回时先注销回调，让 Connect 的业务错误作为 gRPC trailers 发出；
		// 已经启动的取消回调仍需等待，防止 handler 返回后访问 ResponseWriter。
		if !stopClosing() && transport.stop != nil {
			transport.stop()
		}
		cancel()
	}()
	go func() {
		select {
		case <-g.closing:
			cancel()
		case <-connectionCtx.Done():
		}
	}()
	ctx = connectionCtx
	certificate, err := peer(ctx)
	if err != nil {
		return connect.NewError(connect.CodeUnauthenticated, workerprotocol.ErrIdentity)
	}
	identity, err := g.config.Peers.VerifyPeer(ctx, certificate)
	if err != nil {
		return connect.NewError(connect.CodeUnauthenticated, workerprotocol.ErrIdentity)
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || hello.WorkerId != identity.WorkerID || hello.NodeId != identity.NodeID || len(hello.Version) > 128 {
		return connect.NewError(connect.CodeUnauthenticated, workerprotocol.ErrIdentity)
	}
	owner, err := security.NewID("conn")
	if err != nil {
		return err
	}
	nonce, err := security.NewToken("qual_")
	if err != nil {
		return err
	}
	s := &session{identity: identity, certificate: certificate, owner: owner, nonce: nonce, ctx: connectionCtx, cancel: cancel, queue: make(chan *platformv1.ControlEnvelope, g.config.QueueSize)}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return connect.NewError(connect.CodeUnavailable, workerprotocol.ErrIdentity)
	}
	old := g.sessions[identity.NodeID]
	if old == nil && len(g.sessions) >= g.config.MaxSessions {
		g.mu.Unlock()
		return connect.NewError(connect.CodeResourceExhausted, workerprotocol.ErrSpoolFull)
	}
	if err := g.openPresence(connectionCtx, s); err != nil {
		g.mu.Unlock()
		return connect.NewError(connect.CodeUnauthenticated, workerprotocol.ErrIdentity)
	}
	g.sessions[identity.NodeID] = s
	if old != nil {
		old.cancel()
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		if g.sessions[identity.NodeID] == s {
			delete(g.sessions, identity.NodeID)
		}
		g.mu.Unlock()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g.report(g.closePresence(cleanup, s))
	}()
	writerErr := make(chan error, 1)
	writerDone := make(chan struct{})
	defer func() {
		cancel()
		if transport.stop != nil {
			transport.stop()
		}
		<-writerDone
	}()
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-connectionCtx.Done():
				writerErr <- connectionCtx.Err()
				return
			case message := <-s.queue:
				if err := g.check(connectionCtx, s); err != nil {
					cancel()
					writerErr <- err
					return
				}
				if err := stream.Send(message); err != nil {
					cancel()
					writerErr <- err
					return
				}
			}
		}
	}()
	if err := g.enqueue(s, &platformv1.ControlEnvelope{Payload: &platformv1.ControlEnvelope_Welcome{Welcome: &platformv1.WorkerWelcome{WorkerId: identity.WorkerID, NodeId: identity.NodeID, ServerId: g.config.ServerID, ConnectionEpoch: s.epoch, QualificationNonce: s.nonce}}}); err != nil {
		return err
	}
	receiveErr := make(chan error, 1)
	receiverDone := make(chan struct{})
	defer func() {
		cancel()
		if transport.stop != nil {
			transport.stop()
		}
		<-receiverDone
	}()
	go func() {
		defer close(receiverDone)
		for {
			message, err := stream.Receive()
			if err != nil {
				receiveErr <- err
				return
			}
			if err := g.check(connectionCtx, s); err != nil {
				receiveErr <- err
				return
			}
			if err := g.receive(connectionCtx, s, message); err != nil {
				receiveErr <- err
				return
			}
		}
	}()
	ticker := time.NewTicker(g.config.ScheduleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-connectionCtx.Done():
			return connect.NewError(connect.CodeCanceled, workerprotocol.ErrIdentity)
		case err := <-writerErr:
			return err
		case err := <-receiveErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return connect.NewError(connect.CodeUnauthenticated, safeGatewayError(err))
		case <-ticker.C:
			if err := g.check(connectionCtx, s); err != nil {
				return connect.NewError(connect.CodeUnauthenticated, workerprotocol.ErrIdentity)
			}
			g.report(g.schedule(connectionCtx, s))
		}
	}
}
func safeGatewayError(err error) error {
	if errors.Is(err, workerprotocol.ErrIdentity) {
		return workerprotocol.ErrIdentity
	}
	return workerprotocol.ErrProtocol
}
func certificateHash(certificate *x509.Certificate) string {
	sum := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(sum[:])
}
func (g *Gateway) CheckGeneration(ctx context.Context, request *connect.Request[platformv1.WorkerGenerationRequest]) (*connect.Response[platformv1.WorkerGenerationResponse], error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, connect.NewError(connect.CodeUnavailable, workerprotocol.ErrIdentity)
	}
	g.streams.Add(1)
	g.mu.Unlock()
	defer g.streams.Done()
	certificate, err := peer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, workerprotocol.ErrIdentity)
	}
	identity, err := g.config.Peers.VerifyPeer(ctx, certificate)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, workerprotocol.ErrIdentity)
	}
	if g.config.Guard == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, workerprotocol.ErrQualification)
	}
	value := request.Msg
	if value.Generation < 1 || value.WorkspaceId == "" || value.ApplicationId == "" || value.OperationId == "" || value.ReleaseId == "" || value.ClusterNodeUid == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, workerprotocol.ErrProtocol)
	}
	err = g.config.Guard.Check(ctx, deploymentIdentity(identity, certificate), GenerationCheck{WorkspaceID: value.WorkspaceId, ApplicationID: value.ApplicationId, OperationID: value.OperationId, ReleaseID: value.ReleaseId, ClusterNodeUID: value.ClusterNodeUid, Generation: value.Generation})
	if err != nil {
		return nil, connect.NewError(connect.CodeAborted, workerprotocol.ErrIdentity)
	}
	return connect.NewResponse(&platformv1.WorkerGenerationResponse{Current: true}), nil
}
