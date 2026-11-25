-- +goose Up
ALTER TABLE applications DROP CONSTRAINT applications_status_check;
ALTER TABLE applications ADD CONSTRAINT applications_status_check
    CHECK (status IN ('ACTIVE', 'ARCHIVED', 'DELETED'));
-- 【数据库专有】保留原应用名称和外键引用，只对占用名称的记录保证唯一；MySQL 可用可空的生成列建立唯一索引。
CREATE UNIQUE INDEX applications_reserved_name_idx ON applications (workspace_id, name)
    WHERE status IN ('ACTIVE', 'ARCHIVED');
ALTER TABLE applications DROP CONSTRAINT applications_workspace_id_name_key;
