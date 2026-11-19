-- +goose Up
-- 数据库设计规范：每张表必须有单列 id 主键（应用侧由 store 的 Create 回调按
-- security.NewID 前缀风格自动填充）。本迁移为存量表补 id：回填值取业务键拼接
-- 或单列自然键（CONCAT 为 PostgreSQL/MySQL 通用函数），原业务键转 UNIQUE 约束
-- 保留唯一性与外键引用能力。语句顺序：先建业务键 UNIQUE，再删旧复合主键——
-- 引用这些主键的外键依赖由 PostgreSQL 自动转移到同列组的新 UNIQUE 约束。

ALTER TABLE workspace_members ADD COLUMN id TEXT;
UPDATE workspace_members SET id = CONCAT(workspace_id, ':', user_id);
ALTER TABLE workspace_members ALTER COLUMN id SET NOT NULL;
ALTER TABLE workspace_members ADD CONSTRAINT workspace_members_business_key UNIQUE (workspace_id, user_id);
ALTER TABLE workspace_members DROP CONSTRAINT workspace_members_pkey CASCADE;
ALTER TABLE workspace_members ADD CONSTRAINT workspace_members_id_pkey PRIMARY KEY (id);

ALTER TABLE application_members ADD COLUMN id TEXT;
UPDATE application_members SET id = CONCAT(application_id, ':', user_id);
ALTER TABLE application_members ALTER COLUMN id SET NOT NULL;
ALTER TABLE application_members ADD CONSTRAINT application_members_business_key UNIQUE (application_id, user_id);
ALTER TABLE application_members DROP CONSTRAINT application_members_pkey CASCADE;
ALTER TABLE application_members ADD CONSTRAINT application_members_id_pkey PRIMARY KEY (id);

ALTER TABLE access_token_applications ADD COLUMN id TEXT;
UPDATE access_token_applications SET id = CONCAT(token_id, ':', workspace_id, ':', application_id);
ALTER TABLE access_token_applications ALTER COLUMN id SET NOT NULL;
ALTER TABLE access_token_applications ADD CONSTRAINT access_token_applications_business_key UNIQUE (token_id, workspace_id, application_id);
ALTER TABLE access_token_applications DROP CONSTRAINT access_token_applications_pkey CASCADE;
ALTER TABLE access_token_applications ADD CONSTRAINT access_token_applications_id_pkey PRIMARY KEY (id);

ALTER TABLE idempotency_keys ADD COLUMN id TEXT;
UPDATE idempotency_keys SET id = CONCAT(workspace_id, ':', actor_id, ':', action, ':', key);
ALTER TABLE idempotency_keys ALTER COLUMN id SET NOT NULL;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_business_key UNIQUE (workspace_id, actor_id, action, key);
ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey CASCADE;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_id_pkey PRIMARY KEY (id);

ALTER TABLE registry_credential_versions ADD COLUMN id TEXT;
UPDATE registry_credential_versions SET id = CONCAT(registry_id, ':', version);
ALTER TABLE registry_credential_versions ALTER COLUMN id SET NOT NULL;
ALTER TABLE registry_credential_versions ADD CONSTRAINT registry_credential_versions_business_key UNIQUE (registry_id, version);
ALTER TABLE registry_credential_versions DROP CONSTRAINT registry_credential_versions_pkey CASCADE;
ALTER TABLE registry_credential_versions ADD CONSTRAINT registry_credential_versions_id_pkey PRIMARY KEY (id);

ALTER TABLE instance_execution_policies ADD COLUMN id TEXT;
UPDATE instance_execution_policies SET id = CONCAT(instance_id, ':', revision);
ALTER TABLE instance_execution_policies ALTER COLUMN id SET NOT NULL;
ALTER TABLE instance_execution_policies ADD CONSTRAINT instance_execution_policies_business_key UNIQUE (instance_id, revision);
ALTER TABLE instance_execution_policies DROP CONSTRAINT instance_execution_policies_pkey CASCADE;
ALTER TABLE instance_execution_policies ADD CONSTRAINT instance_execution_policies_id_pkey PRIMARY KEY (id);

