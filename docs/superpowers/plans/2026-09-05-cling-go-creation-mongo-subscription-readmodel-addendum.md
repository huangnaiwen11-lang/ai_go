# 创作 MongoDB 仓储与自有订阅读模型补充计划

> **面向 AI 代理的工作者：** 必须使用子代理驱动开发。项目不是 Git 仓库；不得创建提交、分支或工作树。每个任务先写失败测试、确认红灯、最小实现转绿，并依次完成规格与质量审查。

**目标：** 将已完成的创作—预留领域事务落到本地 MongoDB `cling_main`，并以自有订阅读模型提供可信 VIP 快照。

**架构：** `creations` 继续只声明依赖，`data` 实现创作仓储和 `entitlement.SubscriptionReader`。订阅文档以 `user_id` 作为 `_id`，缺失文档返回 `nil` 表示普通用户。创作、步骤、日免、账户、预留和账本分录继续由同一 `MongoTxRunner` Session Transaction 提交或回滚。

**技术栈：** Go 1.25、MongoDB Go Driver v2、本地单节点副本集 `rs0`、Kratos Wire（仅下一任务装配）。

## 固定边界

- 只连接 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true'`，仅数据库 `cling_main`。
- 不创建 HTTP/gRPC 路由，不调用生成中台，不修改 Node/JS、PayCores、现网钱包、生产配置或 `wire_gen.go`。
- `CreateReservedRequest` 不得恢复 `Subscription`、VIP、余额字段；订阅只能由事务内 `entitlement.SubscriptionReader.FindByUserID(txCtx, userID)` 读取。
- 清理只对本测试生成的随机 `_id` 使用逐条 `DeleteOne`；禁止 `DeleteMany`、空过滤、删库或删集合。
- 不预读余额或日免；账本仍以条件更新占用日免或预扣钻石。

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/biz/entitlement/repository.go` | 公开 `SubscriptionReader` 窄领域接口，返回服务端订阅快照。 |
| `internal/biz/creations/usecase.go` | 改用 entitlement 所有的公开订阅读取接口，保持事务内读取。 |
| `internal/data/model/creations.go` | 创作文档增加 `request_fingerprint`。 |
| `internal/data/model/entitlement.go` | 订阅投影 BSON 对象，以用户 ID 为 `_id`。 |
| `internal/data/model/model_test.go` | 锁定创作指纹与订阅投影 BSON 字段。 |
| `internal/data/schema/collections.go` | 声明本地 `subscriptions` 集合。 |
| `internal/data/migrate/local_schema_test.go` | 锁定集合数与 `subscriptions` 存在；无额外二级索引。 |
| `internal/data/creation_repository.go` | 创作、步骤的 Mongo 映射与重复键错误映射。 |
| `internal/data/subscription_repository.go` | 从本地投影读取 VIP 快照；未命中返回 nil。 |
| `internal/data/creation_repository_test.go` | 本地 rs0 的事务、幂等、回滚、订阅读取集成测试。 |

## 任务 4A：先冻结领域与持久化合同

**文件：**

- 创建：`internal/biz/entitlement/repository.go`
- 修改：`internal/biz/creations/usecase.go`
- 修改：`internal/data/model/creations.go`
- 创建：`internal/data/model/entitlement.go`
- 修改：`internal/data/model/model_test.go`
- 修改：`internal/data/schema/collections.go`
- 修改：`internal/data/migrate/local_schema_test.go`

- [ ] **步骤 1：编写失败的模型与 schema 测试**

在 `Test关键持久化对象的BSON字段名` 中要求 `CreationDocument` 含 `request_fingerprint`，并新增如下订阅对象断言：

```go
document := SubscriptionDocument{
    UserID: "user-1", Status: "active", BillingPeriod: "yearly",
    StartsAt: now, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, UpdatedAt: now,
}
wantFields := []string{"_id", "status", "billing_period", "starts_at", "expires_at", "created_at", "updated_at"}
```

将本地 schema 测试的集合数改为 16，并要求 `subscriptions` 存在。新建 `entitlement.SubscriptionReader`：

```go
type SubscriptionReader interface {
    FindByUserID(context.Context, string) (*SubscriptionSnapshot, error)
}
```

让 `creations.Usecase` 的字段和构造器参数使用此公开接口；不改其调用顺序。

- [ ] **步骤 2：运行测试确认失败**

```bash
go test ./internal/data/model ./internal/data/migrate -run 'Test关键持久化对象的BSON字段名|TestEnsureCreatesAllCollectionsAndIsIdempotent' -count=1
go test ./internal/biz/creations -count=1
```

预期：编译失败，缺少订阅对象/字段或构造器接口不一致。

- [ ] **步骤 3：最小实现领域与 BSON 合同**

`CreationDocument` 仅增加：

```go
RequestFingerprint string `bson:"request_fingerprint"`
```

`SubscriptionDocument` 不保存支付渠道、原始订单、钻石、余额或客户端声明；只保存上述快照字段。`subscriptions` 仅依赖 `_id` 默认唯一索引，禁止为了本任务添加无查询用途的二级索引。

- [ ] **步骤 4：运行模型与 schema 测试转绿**

```bash
go test ./internal/data/model ./internal/data/migrate -run 'Test关键持久化对象的BSON字段名|TestEnsureCreatesAllCollectionsAndIsIdempotent' -count=1
go test ./internal/biz/creations -count=1
```

预期：PASS。

## 任务 4B：以 TDD 实现 MongoDB 仓储和事务集成测试

**文件：**

- 创建：`internal/data/creation_repository.go`
- 创建：`internal/data/subscription_repository.go`
- 创建：`internal/data/creation_repository_test.go`

- [ ] **步骤 1：编写失败的本地 rs0 集成测试**

在 `creation_repository_test.go` 建立 `newMongoCreationFixture(t)`：创建 `Data{client, database}`、`NewMongoTxRunner(client)`、`NewUserRepository`、`NewSubscriptionRepository`、`NewCreationRepository`、`NewLedgerRepository`、`entitlement.NewUsecase`、`ledger.NewUsecase`、`creations.NewUsecase`。每个 fixture 使用 UUID 用户、幂等键、创作、步骤、预留和分录 ID，并为每个已知 ID 注册精确 `DeleteOne` 清理。

写入以下测试：

```go
func TestMongo订阅投影缺失视为普通用户(t *testing.T) { /* 10 秒视频返回 VIP_REQUIRED；无创作事实 */ }
func TestMongoCreateReserved日免与创作步骤同事务提交(t *testing.T) { /* 有效订阅、余额0、图片额度10/0，断言 creation/step/reservation/ledger/quota */ }
func TestMongoCreateReserved余额不足整体回滚(t *testing.T) { /* 余额19、图片20，创作和步骤精确计数为0 */ }
func TestMongoCreateReserved同键并发只保留一套事实(t *testing.T) { /* 两 goroutine 同键，返回同一 creation ID，创作/步骤/预留/分录各1 */ }
func TestMongoCreateReserved十秒视频保存两个额度单位(t *testing.T) { /* 有效订阅，quota_units=2 */ }
```

每个断言都用 `_id` 或 `creation_id` 的精确过滤；并发测试不得用余额预读来决定结果。

- [ ] **步骤 2：运行集成测试确认失败**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -run 'TestMongo(订阅投影|CreateReserved)' -count=1
```

