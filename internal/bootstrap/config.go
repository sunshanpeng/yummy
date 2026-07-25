package bootstrap

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	DatabaseURL       string
	RedisAddr         string
	PublicAddr        string
	AgentAddr         string
	ControlURL        string
	WorkerID          string
	Version           string
	CertDir           string
	TLSServerName     string
	PresenceTTL       time.Duration
	HeartbeatInterval time.Duration
}

func ConfigFromEnvironment() Config {
	return Config{
		DatabaseURL:       environmentOrDefault("DATABASE_URL", "postgres://demo:demo@localhost:5432/demo?sslmode=disable"),
		RedisAddr:         environmentOrDefault("REDIS_ADDR", "localhost:6379"),
		PublicAddr:        environmentOrDefault("PUBLIC_ADDR", ":8080"),
		AgentAddr:         environmentOrDefault("AGENT_ADDR", ":8443"),
		ControlURL:        environmentOrDefault("CONTROL_URL", "https://localhost:8443"),
		WorkerID:          environmentOrDefault("WORKER_ID", "worker-1"),
		Version:           environmentOrDefault("WORKER_VERSION", "demo"),
		CertDir:           environmentOrDefault("CERT_DIR", "var/certs"),
		TLSServerName:     environmentOrDefault("TLS_SERVER_NAME", "localhost"),
		PresenceTTL:       30 * time.Second,
		HeartbeatInterval: 10 * time.Second,
	}
}

func ParseFlags(base Config, args []string, output io.Writer) (Config, error) {
	config := base
	flags := flag.NewFlagSet("platform", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&config.DatabaseURL, "database-url", config.DatabaseURL, "PostgreSQL 连接地址")
	flags.StringVar(&config.RedisAddr, "redis-addr", config.RedisAddr, "Redis 地址")
	flags.StringVar(&config.PublicAddr, "public-addr", config.PublicAddr, "控制台和公共 API 监听地址")
	flags.StringVar(&config.AgentAddr, "agent-addr", config.AgentAddr, "worker mTLS 监听地址")
	flags.StringVar(&config.ControlURL, "control-url", config.ControlURL, "worker 连接的控制面地址")
	flags.StringVar(&config.WorkerID, "worker-id", config.WorkerID, "worker ID，必须匹配客户端证书 CN")
	flags.StringVar(&config.Version, "worker-version", config.Version, "worker 版本")
	flags.StringVar(&config.CertDir, "cert-dir", config.CertDir, "mTLS 证书目录")
	flags.StringVar(&config.TLSServerName, "tls-server-name", config.TLSServerName, "控制面服务端证书名称")
	flags.DurationVar(&config.PresenceTTL, "presence-ttl", config.PresenceTTL, "worker 在线状态 TTL")
	flags.DurationVar(&config.HeartbeatInterval, "heartbeat-interval", config.HeartbeatInterval, "worker 心跳间隔")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("不支持的位置参数: %v", flags.Args())
	}
	return config, nil
}

func (c Config) ServerCertFile() string {
	return filepath.Join(c.CertDir, "server.crt")
}

func (c Config) ServerKeyFile() string {
	return filepath.Join(c.CertDir, "server.key")
}

func (c Config) CACertFile() string {
	return filepath.Join(c.CertDir, "ca.crt")
}

func (c Config) WorkerCertFile() string {
	return filepath.Join(c.CertDir, c.WorkerID+".crt")
}

func (c Config) WorkerKeyFile() string {
	return filepath.Join(c.CertDir, c.WorkerID+".key")
}

func environmentOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
