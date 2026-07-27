package connectrpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	demov1 "github.com/sunshanpeng/yummy/gen/go/demo/v1"
	"github.com/sunshanpeng/yummy/gen/go/demo/v1/demov1connect"
	"github.com/sunshanpeng/yummy/internal/control"
	"github.com/sunshanpeng/yummy/internal/testkit"
)

func TestControlHandlerCreatesCommand(t *testing.T) {
	repository := testkit.NewMemoryCommandRepository()
	presence := testkit.NewMemoryPresence("worker-1")
	bus := &testkit.RecordingBus{}
	service := control.NewService(repository, presence, bus, func() string { return "command-1" })
	path, handler := NewControlHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := demov1connect.NewControlServiceClient(server.Client(), server.URL)

	response, err := client.CreateCommand(context.Background(), connect.NewRequest(&demov1.CreateCommandRequest{
		WorkerId: "worker-1",
		Input:    "hello",
	}))
	if err != nil {
		t.Fatalf("CreateCommand() error = %v", err)
	}
	if response.Msg.GetCommand().GetId() != "command-1" {
		t.Fatalf("command id = %q, want %q", response.Msg.GetCommand().GetId(), "command-1")
	}
	if response.Msg.GetCommand().GetStatus() != demov1.CommandStatus_COMMAND_STATUS_DISPATCHED {
		t.Fatalf("command status = %v, want dispatched", response.Msg.GetCommand().GetStatus())
	}
}

func TestControlHandlerMapsInvalidInputToConnectCode(t *testing.T) {
	service := control.NewService(
		testkit.NewMemoryCommandRepository(),
		testkit.NewMemoryPresence(),
		&testkit.RecordingBus{},
		func() string { return "command-1" },
	)
	path, handler := NewControlHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := demov1connect.NewControlServiceClient(server.Client(), server.URL)

	_, err := client.CreateCommand(context.Background(), connect.NewRequest(&demov1.CreateCommandRequest{}))

	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("error code = %v, want invalid_argument", connect.CodeOf(err))
	}
}

func TestControlHandlerReturnsDispatchFailedForOfflineWorker(t *testing.T) {
	service := control.NewService(
		testkit.NewMemoryCommandRepository(),
		testkit.NewMemoryPresence(),
		&testkit.RecordingBus{},
		func() string { return "command-1" },
	)
	path, handler := NewControlHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := demov1connect.NewControlServiceClient(server.Client(), server.URL)

	response, err := client.CreateCommand(context.Background(), connect.NewRequest(&demov1.CreateCommandRequest{
		WorkerId: "offline-worker",
		Input:    "hello",
	}))
	if err != nil {
		t.Fatalf("CreateCommand() error = %v", err)
	}
	if response.Msg.GetCommand().GetStatus() != demov1.CommandStatus_COMMAND_STATUS_DISPATCH_FAILED {
		t.Fatalf("command status = %v, want dispatch_failed", response.Msg.GetCommand().GetStatus())
	}
}

func TestControlHandlerListsWorkers(t *testing.T) {
	service := control.NewService(
		testkit.NewMemoryCommandRepository(),
		testkit.NewMemoryPresence("worker-1"),
		&testkit.RecordingBus{},
		func() string { return "command-1" },
	)
	path, handler := NewControlHandler(service)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := demov1connect.NewControlServiceClient(server.Client(), server.URL)

	response, err := client.ListWorkers(context.Background(), connect.NewRequest(&demov1.ListWorkersRequest{}))
	if err != nil {
		t.Fatalf("ListWorkers() error = %v", err)
	}
	if len(response.Msg.GetWorkers()) != 1 || response.Msg.GetWorkers()[0].GetId() != "worker-1" {
		t.Fatalf("workers = %#v, want worker-1", response.Msg.GetWorkers())
	}
}
