# Cling Go 主站阶段 5：MongoDB 数据模型与索引设计

## 目标

为 Go 模块化主站建立独立 MongoDB 数据模型、不可变的集合命名和必要唯一索引。所有数据仅位于本地 `cling_main` 数据库，为后续身份、模板、权益、账本、生成、支付和创作模块提供持久化基础。

本阶段只创建数据结构和索引，不提供用户业务 API，不创建生成任务，不调用生成中台或 PayCores，也不读取或写入 Node 的数据库、钱包、会话、任务与回调。

## 背景与裁决

旧计划中出现的 Ent、MySQL、SQL 表和数据库迁移术语均属于模板遗留。本阶段统一采用 MongoDB Go Driver v2 的 BSON 文档和 `CreateIndexes`。

集合清单包含 15 项：在原有业务集合基础上新增 `accounts`，用于以 `_id=user_id` 保存自有钻石余额；二级索引总数仍为 17。

## 目录与职责

```text
internal/data/model/       MongoDB 持久化对象（PO），仅包含 BSON 字段与转换所需类型
internal/data/schema/      集合名、稳定索引名和声明式索引规格，不连接数据库
internal/data/migrate/     本地索引初始化器，执行幂等的 CreateIndexes
internal/data/             持有 MongoDB 客户端并把本地初始化器装配给应用
```

- `model` 不导入 `biz`、`service` 或 API DTO；业务领域对象仍由 `internal/biz/<module>` 拥有。
- `schema` 只返回静态描述，不持有 `*mongo.Database`，便于纯单元测试。
- `migrate` 是 `data` 层内部实现，只接收独立数据库句柄。它不向业务层暴露 MongoDB Client 或 Collection。
- 集合和索引初始化在 HTTP/gRPC 开始监听之前执行。索引无法创建时，本地应用直接启动失败，不做自动删除、数据修复或降级运行。
- 当前 `conf.ValidateLocalMongo` 已将运行目标限制为本地 `cling_main`。未来生产迁移必须另立受控命令和审核；本初始化器不承担生产迁移职责。

## 集合、文档与索引

所有主实体使用字符串 `_id` 作为稳定业务 ID；MongoDB 的 `_id` 唯一索引由数据库自动维护。时间统一存 UTC 的 `time.Time`，金额与钻石数量使用整数，状态使用明确的字符串枚举值。

### 身份模块

| 集合 | 关键字段 | 索引 |
| --- | --- | --- |
| `accounts` | `_id`（即 `user_id`）、`diamond_balance`、`created_at`、`updated_at` | `_id` 是唯一账户定位事实；不新增二级索引，条件扣钻直接按 `_id + diamond_balance` 原子更新。 |
| `users` | `_id`、`account_status`、`binding_state`、首次写入的 `timezone`、`session_version`、`content_access`、`created_at`、`updated_at` | `_id` 默认唯一；不增加未经使用面验证的查询索引。 |
| `identities` | `_id`、`provider`、`subject`、`user_id`、`created_at` | 唯一 `provider + subject`，防止一个外部身份绑定多个用户。 |
| `sessions` | `_id`（即 `session_id`）、`user_id`、`session_version`、`revoked_at`、`expires_at` | `_id` 默认唯一；普通 `user_id + revoked_at` 索引供封禁时查找和撤销会话。 |

### 模板与素材模块

| 集合 | 关键字段 | 索引 |
| --- | --- | --- |
| `templates` | `_id`、`template_id`、`version`、`content_surface`、`mode`、`sort_order`、`enabled`、`parameters`、`created_at`、`updated_at` | 唯一 `template_id + version`；普通 `enabled + content_surface + sort_order` 索引仅用于模板清单排序。 |
| `assets` | `_id`、`owner_type`、`owner_id`、`asset_kind`、`storage_key`、`status`、`created_at` | `_id` 默认唯一；普通 `owner_type + owner_id + created_at` 索引。 |

### 创作与账本模块

