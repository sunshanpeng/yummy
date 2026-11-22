// Package workerclient 负责 worker 连接控制面并恢复本地执行与上报。
package workerclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/gen/go/platform/v1/platformv1connect"
	"github.com/sunshanpeng/yummy/internal/infra/workerexec"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
	"google.golang.org/protobuf/proto"
)

// Executor 执行带 generation 和幂等身份的命令，生命周期独立于流连接。
type Executor interface {
	Build(context.Context, *platformv1.WorkerBuildCommand, bool, workerexec.Emitter) error
	Deploy(context.Context, *platformv1.WorkerDeploymentCommand, bool, workerexec.Emitter) error
}

var _ Executor = (*workerexec.Executor)(nil)

type WorkerConfig struct {
	URL, NodeID, WorkerID, Version, ServerID              string
	TLS                                                   *tls.Config
	Journal                                               *Journal
	Executor                                              Executor
	Heartbeat                                             func() *platformv1.WorkerHeartbeat
	Qualification                                         func(string) *platformv1.WorkerQualification
	HeartbeatInterval, ReconnectDelay, RetransmitInterval time.Duration
	MaxExecutions                                         int
	OnError                                               func(error)
	// Instrumentation 可观测性客户端选项（otelconnect 拦截器），装配层注入，
	// 两个网关 client 构造点都会附加。
	Instrumentation []connect.ClientOption
}
type WorkerClient struct {
	config    WorkerConfig
	http      *http.Client
	transport *http.Transport
	mu        sync.Mutex
	active    map[string]bool
	running   bool
	ctx       context.Context
}

func NewWorkerClient(config WorkerConfig) (*WorkerClient, error) {
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || config.NodeID == "" || config.WorkerID == "" || config.Journal == nil || config.Executor == nil || config.TLS == nil || config.TLS.InsecureSkipVerify || len(config.TLS.Certificates) == 0 && config.TLS.GetClientCertificate == nil {
		return nil, workerprotocol.ErrProtocol
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = 15 * time.Second
	}
	if config.ReconnectDelay == 0 {
		config.ReconnectDelay = time.Second
	}
	if config.RetransmitInterval == 0 {
		config.RetransmitInterval = time.Second
	}
	if config.MaxExecutions == 0 {
		config.MaxExecutions = 32
	}
	if config.HeartbeatInterval < 10*time.Millisecond || config.ReconnectDelay < 10*time.Millisecond || config.RetransmitInterval < 10*time.Millisecond || config.MaxExecutions < 1 || config.MaxExecutions > 256 {
		return nil, workerprotocol.ErrProtocol
	}
	tlsConfig := config.TLS.Clone()
	tlsConfig.MinVersion = tls.VersionTLS12
	transport := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true, Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: time.Minute}
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return workerprotocol.ErrIdentity }}
	return &WorkerClient{config: config, http: httpClient, transport: transport, active: map[string]bool{}}, nil
}
func (c *WorkerClient) report(err error) {
	if err != nil && c.config.OnError != nil {
		c.config.OnError(err)
	}
}