ALTER TABLE workspace_allocations ADD COLUMN id TEXT;
UPDATE workspace_allocations SET id = workspace_id;
ALTER TABLE workspace_allocations ALTER COLUMN id SET NOT NULL;
ALTER TABLE workspace_allocations ADD CONSTRAINT workspace_allocations_business_key UNIQUE (workspace_id);
ALTER TABLE workspace_allocations DROP CONSTRAINT workspace_allocations_pkey CASCADE;
ALTER TABLE workspace_allocations ADD CONSTRAINT workspace_allocations_id_pkey PRIMARY KEY (id);

ALTER TABLE workspace_allocation_nodes ADD COLUMN id TEXT;
UPDATE workspace_allocation_nodes SET id = CONCAT(workspace_id, ':', node_id);
ALTER TABLE workspace_allocation_nodes ALTER COLUMN id SET NOT NULL;
ALTER TABLE workspace_allocation_nodes ADD CONSTRAINT workspace_allocation_nodes_business_key UNIQUE (workspace_id, node_id);
ALTER TABLE workspace_allocation_nodes DROP CONSTRAINT workspace_allocation_nodes_pkey CASCADE;
ALTER TABLE workspace_allocation_nodes ADD CONSTRAINT workspace_allocation_nodes_id_pkey PRIMARY KEY (id);

ALTER TABLE worker_certificates ADD COLUMN id TEXT;
UPDATE worker_certificates SET id = serial;
ALTER TABLE worker_certificates ALTER COLUMN id SET NOT NULL;
ALTER TABLE worker_certificates ADD CONSTRAINT worker_certificates_business_key UNIQUE (serial);
ALTER TABLE worker_certificates DROP CONSTRAINT worker_certificates_pkey CASCADE;
ALTER TABLE worker_certificates ADD CONSTRAINT worker_certificates_id_pkey PRIMARY KEY (id);

ALTER TABLE build_config_revisions ADD COLUMN id TEXT;
UPDATE build_config_revisions SET id = CONCAT(workspace_id, ':', application_id, ':', revision);
ALTER TABLE build_config_revisions ALTER COLUMN id SET NOT NULL;
ALTER TABLE build_config_revisions ADD CONSTRAINT build_config_revisions_business_key UNIQUE (workspace_id, application_id, revision);
ALTER TABLE build_config_revisions DROP CONSTRAINT build_config_revisions_pkey CASCADE;
ALTER TABLE build_config_revisions ADD CONSTRAINT build_config_revisions_id_pkey PRIMARY KEY (id);

ALTER TABLE build_events ADD COLUMN id TEXT;
UPDATE build_events SET id = CONCAT(attempt_id, ':', seq);
ALTER TABLE build_events ALTER COLUMN id SET NOT NULL;
ALTER TABLE build_events ADD CONSTRAINT build_events_business_key UNIQUE (attempt_id, seq);
ALTER TABLE build_events DROP CONSTRAINT build_events_pkey CASCADE;
ALTER TABLE build_events ADD CONSTRAINT build_events_id_pkey PRIMARY KEY (id);

ALTER TABLE build_log_chunks ADD COLUMN id TEXT;
UPDATE build_log_chunks SET id = CONCAT(attempt_id, ':', seq);
ALTER TABLE build_log_chunks ALTER COLUMN id SET NOT NULL;
ALTER TABLE build_log_chunks ADD CONSTRAINT build_log_chunks_business_key UNIQUE (attempt_id, seq);
ALTER TABLE build_log_chunks DROP CONSTRAINT build_log_chunks_pkey CASCADE;
ALTER TABLE build_log_chunks ADD CONSTRAINT build_log_chunks_id_pkey PRIMARY KEY (id);

ALTER TABLE application_placements ADD COLUMN id TEXT;
UPDATE application_placements SET id = application_id;
ALTER TABLE application_placements ALTER COLUMN id SET NOT NULL;
ALTER TABLE application_placements ADD CONSTRAINT application_placements_business_key UNIQUE (application_id);
ALTER TABLE application_placements DROP CONSTRAINT application_placements_pkey CASCADE;
ALTER TABLE application_placements ADD CONSTRAINT application_placements_id_pkey PRIMARY KEY (id);