预期：编译失败，`NewCreationRepository` 与 `NewSubscriptionRepository` 不存在。

- [ ] **步骤 3：最小实现 Mongo 仓储**

创作仓储必须提供：

```go
func NewCreationRepository(data *Data) creations.Repository
func (r *mongoCreationRepository) FindByIdempotencyKey(ctx context.Context, key string) (*creations.Creation, error)
func (r *mongoCreationRepository) ListSteps(ctx context.Context, creationID string) ([]creations.CreationStep, error)
func (r *mongoCreationRepository) Create(ctx context.Context, creation *creations.Creation, steps []creations.CreationStep) error
```

`Create` 使用传入 `ctx` 的 `InsertOne` 后 `InsertMany`，不创建 Session。创作或步骤重复键映射 `creations.ErrCreationAlreadyExists`；`ListSteps` 按 `sequence: 1` 排序；未命中创作返回 `(nil, nil)`。

订阅仓储必须提供：

```go
func NewSubscriptionRepository(data *Data) entitlement.SubscriptionReader
func (r *mongoSubscriptionRepository) FindByUserID(ctx context.Context, userID string) (*entitlement.SubscriptionSnapshot, error)
```

未命中返回 `(nil, nil)`；其它错误带操作上下文；所有字段只做 PO/DO 映射。`Data` nil 或 collection nil 时返回明确配置错误，不得 panic。

- [ ] **步骤 4：运行集成测试转绿并回归账本**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -run 'TestMongo(订阅投影|CreateReserved|账本)' -count=1
go test ./internal/biz/creations ./internal/biz/entitlement ./internal/biz/ledger -count=1
```

预期：PASS；提交或回滚后不存在半套事实。

- [ ] **步骤 5：竞态与静态验证**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test -race ./internal/data ./internal/biz/creations -count=1
go vet ./internal/data ./internal/biz/creations ./internal/biz/entitlement
```

预期：PASS。若本地副本集不可达，只记录连接失败和未执行的集成证据，不得改用非本地 URI 或生产环境。

## 自检

- `CreateReservedRequest` 无订阅/VIP/余额字段；订阅只由读模型在 `txCtx` 内读取。
- `request_fingerprint` 已写入并在重复键重读中参与冲突判断。
- Mongo 仓储不自行开事务；所有写操作沿用 `MongoTxRunner` 注入的 Session Context。
- 测试清理仅逐条精确 `DeleteOne`；不执行破坏性数据库命令。
- Wire 注册留给原计划任务 5，当前不得手改或生成 `wire_gen.go`。
