package bootstrap

import (
	"io"
	"testing"
	"time"
)

func TestConfigFromEnvironmentUsesDemoDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("PUBLIC_ADDR", "")
	t.Setenv("AGENT_ADDR", "")
	t.Setenv("CONTROL_URL", "")
	t.Setenv("WORKER_ID", "")
	t.Setenv("CERT_DIR", "")
	t.Setenv("TLS_SERVER_NAME", "")

	config := ConfigFromEnvironment()

	if config.DatabaseURL != "postgres://demo:demo@localhost:5432/demo?sslmode=disable" {
		t.Fatalf("DatabaseURL = %q", config.DatabaseURL)
	}
	if config.RedisAddr != "localhost:6379" {
		t.Fatalf("RedisAddr = %q", config.RedisAddr)
	}
	if config.PublicAddr != "127.0.0.1:8080" || config.AgentAddr != ":8443" {
		t.Fatalf("addresses = %q, %q", config.PublicAddr, config.AgentAddr)
	}
	if config.ControlURL != "https://localhost:8443" || config.WorkerID != "worker-1" {
		t.Fatalf("worker config = %q, %q", config.ControlURL, config.WorkerID)
	}
	if config.PresenceTTL != 30*time.Second {
		t.Fatalf("PresenceTTL = %v, want 30s", config.PresenceTTL)
	}
}

func TestParseFlagsOverridesWorkerID(t *testing.T) {
	config := ConfigFromEnvironment()

	got, err := ParseFlags(config, []string{"--worker-id", "customer-agent"}, io.Discard)
	if err != nil {
		t.Fatalf("ParseFlags() error = %v", err)
	}
	if got.WorkerID != "customer-agent" {
		t.Fatalf("WorkerID = %q, want customer-agent", got.WorkerID)
	}
}
