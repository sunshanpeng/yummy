-- +goose Up
-- 连接epoch属于控制面持久会话栅栏，不是业务配置revision；旧流不能清除新连接在线状态。
ALTER TABLE execution_nodes ADD COLUMN connection_epoch BIGINT NOT NULL DEFAULT 0 CHECK (connection_epoch >= 0);
ALTER TABLE execution_nodes ADD COLUMN connection_owner TEXT NOT NULL DEFAULT '';
