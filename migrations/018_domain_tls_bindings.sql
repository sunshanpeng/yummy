-- +goose Up
-- 域名 TLS 绑定属实例级运行设施：管理员预置证书 Secret 与域名的对应关系，
-- 空串表示未绑定。绑定不冻结进发布（release.Domains 保持域名快照），
-- 发布领取时按当下绑定汇聚，证书轮换无需重新发布。
ALTER TABLE application_domain_claims ADD COLUMN tls_secret_name TEXT NOT NULL DEFAULT '';
-- +goose Down
ALTER TABLE application_domain_claims DROP COLUMN tls_secret_name;