// Run的ctx由worker进程拥有；连接断开只取消流，不取消正在跟踪的Kubernetes执行。
func (c *WorkerClient) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return workerprotocol.ErrProtocol
	}
	c.running = true
	c.ctx = ctx
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.running = false; c.mu.Unlock(); c.transport.CloseIdleConnections() }()
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		c.report(err)
		timer := time.NewTimer(c.config.ReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
func (c *WorkerClient) runOnce(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	client := platformv1connect.NewWorkerGatewayClient(c.http, c.config.URL, append(c.config.Instrumentation, connect.WithGRPC(), connect.WithReadMaxBytes(1<<20), connect.WithSendMaxBytes(1<<20))...)
	stream := client.Connect(ctx)
	defer stream.CloseRequest()
	defer stream.CloseResponse()
	// 唯一发送循环拥有stream.Send；接收/执行管理器均不直接写流。
	outbound := make(chan *platformv1.WorkerEnvelope, 8)
	sendErrors := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-outbound:
				if err := stream.Send(message); err != nil {
					select {
					case sendErrors <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	outbound <- &platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_Hello{Hello: &platformv1.WorkerHello{NodeId: c.config.NodeID, WorkerId: c.config.WorkerID, Version: c.config.Version}}}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	welcome := first.GetWelcome()
	if welcome == nil || welcome.NodeId != c.config.NodeID || welcome.WorkerId != c.config.WorkerID || welcome.ConnectionEpoch < 1 || welcome.QualificationNonce == "" || (c.config.ServerID != "" && welcome.ServerId != c.config.ServerID) {
		return workerprotocol.ErrIdentity
	}
	if c.config.Qualification != nil {
		qualification := c.config.Qualification(welcome.QualificationNonce)
		if qualification != nil {
			qualification = proto.Clone(qualification).(*platformv1.WorkerQualification)
			qualification.Nonce = welcome.QualificationNonce
			outbound <- &platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_Qualification{Qualification: qualification}}
		}
	}
	receiveErrors := make(chan error, 1)
	go func() {
		for {
			message, err := stream.Receive()
			if err != nil {
				receiveErrors <- err
				return
			}
			switch value := message.Payload.(type) {
			case *platformv1.ControlEnvelope_Ack:
				if value.Ack == nil {
					receiveErrors <- workerprotocol.ErrProtocol
					return
				}
				if value.Ack.Channel != "qualification" {
					if err := c.config.Journal.ack(value.Ack); err != nil {
						receiveErrors <- err
						return
					}
				}
			case *platformv1.ControlEnvelope_Rejection:
				if value.Rejection == nil {
					receiveErrors <- workerprotocol.ErrProtocol
					return
				}
				if value.Rejection.Channel != "qualification" {
					// 业务拒绝（如归档应用的重投被控制面拒收）不是传输层错误：
					// 按控制面回传的 (channel, execution_id, seq) 丢弃对应 journal
					// 条目（被拒条目永不会被接受，无限重试会形成重连风暴并连带
					// 中断心跳——2026-11-14 实机发现⑧），上报后保留流继续。
					if err := c.config.Journal.ack(&platformv1.WorkerAck{Channel: value.Rejection.Channel, ExecutionId: value.Rejection.ExecutionId, Seq: value.Rejection.Seq}); err != nil {
						c.report(fmt.Errorf("worker拒绝条目丢弃失败: %w", err))
					}
					c.report(fmt.Errorf("worker被拒绝: channel=%s code=%s execution=%s seq=%d", value.Rejection.Channel, value.Rejection.Code, value.Rejection.ExecutionId, value.Rejection.Seq))
					continue
				}
				// 资格失败不等于断线；保留未就绪状态并允许运维发现问题。
				c.report(workerprotocol.ErrQualification)
			case *platformv1.ControlEnvelope_BuildCommand:
				if err := c.startBuild(parent, value.BuildCommand); err != nil {
					receiveErrors <- err
					return
				}
			case *platformv1.ControlEnvelope_DeploymentCommand:
				if err := c.startDeployment(parent, value.DeploymentCommand); err != nil {
					receiveErrors <- err
					return
				}
			default:
				receiveErrors <- workerprotocol.ErrProtocol
				return
			}
		}
	}()
	heartbeat := time.NewTicker(c.config.HeartbeatInterval)
	defer heartbeat.Stop()
	retransmit := time.NewTicker(c.config.RetransmitInterval)
	defer retransmit.Stop()
	pending := func() error {
		messages, err := c.config.Journal.pending()
		if err != nil {
			return err
		}
		for _, message := range messages {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case outbound <- message:
			}
		}
		return nil
	}
	if err := pending(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-sendErrors:
			return err
		case err := <-receiveErrors:
			return err
		case <-heartbeat.C:
			value := &platformv1.WorkerHeartbeat{}
			if c.config.Heartbeat != nil {
				value = c.config.Heartbeat()
			}
			if value == nil {
				return workerprotocol.ErrProtocol
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case outbound <- &platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_Heartbeat{Heartbeat: value}}:
			}
		case <-retransmit.C:
			if err := pending(); err != nil {
				return err
			}
		}
	}
}

// CheckGeneration 使用与工作流相同的已验证 mTLS 连接；每次 Kubernetes 写入前调用，
// 不把某次成功校验缓存为永久权限。连接异常或身份撤销均失败关闭。
func (c *WorkerClient) CheckGeneration(ctx context.Context, request *platformv1.WorkerGenerationRequest) error {
	if request == nil {
		return workerprotocol.ErrProtocol
	}
	client := platformv1connect.NewWorkerGatewayClient(c.http, c.config.URL, append(c.config.Instrumentation, connect.WithGRPC(), connect.WithReadMaxBytes(1<<20), connect.WithSendMaxBytes(1<<20))...)
	response, err := client.CheckGeneration(ctx, connect.NewRequest(request))
	if err != nil {
		return err
	}
	if response.Msg == nil || !response.Msg.Current {
		return workerprotocol.ErrIdentity
	}
	return nil
}

