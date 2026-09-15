# Cling Go 主站阶段 5：MongoDB 数据模型与索引实现计划

> 面向 AI 代理的工作者：必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框语法跟踪进度。

**目标：** 为独立本地 cling_main MongoDB 创建 14 个业务集合的 PO、稳定索引声明、启动前幂等初始化器和可重复的本地集成测试。

**架构：** internal/data/model 只定义 BSON 持久化对象；internal/data/schema 只描述集合和索引；internal/data/migrate 只把声明应用到 MongoDB。应用通过 LocalSchemaInitializer 在 HTTP/gRPC 监听前完成本地索引初始化。

**技术栈：** Go 1.25、MongoDB Go Driver v2.8、Kratos、Wire、MongoDB 单节点副本集 rs0、标准库 testing。

**固定边界：** 项目没有 Git 元数据。不要创建工作树、提交、推送或运行 Git 命令。所有集成测试只连接 CLING_TEST_MONGO_URI 指向的 127.0.0.1:27017 本地 cling_main；禁止 drop database、drop collection、空条件删除、生产访问、Node/JS、PayCores、钱包或生成中台改动。

**设计依据：** docs/superpowers/specs/2026-09-05-cling-go-mongodb-schema-design.md。

---

## 文件结构

| 文件 | 职责 |
| --- | --- |
| internal/data/model/identity.go | users、identities、sessions 的 BSON PO。 |
| internal/data/model/catalog.go | templates、assets 的 BSON PO。 |
| internal/data/model/creations.go | creations、creation_steps 的 BSON PO。 |
| internal/data/model/ledger.go | reservations、daily_quotas、ledger_entries 的 BSON PO。 |
| internal/data/model/payments.go | payment_orders、payment_receipts、callback_receipts 的 BSON PO。 |
| internal/data/model/outbox.go | outbox_events 的 BSON PO。 |
| internal/data/schema/collections.go | 14 个稳定集合名称。 |
| internal/data/schema/indexes.go | 声明式索引规格与稳定索引名称。 |
| internal/data/schema/indexes_test.go | 纯单元测试，锁定集合、键顺序和唯一性。 |
| internal/data/migrate/local_schema.go | 本地 MongoDB 索引初始化器。 |
| internal/data/migrate/local_schema_test.go | 本地 MongoDB 集成测试。 |
| internal/data/schema_initializer.go | data 层对应用暴露的最小初始化器接口。 |
| internal/data/data.go | 将初始化器纳入 Wire ProviderSet。 |
| cmd/ai-business-service/main.go | 在 HTTP/gRPC 监听前调用 Ensure。 |
| cmd/ai-business-service/main_test.go | 验证初始化失败阻止应用构造。 |
| cmd/ai-business-service/wire.go | Wire 装配声明。 |
| cmd/ai-business-service/wire_gen.go | 由 Wire 生成，绝不手工编辑。 |
| README.md | 更新本地索引初始化说明。 |

## 任务 1：冻结 14 个集合与索引声明

**文件：**

- 创建：internal/data/schema/collections.go
- 创建：internal/data/schema/indexes.go
- 创建：internal/data/schema/indexes_test.go

- [ ] **步骤 1：编写索引规格失败测试。**

    func TestAllCollectionsHasExactlyTheApprovedFourteenCollections(t *testing.T) {
        want := []string{
            "users", "identities", "sessions", "templates", "assets", "creations",
            "creation_steps", "reservations", "daily_quotas", "ledger_entries",
            "payment_orders", "payment_receipts", "callback_receipts", "outbox_events",
        }
        if !reflect.DeepEqual(want, AllCollections()) {
            t.Fatalf("AllCollections() = %#v, want %#v", AllCollections(), want)
        }
    }

    func TestAllIndexesKeepsCriticalUniqueConstraints(t *testing.T) {
        indexes := indexByName(AllIndexes())
        assertIndex(t, indexes, "ux_identities_provider_subject", CollectionIdentities, true,
            bson.D{{Key: "provider", Value: 1}, {Key: "subject", Value: 1}})
        assertIndex(t, indexes, "ux_creations_idempotency_key", CollectionCreations, true,
            bson.D{{Key: "idempotency_key", Value: 1}})
        assertIndex(t, indexes, "ux_payment_receipts_provider_external_transaction", CollectionPaymentReceipts, true,
            bson.D{{Key: "provider", Value: 1}, {Key: "external_transaction_id", Value: 1}})
        assertIndex(t, indexes, "ux_callback_receipts_source_nonce_hash", CollectionCallbackReceipts, true,
            bson.D{{Key: "source", Value: 1}, {Key: "nonce_hash", Value: 1}})
    }

