# ConnectRPC 多拓扑部署 Demo Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建一套 React + Go 代码，使其能够以单进程、前后端分离、控制面与 worker 分离三种拓扑运行，并通过 ConnectRPC 双向流和 mTLS 完成文本大写任务。

**Architecture:** 控制面依赖 `CommandBus`、PostgreSQL repository 与 Redis presence port；单进程装配 `LocalCommandBus`，分布式装配 ConnectRPC connection registry。`platform` 是包含前端资源的万能入口，`control` 与 `worker` 是复用同一 bootstrap 的精简入口。

**Tech Stack:** Go 1.25、ConnectRPC、Protocol Buffers、React 19、TypeScript、Vite、PostgreSQL、Redis、Docker Compose、OpenSSL。

## Global Constraints

- 所有新增文档与代码注释使用中文。
- 直接在当前分支实施，不创建 worktree。
- `platform` 未指定子命令时必须默认执行 `all`。
- 仅实现内置 `uppercase` 任务，不执行任意 Shell 命令。
- PostgreSQL 是任务数据的唯一事实来源；Redis 只保存带 TTL 的在线状态。
- worker 到控制面的远程链路必须使用 ConnectRPC 双向流、HTTP/2 和 mTLS。
- 当前不实现自动重投、多控制面、证书注册后台或 AI 模型调用。

---

## 文件结构

### Go 领域与端口

- `go.mod`：Go 模块、运行时依赖和代码生成工具。
- `internal/domain/command.go`：指令、状态、事件和状态转换规则。
- `internal/domain/worker.go`：worker 在线信息。
- `internal/ports/ports.go`：repository、presence、dispatcher 和事件处理接口。
- `internal/worker/runtime.go`：只负责执行 `uppercase` 并产生领域事件。

### 存储

- `migrations/001_init.sql`：workers 与 commands 表。
- `internal/repository/postgres/commands.go`：指令持久化与条件状态转换。
- `internal/presence/redis/store.go`：在线状态、TTL 与列表查询。
- `internal/storage/open.go`：建立 PostgreSQL、Redis 连接并执行迁移。

### 本地与远程通信

- `internal/commandbus/local.go`：进程内异步派发。
- `api/demo/v1/demo.proto`：ControlService、AgentService 与消息。
- `buf.yaml`、`buf.gen.yaml`：Go、Connect-Go 与 TypeScript 代码生成。
- `gen/go/demo/v1/*`：生成的 Go 消息与 ConnectRPC 接口。
- `web/src/gen/demo/v1/*`：生成的 TypeScript 消息和服务描述。
- `internal/transport/connectrpc/registry.go`：远程连接表与指令发送。
- `internal/transport/connectrpc/agent_handler.go`：双向流服务端。
- `internal/transport/connectrpc/control_handler.go`：React 使用的 Unary RPC。
- `internal/transport/connectrpc/worker_client.go`：mTLS 双向流、心跳与重连。
- `internal/transport/connectrpc/tls.go`：服务端和客户端 TLS 配置。

### 业务与入口

- `internal/control/service.go`：创建、查询指令与处理 worker 事件。
- `internal/bootstrap/config.go`：环境变量与命令行配置。
- `internal/bootstrap/control.go`：控制面公共/agent 两个 HTTP server。
- `internal/bootstrap/worker.go`：远程 worker。
- `internal/bootstrap/all.go`：本地 worker + 控制面。
- `internal/bootstrap/run.go`：模式选择与优雅退出。
- `cmd/platform/main.go`：默认 `all` 的万能入口。
- `cmd/control/main.go`：精简控制面入口。
- `cmd/worker/main.go`：精简 worker 入口。

### React、部署与文档

- `web/package.json`、`web/vite.config.ts`、`web/tsconfig*.json`：前端工具链。
- `web/src/App.tsx`、`web/src/main.tsx`、`web/src/styles.css`：单页 Demo。
- `internal/webui/embed.go`、`internal/webui/dist/*`：嵌入前端产物。
- `scripts/generate-dev-certs.sh`：开发 CA、服务端和 worker 证书。
- `compose.yaml`：PostgreSQL 与 Redis。
- `deploy/nginx.conf`：静态资源和 `/api` 代理。
- `Makefile`：generate、web-build、build、test。
- `README.md`：三种拓扑的运行和验证命令。

---

### Task 1: 建立领域模型、状态机和 worker runtime

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `internal/domain/command.go`
- Create: `internal/domain/command_test.go`
- Create: `internal/domain/worker.go`
- Create: `internal/ports/ports.go`
- Create: `internal/worker/runtime.go`
- Create: `internal/worker/runtime_test.go`

