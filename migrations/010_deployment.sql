-- +goose Up
-- 首次发布受理固定运行位置；发布事实、期望态和观察态分别保存。
CREATE TABLE application_placements (
    application_id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    instance_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    cluster_node_uid TEXT NOT NULL CHECK (cluster_node_uid <> ''),
    policy_revision BIGINT NOT NULL,
    runtime_type TEXT NOT NULL CHECK (runtime_type IN ('HTTP','WORKER')),
    default_domain TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, application_id),
    FOREIGN KEY (workspace_id, application_id) REFERENCES applications(workspace_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (instance_id, node_id) REFERENCES execution_nodes(instance_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (node_id, worker_id) REFERENCES execution_nodes(id, worker_id) ON DELETE RESTRICT,
    FOREIGN KEY (instance_id, policy_revision) REFERENCES instance_execution_policies(instance_id, revision) ON DELETE RESTRICT,
    CHECK (runtime_type = 'HTTP' OR default_domain = '')
);
CREATE TABLE application_releases (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    artifact_id TEXT NOT NULL,
    runtime_config_revision BIGINT NOT NULL CHECK (runtime_config_revision > 0),
    registry_id TEXT NOT NULL,
    credential_version BIGINT NOT NULL,
    repository TEXT NOT NULL CHECK (repository <> ''),
    digest TEXT NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    platform TEXT NOT NULL CHECK (platform = 'linux/amd64'),
    placement_snapshot JSONB NOT NULL CHECK (jsonb_typeof(placement_snapshot) = 'object'),
    domains JSONB NOT NULL CHECK (jsonb_typeof(domains) = 'array'),
    previous_serving_id TEXT,
    rollback_of TEXT,
    source TEXT NOT NULL CHECK (source IN ('DEPLOY','ROLLBACK','RECOVERY')),
    auto_recovery_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    actor_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, application_id, id),
    FOREIGN KEY (workspace_id, application_id) REFERENCES application_placements(workspace_id, application_id) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, artifact_id) REFERENCES build_artifacts(workspace_id, application_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, runtime_config_revision) REFERENCES runtime_config_revisions(workspace_id, application_id, revision) ON DELETE RESTRICT,
    FOREIGN KEY (registry_id, credential_version) REFERENCES registry_credential_versions(registry_id, version) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, previous_serving_id) REFERENCES application_releases(workspace_id, application_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, rollback_of) REFERENCES application_releases(workspace_id, application_id, id) ON DELETE RESTRICT
);
CREATE TABLE deployment_operations (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    release_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    action TEXT NOT NULL CHECK (action IN ('DEPLOY','ROLLBACK','RECOVERY','RESTART','STOP','START')),
    state TEXT NOT NULL CHECK (state IN ('QUEUED','APPLYING','WAITING_READY','VERIFYING_INGRESS','RECONCILING','SUCCEEDED','FAILED','SUPERSEDED')),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    actor_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    fingerprint TEXT NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    error_code TEXT NOT NULL DEFAULT '',
    recovery_of TEXT REFERENCES deployment_operations(id) ON DELETE RESTRICT,
    event_seq BIGINT NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
    deployment_uid TEXT NOT NULL DEFAULT '',
    pod_uid TEXT NOT NULL DEFAULT '',
    kubernetes_ready BOOLEAN NOT NULL DEFAULT FALSE,
    last_observed_at TIMESTAMPTZ,
    next_dispatch_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_observation_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    dispatch_count INTEGER NOT NULL DEFAULT 0 CHECK (dispatch_count >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    UNIQUE (application_id, generation),
    UNIQUE (workspace_id, application_id, id),
    UNIQUE (workspace_id, application_id, actor_id, idempotency_key),
    FOREIGN KEY (workspace_id, application_id, release_id) REFERENCES application_releases(workspace_id, application_id, id) ON DELETE RESTRICT,
    CHECK (active = (state IN ('QUEUED','APPLYING','WAITING_READY','VERIFYING_INGRESS','RECONCILING')))
);
CREATE UNIQUE INDEX deployment_operations_one_active_idx ON deployment_operations(application_id) WHERE active;
CREATE UNIQUE INDEX deployment_operations_one_recovery_idx ON deployment_operations(recovery_of) WHERE recovery_of IS NOT NULL;
CREATE INDEX deployment_operations_pending_idx ON deployment_operations(next_dispatch_at, id) WHERE active;
CREATE INDEX deployment_operations_history_idx ON deployment_operations(workspace_id, application_id, created_at DESC, id DESC);
CREATE TABLE application_runtime_states (
    application_id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    generation BIGINT NOT NULL DEFAULT 0 CHECK (generation >= 0),
    desired_state TEXT NOT NULL CHECK (desired_state IN ('RUN','STOP')),
    desired_release_id TEXT,
    serving_release_id TEXT,
    active_operation_id TEXT,
    serving_generation BIGINT NOT NULL DEFAULT 0 CHECK (serving_generation >= 0),
    observed_generation BIGINT NOT NULL DEFAULT 0 CHECK (observed_generation >= 0),
    observed_state TEXT NOT NULL DEFAULT 'NOT_DEPLOYED' CHECK (observed_state IN ('NOT_DEPLOYED','PENDING','RUNNING','UNHEALTHY','STOPPING','STOPPED','UNKNOWN')),
    deployment_uid TEXT NOT NULL DEFAULT '',
    pod_uid TEXT NOT NULL DEFAULT '',
    last_observed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (workspace_id, application_id) REFERENCES applications(workspace_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, desired_release_id) REFERENCES application_releases(workspace_id, application_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, serving_release_id) REFERENCES application_releases(workspace_id, application_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, active_operation_id) REFERENCES deployment_operations(workspace_id, application_id, id) ON DELETE RESTRICT,
    CHECK (serving_generation <= generation AND observed_generation <= generation)
);
-- 历史发布引用的域名不转授其他应用，默认域名更不会随实例策略变更。
CREATE TABLE application_domain_claims (
    domain TEXT PRIMARY KEY CHECK (domain <> '' AND domain = LOWER(domain)),
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    is_default BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (workspace_id, application_id) REFERENCES application_placements(workspace_id, application_id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX application_domain_claims_default_idx ON application_domain_claims(application_id) WHERE is_default;
CREATE TABLE runtime_observations (
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    seq BIGINT NOT NULL CHECK (seq > 0),
    node_id TEXT NOT NULL REFERENCES execution_nodes(id) ON DELETE RESTRICT,
    worker_id TEXT NOT NULL,
    certificate_serial TEXT NOT NULL REFERENCES worker_certificates(serial) ON DELETE RESTRICT,
    payload_hash TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    collected_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (operation_id, seq),
    FOREIGN KEY (workspace_id, application_id, operation_id) REFERENCES deployment_operations(workspace_id, application_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (node_id, worker_id) REFERENCES execution_nodes(id, worker_id) ON DELETE RESTRICT
);
-- 入口探测只能由控制面提交，不能以 worker 声称的 HTTP 成功替代。
CREATE TABLE deployment_probe_evidence (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    domain TEXT NOT NULL REFERENCES application_domain_claims(domain) ON DELETE RESTRICT,
    path TEXT NOT NULL,
    pod_uid TEXT NOT NULL,
    tls_verified BOOLEAN NOT NULL,
    healthy BOOLEAN NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (workspace_id, application_id, operation_id) REFERENCES deployment_operations(workspace_id, application_id, id) ON DELETE RESTRICT
);
