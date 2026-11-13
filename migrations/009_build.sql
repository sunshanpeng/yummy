-- +goose Up
-- 构建请求、一次执行身份和产物验证分别保存；不把 LOST 当作已停止。
CREATE TABLE build_config_revisions (
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    spec JSONB NOT NULL CHECK (jsonb_typeof(spec) = 'object'),
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, application_id, revision),
    FOREIGN KEY (workspace_id, application_id) REFERENCES applications(workspace_id, id) ON DELETE RESTRICT
);
CREATE TABLE builds (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    actor_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    actor_auth_version BIGINT NOT NULL,
    token_id TEXT REFERENCES access_tokens(id) ON DELETE RESTRICT,
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    fingerprint TEXT NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    source_snapshot_id TEXT NOT NULL,
    source_sha256 TEXT NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
    config_revision BIGINT NOT NULL CHECK (config_revision >= 0),
    config_snapshot JSONB NOT NULL CHECK (jsonb_typeof(config_snapshot) = 'object'),
    instance_id TEXT NOT NULL,
    policy_revision BIGINT NOT NULL,
    node_id TEXT NOT NULL,
    registry_id TEXT NOT NULL,
    credential_version BIGINT NOT NULL,
    repository TEXT NOT NULL CHECK (repository <> ''),
    tag TEXT NOT NULL CHECK (tag = id),
    platform TEXT NOT NULL CHECK (platform = 'linux/amd64'),
    execution_snapshot JSONB NOT NULL CHECK (jsonb_typeof(execution_snapshot) = 'object'),
    auto_deploy BOOLEAN NOT NULL DEFAULT FALSE,
    runtime_config_revision BIGINT,
    expected_serving_release TEXT NOT NULL DEFAULT '',
    retry_of TEXT,
    state TEXT NOT NULL DEFAULT 'QUEUED' CHECK (state IN ('QUEUED','STARTING','RUNNING','CANCELING','RECONCILING','SUCCEEDED','FAILED','CANCELED','TIMED_OUT','LOST')),
    stage TEXT NOT NULL DEFAULT '',
    desired_state TEXT NOT NULL DEFAULT 'RUN' CHECK (desired_state IN ('RUN','CANCEL')),
    stop_reason TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    queue_reason TEXT NOT NULL DEFAULT '',
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    logs_bytes BIGINT NOT NULL DEFAULT 0 CHECK (logs_bytes BETWEEN 0 AND 20971520),
    logs_complete BOOLEAN NOT NULL DEFAULT FALSE,
    logs_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    deadline_at TIMESTAMPTZ NOT NULL,
    UNIQUE (workspace_id, actor_id, idempotency_key),
    UNIQUE (workspace_id, application_id, id),
    UNIQUE (workspace_id, application_id, id, node_id),
    FOREIGN KEY (workspace_id, application_id) REFERENCES applications(workspace_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, source_snapshot_id) REFERENCES source_snapshots(workspace_id, application_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (instance_id, policy_revision) REFERENCES instance_execution_policies(instance_id, revision) ON DELETE RESTRICT,
    FOREIGN KEY (instance_id, node_id) REFERENCES execution_nodes(instance_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (instance_id, registry_id) REFERENCES registries(instance_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (registry_id, credential_version) REFERENCES registry_credential_versions(registry_id, version) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, runtime_config_revision) REFERENCES runtime_config_revisions(workspace_id, application_id, revision) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, retry_of) REFERENCES builds(workspace_id, application_id, id) ON DELETE RESTRICT,
    CHECK (auto_deploy = (runtime_config_revision IS NOT NULL)),
    CHECK (deadline_at > created_at)
);
CREATE INDEX builds_queue_idx ON builds(created_at, id) WHERE state = 'QUEUED';
CREATE INDEX builds_application_idx ON builds(workspace_id, application_id, created_at DESC, id DESC);
CREATE TABLE build_attempts (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    build_id TEXT NOT NULL UNIQUE,
    node_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    job_name TEXT NOT NULL UNIQUE,
    job_uid TEXT NOT NULL DEFAULT '',
    occupied BOOLEAN NOT NULL DEFAULT TRUE,
    event_seq BIGINT NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
    log_seq BIGINT NOT NULL DEFAULT 0 CHECK (log_seq >= 0),
    dispatch_count INTEGER NOT NULL DEFAULT 0 CHECK (dispatch_count >= 0),
    next_dispatch_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_event_at TIMESTAMPTZ,
    reconciling_at TIMESTAMPTZ,
    stopped_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, application_id, build_id, id),
    FOREIGN KEY (workspace_id, application_id, build_id, node_id) REFERENCES builds(workspace_id, application_id, id, node_id) ON DELETE RESTRICT,
    FOREIGN KEY (node_id, worker_id) REFERENCES execution_nodes(id, worker_id) ON DELETE RESTRICT,
    CHECK (occupied OR stopped_at IS NOT NULL)
);
CREATE INDEX build_attempts_node_occupancy_idx ON build_attempts(node_id) WHERE occupied;
CREATE INDEX build_attempts_workspace_occupancy_idx ON build_attempts(workspace_id) WHERE occupied;
CREATE TABLE build_events (
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    build_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    seq BIGINT NOT NULL CHECK (seq > 0),
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (attempt_id, seq),
    FOREIGN KEY (workspace_id, application_id, build_id, attempt_id) REFERENCES build_attempts(workspace_id, application_id, build_id, id) ON DELETE RESTRICT
);
CREATE TABLE build_log_chunks (
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    build_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    seq BIGINT NOT NULL CHECK (seq > 0),
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    content BYTEA NOT NULL CHECK (octet_length(content) <= 16384),
    truncated BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (attempt_id, seq),
    FOREIGN KEY (workspace_id, application_id, build_id, attempt_id) REFERENCES build_attempts(workspace_id, application_id, build_id, id) ON DELETE RESTRICT
);
CREATE TABLE build_artifacts (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    build_id TEXT NOT NULL UNIQUE,
    attempt_id TEXT NOT NULL,
    registry_id TEXT NOT NULL,
    credential_version BIGINT NOT NULL,
    repository TEXT NOT NULL,
    tag TEXT NOT NULL,
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    platform TEXT NOT NULL CHECK (platform = 'linux/amd64'),
    state TEXT NOT NULL CHECK (state IN ('VERIFYING','VERIFIED','RESIDUAL','INVALID')),
    verified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, application_id, id),
    FOREIGN KEY (workspace_id, application_id, build_id, attempt_id) REFERENCES build_attempts(workspace_id, application_id, build_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (registry_id, credential_version) REFERENCES registry_credential_versions(registry_id, version) ON DELETE RESTRICT
);
-- +goose Down
DROP TABLE build_artifacts;
DROP TABLE build_log_chunks;
DROP TABLE build_events;
DROP TABLE build_attempts;
DROP TABLE builds;
DROP TABLE build_config_revisions;
