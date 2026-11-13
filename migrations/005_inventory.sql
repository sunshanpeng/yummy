-- +goose Up
-- 实例资源不是工作空间拥有的资产；工作空间只消费显式分配范围。
CREATE TABLE instance_settings (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    id TEXT NOT NULL UNIQUE,
    policy_revision BIGINT NOT NULL DEFAULT 0 CHECK (policy_revision >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE execution_nodes (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instance_settings(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (BTRIM(name) <> ''),
    worker_id TEXT NOT NULL UNIQUE CHECK (worker_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED', 'DRAINING')),
    cluster_node_uid TEXT NOT NULL DEFAULT '',
    os TEXT NOT NULL DEFAULT '',
    arch TEXT NOT NULL DEFAULT '',
    connected_at TIMESTAMPTZ,
    last_seen TIMESTAMPTZ,
    build_ready BOOLEAN NOT NULL DEFAULT FALSE,
    runtime_ready BOOLEAN NOT NULL DEFAULT FALSE,
    ingress_ready BOOLEAN NOT NULL DEFAULT FALSE,
    metrics_ready BOOLEAN NOT NULL DEFAULT FALSE,
    readiness_reason TEXT NOT NULL DEFAULT '等待真实节点资格检查',
    capacity INTEGER NOT NULL DEFAULT 1 CHECK (capacity >= 1),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (instance_id, id),
    CHECK (NOT (build_ready OR runtime_ready OR ingress_ready OR metrics_ready) OR (cluster_node_uid <> '' AND os <> '' AND arch <> '' AND last_seen IS NOT NULL))
);
CREATE INDEX execution_nodes_instance_idx ON execution_nodes (instance_id, id);

CREATE TABLE registries (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instance_settings(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (BTRIM(name) <> ''),
    url TEXT NOT NULL CHECK (url LIKE 'https://%'),
    repository_prefix TEXT NOT NULL DEFAULT '',
    username TEXT NOT NULL DEFAULT '',
    encrypted_password BYTEA NOT NULL,
    credential_version BIGINT NOT NULL DEFAULT 1 CHECK (credential_version >= 1),
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED')),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    verified_at TIMESTAMPTZ,
    verified_revision BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (instance_id, id),
    CHECK ((verified_at IS NULL) = (verified_revision IS NULL)),
    CHECK (verified_revision IS NULL OR verified_revision <= revision)
);
CREATE INDEX registries_instance_idx ON registries (instance_id, id);

-- 历史产物以后引用此不可变版本，替换当前口令不抹除旧发布的可追踪身份。
CREATE TABLE registry_credential_versions (
    registry_id TEXT NOT NULL REFERENCES registries(id) ON DELETE RESTRICT,
    version BIGINT NOT NULL CHECK (version >= 1),
    username TEXT NOT NULL DEFAULT '',
    encrypted_password BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (registry_id, version)
);

CREATE TABLE instance_execution_policies (
    instance_id TEXT NOT NULL REFERENCES instance_settings(id) ON DELETE RESTRICT,
    revision BIGINT NOT NULL CHECK (revision >= 1),
    build_node_id TEXT NOT NULL,
    runtime_node_id TEXT NOT NULL,
    registry_id TEXT NOT NULL,
    repository_prefix TEXT NOT NULL DEFAULT '',
    base_domain TEXT NOT NULL DEFAULT '',
    resource_default JSONB NOT NULL CHECK (JSONB_TYPEOF(resource_default) = 'object'),
    resource_limits JSONB NOT NULL CHECK (JSONB_TYPEOF(resource_limits) = 'object'),
    auto_recovery_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (instance_id, revision),
    FOREIGN KEY (instance_id, build_node_id) REFERENCES execution_nodes(instance_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (instance_id, runtime_node_id) REFERENCES execution_nodes(instance_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (instance_id, registry_id) REFERENCES registries(instance_id, id) ON DELETE RESTRICT
);

CREATE TABLE workspace_allocations (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE RESTRICT,
    instance_id TEXT NOT NULL REFERENCES instance_settings(id) ON DELETE RESTRICT,
    max_applications INTEGER NOT NULL CHECK (max_applications >= 1),
    build_concurrency INTEGER NOT NULL CHECK (build_concurrency >= 1),
    cpu_millis BIGINT NOT NULL CHECK (cpu_millis >= 1),
    memory_mib BIGINT NOT NULL CHECK (memory_mib >= 1),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, instance_id)
);
CREATE TABLE workspace_allocation_nodes (
    workspace_id TEXT NOT NULL,
    instance_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    PRIMARY KEY (workspace_id, node_id),
    FOREIGN KEY (workspace_id, instance_id) REFERENCES workspace_allocations(workspace_id, instance_id) ON DELETE CASCADE,
    FOREIGN KEY (instance_id, node_id) REFERENCES execution_nodes(instance_id, id) ON DELETE RESTRICT
);
CREATE INDEX workspace_allocation_nodes_node_idx ON workspace_allocation_nodes (instance_id, node_id);
