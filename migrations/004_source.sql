-- +goose Up
-- 源码上传与不可变快照分开：只有落盘并验证完成的快照才是 READY。
CREATE TABLE source_uploads (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    expected_size BIGINT NOT NULL CHECK (expected_size > 0 AND expected_size <= 268435456),
    expected_sha256 TEXT NOT NULL CHECK (expected_sha256 ~ '^[0-9a-f]{64}$'),
    dockerfile TEXT NOT NULL CHECK (dockerfile <> ''),
    state TEXT NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'UPLOADED', 'COMPLETE', 'EXPIRED')),
    storage_key TEXT NOT NULL UNIQUE CHECK (storage_key ~ '^upload_[0-9a-f]{32}\.tar\.gz$'),
    snapshot_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    uploaded_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    UNIQUE (workspace_id, application_id, id),
    CONSTRAINT source_uploads_application_fk FOREIGN KEY (workspace_id, application_id)
        REFERENCES applications(workspace_id, id) ON DELETE RESTRICT,
    CHECK (expires_at > created_at),
    CHECK ((state = 'COMPLETE') = (snapshot_id IS NOT NULL)),
    CHECK ((state = 'COMPLETE') = (completed_at IS NOT NULL))
);
CREATE INDEX source_uploads_expiry_idx ON source_uploads (expires_at) WHERE state <> 'COMPLETE';
CREATE INDEX source_uploads_application_idx ON source_uploads (application_id, created_at DESC, id);

CREATE TABLE source_snapshots (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    upload_id TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    state TEXT NOT NULL CHECK (state IN ('READY', 'EXPIRED')),
    storage_key TEXT NOT NULL UNIQUE CHECK (storage_key ~ '^snapshot_[0-9a-f]{32}\.tar\.gz$'),
    sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    compressed_bytes BIGINT NOT NULL CHECK (compressed_bytes > 0 AND compressed_bytes <= 268435456),
    expanded_bytes BIGINT NOT NULL CHECK (expanded_bytes >= 0 AND expanded_bytes <= 1073741824),
    file_bytes BIGINT NOT NULL CHECK (file_bytes >= 0 AND file_bytes <= expanded_bytes),
    entry_count BIGINT NOT NULL CHECK (entry_count > 0 AND entry_count <= 50000),
    dockerfile TEXT NOT NULL CHECK (dockerfile <> ''),
    entries JSONB NOT NULL CHECK (JSONB_TYPEOF(entries) = 'array'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    UNIQUE (workspace_id, application_id, id),
    CONSTRAINT source_snapshots_application_fk FOREIGN KEY (workspace_id, application_id)
        REFERENCES applications(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT source_snapshots_upload_fk FOREIGN KEY (workspace_id, application_id, upload_id)
        REFERENCES source_uploads(workspace_id, application_id, id) ON DELETE RESTRICT,
    CHECK (expires_at > created_at)
);
CREATE INDEX source_snapshots_application_idx ON source_snapshots (application_id, created_at DESC, id);
CREATE INDEX source_snapshots_expiry_idx ON source_snapshots (expires_at) WHERE state = 'READY';
ALTER TABLE source_uploads ADD CONSTRAINT source_uploads_snapshot_fk
    FOREIGN KEY (workspace_id, application_id, snapshot_id)
    REFERENCES source_snapshots(workspace_id, application_id, id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE source_uploads DROP CONSTRAINT source_uploads_snapshot_fk;
DROP TABLE source_snapshots;
DROP TABLE source_uploads;