assertIndex 必须同时比较集合名、索引名、唯一性和 bson.D 的键顺序，不能将键转换为 map。

- [ ] **步骤 2：运行测试确认失败。**

运行：

    cd /Users/huangnaiwen/project/ai-business-service
    go test ./internal/data/schema -count=1

预期：失败，提示 package、AllCollections 或 AllIndexes 尚不存在。

- [ ] **步骤 3：实现集合和索引声明。**

collections.go 仅定义以下常量，并在 AllCollections 中按此顺序返回：

    const (
        CollectionUsers            = "users"
        CollectionIdentities       = "identities"
        CollectionSessions         = "sessions"
        CollectionTemplates        = "templates"
        CollectionAssets           = "assets"
        CollectionCreations        = "creations"
        CollectionCreationSteps    = "creation_steps"
        CollectionReservations     = "reservations"
        CollectionDailyQuotas      = "daily_quotas"
        CollectionLedgerEntries    = "ledger_entries"
        CollectionPaymentOrders    = "payment_orders"
        CollectionPaymentReceipts  = "payment_receipts"
        CollectionCallbackReceipts = "callback_receipts"
        CollectionOutboxEvents     = "outbox_events"
    )

indexes.go 定义 IndexSpec。AllIndexes 只返回下列索引，所有 Name 使用稳定的 ux_ 或 ix_ 前缀：

| 集合 | Name | 键 | 唯一 |
| --- | --- | --- | --- |
| identities | ux_identities_provider_subject | provider, subject | 是 |
| sessions | ix_sessions_user_revoked | user_id, revoked_at | 否 |
| templates | ux_templates_template_version | template_id, version | 是 |
| templates | ix_templates_enabled_surface_sort | enabled, content_surface, sort_order | 否 |
| assets | ix_assets_owner_created | owner_type, owner_id, created_at:-1 | 否 |
| creations | ux_creations_idempotency_key | idempotency_key | 是 |
| creations | ix_creations_user_created | user_id, created_at:-1 | 否 |
| creation_steps | ux_creation_steps_creation_sequence | creation_id, sequence | 是 |
| creation_steps | ix_creation_steps_external_execution | external_execution_id | 否 |
| reservations | ux_reservations_creation_id | creation_id | 是 |
| daily_quotas | ux_daily_quotas_user_kind_date | user_id, quota_kind, local_date | 是 |
| ledger_entries | ux_ledger_entries_idempotency_key | idempotency_key | 是 |
| ledger_entries | ix_ledger_entries_account_created | account_id, created_at:-1 | 否 |
| payment_orders | ix_payment_orders_user_created | user_id, created_at:-1 | 否 |
| payment_receipts | ux_payment_receipts_provider_external_transaction | provider, external_transaction_id | 是 |
| callback_receipts | ux_callback_receipts_source_nonce_hash | source, nonce_hash | 是 |
| outbox_events | ix_outbox_events_status_next_attempt | delivery_status, next_attempt_at | 否 |

不要为 users、payment_orders、outbox_events 添加没有业务查询证据的额外唯一索引；MongoDB 默认维护所有集合的 _id 唯一索引。

- [ ] **步骤 4：运行单元测试确认通过。**

    go test ./internal/data/schema -count=1

预期：通过；集合数量、集合顺序、索引名、键顺序和关键唯一性未漂移。

- [ ] **步骤 5：格式化并记录结果。**

    gofmt -w internal/data/schema/collections.go internal/data/schema/indexes.go internal/data/schema/indexes_test.go
    gofmt -d internal/data/schema/collections.go internal/data/schema/indexes.go internal/data/schema/indexes_test.go

预期：第二个命令无输出。项目不使用 Git，不执行提交操作。

## 任务 2：创建 BSON 持久化对象

**文件：**

- 创建：internal/data/model/identity.go
- 创建：internal/data/model/catalog.go
- 创建：internal/data/model/creations.go
- 创建：internal/data/model/ledger.go
- 创建：internal/data/model/payments.go
- 创建：internal/data/model/outbox.go
- 创建：internal/data/model/model_test.go