func (c *WorkerClient) startBuild(ctx context.Context, command *platformv1.WorkerBuildCommand) error {
	if command == nil || command.AttemptId == "" || command.StableJobName == "" || command.NodeId != c.config.NodeID || command.WorkerId != c.config.WorkerID {
		return workerprotocol.ErrIdentity
	}
	command = proto.Clone(command).(*platformv1.WorkerBuildCommand)
	if command.Action != "RUN" && command.Action != "CANCEL" && command.Action != "RECONCILE" {
		return workerprotocol.ErrProtocol
	}
	fingerprint, err := buildFingerprint(command)
	if err != nil {
		return err
	}
	return c.start(ctx, "build:"+command.AttemptId, command.Action, fingerprint, func(reconcile bool, emitter workerexec.Emitter) error {
		return c.config.Executor.Build(ctx, command, reconcile, emitter)
	})
}
func (c *WorkerClient) startDeployment(ctx context.Context, command *platformv1.WorkerDeploymentCommand) error {
	if command == nil || command.OperationId == "" || command.Generation < 1 || command.NodeId != c.config.NodeID || command.WorkerId != c.config.WorkerID {
		return workerprotocol.ErrIdentity
	}
	switch command.Action {
	case "DEPLOY", "ROLLBACK", "RECOVERY", "RESTART", "STOP", "START", "RECONCILE":
	default:
		return workerprotocol.ErrProtocol
	}
	command = proto.Clone(command).(*platformv1.WorkerDeploymentCommand)
	fingerprint, err := deploymentFingerprint(command)
	if err != nil {
		return err
	}
	if err := c.config.Journal.generation(command.WorkspaceId+":"+command.ApplicationId, command.Generation); err != nil {
		return err
	}
	// 观察恢复用独立 journal 执行键：journal spool 跨重启持久化原 DEPLOY 指纹，
	// RECONCILE 的 Action 不同必然指纹失配（workerprotocol.ErrProtocol）；独立键让恢复与原操作
	// 的重投证据互不干扰，恢复完成与否都不封存原操作。
	key := "deployment:" + command.OperationId
	if command.Action == "RECONCILE" {
		key += ":RECONCILE"
	}
	return c.start(ctx, key, command.Action, fingerprint, func(reconcile bool, emitter workerexec.Emitter) error {
		return c.config.Executor.Deploy(ctx, command, reconcile, emitter)
	})
}
func (c *WorkerClient) start(ctx context.Context, key, action, fingerprint string, execute func(bool, workerexec.Emitter) error) error {
	activeKey := key
	if action == "CANCEL" || action == "RECONCILE" {
		activeKey = key + ":" + action
	}
	c.mu.Lock()
	if c.active[activeKey] {
		c.mu.Unlock()
		return nil
	}
	if len(c.active) >= c.config.MaxExecutions {
		c.mu.Unlock()
		return workerprotocol.ErrSpoolFull
	}
	reconcile, done, err := c.config.Journal.begin(key, fingerprint)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if done && action != "CANCEL" && action != "RECONCILE" {
		c.mu.Unlock()
		return nil
	}
	c.active[activeKey] = true
	c.mu.Unlock()
	go func() {
		defer func() { c.mu.Lock(); delete(c.active, activeKey); c.mu.Unlock() }()
		emitter := &journalEmitter{journal: c.config.Journal}
		err := execute(reconcile || done, emitter)
		if err != nil {
			c.report(err)
			return
		}
		if err := emitter.failure(); err != nil {
			c.report(err)
			return
		}
		// 长期运行观察与阶段性查询返回 nil 不代表终态；只有已持久化的终态证据
		// 能封存构建。取消/查询可能与原任务并发，不得封存仍在执行的原操作。
		if action == "RUN" && emitter.terminalBuild() {
			c.report(c.config.Journal.done(key))
		}
	}()
	return nil
}

type journalEmitter struct {
	journal  *Journal
	mu       sync.Mutex
	err      error
	terminal bool
}

func (e *journalEmitter) persist(value *platformv1.WorkerEnvelope) error {
	err := e.journal.append(value)
	if err != nil {
		e.mu.Lock()
		e.err = errors.Join(e.err, err)
		e.mu.Unlock()
	}
	return err
}
func (e *journalEmitter) failure() error      { e.mu.Lock(); defer e.mu.Unlock(); return e.err }
func (e *journalEmitter) terminalBuild() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.terminal }
func (e *journalEmitter) BuildEvent(value *platformv1.WorkerBuildEvent) error {
	if value == nil {
		return workerprotocol.ErrProtocol
	}
	err := e.persist(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_BuildEvent{BuildEvent: value}})
	if err == nil && (value.Kind == "SUCCEEDED" || value.Kind == "FAILED" || value.Kind == "STOPPED") {
		e.mu.Lock()
		e.terminal = true
		e.mu.Unlock()
	}
	return err
}
func (e *journalEmitter) BuildLog(value *platformv1.WorkerBuildLog) error {
	if value == nil {
		return workerprotocol.ErrProtocol
	}
	return e.persist(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_BuildLog{BuildLog: value}})
}
func (e *journalEmitter) Observation(value *platformv1.WorkerDeploymentObservation) error {
	if value == nil {
		return workerprotocol.ErrProtocol
	}
	return e.persist(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_DeploymentObservation{DeploymentObservation: value}})
}
func (e *journalEmitter) RuntimeLog(value *platformv1.WorkerRuntimeLog) error {
	if value == nil {
		return workerprotocol.ErrProtocol
	}
	return e.persist(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_RuntimeLog{RuntimeLog: value}})
}
func (e *journalEmitter) RuntimeMetric(value *platformv1.WorkerRuntimeMetric) error {
	if value == nil {
		return workerprotocol.ErrProtocol
	}
	return e.persist(&platformv1.WorkerEnvelope{Payload: &platformv1.WorkerEnvelope_RuntimeMetric{RuntimeMetric: value}})
}
