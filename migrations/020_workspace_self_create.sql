-- +goose Up
-- SaaS 自由建空间开关（实例级，默认关=只有实例管理员可创建工作空间）。
ALTER TABLE instance_settings ADD COLUMN IF NOT EXISTS workspace_self_create boolean NOT NULL DEFAULT false;
-- +goose Down
ALTER TABLE instance_settings DROP COLUMN workspace_self_create;
