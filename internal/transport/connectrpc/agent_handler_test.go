package connectrpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	demov1 "example.com/multitopology-demo/gen/go/demo/v1"
	"example.com/multitopology-demo/gen/go/demo/v1/demov1connect"
	"example.com/multitopology-demo/internal/domain"
	"example.com/multitopology-demo/internal/ports"
	"example.com/multitopology-demo/internal/testkit"
)

func TestAgentHandlerConnectsAndRelaysMessages(t *testing.T) {
	certificates := writeTestCertificates(t)
	registry := NewRegistry(time.Second)
	presence := testkit.NewMemoryPresence()
	events := make(chan domain.Event, 2)
	eventHandler := ports.EventHandlerFunc(func(_ context.Context, event domain.Event) error {
		events <- event
		return nil
	})
	path, handler := NewAgentHandler(registry, presence, eventHandler, "control-1")
	server, client := newAgentTestServer(t, certificates, path, handler)
	defer server.Close()

	streamContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	agentClient := demov1connect.NewAgentServiceClient(client, server.URL, connect.WithGRPC())
	stream := agentClient.Connect(streamContext)
	if err := stream.Send(&demov1.AgentMessage{
		Payload: &demov1.AgentMessage_Hello{
			Hello: &demov1.Hello{WorkerId: "worker-1", Version: "test"},
		},
	}); err != nil {
		t.Fatalf("Send(hello) error = %v", err)
	}
	welcome, err := stream.Receive()
	if err != nil {
		t.Fatalf("Receive(welcome) error = %v", err)
	}
	if welcome.GetWelcome().GetWorkerId() != "worker-1" {
		t.Fatalf("welcome worker id = %q, want worker-1", welcome.GetWelcome().GetWorkerId())
	}

	online, err := presence.IsOnline(context.Background(), "worker-1")
	if err != nil || !online {
		t.Fatalf("worker online = %v, error = %v", online, err)
	}

	if err := registry.Dispatch(
		context.Background(),
		domain.NewCommand("command-1", "worker-1", "hello"),
	); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	command, err := stream.Receive()
	if err != nil {
		t.Fatalf("Receive(command) error = %v", err)
	}
	if command.GetCommand().GetId() != "command-1" {
		t.Fatalf("command id = %q, want command-1", command.GetCommand().GetId())
	}

	if err := stream.Send(&demov1.AgentMessage{
		Payload: &demov1.AgentMessage_Accepted{
			Accepted: &demov1.CommandAccepted{CommandId: "command-1"},
		},
	}); err != nil {
		t.Fatalf("Send(accepted) error = %v", err)
	}
	if err := stream.Send(&demov1.AgentMessage{
		Payload: &demov1.AgentMessage_Result{
			Result: &demov1.CommandResult{CommandId: "command-1", Output: "HELLO"},
		},
	}); err != nil {
		t.Fatalf("Send(result) error = %v", err)
	}

	assertEventType(t, events, domain.EventAccepted)
	assertEventType(t, events, domain.EventSucceeded)
	_ = stream.CloseRequest()
}

func TestAgentHandlerRejectsCertificateIdentityMismatch(t *testing.T) {
	certificates := writeTestCertificates(t)
	path, handler := NewAgentHandler(
		NewRegistry(time.Second),
		testkit.NewMemoryPresence(),
		ports.EventHandlerFunc(func(context.Context, domain.Event) error { return nil }),
		"control-1",
	)
	server, client := newAgentTestServer(t, certificates, path, handler)
	defer server.Close()

	agentClient := demov1connect.NewAgentServiceClient(client, server.URL, connect.WithGRPC())
	stream := agentClient.Connect(context.Background())
	if err := stream.Send(&demov1.AgentMessage{
		Payload: &demov1.AgentMessage_Hello{
			Hello: &demov1.Hello{WorkerId: "other-worker", Version: "test"},
		},
	}); err != nil {
		t.Fatalf("Send(hello) error = %v", err)
	}
	_, err := stream.Receive()
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("Receive() code = %v, want unauthenticated; error = %v", connect.CodeOf(err), err)
	}
}

func newAgentTestServer(
	t *testing.T,
	certificates testCertificates,
	path string,
	handler http.Handler,
) (*httptest.Server, *http.Client) {
	t.Helper()
	serverTLS, err := LoadServerTLSConfig(certificates.serverCert, certificates.serverKey, certificates.caCert)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig() error = %v", err)
	}
	clientTLS, err := LoadClientTLSConfig(
		certificates.workerCert,
		certificates.workerKey,
		certificates.caCert,
		"localhost",
	)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig() error = %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(path, PeerIdentityMiddleware(handler))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.TLS = serverTLS
	server.StartTLS()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   clientTLS,
		ForceAttemptHTTP2: true,
	}}
	return server, client
}

func assertEventType(t *testing.T, events <-chan domain.Event, want domain.EventType) {
	t.Helper()
	select {
	case event := <-events:
		if event.Type != want {
			t.Fatalf("event type = %q, want %q", event.Type, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("没有收到 %q 事件", want)
	}
}
