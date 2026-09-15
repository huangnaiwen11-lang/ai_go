# Cling Go 本地账本与预留模块实现计划

> **面向 AI 代理的工作者：** 本项目没有 Git 仓库。必须使用 TDD，不能创建提交、分支或工作树。

**目标：** 在本地 `cling_main` 实现并发安全的日免占用、钻石预扣、失败冲正和审核没收基础能力。

**架构：** `biz/ledger` 协调预留状态机和 `shared.TxRunner`；`data` 用 `accounts` 条件余额更新、`daily_quotas` 条件额度更新、`reservations` 唯一事实和 `ledger_entries` 幂等分录共同组成一个事务。不会新增业务路由。

**技术栈：** Go 1.25、MongoDB Go Driver v2、本地副本集 `rs0`、Kratos Wire。

---

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/biz/ledger/model.go` | 预留来源、状态、日免上下文、预留和账本领域对象。 |
| `internal/biz/ledger/repository.go` | 事务内余额、额度、预留和分录的反转依赖接口。 |
| `internal/biz/ledger/usecase.go` | 预留、冲正、没收状态机及命令一致性校验。 |
| `internal/biz/ledger/usecase_test.go` | 内存仓储下的幂等、冲正、没收和不足余额规则测试。 |
| `internal/biz/ledger/provider.go` | 注册账本用例。 |
| `internal/data/model/ledger.go` | `accounts`、预留扩展字段和账本分录 BSON PO。 |
| `internal/data/ledger_repository.go` | MongoDB 条件更新、PO/DO 转换和存储错误映射。 |
| `internal/data/ledger_repository_test.go` | 本地 `rs0` 的预留、冲正和并发最后日免集成测试。 |
| `internal/data/schema/collections.go` | 增加 `accounts`，集合总数从 14 变为 15。 |
| `internal/data/schema/indexes_test.go`、`internal/data/migrate/local_schema_test.go` | 更新集合清单；二级索引仍为 17 个。 |
| `internal/data/data.go`、`internal/biz/biz.go` | Wire 装配数据仓储和账本用例，不注册传输层。 |
| `README.md`、`internal/data/README.md`、MongoDB schema 设计文档 | 说明独立账户余额聚合和新的 15 集合现状。 |

## 任务 1：建立失败的领域状态机测试

- [x] 在 `internal/biz/ledger/usecase_test.go` 创建内存仓储与事务模拟，先编写：零余额但日免可用时产生 0 钻预留分录；日免耗尽且余额不足时返回 `shared.ErrInsufficientFunds` 且四类事实均不变。
- [x] 再编写：相同 `creation_id` 重试只返回同一预留；付费预留从 20 扣到 0 后冲正回 20 且只追加一条正向分录；日免冲正只释放原日期与原单位；审核没收不退款。
- [x] 运行 `go test ./internal/biz/ledger -count=1`，确认因缺少领域类型、仓储接口和用例而失败。

## 任务 2：实现领域模型与状态机

- [x] 在 `model.go` 定义 `BenefitSource`（`daily_quota`、`diamonds`）、`ReservationStatus`（`reserved`、`reversed`、`confiscated`）、`QuotaReservation`、`ReserveRequest`、`Reservation` 与 `LedgerEntry`；`ReserveRequest` 必须带固定价格与日免上下文，不带模板、中台或支付字段。
- [x] 在 `repository.go` 声明 `FindReservation`、`TryConsumeQuota`、`TryDebitDiamonds`、`CreateReservation`、`AppendLedgerEntry`、`TransitionReservation`、`RestoreQuota` 与 `CreditDiamonds`；所有方法只接收 `context.Context` 与领域对象。
- [x] 在 `usecase.go` 用 `shared.TxRunner` 实现 `Reserve`：先查同创作预留，随后完整尝试日免，失败后条件扣钻，最后写预留和账本；不足余额返回 `shared.ErrInsufficientFunds`，不做余额预查询。
- [x] 在 `usecase.go` 实现 `Reverse` 与 `Confiscate`：只允许从 `reserved` 迁移；冲正依据预留中保存的来源、日期和单位执行反向写入并追加幂等分录；没收只更新状态。
- [x] 运行 `go test ./internal/biz/ledger -count=1`，确认领域状态机测试通过。

## 任务 3：先建立 MongoDB 仓储红灯测试

- [x] 在 `internal/data/ledger_repository_test.go` 以随机账户和创作 ID 先编写付费预留、一次冲正与并发抢最后日免测试；此时因缺少账户集合、仓储构造器或领域接口实现而失败。
- [x] 为每个随机 `_id` 预先编写精确 `DeleteOne` 清理逻辑；不得使用空过滤条件删除、`drop database` 或 `drop collection`。

## 任务 4：扩展本地 schema 与 MongoDB 仓储

- [x] 在 `collections.go` 添加 `CollectionAccounts`，并同步集合顺序与测试期望；不新增二级索引，因为账户以 `_id = user_id` 唯一定位。
- [x] 在 `model/ledger.go` 添加 `AccountDocument`，并补齐预留所需的用户、额度来源和时间字段；更新 BSON 字段名测试。
- [x] 在 `ledger_repository.go` 实现：额度过滤 `used_count <= limit - quota_units` 的原子递增、账户 `diamond_balance >= amount` 的原子扣减、唯一预留写入、分录写入、精确额度释放和余额返还。所有 MongoDB 调用必须传入事务 context。
- [x] 把仓储构造器加入 `data.ProviderSet`，账本用例加入 `biz.ProviderSet`；执行 `go generate ./cmd/ai-business-service`，不手改 `wire_gen.go`。

## 任务 5：验证本地 MongoDB 原子性

- [x] 在 `ledger_repository_test.go` 以随机账户和创作 ID 测试付费预留与一次冲正，验证余额 `20 → 0 → 20`、预留状态 `reserved → reversed`、账本分录数 `1 → 2`。
- [x] 以随机用户、日免文档与余额 0 并发执行两次预留，验证仅一个事务成功、另一请求为 `INSUFFICIENT_FUNDS`、日免只增加一次、预留和日免分录各仅一条。
- [x] 以非默认额度种类、本地日期和单位数验证日免预留后冲正精确恢复原事实；再分别删除随机账户或额度事实，验证冲正失败不会留下状态迁移或额外分录。
- [x] 对每个随机 `_id` 使用 `DeleteOne` 清理 `accounts`、`daily_quotas`、`reservations` 与 `ledger_entries`；不得使用空过滤条件删除。
- [x] 用 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -count=1` 验证。

## 任务 6：全量复核

- [x] 运行 `go generate ./cmd/ai-business-service`、`go test ./... -count=1`、`go test -race ./... -count=1`、`go vet ./...`、`go build ./cmd/ai-business-service` 与 `go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json`。
- [x] 静态检索确认账本模块不新增 HTTP/gRPC 路由、Node 文件、PayCores/中台调用或现网钱包访问；MongoDB 访问仅位于 `internal/data`。
