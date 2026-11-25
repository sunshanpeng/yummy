-- +goose Up
-- 存量自动发布未保存运行代际，NULL 保留原行为。新客户端可显式保存 0 表示从未发布。
ALTER TABLE builds ADD COLUMN IF NOT EXISTS expected_runtime_generation BIGINT DEFAULT NULL
    CHECK (expected_runtime_generation >= 0);