- [ ] **步骤 1：编写 BSON 映射失败测试。**

    func TestCriticalDocumentsMarshalToApprovedBSONFieldNames(t *testing.T) {
        cases := []struct {
            name  string
            value any
            keys  []string
        }{
            {"user", UserDocument{ID: "user-1", Timezone: "Asia/Shanghai"}, []string{"_id", "timezone"}},
            {"creation", CreationDocument{ID: "creation-1", IdempotencyKey: "key-1"}, []string{"_id", "idempotency_key"}},
            {"receipt", PaymentReceiptDocument{ID: "receipt-1", Provider: "paycores", ExternalTransactionID: "txn-1"}, []string{"_id", "provider", "external_transaction_id"}},
        }
        for _, tc := range cases {
            t.Run(tc.name, func(t *testing.T) {
                encoded, err := bson.Marshal(tc.value)
                if err != nil { t.Fatal(err) }
                var got bson.M
                if err := bson.Unmarshal(encoded, &got); err != nil { t.Fatal(err) }
                for _, key := range tc.keys {
                    if _, ok := got[key]; !ok { t.Fatalf("missing BSON key %q", key) }
                }
            })
        }
    }

- [ ] **步骤 2：运行测试确认失败。**

    go test ./internal/data/model -count=1

预期：失败，提示 UserDocument、CreationDocument 和 PaymentReceiptDocument 未定义。

- [ ] **步骤 3：实现 14 个 PO。**

每个 PO 只包含 BSON 字段和中文注释，不导入 biz、service、DTO 或 MongoDB Client。所有业务 ID 使用 bson _id 字段；可选时刻用 *time.Time；模板参数和 Outbox 载荷使用 bson.Raw。字段名是固定契约：

| 文件 | 类型与字段 |
| --- | --- |
| identity.go | UserDocument：id、account_status、binding_state、timezone、session_version、content_access、created_at、updated_at；IdentityDocument：id、provider、subject、user_id、created_at；SessionDocument：id、user_id、session_version、revoked_at、expires_at。 |
| catalog.go | TemplateDocument：id、template_id、version、content_surface、mode、sort_order、enabled、parameters、created_at、updated_at；AssetDocument：id、owner_type、owner_id、asset_kind、storage_key、status、created_at。 |
| creations.go | CreationDocument：id、idempotency_key、user_id、template_id、template_version、parent_id、status、version、created_at、updated_at；CreationStepDocument：id、creation_id、sequence、atom、external_execution_id、submit_status、callback_version、created_at。 |
| ledger.go | ReservationDocument：id、creation_id、price_diamonds、reserved_diamonds、benefit_source、status、settled_at、reversal_reason；DailyQuotaDocument：id、user_id、quota_kind、local_date、used_count、limit、updated_at；LedgerEntryDocument：id、idempotency_key、account_id、delta_diamonds、reason、reservation_id、payment_receipt_id、created_at。 |
| payments.go | PaymentOrderDocument：id、user_id、provider、product_id、status、provider_order_id、created_at、updated_at；PaymentReceiptDocument：id、provider、external_transaction_id、payment_order_id、ledger_entry_id、received_at；CallbackReceiptDocument：id、source、nonce_hash、received_at、payload_digest。 |
| outbox.go | OutboxEventDocument：id、aggregate_id、event_type、payload、delivery_status、attempt_count、next_attempt_at、created_at。 |

身份 PO 采用一字段一行、带中文注释的形式：

    // UserDocument 是 Go 主站自有用户的持久化对象。
    type UserDocument struct {
        ID             string
        AccountStatus  string
        BindingState   string
        Timezone       string
        SessionVersion int64
        ContentAccess  string
        CreatedAt      time.Time
        UpdatedAt      time.Time
    }

在实现中为每个字段填入表中同名的 bson 标签。禁止把多字段压缩在同一行，也不要在此任务编写业务方法或集合读写逻辑。

- [ ] **步骤 4：运行 BSON 映射测试确认通过。**

    go test ./internal/data/model -count=1

预期：通过；关键 PO 序列化为已冻结的 BSON 字段名。

- [ ] **步骤 5：格式化并记录结果。**

    gofmt -w internal/data/model
    gofmt -d internal/data/model

预期：第二个命令无输出。项目不使用 Git，不执行提交操作。

## 任务 3：实现本地索引初始化器与真实约束测试

**文件：**

- 创建：internal/data/migrate/local_schema.go
- 创建：internal/data/migrate/local_schema_test.go

