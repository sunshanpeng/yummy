-- +goose Up
-- 每次构建固定一个下载授权，明文经主密钥加密，摘要用于认证；不保存到 URL 或日志。
CREATE TABLE source_build_grants (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    build_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL UNIQUE,
    node_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    certificate_serial TEXT NOT NULL REFERENCES worker_certificates(serial) ON DELETE RESTRICT,
    token_hash TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    encrypted_token BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (workspace_id, application_id, snapshot_id)
        REFERENCES source_snapshots(workspace_id, application_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, build_id, attempt_id)
        REFERENCES build_attempts(workspace_id, application_id, build_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (node_id, worker_id) REFERENCES execution_nodes(id, worker_id) ON DELETE RESTRICT,
    CHECK (expires_at > created_at)
);
CREATE INDEX source_build_grants_expiry_idx ON source_build_grants(expires_at);

-- +goose Down
DROP TABLE source_build_grants;
