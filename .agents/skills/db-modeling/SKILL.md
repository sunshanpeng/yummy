---
name: db-modeling
description: 本工程数据库建模与迁移规范。设计或修改数据库表结构、编写/评审 goose 迁移、新建或调整 GORM 模型、做 schema 评审时必须加载本规范后再动手。
---

# 数据库建模规范

适用于 PostgreSQL（主库）与 MySQL（保持可切换能力）双方言的建模约束。本仓库 schema 唯一事实来源是 `migrations/` 下的 goose 迁移 + `internal/infra/store/model` 与各领域私有模型；禁止 AutoMigrate，禁止绕过迁移直接改库。

## 1. 必备列（五件套）

每张业务表必须包含以下五列，缺一不可：

| 列 | 类型 | 约束 | 说明 |
|---|---|---|---|
| `id` | TEXT | PRIMARY KEY | 代理主键，应用侧 `security.NewID(前缀)` 生成 |
| `created_at` | TIMESTAMPTZ | NOT NULL DEFAULT NOW() | 行创建时间，数据库时钟 |
| `updated_at` | TIMESTAMPTZ | NOT NULL DEFAULT NOW() | 行最后修改时间 |
| `created_by` | TEXT | NOT NULL DEFAULT '' | 创建者用户标识，请求上下文无 actor 时留空串 |
| `updated_by` | TEXT | NOT NULL DEFAULT '' | 最后修改者，同上 |

- 主键 id 由 `infra/store` 的 Create 回调按模型 `IDPrefix()` 自动填充（`security.NewID` 前缀风格），业务写入点不手写 id；新建模型必须实现 `IDPrefix()`。
- `created_at`/`updated_at` 由 GORM 按字段名自动维护；模型必须有对应 `time.Time` 字段。
- `created_by`/`updated_by` 由回调从上下文 actor 填充（授权内核 `Do/DoInTx` 统一注入）；机器写入（worker 回报、观测采集、内部任务）没有 actor，保持空串，不要伪造。
- **豁免**：append-only 的事件/观测/协议存储表（审计 `audit_events`、节点事件 `worker_credential_events`、观测流 `runtime_log_chunks`/`runtime_metric_samples`/`runtime_observations`、构建事件流 `build_events`/`build_log_chunks`、OAuth 协议存储 `oauth_sessions`/`oauth_client_assertions`/`oidc_identity_bindings`）可缺 `updated_at`/`updated_by`，其余列仍必须。新增此类表在迁移注释里标注豁免理由。
- 迁移里为存量表补列时用 `ADD COLUMN IF NOT EXISTS` + `DEFAULT`，无法可靠回填的审计列保持空串。

## 2. 主键与业务键

- 每张表**必须有单列 `id` 代理主键**，游标分页与外部引用统一依赖 id。
- 原复合业务键（成员唯一、幂等键、attempt+seq 等）一律转 `UNIQUE` 约束保留——唯一性语义不丢失。
- 业务键本身不做主键；不要用 `serial`/`BIGSERIAL`/UUID 作主键（与现有 TEXT id 风格冲突）。
- 外键引用业务键列组时，该列组必须是 UNIQUE 约束（PG 外键可引用 UNIQUE）。

## 3. 命名

- 表名与列名一律 `snake_case`；表名用领域语义复数形式（`workspace_members`、`build_attempts`）。
- Go 模型字段用驼峰，gorm tag 显式写 `column:`，避免依赖隐式转换。
- 布尔列语义可从名称判断（`active`、`occupied`、`is_default`）。

## 4. 类型

- 字符串一律 TEXT，不用 VARCHAR(n)；长度约束用 `CHECK`（如 `CHECK (domain <> '')`）。
- 时间一律 TIMESTAMPTZ（带时区），不用 TIMESTAMP/DATETIME。
- 枚举值用 `CHECK (state IN (...))`，不加数据库原生 ENUM 类型（跨方言兼容差，改值要重建类型）。
- 结构化载荷用 JSONB（PG）——这是方言差异点，MySQL 切换时需整体评估（见 §6）。
- 大二进制（源码归档）不入库，只存 key 与摘要。

## 5. 迁移

- 只写**前向迁移**（goose Up），Down 仅作结构演示；禁止 AutoMigrate。
- 迁移文件是 PG 方言（TIMESTAMPTZ/JSONB/部分索引已是既有事实）；保持 MySQL 可切换的手段是**业务 SQL 通用化**，切换时迁移目录整体按方言重写，不做逐文件兼容层。
- 迁移必须可在有数据的库上执行：加列用 `ADD COLUMN IF NOT EXISTS` + `DEFAULT`，回填用 `UPDATE`（通用函数 CONCAT）；删除被外键依赖的主键会报 2BP01——先建同列组 UNIQUE 再 `DROP ... CASCADE` 并在迁移内重建外键（见 022 的先例）。
- 新表建表即带五件套与业务键 UNIQUE，不要先建后补。
- 迁移加列后：模型加字段（含 tag）→ `go run ./internal/infra/store/generate` 重生成 DAO → 真库测试（`TEST_DATABASE_URL`）验证。

## 6. 方言限制（PostgreSQL / MySQL）

- 默认只用两方言通用的 SQL：标准 DML、JOIN、`IN`、`EXISTS`、`CONCAT`、`RENAME COLUMN`、窗口函数（两者 8.0+ 均支持）。
- 确需 PG 特有能力（advisory lock、RETURNING、JSONB 操作符 `@>`/`||`、`make_interval`、`::cast`、数据修改 CTE）时：
  - 收口到领域的数据访问文件，不散落业务代码；
  - 在该 SQL 处加中文注释【数据库专有】说明动机与 MySQL 替代方案；
  - 已有先例：`identity/inventory/build` 的 store.go、`runtimeobs.go`、`presence.go`、`oauth/store.go`。
- 依赖 gorm 方言分支的（如 `INTERVAL` 语法），用 `infra/store.NowPlusSeconds` 这类封装，不手写。
- 模型 tag 只用两方言通用语义；`type:jsonb` 这类方言 tag 集中在少数位置。

## 7. 索引与查询形态

- 外键列、高频 WHERE/ORDER 列建索引；部分索引用于状态机表（如 `WHERE state <> 'COMPLETE'` 的未完成快照索引，先例见 004）。
- 复合索引列序与查询谓词一致（先等值列后范围列）。
- 游标分页统一按 `id > ? ORDER BY id`，不用 offset。
- 高频写入的观测表（metric samples/log chunks）在写入事务内做保留期收敛（先例：`convergeRuntimeMetrics`），不依赖外部清理任务保证约束。

## 8. 建模流程（动手顺序）

1. 先想清楚表的类别：可变实体 / append-only 事件 / 版本快照 / 协议存储——决定 §1 的豁免范围。
2. 写迁移（新表或 ALTER），遵守 §1-§4、§5-§6。
3. 模型加字段与 `IDPrefix()`（领域私有模型）或改 `infra/store/model`（gen 管理的共享表）→ 重生成 DAO。
4. 数据访问放领域 `store.go`，复杂 SQL 按 §6 处理方言。
5. 跑真库集成测试（`TEST_DATABASE_URL`），让迁移、唯一约束、回调填充全部走真实路径。
