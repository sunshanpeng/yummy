-- +goose Up
-- 应用运行日志块与 CPU/内存采样：worker 经 WorkerGateway 上行，控制面持久化并提供查询。
-- 不建后台清理服务（控制面 + worker 两进程边界），保留窗口在写入事务内收敛，
-- 常量与语义见 internal/platform/deployment 的 runtimeobs.go。
CREATE TABLE runtime_log_chunks (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    pod_uid TEXT NOT NULL CHECK (pod_uid <> '' AND length(pod_uid) <= 64),
    container TEXT NOT NULL CHECK (container <> '' AND length(container) <= 128),
    restart_count INTEGER NOT NULL CHECK (restart_count >= 0),
    seq BIGINT NOT NULL CHECK (seq > 0),
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    content BYTEA NOT NULL CHECK (octet_length(content) <= 16384),
    truncated BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 幂等去重键：同键同 hash 幂等 ack，同键不同 hash 视为序号冲突。
    UNIQUE (workspace_id, application_id, pod_uid, container, restart_count, seq),
    FOREIGN KEY (workspace_id, application_id) REFERENCES applications(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX runtime_log_chunks_listing_idx ON runtime_log_chunks(workspace_id, application_id, created_at, id);
CREATE TABLE runtime_metric_samples (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    release_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    pod_uid TEXT NOT NULL CHECK (pod_uid <> '' AND length(pod_uid) <= 128),
    cpu_nano_cores BIGINT NOT NULL CHECK (cpu_nano_cores >= 0),
    cpu_window_millis BIGINT NOT NULL CHECK (cpu_window_millis > 0),
    memory_working_set_bytes BIGINT NOT NULL CHECK (memory_working_set_bytes >= 0),
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    collected_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 幂等键：worker 每个采样点只落库一次；重传同键同 hash 幂等，异值冲突。
    UNIQUE (pod_uid, collected_at),
    FOREIGN KEY (workspace_id, application_id) REFERENCES applications(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX runtime_metric_samples_window_idx ON runtime_metric_samples(workspace_id, application_id, collected_at);
-- +goose Down
DROP TABLE runtime_metric_samples;
DROP TABLE runtime_log_chunks;
