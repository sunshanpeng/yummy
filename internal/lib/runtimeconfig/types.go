// Package runtimeconfig 定义控制面与执行适配器共用的运行配置快照和校验规则。
package runtimeconfig

import "time"

const (
	TypeHTTP   = "HTTP"
	TypeWorker = "WORKER"
)

type Resources struct {
	CPURequestMilli  int64 `json:"cpuRequestMilli"`
	CPULimitMilli    int64 `json:"cpuLimitMilli"`
	MemoryRequestMiB int64 `json:"memoryRequestMiB"`
	MemoryLimitMiB   int64 `json:"memoryLimitMiB"`
}

type Probe struct {
	Path                string `json:"path"`
	InitialDelaySeconds int32  `json:"initialDelaySeconds"`
	PeriodSeconds       int32  `json:"periodSeconds"`
	TimeoutSeconds      int32  `json:"timeoutSeconds"`
	FailureThreshold    int32  `json:"failureThreshold"`
}

type VariableInput struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Sensitive bool   `json:"sensitive"`
	// KeepExisting 与 Value 互斥；保持秘密只能引用该键上一版本的敏感值。
	KeepExisting bool `json:"keepExisting"`
}

type Variable struct {
	Name       string `json:"name"`
	Value      string `json:"value"`
	Sensitive  bool   `json:"sensitive"`
	Configured bool   `json:"configured"`
}

type FileInput struct {
	Path         string `json:"path"`
	Content      string `json:"content"`
	Sensitive    bool   `json:"sensitive"`
	KeepExisting bool   `json:"keepExisting"`
}

type ConfigFile struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Sensitive  bool   `json:"sensitive"`
	Configured bool   `json:"configured"`
}

type Volume struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SizeGiB   int64  `json:"sizeGiB"`
}

type Input struct {
	Type             string          `json:"type"`
	Port             int32           `json:"port"`
	Probe            Probe           `json:"probe"`
	Command          []string        `json:"command"`
	Args             []string        `json:"args"`
	Variables        []VariableInput `json:"variables"`
	Files            []FileInput     `json:"files"`
	Volumes          []Volume        `json:"volumes"`
	Domains          []string        `json:"domains"`
	Resources        Resources       `json:"resources"`
	Confirmed        bool            `json:"confirmed"`
	ExpectedRevision int64           `json:"expectedRevision"`
	IdempotencyKey   string          `json:"idempotencyKey,omitempty"`
}

type View struct {
	WorkspaceID   string       `json:"workspaceId"`
	ApplicationID string       `json:"applicationId"`
	Revision      int64        `json:"revision"`
	Confirmed     bool         `json:"confirmed"`
	Type          string       `json:"type"`
	Port          int32        `json:"port"`
	Probe         Probe        `json:"probe"`
	Command       []string     `json:"command"`
	Args          []string     `json:"args"`
	Variables     []Variable   `json:"variables"`
	Files         []ConfigFile `json:"files"`
	Volumes       []Volume     `json:"volumes"`
	Domains       []string     `json:"domains"`
	Resources     Resources    `json:"resources"`
	CreatedAt     time.Time    `json:"createdAt"`
}

// Materialized 仅供已授权 worker 调度服务使用，不可作为浏览器/CLI/MCP DTO 返回。
type Materialized struct {
	View      View
	Variables map[string]string
	Files     map[string]string
}
