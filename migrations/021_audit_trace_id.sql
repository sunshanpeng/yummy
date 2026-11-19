-- +goose Up
-- 排障标识统一为 W3C trace_id：审计与凭据事件的 request_id 列改为 trace_id，
-- 取值来自请求入口的 OpenTelemetry span（响应头 X-Trace-Id 与日志字段同源）。
ALTER TABLE audit_events RENAME COLUMN request_id TO trace_id;
ALTER TABLE worker_credential_events RENAME COLUMN request_id TO trace_id;
-- +goose Down
ALTER TABLE audit_events RENAME COLUMN trace_id TO request_id;
ALTER TABLE worker_credential_events RENAME COLUMN trace_id TO request_id;
