-- +goose Up
-- 凭据版本必须绑定原始仓库目标，防止修改连接地址后把历史凭据发送到新主机。
ALTER TABLE registry_credential_versions ADD COLUMN registry_url TEXT;
ALTER TABLE registry_credential_versions ADD COLUMN repository_prefix TEXT;
UPDATE registry_credential_versions v SET registry_url = r.url, repository_prefix = r.repository_prefix
FROM registries r WHERE r.id = v.registry_id;
ALTER TABLE registry_credential_versions ALTER COLUMN registry_url SET NOT NULL;
ALTER TABLE registry_credential_versions ALTER COLUMN repository_prefix SET NOT NULL;
ALTER TABLE registry_credential_versions ADD CONSTRAINT registry_credential_versions_https CHECK (registry_url LIKE 'https://%');
