-- +goose Up
-- 历史日志缺少来源 Pod 的发布标识，保留为空，仍可按应用查询。
ALTER TABLE runtime_log_chunks ADD COLUMN IF NOT EXISTS release_id TEXT;
ALTER TABLE runtime_log_chunks ADD CONSTRAINT runtime_log_chunks_release_fk
    FOREIGN KEY (workspace_id, application_id, release_id)
    REFERENCES application_releases (workspace_id, application_id, id) ON DELETE RESTRICT;
CREATE INDEX runtime_log_chunks_release_idx ON runtime_log_chunks (workspace_id, application_id, release_id, created_at, id);
