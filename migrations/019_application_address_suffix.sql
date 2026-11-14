-- +goose Up
-- 默认地址规范（2026-10-26 设计）：默认地址为 <应用名>-<6位后缀>.<应用根域名>，
-- 后缀创建时生成且不变。存量应用回填随机后缀（小写字母数字）。
ALTER TABLE applications ADD COLUMN IF NOT EXISTS address_suffix text;
UPDATE applications SET address_suffix = substring(md5(random()::text || id) from 1 for 1) ||
    substring(md5(random()::text || id) from 3 for 5)
  WHERE address_suffix IS NULL;
ALTER TABLE applications ALTER COLUMN address_suffix SET NOT NULL;
-- +goose Down
ALTER TABLE applications DROP COLUMN address_suffix;