**Interfaces:**
- Produces: `domain.Command`、`domain.Event`、`domain.CanTransition`。
- Produces: `ports.CommandRepository`、`ports.PresenceStore`、`ports.CommandBus`、`ports.EventHandler`。
- Produces: `worker.Runtime.Execute(context.Context, domain.Command, func(domain.Event))`。

- [ ] **Step 1: 写状态机失败测试**

```go
func TestCanTransition(t *testing.T) {
    tests := []struct {
        from domain.CommandStatus
        to   domain.CommandStatus
        want bool
    }{
        {domain.StatusCreated, domain.StatusDispatched, true},
        {domain.StatusDispatched, domain.StatusRunning, true},
        {domain.StatusRunning, domain.StatusSucceeded, true},
        {domain.StatusSucceeded, domain.StatusRunning, false},
    }
    for _, tt := range tests {
        if got := domain.CanTransition(tt.from, tt.to); got != tt.want {
            t.Fatalf("CanTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
        }
    }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/domain`

Expected: FAIL，提示 `domain.CommandStatus` 或 `domain.CanTransition` 未定义。

- [ ] **Step 3: 实现最小领域模型**

```go
type CommandStatus string

const (
    StatusCreated        CommandStatus = "CREATED"
    StatusDispatched     CommandStatus = "DISPATCHED"
    StatusRunning        CommandStatus = "RUNNING"
    StatusSucceeded      CommandStatus = "SUCCEEDED"
    StatusFailed         CommandStatus = "FAILED"
    StatusDispatchFailed CommandStatus = "DISPATCH_FAILED"
)

type Command struct {
    ID        string
    WorkerID  string
    Input     string
    Output    string
    Error     string
    Status    CommandStatus
    CreatedAt time.Time
    UpdatedAt time.Time
}

type EventType string

const (
    EventAccepted  EventType = "ACCEPTED"
    EventProgress  EventType = "PROGRESS"
    EventSucceeded EventType = "SUCCEEDED"
    EventFailed    EventType = "FAILED"
)

type Event struct {
    CommandID string
    Type      EventType
    Message   string
    Output    string
    Error     string
}

var (
    ErrNotFound     = errors.New("记录不存在")
    ErrStateConflict = errors.New("指令状态冲突")
)

func CanTransition(from, to CommandStatus) bool {
    allowed := map[CommandStatus]map[CommandStatus]bool{
        StatusCreated:    {StatusDispatched: true, StatusDispatchFailed: true},
        StatusDispatched: {StatusRunning: true, StatusDispatchFailed: true},
        StatusRunning:    {StatusSucceeded: true, StatusFailed: true},
    }
    return allowed[from][to]
}

func NewCommand(id, workerID, input string) Command {
    now := time.Now().UTC()
    return Command{
        ID: id, WorkerID: workerID, Input: input,
        Status: StatusCreated, CreatedAt: now, UpdatedAt: now,
    }
}
```

`internal/domain/worker.go` 使用以下类型：

```go
type WorkerPresence struct {
    WorkerID   string
    InstanceID string
    Version    string
    LastSeen   time.Time
}
```

- [ ] **Step 4: 定义稳定端口**

```go
type CommandRepository interface {
    Create(context.Context, domain.Command) error
    Get(context.Context, string) (domain.Command, error)
    Transition(
        context.Context,
        string,
        domain.CommandStatus,
        domain.CommandStatus,
        string,
        string,
    ) error
}

type PresenceStore interface {
    Upsert(context.Context, domain.WorkerPresence) error
    IsOnline(context.Context, string) (bool, error)
    ListOnline(context.Context) ([]domain.WorkerPresence, error)
}

type CommandBus interface {
    Dispatch(context.Context, domain.Command) error
}

type EventHandler interface {
    HandleEvent(context.Context, domain.Event) error
}

type EventHandlerFunc func(context.Context, domain.Event) error

func (f EventHandlerFunc) HandleEvent(ctx context.Context, event domain.Event) error {
    return f(ctx, event)
}
```

- [ ] **Step 5: 写 worker runtime 失败测试**

```go
func TestRuntimeUppercase(t *testing.T) {
    runtime := worker.NewRuntime()
    var events []domain.Event
    runtime.Execute(context.Background(), domain.Command{
        ID: "cmd-1", WorkerID: "worker-1", Input: "Hello 世界",
    }, func(event domain.Event) {
        events = append(events, event)
    })

    if len(events) != 3 {
        t.Fatalf("事件数 = %d, want 3", len(events))
    }
    if events[0].Type != domain.EventAccepted {
        t.Fatalf("首个事件 = %q, want ACCEPTED", events[0].Type)
    }
    if events[1].Type != domain.EventProgress {
        t.Fatalf("第二个事件 = %q, want PROGRESS", events[1].Type)
    }
    if events[2].Output != "HELLO 世界" {
        t.Fatalf("输出 = %q, want %q", events[2].Output, "HELLO 世界")
    }
}
```

