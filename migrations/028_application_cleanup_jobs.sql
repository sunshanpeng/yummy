-- +goose Up
CREATE TABLE application_cleanup_jobs (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL UNIQUE,
    node_id TEXT NOT NULL REFERENCES execution_nodes(id) ON DELETE RESTRICT,
    cluster_node_uid TEXT NOT NULL DEFAULT '',
    requested_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    delete_volumes BOOLEAN NOT NULL DEFAULT FALSE,
    state TEXT NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'WAITING_VOLUMES', 'FAILED', 'DELETED')),
    error_code TEXT NOT NULL DEFAULT '',
    lease_revision BIGINT NOT NULL DEFAULT 0 CHECK (lease_revision >= 0),
    connection_epoch BIGINT NOT NULL DEFAULT 0 CHECK (connection_epoch >= 0),
    lease_until TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (workspace_id, application_id) REFERENCES applications(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX application_cleanup_jobs_dispatch_idx ON application_cleanup_jobs (node_id, state, lease_until, id);
CREATE INDEX application_cleanup_jobs_workspace_idx ON application_cleanup_jobs (workspace_id, state);
