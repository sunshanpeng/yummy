-- +goose Up
-- 当前仅用于有明确返回资源的创建操作，不把本表扩成通用任务/工作流引擎。
CREATE TABLE idempotency_keys (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    actor_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action TEXT NOT NULL CHECK (action <> ''),
    key TEXT NOT NULL CHECK (LENGTH(key) BETWEEN 1 AND 128 AND BTRIM(key) <> ''),
    fingerprint TEXT NOT NULL CHECK (fingerprint <> ''),
    result_id TEXT NOT NULL CHECK (result_id <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, actor_id, action, key)
);
