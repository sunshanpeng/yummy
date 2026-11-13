-- +goose Up
-- 注册码只存摘要；worker 私钥不上传，证书序列和登记节点共同决定有效身份。
ALTER TABLE execution_nodes ADD CONSTRAINT execution_nodes_worker_identity_unique UNIQUE (id, worker_id);
CREATE TABLE worker_registration_tokens (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES execution_nodes(id) ON DELETE RESTRICT,
    token_hash TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    CHECK (expires_at > created_at)
);
CREATE INDEX worker_registration_tokens_node_idx ON worker_registration_tokens(node_id, created_at DESC);
CREATE TABLE worker_certificates (
    serial TEXT PRIMARY KEY CHECK (serial ~ '^[0-9a-f]{1,40}$'),
    node_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    certificate_sha256 TEXT NOT NULL UNIQUE CHECK (certificate_sha256 ~ '^[0-9a-f]{64}$'),
    public_key_sha256 TEXT NOT NULL CHECK (public_key_sha256 ~ '^[0-9a-f]{64}$'),
    not_before TIMESTAMPTZ NOT NULL,
    not_after TIMESTAMPTZ NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    rotated_from TEXT REFERENCES worker_certificates(serial) ON DELETE RESTRICT,
    CONSTRAINT worker_certificates_node_identity_fk FOREIGN KEY (node_id, worker_id)
        REFERENCES execution_nodes(id, worker_id) ON DELETE RESTRICT,
    CHECK (not_after > not_before)
);
CREATE UNIQUE INDEX worker_certificates_one_live_per_node_idx ON worker_certificates(node_id) WHERE revoked_at IS NULL;
CREATE INDEX worker_certificates_node_idx ON worker_certificates(node_id, issued_at DESC);

-- worker 不是用户；不伪造 users actor 写入用户审计。该追加表记录认证节点的证书操作。
CREATE TABLE worker_credential_events (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    serial TEXT NOT NULL REFERENCES worker_certificates(serial) ON DELETE RESTRICT,
    action TEXT NOT NULL CHECK (action IN ('REGISTER', 'ROTATE')),
    request_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (node_id, worker_id) REFERENCES execution_nodes(id, worker_id) ON DELETE RESTRICT
);
CREATE INDEX worker_credential_events_node_idx ON worker_credential_events(node_id, created_at DESC);

-- +goose Down
DROP TABLE worker_credential_events;
DROP TABLE worker_certificates;
DROP TABLE worker_registration_tokens;
ALTER TABLE execution_nodes DROP CONSTRAINT execution_nodes_worker_identity_unique;