- [ ] **Step 6: 实现 worker runtime 并运行测试**

```go
func (r *Runtime) Execute(ctx context.Context, command domain.Command, emit func(domain.Event)) {
    emit(domain.Event{CommandID: command.ID, Type: domain.EventAccepted})
    emit(domain.Event{CommandID: command.ID, Type: domain.EventProgress, Message: "正在转换文本"})
    select {
    case <-ctx.Done():
        emit(domain.Event{CommandID: command.ID, Type: domain.EventFailed, Error: ctx.Err().Error()})
    default:
        emit(domain.Event{
            CommandID: command.ID,
            Type:      domain.EventSucceeded,
            Output:    strings.ToUpper(command.Input),
        })
    }
}
```

Run: `go test ./internal/domain ./internal/worker`

Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add go.mod .gitignore internal/domain internal/ports internal/worker
git commit -m "feat: 添加指令领域模型和 worker runtime"
```

### Task 2: 实现控制面服务和本地 CommandBus

**Files:**
- Create: `internal/control/service.go`
- Create: `internal/control/service_test.go`
- Create: `internal/commandbus/local.go`
- Create: `internal/commandbus/local_test.go`
- Create: `internal/testkit/memory.go`

**Interfaces:**
- Consumes: Task 1 的领域类型与 ports。
- Produces: `control.Service.CreateCommand`、`control.Service.GetCommand`、`control.Service.HandleEvent`。
- Produces: `commandbus.NewLocal(runtime, eventHandler)`。

- [ ] **Step 1: 写控制面状态顺序失败测试**

```go
func TestCreateCommandTransitionsBeforeDispatch(t *testing.T) {
    repo := testkit.NewMemoryCommandRepository()
    presence := testkit.NewMemoryPresence("worker-1")
    bus := &testkit.RecordingBus{}
    service := control.NewService(repo, presence, bus, func() string { return "cmd-1" })

    got, err := service.CreateCommand(context.Background(), "worker-1", "hello")
    if err != nil {
        t.Fatal(err)
    }
    if got.Status != domain.StatusDispatched {
        t.Fatalf("状态 = %q, want DISPATCHED", got.Status)
    }
    if bus.Commands[0].ID != "cmd-1" {
        t.Fatalf("下发 ID = %q, want cmd-1", bus.Commands[0].ID)
    }
    if repo.Transitions[0] != "CREATED->DISPATCHED" {
        t.Fatalf("首个转换 = %q", repo.Transitions[0])
    }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/control`

Expected: FAIL，提示 `control.NewService` 未定义。

- [ ] **Step 3: 实现控制面最小逻辑**

```go
func (s *Service) CreateCommand(ctx context.Context, workerID, input string) (domain.Command, error) {
    if strings.TrimSpace(workerID) == "" || strings.TrimSpace(input) == "" {
        return domain.Command{}, ErrInvalidInput
    }
    online, err := s.presence.IsOnline(ctx, workerID)
    if err != nil {
        return domain.Command{}, err
    }
    command := domain.NewCommand(s.newID(), workerID, input)
    if err := s.repo.Create(ctx, command); err != nil {
        return domain.Command{}, err
    }
    if !online {
        _ = s.repo.Transition(ctx, command.ID, domain.StatusCreated, domain.StatusDispatchFailed, "", "worker 不在线")
        return s.repo.Get(ctx, command.ID)
    }
    if err := s.repo.Transition(ctx, command.ID, domain.StatusCreated, domain.StatusDispatched, "", ""); err != nil {
        return domain.Command{}, err
    }
    if err := s.bus.Dispatch(ctx, command); err != nil {
        _ = s.repo.Transition(ctx, command.ID, domain.StatusDispatched, domain.StatusDispatchFailed, "", err.Error())
    }
    return s.repo.Get(ctx, command.ID)
}

func (s *Service) HandleEvent(ctx context.Context, event domain.Event) error {
    switch event.Type {
    case domain.EventAccepted:
        return s.repo.Transition(
            ctx, event.CommandID,
            domain.StatusDispatched, domain.StatusRunning, "", "",
        )
    case domain.EventSucceeded:
        return s.repo.Transition(
            ctx, event.CommandID,
            domain.StatusRunning, domain.StatusSucceeded, event.Output, "",
        )
    case domain.EventFailed:
        return s.repo.Transition(
            ctx, event.CommandID,
            domain.StatusRunning, domain.StatusFailed, "", event.Error,
        )
    case domain.EventProgress:
        return nil
    default:
        return fmt.Errorf("未知事件类型 %q", event.Type)
    }
}
```

- [ ] **Step 4: 写本地总线端到端测试**

```go
func TestLocalBusProducesUppercaseResult(t *testing.T) {
    events := make(chan domain.Event, 2)
    bus := commandbus.NewLocal(worker.NewRuntime(), ports.EventHandlerFunc(
        func(_ context.Context, event domain.Event) error {
            events <- event
            return nil
        },
    ))
    err := bus.Dispatch(context.Background(), domain.Command{
        ID: "cmd-1", WorkerID: "local", Input: "hello",
    })
    if err != nil {
        t.Fatal(err)
    }
    <-events
    <-events
    result := <-events
    if result.Output != "HELLO" {
        t.Fatalf("输出 = %q", result.Output)
    }
}
```

- [ ] **Step 5: 实现本地总线并运行测试**

```go
func (b *Local) Dispatch(ctx context.Context, command domain.Command) error {
    go b.runtime.Execute(ctx, command, func(event domain.Event) {
        _ = b.events.HandleEvent(context.Background(), event)
    })
    return nil
}
```

Run: `go test ./internal/control ./internal/commandbus`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/control internal/commandbus internal/testkit
git commit -m "feat: 添加控制面服务和本地指令总线"
```

### Task 3: 接入 PostgreSQL 与 Redis

**Files:**
- Create: `migrations/001_init.sql`
- Create: `internal/repository/postgres/commands.go`
- Create: `internal/repository/postgres/commands_test.go`
- Create: `internal/presence/redis/store.go`
- Create: `internal/presence/redis/store_test.go`
- Create: `internal/storage/open.go`
- Create: `compose.yaml`

**Interfaces:**
- Consumes: `ports.CommandRepository`、`ports.PresenceStore`。
- Produces: `postgres.New(*pgxpool.Pool)`、`redis.New(*goredis.Client, ttl)`。
- Produces: `storage.Open(context.Context, Config)` 与 migration 执行。

- [ ] **Step 1: 写 PostgreSQL migration**

```sql
CREATE TABLE IF NOT EXISTS workers (
    id TEXT PRIMARY KEY,
    version TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS commands (
    id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL,
    input TEXT NOT NULL,
    output TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS commands_created_at_idx
    ON commands (created_at DESC);
```

- [ ] **Step 2: 写 repository 条件转换测试**

测试使用 `TEST_DATABASE_URL`；变量为空时调用 `t.Skip`。插入 `CREATED` 后，断言 `CREATED -> DISPATCHED` 成功，第二次以 `CREATED` 为当前状态转换时返回 `domain.ErrStateConflict`。

Run: `TEST_DATABASE_URL=postgres://demo:demo@localhost:5432/demo?sslmode=disable go test ./internal/repository/postgres -run TestTransition -v`

Expected: 初次运行 FAIL，提示 `postgres.New` 未定义。

- [ ] **Step 3: 实现 repository**

核心状态更新必须使用条件：

```sql
UPDATE commands
SET status = $3, output = $4, error = $5, updated_at = NOW()
WHERE id = $1 AND status = $2
```

当 `RowsAffected()` 为 0 时返回 `domain.ErrStateConflict`。

- [ ] **Step 4: 写 Redis TTL 测试**

测试使用 `TEST_REDIS_ADDR`；变量为空时跳过。调用 `Upsert` 后断言在线，等待测试专用短 TTL 后断言离线。

Run: `TEST_REDIS_ADDR=localhost:6379 go test ./internal/presence/redis -run TestPresenceExpires -v`

Expected: 初次运行 FAIL，提示 `redis.New` 未定义。

- [ ] **Step 5: 实现 Redis presence**

key 使用 `demo:worker:<workerID>`，value 为 JSON：

```go
type record struct {
    WorkerID  string    `json:"workerId"`
    Instance  string    `json:"instance"`
    Version   string    `json:"version"`
    LastSeen  time.Time `json:"lastSeen"`
}
```

`Upsert` 使用 `SET key value ttl`；`IsOnline` 使用 `EXISTS`；`ListOnline` 使用 `SCAN demo:worker:*`，不使用阻塞式 `KEYS`。

- [ ] **Step 6: 启动依赖并运行集成测试**

Run: `docker compose up -d postgres redis`

Run: `TEST_DATABASE_URL=postgres://demo:demo@localhost:5432/demo?sslmode=disable TEST_REDIS_ADDR=localhost:6379 go test ./internal/repository/postgres ./internal/presence/redis`

Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add migrations internal/repository internal/presence internal/storage compose.yaml go.mod go.sum
git commit -m "feat: 接入 PostgreSQL 和 Redis"
```

### Task 4: 定义 Protobuf 并生成 ConnectRPC 客户端

**Files:**
- Create: `api/demo/v1/demo.proto`
- Create: `buf.yaml`
- Create: `buf.gen.yaml`
- Create: `web/package.json`
- Create: `web/tsconfig.json`
- Generate: `gen/go/demo/v1/*`
- Generate: `web/src/gen/demo/v1/*`

**Interfaces:**
- Produces: `ControlService` 的 ListWorkers、CreateCommand、GetCommand。
- Produces: `AgentService.Connect` 双向流。
- Produces: Go 和 TypeScript 消息类型。

- [ ] **Step 1: 写完整 Proto 契约**

```protobuf
syntax = "proto3";

package demo.v1;

option go_package = "github.com/sunshanpeng/yummy/gen/go/demo/v1;demov1";

service ControlService {
  rpc ListWorkers(ListWorkersRequest) returns (ListWorkersResponse);
  rpc CreateCommand(CreateCommandRequest) returns (CreateCommandResponse);
  rpc GetCommand(GetCommandRequest) returns (GetCommandResponse);
}

service AgentService {
  rpc Connect(stream AgentMessage) returns (stream ControlMessage);
}

enum CommandStatus {
  COMMAND_STATUS_UNSPECIFIED = 0;
  COMMAND_STATUS_CREATED = 1;
  COMMAND_STATUS_DISPATCHED = 2;
  COMMAND_STATUS_RUNNING = 3;
  COMMAND_STATUS_SUCCEEDED = 4;
  COMMAND_STATUS_FAILED = 5;
  COMMAND_STATUS_DISPATCH_FAILED = 6;
}

message Worker {
  string id = 1;
  string version = 2;
  string last_seen = 3;
}

message Command {
  string id = 1;
  string worker_id = 2;
  string input = 3;
  string output = 4;
  string error = 5;
  CommandStatus status = 6;
  string created_at = 7;
  string updated_at = 8;
}

message ListWorkersRequest {}
message ListWorkersResponse {
  repeated Worker workers = 1;
}

message CreateCommandRequest {
  string worker_id = 1;
  string input = 2;
}
message CreateCommandResponse {
  Command command = 1;
}

message GetCommandRequest {
  string id = 1;
}
message GetCommandResponse {
  Command command = 1;
}

message Hello {
  string worker_id = 1;
  string version = 2;
}
message Heartbeat {
  int64 unix_millis = 1;
}
message CommandAccepted {
  string command_id = 1;
}
message CommandProgress {
  string command_id = 1;
  string message = 2;
}
message CommandResult {
  string command_id = 1;
  string output = 2;
  string error = 3;
}

message AgentMessage {
  oneof payload {
    Hello hello = 1;
    Heartbeat heartbeat = 2;
    CommandAccepted accepted = 3;
    CommandProgress progress = 4;
    CommandResult result = 5;
  }
}

message Welcome {
  string worker_id = 1;
}

message ControlMessage {
  oneof payload {
    Welcome welcome = 1;
    Command command = 2;
  }
}
```

- [ ] **Step 2: 配置本地生成插件**

`buf.gen.yaml` 使用本地 `protoc-gen-go`、`protoc-gen-connect-go` 与前端 `protoc-gen-es`，Go 输出到 `gen/go`，TypeScript 输出到 `web/src/gen`。

- [ ] **Step 3: 安装依赖并生成**

Run: `go get -tool github.com/bufbuild/buf/cmd/buf google.golang.org/protobuf/cmd/protoc-gen-go connectrpc.com/connect/cmd/protoc-gen-connect-go`

Run: `pnpm --dir web install`

Run: `go tool buf lint`

Run: `go tool buf generate`

Expected: 命令成功，Go 与 TypeScript 生成目录均非空。

- [ ] **Step 4: 编译生成代码**

Run: `go test ./gen/go/...`

Run: `pnpm --dir web exec tsc --noEmit`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add api buf.yaml buf.gen.yaml gen/go web/package.json web/pnpm-lock.yaml web/tsconfig.json web/src/gen go.mod go.sum
git commit -m "feat: 添加 ConnectRPC 协议契约"
```

### Task 5: 实现 ConnectRPC 控制接口与 mTLS 双向流

**Files:**
- Create: `internal/transport/connectrpc/mapper.go`
- Create: `internal/transport/connectrpc/control_handler.go`
- Create: `internal/transport/connectrpc/control_handler_test.go`
- Create: `internal/transport/connectrpc/registry.go`
- Create: `internal/transport/connectrpc/registry_test.go`
- Create: `internal/transport/connectrpc/agent_handler.go`
- Create: `internal/transport/connectrpc/agent_handler_test.go`
- Create: `internal/transport/connectrpc/worker_client.go`
- Create: `internal/transport/connectrpc/tls.go`
- Create: `scripts/generate-dev-certs.sh`

**Interfaces:**
- Consumes: Task 2 的控制面服务和 worker runtime。
- Produces: `transport.NewControlHandler`、`transport.NewAgentHandler`。
- Produces: `Registry`，同时实现远程 `CommandBus`。
- Produces: `WorkerClient.Run(context.Context)`。

- [ ] **Step 1: 写 registry 失败测试**

```go
func TestRegistryDispatchesToWorker(t *testing.T) {
    registry := transport.NewRegistry(time.Second)
    send := make(chan *demov1.ControlMessage, 1)
    unregister := registry.Register("worker-1", send)
    defer unregister()

    err := registry.Dispatch(context.Background(), domain.Command{
        ID: "cmd-1", WorkerID: "worker-1", Input: "hello",
    })
    if err != nil {
        t.Fatal(err)
    }
    if got := (<-send).GetCommand().GetInput(); got != "hello" {
        t.Fatalf("输入 = %q", got)
    }
}
```

- [ ] **Step 2: 实现线程安全连接表**

`Registry` 使用 `sync.RWMutex` 和 `map[string]chan *demov1.ControlMessage`。重复 worker ID 注册时替换旧连接；注销闭包只删除仍属于本次注册的连接，避免旧连接关闭误删新连接。

- [ ] **Step 3: 写 ControlService handler 测试并实现**

使用内存 repository 与 presence，调用生成的 Connect 客户端：

```go
client := demov1connect.NewControlServiceClient(server.Client(), server.URL+"/api")
response, err := client.CreateCommand(ctx, connect.NewRequest(&demov1.CreateCommandRequest{
    WorkerId: "worker-1",
    Input:    "hello",
}))
```

断言响应状态为 `DISPATCHED`，空输入返回 `connect.CodeInvalidArgument`，离线 worker 返回带 `DISPATCH_FAILED` 状态的响应。

- [ ] **Step 4: 生成开发证书并写 TLS 测试**

脚本使用 OpenSSL 生成：

- `var/certs/ca.crt`、`ca.key`；
- `server.crt`、`server.key`，SAN 为 `DNS:localhost,IP:127.0.0.1`；
- `worker-1.crt`、`worker-1.key`，Subject CN 为 `worker-1`。

测试断言：

- 无客户端证书无法完成 TLS 握手；
- worker-1 证书可以连接；
- `Hello.worker_id` 与证书 CN 不一致时返回 `connect.CodeUnauthenticated`。

- [ ] **Step 5: 实现 AgentService handler**

handler 首条消息必须是 `Hello`。验证证书身份后注册发送 channel，写入 presence，并并行执行：

- 单一发送循环：从 channel 读取并调用 `stream.Send`；
- 接收循环：调用 `stream.Receive`，处理心跳和领域事件；
- 任一循环结束即取消另一个循环并注销连接。

禁止多个 goroutine 同时调用同一个流的 `Send`。

- [ ] **Step 6: 实现 WorkerClient**

`Run` 使用带客户端证书的 `http.Client` 创建 Connect 客户端。每次连接：

1. 发送 `Hello`；
2. 启动单一事件发送循环；
3. 每 10 秒发送心跳；
4. 接收 `Command` 后调用同一个 `worker.Runtime`；
5. 断线后按 1 秒、2 秒、4 秒递增，最大 30 秒重连。

- [ ] **Step 7: 运行传输测试**

Run: `go test ./internal/transport/connectrpc -race`

Expected: PASS，且无 data race。

- [ ] **Step 8: 提交**

```bash
git add internal/transport scripts .gitignore go.mod go.sum
git commit -m "feat: 添加 ConnectRPC 双向流和 mTLS"
```

### Task 6: 装配三个入口

**Files:**
- Create: `internal/bootstrap/config.go`
- Create: `internal/bootstrap/config_test.go`
- Create: `internal/bootstrap/control.go`
- Create: `internal/bootstrap/worker.go`
- Create: `internal/bootstrap/all.go`
- Create: `internal/bootstrap/run.go`
- Create: `cmd/platform/main.go`
- Create: `cmd/platform/main_test.go`
- Create: `cmd/control/main.go`
- Create: `cmd/worker/main.go`

**Interfaces:**
- Consumes: 前述所有业务、存储和传输组件。
- Produces: `bootstrap.Run(ctx, mode, args, webFS)`。
- Produces: 三个可执行入口。

- [ ] **Step 1: 写 platform 默认模式测试**

```go
func TestParseModeDefaultsToAll(t *testing.T) {
    mode, rest, err := parseMode(nil)
    if err != nil {
        t.Fatal(err)
    }
    if mode != "all" || len(rest) != 0 {
        t.Fatalf("mode=%q rest=%v", mode, rest)
    }
}
```

同时测试 `all`、`control`、`worker` 被识别，未知子命令返回错误。

- [ ] **Step 2: 实现配置与模式解析**

默认配置：

```text
DATABASE_URL=postgres://demo:demo@localhost:5432/demo?sslmode=disable
REDIS_ADDR=localhost:6379
PUBLIC_ADDR=:8080
AGENT_ADDR=:8443
CONTROL_URL=https://localhost:8443
WORKER_ID=worker-1
CERT_DIR=var/certs
```

命令行 `--worker-id` 覆盖环境变量。`platform` 没有参数或首个参数以 `-` 开头时使用 `all`。

- [ ] **Step 3: 装配 control**

`RunControl` 必须：

1. 连接 PostgreSQL 与 Redis并执行 migration；
2. 创建 control service 与远程 registry；
3. 在公共 mux 的 `/api/` 挂载 ControlService；
4. 在 agent mux 只挂载 AgentService；
5. 使用普通 HTTP 启动公共 server；
6. 使用强制 mTLS 启动 agent server；
7. 在 context 取消时优雅关闭。

- [ ] **Step 4: 装配 all**

`RunAll` 复用 `RunControl` 的组件构建函数，但把 dispatcher 替换为 `LocalCommandBus`，注册 ID 为 `local-worker` 的本地 presence，并启动每 10 秒刷新一次的本地心跳循环。

- [ ] **Step 5: 装配 worker 与三个 main**

`RunWorker` 创建 TLS 配置、worker runtime 和 `WorkerClient`。三个 main 只建立 signal context、解析参数并调用 bootstrap。

- [ ] **Step 6: 构建与测试**

Run: `go test ./internal/bootstrap ./cmd/platform`

Run: `go build -o bin/platform ./cmd/platform`

Run: `go build -o bin/control ./cmd/control`

Run: `go build -o bin/worker ./cmd/worker`

Expected: 三个构建命令成功；`./bin/platform --help` 显示默认模式为 all。

- [ ] **Step 7: 提交**

```bash
git add internal/bootstrap cmd go.mod go.sum
git commit -m "feat: 添加单进程和拆分部署入口"
```

### Task 7: 实现 React 页面与嵌入资源

**Files:**
- Create: `web/index.html`
- Create: `web/vite.config.ts`
- Create: `web/tsconfig.app.json`
- Create: `web/src/main.tsx`
- Create: `web/src/App.tsx`
- Create: `web/src/App.test.tsx`
- Create: `web/src/styles.css`
- Create: `internal/webui/embed.go`
- Generate: `internal/webui/dist/*`
- Modify: `internal/bootstrap/control.go`
- Modify: `cmd/platform/main.go`

**Interfaces:**
- Consumes: 生成的 TypeScript ControlService。
- Produces: worker 列表、任务提交和状态轮询页面。
- Produces: `webui.FS()`。

- [ ] **Step 1: 写页面失败测试**

使用 Vitest 与 Testing Library，注入假的 API：

```tsx
it("选择 worker 后提交任务并显示结果", async () => {
  render(<App api={fakeAPI({
    workers: [{ id: "worker-1", version: "demo" }],
    command: { id: "cmd-1", status: "SUCCEEDED", output: "HELLO" },
  })} />);

  await userEvent.type(screen.getByLabelText("任务文本"), "hello");
  await userEvent.click(screen.getByRole("button", { name: "下发任务" }));

  expect(await screen.findByText("HELLO")).toBeInTheDocument();
});
```

- [ ] **Step 2: 运行测试确认失败**

Run: `pnpm --dir web test --run`

Expected: FAIL，提示 `App` 或页面元素未定义。

- [ ] **Step 3: 实现页面**

页面只包含：

- 部署拓扑说明；
- 在线 worker 下拉框；
- 任务文本输入；
- 下发按钮；
- 当前任务状态、输出和错误。

API adapter 使用 `createClient(ControlService, createConnectTransport({baseUrl: "/api"}))`。任务创建后每 500 毫秒调用 `GetCommand`，到终态后停止。

- [ ] **Step 4: 实现嵌入静态资源**

```go
//go:embed dist
var assets embed.FS

func FS() fs.FS {
    root, err := fs.Sub(assets, "dist")
    if err != nil {
        panic(err)
    }
    return root
}
```

公共 server 使用 SPA fallback：找不到静态文件时返回 `index.html`；`/api/` 不得进入 fallback。

- [ ] **Step 5: 构建并测试**

Run: `pnpm --dir web test --run`

Run: `pnpm --dir web build`

Run: `rsync -a --delete web/dist/ internal/webui/dist/`

Run: `go test ./...`

Run: `go build -o bin/platform ./cmd/platform`

Expected: 全部通过，`bin/platform` 包含可访问的 React 页面。

- [ ] **Step 6: 提交**

```bash
git add web internal/webui internal/bootstrap/control.go cmd/platform/main.go
git commit -m "feat: 添加 React 控制台和嵌入资源"
```

### Task 8: 部署文件、文档和三拓扑验证

**Files:**
- Create: `deploy/nginx.conf`
- Create: `Makefile`
- Create: `README.md`
- Create: `scripts/smoke.sh`
- Modify: `compose.yaml`

**Interfaces:**
- Produces: `make generate`、`make build`、`make test`、`make certs`。
- Produces: 自动验证三个拓扑的脚本和人工运行说明。

- [ ] **Step 1: 添加 Nginx 与 Makefile**

Nginx：

```nginx
location / {
    try_files $uri $uri/ /index.html;
}

location /api/ {
    proxy_pass http://control:8080;
    proxy_http_version 1.1;
}
```

Makefile 的 `build` 依次执行 generate、前端构建、复制嵌入资源和三个 `go build`。

- [ ] **Step 2: 编写 README**

README 必须给出：

1. `docker compose up -d postgres redis`；
2. `make certs build`；
3. `./bin/platform` 单进程；
4. `./bin/control` + `pnpm --dir web dev` 前后端分离；
5. `./bin/control` + `./bin/worker --worker-id worker-1` 控制面/worker 分离；
6. 访问地址、环境变量和 PostgreSQL/Redis 检查命令。

- [ ] **Step 3: 编写冒烟脚本**

`scripts/smoke.sh` 依次使用临时端口启动每种后端拓扑，通过生成的 Go ControlService client 或 `curl` 调用 Connect JSON Unary 接口，断言最终输出为 `HELLO`，并在退出时清理自己启动的进程。

- [ ] **Step 4: 运行完整验证**

Run: `make test`

Expected: Go 单元、race、前端和集成测试全部通过。

Run: `make build`

Expected: 生成 `bin/platform`、`bin/control`、`bin/worker`。

Run: `./scripts/smoke.sh`

Expected: 输出三行 PASS，分别对应单进程、前后端分离 API、远程 worker。

- [ ] **Step 5: 检查二进制入口**

Run: `./bin/platform --help`

Run: `./bin/platform all --help`

Run: `./bin/platform control --help`

Run: `./bin/platform worker --help`

Expected: 无子命令时帮助说明明确默认 `all`，三个显式子命令均可用。

- [ ] **Step 6: 提交**

```bash
git add deploy Makefile README.md scripts/smoke.sh compose.yaml
git commit -m "docs: 添加三种部署模式和验证脚本"
```

### Task 9: 最终回归与交付

**Files:**
- Modify: 仅修改最终回归发现的、与本 Demo 直接相关的文件。

**Interfaces:**
- Produces: 可复现构建、通过的测试和干净工作区。

- [ ] **Step 1: 格式与静态检查**

Run: `gofmt -w cmd internal gen/go`

Run: `go vet ./...`

Run: `pnpm --dir web exec tsc --noEmit`

Expected: 无错误。

- [ ] **Step 2: 完整测试**

Run: `go test ./... -race`

Run: `pnpm --dir web test --run`

Expected: 全部 PASS。

- [ ] **Step 3: 干净环境构建**

Run: `make clean build`

Expected: 三个二进制重新生成成功。

- [ ] **Step 4: 三拓扑验证**

Run: `./scripts/smoke.sh`

Expected: 单进程、前后端分离 API、远程 worker 三项全部 PASS。

- [ ] **Step 5: 检查 Git 状态并提交必要修复**

Run: `git status --short`

Expected: 除预期生成物规则外工作区干净；如有最终修复，使用中文提交信息单独提交。
