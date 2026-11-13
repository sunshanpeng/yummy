-- +goose Up
-- 只新增正式产品表；旧 demo 的 workers / commands 保留原样。
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED')),
    auth_version BIGINT NOT NULL DEFAULT 1 CHECK (auth_version >= 1),
    instance_admin BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT users_username_normalized CHECK (username <> '' AND username = LOWER(BTRIM(username)))
);

CREATE TABLE workspaces (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL CHECK (BTRIM(name) <> ''),
    slug TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z][a-z0-9-]{0,62}$' AND RIGHT(slug, 1) <> '-'),
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'ARCHIVED')),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE workspace_members (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, user_id)
);
CREATE INDEX workspace_members_user_idx ON workspace_members (user_id, workspace_id);

CREATE TABLE applications (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$' AND RIGHT(name, 1) <> '-'),
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'ARCHIVED')),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision >= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, name),
    UNIQUE (workspace_id, id)
);
CREATE INDEX applications_workspace_created_idx ON applications (workspace_id, created_at DESC, id);

CREATE TABLE application_members (
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('maintainer', 'developer', 'viewer')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (application_id, user_id),
    CONSTRAINT application_members_application_fk FOREIGN KEY (workspace_id, application_id)
        REFERENCES applications(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT application_members_workspace_member_fk FOREIGN KEY (workspace_id, user_id)
        REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE
);
CREATE INDEX application_members_user_idx ON application_members (user_id, workspace_id, application_id);
CREATE INDEX application_members_workspace_member_idx ON application_members (workspace_id, user_id);

CREATE TABLE invitations (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    username TEXT NOT NULL CHECK (username <> '' AND username = LOWER(BTRIM(username))),
    role TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    token_hash TEXT NOT NULL UNIQUE CHECK (token_hash <> ''),
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    consumed_by TEXT REFERENCES users(id) ON DELETE RESTRICT,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (expires_at > created_at),
    CHECK ((consumed_at IS NULL) = (consumed_by IS NULL))
);
CREATE INDEX invitations_workspace_created_idx ON invitations (workspace_id, created_at DESC, id);

CREATE TABLE access_tokens (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (BTRIM(name) <> ''),
    token_hash TEXT NOT NULL UNIQUE CHECK (token_hash <> ''),
    scopes JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (JSONB_TYPEOF(scopes) = 'array'),
    auth_version BIGINT NOT NULL CHECK (auth_version >= 1),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    CHECK (expires_at > created_at)
);
CREATE INDEX access_tokens_user_created_idx ON access_tokens (user_id, created_at DESC, id);

-- 令牌授权必须显式列出应用；没有任何关联行表示没有应用权限，不表示全部应用。
CREATE TABLE access_token_applications (
    token_id TEXT NOT NULL REFERENCES access_tokens(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    application_id TEXT NOT NULL,
    PRIMARY KEY (token_id, workspace_id, application_id),
    CONSTRAINT access_token_applications_application_fk FOREIGN KEY (workspace_id, application_id)
        REFERENCES applications(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX access_token_applications_application_idx ON access_token_applications (workspace_id, application_id);

CREATE TABLE password_resets (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    token_hash TEXT NOT NULL UNIQUE CHECK (token_hash <> ''),
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (expires_at > created_at)
);
CREATE INDEX password_resets_user_idx ON password_resets (user_id, created_at DESC);

-- 审计只追加，业务用户和应用使用归档/停用，不因删除级联抹除历史。
CREATE TABLE audit_events (
    id TEXT PRIMARY KEY,
    workspace_id TEXT REFERENCES workspaces(id) ON DELETE RESTRICT,
    application_id TEXT,
    actor_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action TEXT NOT NULL CHECK (action <> ''),
    object_type TEXT NOT NULL,
    object_id TEXT NOT NULL,
    result TEXT NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (application_id IS NULL OR workspace_id IS NOT NULL),
    CONSTRAINT audit_events_application_fk FOREIGN KEY (workspace_id, application_id)
        REFERENCES applications(workspace_id, id) ON DELETE RESTRICT
);
CREATE INDEX audit_events_workspace_created_idx ON audit_events (workspace_id, created_at DESC, id);
CREATE INDEX audit_events_actor_created_idx ON audit_events (actor_id, created_at DESC, id);

-- 首期生产只支持向前升级；不提供删除身份/审计历史的破坏性 Down。
