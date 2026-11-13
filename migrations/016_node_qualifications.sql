-- +goose Up
-- 节点资格只来自安装者线下实际验收后由实例管理员登记的记录；连接自报不是资格来源。
CREATE TABLE node_qualifications (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    cluster_node_uid TEXT NOT NULL CHECK (cluster_node_uid <> '' AND length(cluster_node_uid) <= 256),
    os TEXT NOT NULL DEFAULT 'linux',
    arch TEXT NOT NULL DEFAULT 'amd64',
    build_ready BOOLEAN NOT NULL DEFAULT FALSE,
    runtime_ready BOOLEAN NOT NULL DEFAULT FALSE,
    ingress_ready BOOLEAN NOT NULL DEFAULT FALSE,
    metrics_ready BOOLEAN NOT NULL DEFAULT FALSE,
    qualification_id TEXT NOT NULL CHECK (qualification_id ~ '^[A-Za-z0-9._:-]{1,128}$'),
    evidence JSONB NOT NULL CHECK (JSONB_TYPEOF(evidence) = 'object'),
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'REVOKED')),
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    FOREIGN KEY (node_id, worker_id) REFERENCES execution_nodes(id, worker_id) ON DELETE RESTRICT,
    CHECK ((status = 'ACTIVE') = (revoked_at IS NULL))
);
-- 撤销后 qualification_id 允许复用；活动记录保证上报标识与节点一一对应。
CREATE UNIQUE INDEX node_qualifications_active_qualification_idx ON node_qualifications(qualification_id) WHERE status = 'ACTIVE';
CREATE UNIQUE INDEX node_qualifications_active_node_idx ON node_qualifications(node_id) WHERE status = 'ACTIVE';
CREATE INDEX node_qualifications_node_idx ON node_qualifications(node_id, id);

-- +goose Down
DROP TABLE node_qualifications;