ALTER TABLE application_runtime_states ADD COLUMN id TEXT;
UPDATE application_runtime_states SET id = application_id;
ALTER TABLE application_runtime_states ALTER COLUMN id SET NOT NULL;
ALTER TABLE application_runtime_states ADD CONSTRAINT application_runtime_states_business_key UNIQUE (application_id);
ALTER TABLE application_runtime_states DROP CONSTRAINT application_runtime_states_pkey CASCADE;
ALTER TABLE application_runtime_states ADD CONSTRAINT application_runtime_states_id_pkey PRIMARY KEY (id);

ALTER TABLE application_domain_claims ADD COLUMN id TEXT;
UPDATE application_domain_claims SET id = domain;
ALTER TABLE application_domain_claims ALTER COLUMN id SET NOT NULL;
ALTER TABLE application_domain_claims ADD CONSTRAINT application_domain_claims_business_key UNIQUE (domain);
ALTER TABLE application_domain_claims DROP CONSTRAINT application_domain_claims_pkey CASCADE;
ALTER TABLE application_domain_claims ADD CONSTRAINT application_domain_claims_id_pkey PRIMARY KEY (id);

ALTER TABLE runtime_observations ADD COLUMN id TEXT;
UPDATE runtime_observations SET id = CONCAT(operation_id, ':', seq);
ALTER TABLE runtime_observations ALTER COLUMN id SET NOT NULL;
ALTER TABLE runtime_observations ADD CONSTRAINT runtime_observations_business_key UNIQUE (operation_id, seq);
ALTER TABLE runtime_observations DROP CONSTRAINT runtime_observations_pkey CASCADE;
ALTER TABLE runtime_observations ADD CONSTRAINT runtime_observations_id_pkey PRIMARY KEY (id);

ALTER TABLE oauth_sessions ADD COLUMN id TEXT;
UPDATE oauth_sessions SET id = CONCAT(kind, ':', signature);
ALTER TABLE oauth_sessions ALTER COLUMN id SET NOT NULL;
ALTER TABLE oauth_sessions ADD CONSTRAINT oauth_sessions_business_key UNIQUE (kind, signature);
ALTER TABLE oauth_sessions DROP CONSTRAINT oauth_sessions_pkey CASCADE;
ALTER TABLE oauth_sessions ADD CONSTRAINT oauth_sessions_id_pkey PRIMARY KEY (id);

ALTER TABLE oauth_client_assertions ADD COLUMN id TEXT;
UPDATE oauth_client_assertions SET id = jti_hash;
ALTER TABLE oauth_client_assertions ALTER COLUMN id SET NOT NULL;
ALTER TABLE oauth_client_assertions ADD CONSTRAINT oauth_client_assertions_business_key UNIQUE (jti_hash);
ALTER TABLE oauth_client_assertions DROP CONSTRAINT oauth_client_assertions_pkey CASCADE;
ALTER TABLE oauth_client_assertions ADD CONSTRAINT oauth_client_assertions_id_pkey PRIMARY KEY (id);

ALTER TABLE oidc_identity_bindings ADD COLUMN id TEXT;
UPDATE oidc_identity_bindings SET id = CONCAT(issuer, ':', subject);
ALTER TABLE oidc_identity_bindings ALTER COLUMN id SET NOT NULL;
ALTER TABLE oidc_identity_bindings ADD CONSTRAINT oidc_identity_bindings_business_key UNIQUE (issuer, subject);
ALTER TABLE oidc_identity_bindings DROP CONSTRAINT oidc_identity_bindings_pkey CASCADE;
ALTER TABLE oidc_identity_bindings ADD CONSTRAINT oidc_identity_bindings_id_pkey PRIMARY KEY (id);

