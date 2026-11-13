-- +goose Up
-- OAuth 协议状态属于现有控制面 PostgreSQL，令牌只保存 Fosite 签名，不保存完整 Bearer。
CREATE TABLE oauth_clients (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, source TEXT NOT NULL,
 redirect_uris JSONB NOT NULL, scopes JSONB NOT NULL, resources JSONB NOT NULL,
 active BOOLEAN NOT NULL DEFAULT TRUE, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE oauth_requests (
 id TEXT PRIMARY KEY, parameters JSONB NOT NULL, user_id TEXT REFERENCES users(id) ON DELETE RESTRICT,
 csrf_hash TEXT NOT NULL DEFAULT '', expires_at TIMESTAMPTZ NOT NULL, consumed_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE oauth_grants (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
 client_id TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE RESTRICT,
 resource TEXT NOT NULL, application_ids JSONB NOT NULL, scopes JSONB NOT NULL,
 auth_version BIGINT NOT NULL CHECK(auth_version >= 1), expires_at TIMESTAMPTZ NOT NULL,
 revoked_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX oauth_grants_owner_idx ON oauth_grants(user_id, created_at DESC, id);
CREATE TABLE oauth_sessions (
 kind TEXT NOT NULL CHECK(kind IN ('code','access','refresh','pkce')),
 signature TEXT NOT NULL, request_id TEXT NOT NULL REFERENCES oauth_grants(id) ON DELETE RESTRICT,
 payload JSONB NOT NULL, active BOOLEAN NOT NULL DEFAULT TRUE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY(kind,signature)
);
CREATE INDEX oauth_sessions_request_idx ON oauth_sessions(request_id,kind);
CREATE TABLE oauth_client_assertions (
 jti_hash TEXT PRIMARY KEY, expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE oidc_identity_bindings (
 issuer TEXT NOT NULL, subject TEXT NOT NULL, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(issuer,subject)
);

CREATE TABLE oidc_login_requests (
 id TEXT PRIMARY KEY, provider_name TEXT NOT NULL, state_hash TEXT NOT NULL UNIQUE,
 browser_binding_hash TEXT NOT NULL, nonce_hash TEXT NOT NULL,
 pkce_verifier TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL, consumed_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX oidc_login_requests_expiry_idx ON oidc_login_requests(expires_at);

-- +goose Down
DROP TABLE oidc_login_requests;
DROP TABLE oidc_identity_bindings;
DROP TABLE oauth_client_assertions;
DROP TABLE oauth_sessions;
DROP TABLE oauth_grants;
DROP TABLE oauth_requests;
DROP TABLE oauth_clients;