- [ ] **步骤 1：编写初始化器与唯一约束失败测试。**

    func TestEnsureCreatesIndexesAndIsIdempotent(t *testing.T) {
        database, cleanup := newLocalTestDatabase(t)
        defer cleanup()
        initializer := NewInitializer(database)
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        if err := initializer.Ensure(ctx); err != nil { t.Fatal(err) }
        if err := initializer.Ensure(ctx); err != nil { t.Fatalf("second Ensure() = %v", err) }
        for _, spec := range schema.AllIndexes() {
            assertIndexNameExists(t, database.Collection(spec.Collection), spec.Name)
        }
    }

    func TestUniqueIndexesRejectDuplicateBusinessFacts(t *testing.T) {
        // 对 identities、creations、creation_steps、reservations、daily_quotas、
        // ledger_entries、payment_receipts、callback_receipts 各执行两次相同唯一键插入。
        // 每个首插入使用随机 UUID _id；第二次必须匹配 mongo.IsDuplicateKeyError(err)。
    }

newLocalTestDatabase 读取 CLING_TEST_MONGO_URI，构造完整 conf.Data 并先调用 conf.ValidateLocalMongo，然后才连接。测试使用 UUID 生成精确 _id，清理仅可用带这些 ID 的 DeleteMany；不允许删除集合、索引或无条件删除文档。

- [ ] **步骤 2：运行测试确认失败。**

    CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data/migrate -count=1

预期：失败，提示 NewInitializer 和 Ensure 尚不存在。

- [ ] **步骤 3：实现初始化器。**

    // Package migrate 只初始化 Go 主站本地 MongoDB 的集合和索引。
    package migrate

    type Initializer struct {
        database *mongo.Database
    }

    func NewInitializer(database *mongo.Database) *Initializer {
        return &Initializer{database: database}
    }

    func (initializer *Initializer) Ensure(ctx context.Context) error {
        if initializer == nil || initializer.database == nil {
            return errors.New("local MongoDB schema initializer is not configured")
        }
        byCollection := make(map[string][]mongo.IndexModel)
        for _, spec := range schema.AllIndexes() {
            indexOptions := options.Index().SetName(spec.Name)
            if spec.Unique { indexOptions.SetUnique(true) }
            byCollection[spec.Collection] = append(byCollection[spec.Collection], mongo.IndexModel{
                Keys: spec.Keys, Options: indexOptions,
            })
        }
        for _, name := range schema.AllCollections() {
            if err := initializer.ensureCollection(ctx, name); err != nil { return err }
            models := byCollection[name]
            if len(models) == 0 { continue }
            if _, err := initializer.database.Collection(name).Indexes().CreateMany(ctx, models); err != nil {
                return fmt.Errorf("create indexes for collection %q: %w", name, err)
            }
        }
        return nil
    }

ensureCollection 只调用 CreateCollection；只忽略 MongoDB 的 NamespaceExists 错误，其他错误必须附带集合名返回。所有控制流拆为独立行，新增注释必须为中文。

- [ ] **步骤 4：运行集成测试确认通过。**

    docker compose up -d mongo mongo-init
    CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data/migrate -count=1

预期：通过；第二次 Ensure 成功，8 类重复业务事实均被 MongoDB 唯一索引拒绝。

- [ ] **步骤 5：格式化并记录结果。**

    gofmt -w internal/data/migrate
    gofmt -d internal/data/migrate

预期：第二个命令无输出。项目不使用 Git，不执行提交操作。

## 任务 4：在应用监听前执行本地索引初始化

**文件：**

- 创建：internal/data/schema_initializer.go
- 修改：internal/data/data.go
- 修改：cmd/ai-business-service/main.go
- 创建：cmd/ai-business-service/main_test.go
- 修改：cmd/ai-business-service/wire.go
- 修改（生成）：cmd/ai-business-service/wire_gen.go

- [ ] **步骤 1：编写启动门禁失败测试。**

    type fakeSchemaInitializer struct {
        err    error
        called bool
    }

    func (fake *fakeSchemaInitializer) Ensure(context.Context) error {
        fake.called = true
        return fake.err
    }

    func TestNewAppStopsWhenLocalSchemaInitializationFails(t *testing.T) {
        initializer := &fakeSchemaInitializer{err: errors.New("index conflict")}
        app, err := newApp(testLogger(), testGRPCServer(), testHTTPServer(),
            biz.NewModuleRegistry(), nil, initializer)
        if err == nil || app != nil { t.Fatalf("newApp() = %v, %v; want initialization error", app, err) }
        if !initializer.called { t.Fatal("schema initializer was not called") }
    }

testGRPCServer 和 testHTTPServer 必须使用 127.0.0.1:0，且不能调用 Start。

- [ ] **步骤 2：运行测试确认失败。**

    go test ./cmd/ai-business-service -run TestNewAppStopsWhenLocalSchemaInitializationFails -count=1

预期：失败，提示 LocalSchemaInitializer 或 newApp 的初始化参数尚不存在。

