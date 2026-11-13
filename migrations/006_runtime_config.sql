-- +goose Up
-- 保存产生不可变配置版本；发布生效由独立 Release 引用，不在此处修改线上状态。
CREATE TABLE runtime_config_revisions (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision >= 1),
    confirmed BOOLEAN NOT NULL DEFAULT FALSE,
    spec JSONB NOT NULL,
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (application_id, revision),
    UNIQUE (workspace_id, application_id, revision),
    FOREIGN KEY (workspace_id, application_id) REFERENCES applications(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX runtime_config_application_time_idx ON runtime_config_revisions(application_id, created_at DESC);