ALTER TABLE application_build_secret_versions ADD COLUMN id TEXT;
UPDATE application_build_secret_versions SET id = CONCAT(secret_id, ':', version);
ALTER TABLE application_build_secret_versions ALTER COLUMN id SET NOT NULL;
ALTER TABLE application_build_secret_versions ADD CONSTRAINT application_build_secret_versions_business_key UNIQUE (secret_id, version);
ALTER TABLE application_build_secret_versions DROP CONSTRAINT application_build_secret_versions_pkey CASCADE;
ALTER TABLE application_build_secret_versions ADD CONSTRAINT application_build_secret_versions_id_pkey PRIMARY KEY (id);

ALTER TABLE build_secret_references ADD COLUMN id TEXT;
UPDATE build_secret_references SET id = CONCAT(build_id, ':', secret_id);
ALTER TABLE build_secret_references ALTER COLUMN id SET NOT NULL;
ALTER TABLE build_secret_references ADD CONSTRAINT build_secret_references_business_key UNIQUE (build_id, secret_id);
ALTER TABLE build_secret_references DROP CONSTRAINT build_secret_references_pkey CASCADE;
ALTER TABLE build_secret_references ADD CONSTRAINT build_secret_references_id_pkey PRIMARY KEY (id);

-- 上述 DROP ... CASCADE 会级联删除引用旧主键的外键约束，这里统一重建到业务键 UNIQUE：
ALTER TABLE application_members
    ADD CONSTRAINT application_members_workspace_fk FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE;
ALTER TABLE builds
    ADD CONSTRAINT builds_policy_fk FOREIGN KEY (instance_id, policy_revision) REFERENCES instance_execution_policies(instance_id, revision) ON DELETE RESTRICT;
ALTER TABLE builds
    ADD CONSTRAINT builds_registry_credential_fk FOREIGN KEY (registry_id, credential_version) REFERENCES registry_credential_versions(registry_id, version) ON DELETE RESTRICT;
ALTER TABLE build_artifacts
    ADD CONSTRAINT build_artifacts_registry_credential_fk FOREIGN KEY (registry_id, credential_version) REFERENCES registry_credential_versions(registry_id, version) ON DELETE RESTRICT;
ALTER TABLE application_placements
    ADD CONSTRAINT application_placements_policy_fk FOREIGN KEY (instance_id, policy_revision) REFERENCES instance_execution_policies(instance_id, revision) ON DELETE RESTRICT;
ALTER TABLE application_releases
    ADD CONSTRAINT application_releases_registry_credential_fk FOREIGN KEY (registry_id, credential_version) REFERENCES registry_credential_versions(registry_id, version) ON DELETE RESTRICT;
ALTER TABLE worker_certificates
    ADD CONSTRAINT worker_certificates_rotated_from_fk FOREIGN KEY (rotated_from) REFERENCES worker_certificates(serial) ON DELETE RESTRICT;
ALTER TABLE worker_credential_events
    ADD CONSTRAINT worker_credential_events_serial_fk FOREIGN KEY (serial) REFERENCES worker_certificates(serial) ON DELETE RESTRICT;
ALTER TABLE runtime_observations
    ADD CONSTRAINT runtime_observations_certificate_fk FOREIGN KEY (certificate_serial) REFERENCES worker_certificates(serial) ON DELETE RESTRICT;
ALTER TABLE source_build_grants
    ADD CONSTRAINT source_build_grants_certificate_fk FOREIGN KEY (certificate_serial) REFERENCES worker_certificates(serial) ON DELETE RESTRICT;
ALTER TABLE deployment_probe_evidence
    ADD CONSTRAINT deployment_probe_evidence_domain_fk FOREIGN KEY (domain) REFERENCES application_domain_claims(domain) ON DELETE RESTRICT;

-- +goose Down
-- 破坏式回滚仅作结构演示：id 列删除后原复合主键需重建，历史行可按 id 逆推业务键。
ALTER TABLE build_secret_references DROP CONSTRAINT build_secret_references_id_pkey CASCADE;
ALTER TABLE build_secret_references DROP CONSTRAINT build_secret_references_business_key;
ALTER TABLE build_secret_references ADD PRIMARY KEY (build_id, secret_id);
ALTER TABLE build_secret_references DROP COLUMN id;
