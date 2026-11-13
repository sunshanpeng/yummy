-- +goose Up
-- 构建配置只含版本引用，凭证明文永不进入构建或事件快照。
CREATE TABLE build_secret_references (
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    build_id TEXT NOT NULL,
    secret_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    name TEXT NOT NULL,
    PRIMARY KEY (build_id, secret_id),
    UNIQUE (build_id, name),
    FOREIGN KEY (workspace_id, application_id, build_id) REFERENCES builds(workspace_id, application_id, id) ON DELETE RESTRICT,
    FOREIGN KEY (workspace_id, application_id, secret_id, version) REFERENCES application_build_secret_versions(workspace_id, application_id, secret_id, version) ON DELETE RESTRICT
);
-- +goose Down
DROP TABLE build_secret_references;
