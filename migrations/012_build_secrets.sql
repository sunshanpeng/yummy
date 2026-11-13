-- +goose Up
-- 构建密钥直接属于应用；运行变量和平台输出仓库凭据不复用此生命周期。
CREATE TABLE application_build_secrets (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    name TEXT NOT NULL CHECK (name ~ '^[A-Za-z_][A-Za-z0-9_.-]{0,62}$'),
    purpose TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED')),
    current_version BIGINT NOT NULL DEFAULT 1 CHECK (current_version >= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, application_id, id),
    UNIQUE (application_id, name),
    FOREIGN KEY (workspace_id, application_id) REFERENCES applications(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX application_build_secrets_application_idx ON application_build_secrets (workspace_id, application_id, id);

CREATE TABLE application_build_secret_versions (
    secret_id TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version >= 1),
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    encrypted_value BYTEA NOT NULL CHECK (OCTET_LENGTH(encrypted_value) BETWEEN 28 AND 65564),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (secret_id, version),
    UNIQUE (workspace_id, application_id, secret_id, version),
    FOREIGN KEY (workspace_id, application_id, secret_id)
        REFERENCES application_build_secrets(workspace_id, application_id, id) ON DELETE RESTRICT
);
ALTER TABLE application_build_secrets ADD CONSTRAINT application_build_secrets_current_version_fk
    FOREIGN KEY (workspace_id, application_id, id, current_version)
    REFERENCES application_build_secret_versions(workspace_id, application_id, secret_id, version)
    DEFERRABLE INITIALLY DEFERRED;
