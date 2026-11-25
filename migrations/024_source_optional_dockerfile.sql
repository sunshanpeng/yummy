-- +goose Up
-- 构建方式自动选择（2026-11-23）：上传时不再强制 Dockerfile。空串表示未显式指定路径，
-- 此时是否使用 Dockerfile 由构建受理按上下文根目录自动决定，缺失则交给 Railpack。
-- 显式路径的存在性仍由上传完成时的归档检查保证。
ALTER TABLE source_uploads DROP CONSTRAINT IF EXISTS source_uploads_dockerfile_check;
ALTER TABLE source_snapshots DROP CONSTRAINT IF EXISTS source_snapshots_dockerfile_check;

-- +goose Down
-- 仅作结构演示：回退要求现存行都有 Dockerfile，纯源码快照需先清理。
ALTER TABLE source_uploads ADD CONSTRAINT source_uploads_dockerfile_check CHECK (dockerfile <> '');
ALTER TABLE source_snapshots ADD CONSTRAINT source_snapshots_dockerfile_check CHECK (dockerfile <> '');