- [ ] **步骤 3：实现最小装配并生成 Wire。**

    // LocalSchemaInitializer 是应用启动前创建本地索引的最小边界。
    type LocalSchemaInitializer interface {
        Ensure(context.Context) error
    }

    func NewLocalSchemaInitializer(data *Data) LocalSchemaInitializer {
        return migrate.NewInitializer(data.database)
    }

    const schemaInitializationTimeout = 10 * time.Second

    func newApp(logger *slog.Logger, gs *grpc.Server, hs *http.Server,
        modules *biz.ModuleRegistry, _ shared.TxRunner,
        initializer data.LocalSchemaInitializer) (*kratos.App, error) {
        ctx, cancel := context.WithTimeout(context.Background(), schemaInitializationTimeout)
        defer cancel()
        if err := initializer.Ensure(ctx); err != nil {
            return nil, fmt.Errorf("initialize local MongoDB schema: %w", err)
        }
        return kratos.New(
            kratos.ID(id),
            kratos.Name(Name),
            kratos.Version(Version),
            kratos.Metadata(map[string]string{
                "business_modules": strings.Join(modules.Names, ","),
            }),
            kratos.Logger(logger),
            kratos.Server(gs, hs),
        ), nil
    }

将 NewLocalSchemaInitializer 加入 data.ProviderSet，修改 wire.go 的 wireApp 返回签名以适配 newApp 的 error。然后运行：

    go generate ./cmd/ai-business-service

只允许该命令更新 wire_gen.go；不得手工编辑生成文件。

- [ ] **步骤 4：运行启动门禁测试确认通过。**

    go test ./cmd/ai-business-service -count=1
    go build ./cmd/ai-business-service

预期：通过；索引初始化失败时应用不构造，Wire 生成代码可编译。

- [ ] **步骤 5：格式化并记录结果。**

    gofmt -w internal/data/schema_initializer.go internal/data/data.go cmd/ai-business-service/main.go cmd/ai-business-service/main_test.go cmd/ai-business-service/wire.go
    gofmt -d internal/data/schema_initializer.go internal/data/data.go cmd/ai-business-service/main.go cmd/ai-business-service/main_test.go cmd/ai-business-service/wire.go

预期：第二个命令无输出。项目不使用 Git，不执行提交操作。

## 任务 5：完成本地验证与使用说明

**文件：**

- 修改：README.md
- 修改：docs/superpowers/specs/2026-09-05-cling-go-mongodb-schema-design.md（仅在实现与设计不一致时修正；不得改变已批准范围）

- [ ] **步骤 1：更新 README。**

在「当前范围」与「本地验证」中说明：服务只在独立本地 cling_main 创建 14 个空集合和索引；启动前创建失败会拒绝监听；阶段 5 不创建业务数据、不调用外部中台、不修改 Node 数据。不得写成已实现登录、支付、预扣或生成业务。

- [ ] **步骤 2：运行完整本地验证。**

    docker compose config --quiet
    docker compose up -d mongo mongo-init
    CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./... -count=1
    CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test -race ./... -count=1
    go vet ./...
    go build ./cmd/ai-business-service
    go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json

预期：所有命令退出码为 0，语义案例显示 semantic cases valid: 14。

- [ ] **步骤 3：执行实际启动检查。**

    go run ./cmd/ai-business-service
    curl --include --max-time 10 http://127.0.0.1:18000/healthz
    curl --include --max-time 10 http://127.0.0.1:18000/v1/todos/list
    curl --include --max-time 10 --request POST http://127.0.0.1:18000/healthz

预期：服务只监听 127.0.0.1:18000/19000；健康接口返回根层成功信封；未知路径返回 JSON 404 NOT_FOUND；错误方法返回 JSON 405 METHOD_NOT_ALLOWED。验证后停止临时进程，并删除本次 go build 生成的单个临时二进制（如存在）；不得删除容器、数据库或集合。

- [ ] **步骤 4：复核范围并记录结果。**

确认本阶段只新增 PO、索引、初始化器、测试、Wire 装配和 README；没有修改 Node/JS、PayCores、生成中台、钱包、对外业务路由、生产配置或 MongoDB 业务数据。项目不使用 Git，不执行提交操作。

## 计划自检

- 设计中的 14 个集合由任务 1 的集合常量和任务 2 的 PO 覆盖。
- 设计中的所有唯一约束由任务 1 声明、任务 3 真实 MongoDB 测试覆盖。
- 启动前初始化与失败即退出由任务 4 覆盖。
- 本地隔离、无业务 API、无外部调用、无自动生产迁移由任务 3、4、5 的固定边界和验收覆盖。