| 集合 | 关键字段 | 索引 |
| --- | --- | --- |
| `creations` | `_id`、`idempotency_key`、`user_id`、`template_id`、`template_version`、`parent_id`、`status`、`version`、`created_at`、`updated_at` | 唯一 `idempotency_key`；普通 `user_id + created_at` 索引。 |
| `creation_steps` | `_id`、`creation_id`、`sequence`、`atom`、`external_execution_id`、`submit_status`、`callback_version`、`created_at` | 唯一 `creation_id + sequence`；普通 `external_execution_id` 索引仅用于回调定位。 |
| `reservations` | `_id`、`creation_id`、`user_id`、`price_diamonds`、`reserved_diamonds`、`benefit_source`、`quota_kind`、`local_date`、`quota_limit`、`quota_units`、`status`、`created_at`、`updated_at` | 唯一 `creation_id`，保留原额度快照，确保同一创作只拥有一条预扣或日免占用事实且可精确冲正。 |
| `daily_quotas` | `_id`、`user_id`、`quota_kind`、`local_date`、`used_count`、`limit`、`updated_at` | 唯一 `user_id + quota_kind + local_date`。额度条件递增由后续事务写入实现。 |
| `ledger_entries` | `_id`、`idempotency_key`、`account_id`、`creation_id`、`delta_diamonds`、`reason`、`reservation_id`、`created_at` | 唯一 `idempotency_key`；普通 `account_id + created_at` 索引。 |

日免的「同一任务只能占用一次」由 `reservations.creation_id` 唯一索引保证。`daily_quotas` 只保存每日聚合计数；后续同一事务内同时写入 reservation 和条件递增 quota，不能只依赖数组字段或单一额度文档判断幂等。

### 支付、回调与 Outbox

| 集合 | 关键字段 | 索引 |
| --- | --- | --- |
| `payment_orders` | `_id`、`user_id`、`provider`、`product_id`、`status`、`provider_order_id`、`created_at`、`updated_at` | `_id` 默认唯一；普通 `user_id + created_at` 索引。外部支付事实的幂等由支付回执集合承担。 |
| `payment_receipts` | `_id`、`provider`、`external_transaction_id`、`payment_order_id`、`ledger_entry_id`、`received_at` | 唯一 `provider + external_transaction_id`，确保同一笔支付永不重复入账。 |
| `callback_receipts` | `_id`、`source`、`nonce_hash`、`received_at`、`payload_digest` | 唯一 `source + nonce_hash`，阻止生成和支付回调重放。 |
| `outbox_events` | `_id`（即 `event_id`）、`aggregate_id`、`event_type`、`payload`、`delivery_status`、`attempt_count`、`next_attempt_at`、`created_at` | `_id` 默认唯一；普通 `delivery_status + next_attempt_at` 索引供投递器取件。 |

所有唯一索引均使用稳定、可读的显式索引名。重复创建索引是幂等操作；若历史数据与唯一约束冲突，初始化器返回错误并停止启动，绝不自动删除或合并数据。

## 初始化流程

```text
读取本地配置
  → ValidateLocalMongo 限制独立 cling_main
  → NewData 连接并 Ping 本地 rs0
  → LocalSchemaInitializer.Ensure 创建 15 个集合与 17 个二级索引
  → 成功后才启动 HTTP 与 gRPC
```

`Ensure` 只调用 MongoDB 的索引创建 API。MongoDB 会在首次创建索引时创建空集合；本阶段不插入任何业务文档。每次启动都会重新声明相同索引，以便缺失索引得到补齐；索引定义冲突则作为明确错误返回。

## 测试与验收

测试只使用 `CLING_TEST_MONGO_URI` 指向 `127.0.0.1:27017` 的本地 `rs0` 和 `cling_main`。每条集成测试生成随机前缀 ID，只删除自己写入的精确测试文档，绝不 `drop database`、`drop collection` 或批量清空集合。

必须覆盖：

1. 静态集合与索引规格包含全部 15 个集合和 17 个二级索引，索引名称、键顺序和唯一性稳定。
2. 初始化器可重复运行，不因为已经存在的索引失败。
3. 重复 `provider + external_transaction_id` 的 payment receipt 被 MongoDB 拒绝。
4. 重复 creation `idempotency_key`、重复 `creation_id + sequence`、重复 reservation `creation_id`、重复 quota 三元组、重复 ledger `idempotency_key` 和重复 callback nonce 均被拒绝。
5. 事务内第一条写入后返回错误时，不遗留任何测试文档。
6. `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./cmd/ai-business-service` 与语义契约检查全部通过。

## 明确不做的内容

- 不实现身份登录、游客绑定、封禁会话、模板编译、VIP 额度、预扣、账本写入、生成提交、回调消费或支付入账。
- 不新增对外 HTTP/gRPC 业务路由，不切换 Gateway，不访问 Node、PayCores 或生成中台。
- 不引入 MySQL、Ent、SQL、外键或自动生产迁移。
