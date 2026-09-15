# Cling Go 本地生成提交与 Outbox 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `subagent-driven-development`（推荐）或 `executing-plans` 逐任务实施本计划。每步使用复选框跟踪。本项目不是 Git 仓库：不得初始化 Git、创建分支、工作树、提交或推送。

**目标：** 让已完成占位和预扣的首个技术步骤以事务内 Outbox 方式安全提交到本地模拟的 `execution.v2` 生成中台，并在明确未受理时立即且仅一次冲正。

**架构：** `creations` 只在创建事务内写入最小技术请求快照和稳定 Outbox 事件；`outbox` 管理领取租约；`generation` 管理 V2 协议和提交状态；`worker` 负责投递、查询对账与失败补偿；`ledger` 只通过事务内冲正接口更新自己的账本事实。所有 MongoDB 写入使用现有 `shared.TxRunner`，不新增对外路由。

**技术栈：** Go 1.25、MongoDB Go Driver v2、本地 `cling_main` / `rs0`、标准库 `net/http` / `httptest` / `crypto/hmac` / `crypto/sha256`。

---

## 固定边界

- 不修改 Node/JS、`generation-service`、PayCores、现网钱包、生产配置或既有对外 API。
- 不创建 HTTP/gRPC 业务路由；回调 Handler、回调 nonce 和终态媒体处理属于下一计划。
- 只支持 `text_to_image`、`image_edit`、`image_to_video`；明确拒绝 Animate 和其他原子。
- 生成中台只接收 `execution.v2` 允许的技术字段；递归拒绝用户 ID、模板 ID、钻石、余额、VIP、额度、价格、支付、账本、风控、工作流和旧回调字段。
- 连接 MongoDB 的集成测试只能使用 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true'`，且仅连接 `cling_main`。
- 测试清理仅能对随机精确 `_id` 逐条 `DeleteOne`；禁止 `DeleteMany`、删集合、删库和生产 URI。
- 所有新增 Go 注释均使用简体中文。

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/biz/outbox/model.go` | 定义事件、领取租约、状态与提交请求快照，不涉及 MongoDB。 |
| `internal/biz/outbox/repository.go` | 定义入队、条件领取、重试、结案的反转依赖。 |
| `internal/biz/creations/model.go` | 增加仅供服务端编译器使用的首步骤技术快照与提交失败状态。 |
| `internal/biz/creations/usecase.go` | 将首个 `ready` 步骤的稳定 Outbox 事件写入既有创建总事务。 |
| `internal/biz/ledger/usecase.go` | 提供 `ReverseInTx`，供提交失败补偿与任务状态同事务收敛。 |
| `internal/biz/generation/submission.go` | 定义 V2 提交命令、结果分类、步骤状态转换和窄仓储接口。 |
| `internal/integrations/generation/client.go` | 用标准库 HTTP 发送和查询 `execution.v2`，不认识账本与用户业务。 |
| `internal/integrations/generation/signer.go` | 实现现网方向性 V2 请求签名和安全 URL 拼接。 |
| `internal/data/outbox_repository.go` | 实现 Outbox 事件 BSON 映射、租约条件更新和逐条读取。 |
| `internal/data/generation_submission_repository.go` | 在同一事务中更新步骤、创作与 Outbox，绝不直接处理余额。 |
| `internal/worker/generation_submission.go` | 领取事件、投递、查询对账和调用事务内冲正。 |
| `internal/conf/conf.proto`、`validate.go` | 拆分两个方向的 HMAC 配置，并把回调基地址限定为 Origin。 |
| `internal/data/model/outbox.go`、`internal/data/model/creations.go` | 增加租约和提交状态必要 BSON 字段。 |
| `internal/data/schema/indexes.go` | 增加 Outbox 领取查询需要的组合索引，不改变既有唯一约束。 |

## 任务 1：先冻结 execution.v2 与方向性签名合同

**文件：**

- 创建：`internal/integrations/generation/types.go`
- 创建：`internal/integrations/generation/signer.go`
- 创建：`internal/integrations/generation/signer_test.go`
- 创建：`internal/integrations/generation/client.go`
- 创建：`internal/integrations/generation/client_test.go`
- 修改：`internal/conf/conf.proto`
- 修改：`internal/conf/conf.pb.go`（只能通过 `make config` 生成）
- 修改：`internal/conf/validate.go`
- 修改：`internal/conf/validate_test.go`
- 修改：`configs/config.yaml`
- 修改：`configs/config.local.yaml.example`

- [ ] **步骤 1：编写 V2 字段白名单和签名红灯测试。**

```go
func TestBuildExecutionRequest拒绝商业字段并只保留V2允许字段(t *testing.T) {
	request, err := generation.BuildExecutionRequest(generation.SubmissionCommand{
		CreationID: "creation-1", StepID: "step-1", Atom: "text_to_image",
		ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[],"parameters":{}}`),
	})
	if err != nil { t.Fatalf("BuildExecutionRequest() error = %v", err) }
	body, err := json.Marshal(request)
	if err != nil { t.Fatal(err) }
	for _, forbidden := range []string{"userId", "templateId", "diamonds", "balance", "vip", "ledger", "callbackUrl"} {
		if bytes.Contains(body, []byte(forbidden)) { t.Fatalf("body contains %q: %s", forbidden, body) }
	}
}

func TestSignRequest匹配现网V2签名载荷(t *testing.T) {
	headers, err := generation.SignRequest("POST", "/api/v2/executions", []byte(`{"contractVersion":"execution.v2"}`), "1710000000000", "12345678-1234-4123-8123-123456789abc", testRequestKey)
	if err != nil { t.Fatalf("SignRequest() error = %v", err) }
	if headers.Get("X-Service-Id") != "main-backend" || headers.Get("X-Signature-V2") == "" { t.Fatalf("headers = %#v", headers) }
}
```

- [ ] **步骤 2：运行红灯测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/integrations/generation -run 'TestBuildExecutionRequest|TestSignRequest' -count=1
```

预期：编译失败，提示 `BuildExecutionRequest`、`SubmissionCommand` 与 `SignRequest` 尚未定义。

- [ ] **步骤 3：实现最小 V2 类型、递归字段校验和签名。**

`types.go` 定义 `SubmissionCommand`、`ExecutionRequest`、`ExecutionResponse`，并固定：

```go
const (
	ExecutionContractVersion = "execution.v2"
	ExecutionCreatePath      = "/api/v2/executions"
	ExecutionLookupPath      = "/api/v2/executions/lookup"
)
```

`BuildExecutionRequest` 必须：验证步骤 ID、原子、模型 SKU 和 `input` JSON 对象；只允许 `assets`、`prompt`、`negativePrompt`、`parameters`；递归拒绝共享合同中的商业字段；以 `cling-step:<step_id>` 同时派生 `idempotencyKey`，以 `step_id` 填充 `externalRef`；固定 `delivery.callback=tenant`、`delivery.resultUrlPolicy=permanent`、`priorityClass=standard`、`metadata.site=cling-go-main`。

`SignRequest` 计算原始 Body SHA-256，并以固定换行载荷签名：

```text
main-backend-generation-request-v2
main-backend
generation-service
POST
/api/v2/executions
<timestamp>
<nonce>
<body-sha256>
```

不得从日志或错误中返回 Body、提示词、素材 URL、nonce、签名或密钥。

- [ ] **步骤 4：以测试先行拆分方向性配置。**

先增加以下配置红灯测试：缺少 `generation_request_hmac_key` 或 `generation_callback_hmac_key` 时校验失败；两个值相同也失败；`callback_base_url` 包含路径、查询或片段时失败；本地回环 HTTP Origin 可通过，非本地环境只能使用 HTTPS Origin。然后在 `Security` 中以两个独立字段替换通用 `callback_hmac_key`，运行 `make config` 生成 `conf.pb.go`，并把两个配置样例替换为无效的本地占位值。

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/conf -run 'TestValidate.*Generation.*Key|TestValidate.*Callback.*Origin' -count=1
cd /Users/huangnaiwen/project/ai-business-service && make config
```

预期：配置校验与生成均通过；仓库不保存任何真实密钥。

- [ ] **步骤 5：实现 `httptest` 客户端行为测试。**

增加测试服务器，断言请求精确指向 `/api/v2/executions`、携带 V2 签名头、只包含允许字段。覆盖 `201` 接受、`400` 明确拒绝、网络超时和 `500` 未知结果，以及 `GET /api/v2/executions/lookup` 使用稳定 `idempotencyKey` 与 `externalRef` 查询。

- [ ] **步骤 6：运行转绿验证。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/integrations/generation -count=1
```

预期：PASS；测试不产生真实网络请求。

## 任务 2：以 TDD 落地可领取的 MongoDB Outbox

**文件：**

- 创建：`internal/biz/outbox/model.go`
- 创建：`internal/biz/outbox/repository.go`
- 创建：`internal/biz/outbox/model_test.go`
- 创建：`internal/data/outbox_repository.go`
- 创建：`internal/data/outbox_repository_test.go`
- 修改：`internal/data/model/outbox.go`
- 修改：`internal/data/schema/indexes.go`
- 修改：`internal/data/data.go`

- [ ] **步骤 1：先写领域状态和稳定事件 ID 的红灯测试。**

```go
func TestSubmissionEventID对同一步骤稳定且不同步骤隔离(t *testing.T) {
	if got := outbox.SubmissionEventID("step-1"); got != "generation.submission:step-1" { t.Fatalf("ID = %q", got) }
	if outbox.SubmissionEventID("step-1") == outbox.SubmissionEventID("step-2") { t.Fatal("不同步骤不得共享事件 ID") }
}

func TestEventTransition只允许领取后结案(t *testing.T) {
	event := outbox.NewPending(outbox.SubmissionEventID("step-1"), "creation-1", []byte(`{}`), now)
	if err := event.MarkDelivered(now); !errors.Is(err, outbox.ErrInvalidTransition) { t.Fatalf("err = %v", err) }
}
```

- [ ] **步骤 2：运行红灯测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/biz/outbox -count=1
```

预期：编译失败，因为 Outbox 领域包尚不存在。

- [ ] **步骤 3：定义窄领域接口和 BSON 文档。**

`outbox.Event` 至少包含 `ID`、`AggregateID`、`EventType`、`Payload`、`DeliveryStatus`、`AttemptCount`、`NextAttemptAt`、`LeaseToken`、`LeaseUntil`、`LastError`、`CreatedAt`、`UpdatedAt`。`Repository` 只声明：

```go
Enqueue(context.Context, *Event) error
Claim(context.Context, string, time.Time, time.Time) (*Event, error)
Requeue(context.Context, string, string, time.Time) error
MarkDelivered(context.Context, string, string, time.Time) error
MarkFailed(context.Context, string, string, time.Time) error
```

`Claim` 必须使用一次 `FindOneAndUpdate`：只匹配 `pending` / `reconciling`、`next_attempt_at <= now` 且不存在有效租约的事件；写入 `dispatching`、随机租约 token、租约到期和递增次数。任何未命中都返回 `(nil, nil)`。

- [ ] **步骤 4：编写本地 MongoDB 竞争红灯测试。**

使用随机事件 ID 插入一个 `pending` 事件，同时启动两个 `Claim` 调用，断言只有一个返回租约。通过 `t.Cleanup` 对该事件 ID 单条 `DeleteOne` 清理。再断言错误 token 不能 `MarkDelivered`，已结案事件不能被再次领取。

- [ ] **步骤 5：实现仓储并运行绿色验证。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/biz/outbox ./internal/data -run 'Test(SubmissionEventID|EventTransition|MongoOutbox)' -count=1
```

预期：PASS；不改变现有 16 个集合，`outbox_events` 只新增领取所需字段和组合索引。

## 任务 3：把首步骤提交事件纳入创作与预扣总事务

**文件：**

- 修改：`internal/biz/creations/model.go`
- 修改：`internal/biz/creations/repository.go`
- 修改：`internal/biz/creations/usecase.go`
- 修改：`internal/biz/creations/usecase_test.go`
- 修改：`internal/data/creation_repository.go`
- 修改：`internal/data/creation_repository_test.go`
- 修改：`internal/biz/creations/provider.go`

- [ ] **步骤 1：为原子入队写失败测试。**

在既有内存事务夹具中增加假 `outbox.Writer`，写入：

```go
func TestCreateReserved同事务写入首步骤提交事件(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundFreeUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	request := validImageRequest()
	request.InitialSubmission = validInitialSubmission("ps-image-v1", `{"prompt":"x","assets":[],"parameters":{}}`)

	result, err := fixture.usecase.CreateReserved(context.Background(), request)
	if err != nil { t.Fatalf("CreateReserved() error = %v", err) }
	fixture.assertPendingSubmissionEvent(outbox.SubmissionEventID(result.Steps[0].ID), result.Creation.ID, result.Steps[0].ID)
}

func TestCreateReserved预留失败时不遗留Outbox事件(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.failReservation = errors.New("reserve failed")
	_, _ = fixture.usecase.CreateReserved(context.Background(), validRequestWithSubmission())
	fixture.assertNoSubmissionEvents()
}
```

- [ ] **步骤 2：运行红灯测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/biz/creations -run 'TestCreateReserved.*提交事件' -count=1
```

预期：编译失败，提示 `InitialSubmission` 与 Outbox 依赖尚未定义。

- [ ] **步骤 3：最小化扩展创建命令与用例。**

增加仅供服务端模板编译器调用的 `InitialSubmission`：它只能对应计划中第一个 `ready` 步骤，包含模型 SKU 和技术 `input` JSON；不得由未来 HTTP 客户端提交原子类型。`CreateReserved` 在既有 `Repository.Create` 与 `ReserveInTx` 均成功后，用固定 `generation.submission:<step_id>` 调用 `outbox.Writer.Enqueue`。若任意一步失败，当前 MongoDB 事务必须回滚创作、步骤、预留、分录和事件。

文生视频只对第一个 `text_to_image` 步骤写事件；第二步仍为 `blocked` 且不得提前含有可提交事件。既有 `InputDigest` 规则不变，技术请求原文只出现于 Outbox Payload，不写入 `Creation` 或 `CreationStep` 文档。

- [ ] **步骤 4：补充真实 MongoDB 集成测试并转绿。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/biz/creations ./internal/data -run 'Test(CreateReserved.*提交事件|MongoCreateReserved.*Outbox)' -count=1
```

预期：PASS；同幂等键重放只有一套创作、预留、分录和 Outbox 事件。

## 任务 4：补足事务内冲正和提交状态条件更新

**文件：**

- 修改：`internal/biz/ledger/usecase.go`
- 修改：`internal/biz/ledger/usecase_test.go`
- 创建：`internal/biz/generation/submission.go`
- 创建：`internal/biz/generation/submission_test.go`
- 创建：`internal/data/generation_submission_repository.go`
- 创建：`internal/data/generation_submission_repository_test.go`
- 修改：`internal/data/model/creations.go`
- 修改：`internal/data/creation_repository.go`

- [ ] **步骤 1：先写 `ReverseInTx` 不创建嵌套事务的红灯测试。**

```go
func TestReverseInTx复用调用方事务且只冲正一次(t *testing.T) {
	repository := newMemoryRepository()
	repository.reserve(testReservation("creation-1"))
	runner := &countingTxRunner{}
	usecase := ledger.NewUsecase(repository, runner)

	result, err := usecase.ReverseInTx(context.Background(), "creation-1", "submission_rejected", fixedNow)
	if err != nil { t.Fatalf("ReverseInTx() error = %v", err) }
	if result.Status != ledger.ReservationStatusReversed || runner.calls != 0 { t.Fatalf("result=%#v calls=%d", result, runner.calls) }
}
```

- [ ] **步骤 2：运行红灯测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/biz/ledger -run TestReverseInTx复用调用方事务且只冲正一次 -count=1
```

预期：编译失败，因为 `ReverseInTx` 尚未定义。

- [ ] **步骤 3：提取账本冲正主体并定义生成提交仓储。**

保留 `Reverse` 的公开行为：它在事务外冻结 UTC `businessAt`，再调用新增 `ReverseInTx(ctx, creationID, reason, businessAt)`。新入口不得调用 `WithinTx`，只接受 `reserved -> reversed` 的条件迁移，并复用原始余额或日免快照。

`generation.SubmissionStore` 定义以下事务内方法：

```go
ClaimedSubmission(context.Context, string) (*generation.SubmissionRecord, error)
MarkSubmitted(context.Context, generation.SubmittedCommand) error
MarkReconciling(context.Context, generation.ReconcilingCommand) error
MarkRejected(context.Context, generation.RejectedCommand) error
```

`MarkSubmitted` 只接受当前 `dispatching` 或 `reconciling` 与匹配租约的步骤，写入唯一中台 `jobId`、步骤 `submitted` 和 Outbox `delivered`。`MarkRejected` 只接受相同条件，写步骤和创作的 `submission_failed`；账本冲正由工作者在同一个 `TxRunner` 回调紧随其后执行，最后标记 Outbox `failed`。所有转换使用 MongoDB 条件更新，未命中必须返回明确冲突而不是覆盖状态。

- [ ] **步骤 4：编写本地 MongoDB CAS 测试并转绿。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/biz/ledger ./internal/biz/generation ./internal/data -run 'Test(ReverseInTx|MongoSubmission.*CAS)' -count=1
```

预期：PASS；并发结案者只有一个可修改任务和预留。

## 任务 5：实现本地提交工作者、对账和明确失败补偿

**文件：**

- 创建：`internal/worker/generation_submission.go`
- 创建：`internal/worker/generation_submission_test.go`
- 修改：`internal/biz/generation/provider.go`
- 修改：`internal/data/data.go`
- 修改：`internal/biz/biz.go`
- 修改：`cmd/ai-business-service/main.go`
- 修改：`cmd/ai-business-service/main_test.go`

- [ ] **步骤 1：先写工作者明确失败、未知结果和重放红灯测试。**

```go
func TestDeliverOnce明确拒绝时事务内冲正一次(t *testing.T) {
	fixture := newSubmissionFixture(t, http.StatusBadRequest, `{"code":"INVALID_INPUT"}`)
	err := fixture.worker.DeliverOnce(fixture.ctx, fixture.eventID)
	if err != nil { t.Fatalf("DeliverOnce() error = %v", err) }
	fixture.assertSubmissionFailedAndReversed()
}

func TestDeliverOnce请求超时先查询且不创建第二个技术任务(t *testing.T) {
	fixture := newUnknownSubmissionFixture(t)
	_ = fixture.worker.DeliverOnce(fixture.ctx, fixture.eventID)
	fixture.assertOnePostAndReconciling()
	fixture.answerLookupAccepted("job-1")
	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.eventID); err != nil { t.Fatal(err) }
	fixture.assertSubmitted("job-1")
	fixture.assertPostCount(1)
}
```

- [ ] **步骤 2：运行红灯测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/worker -run 'TestDeliverOnce' -count=1
```

预期：编译失败，因为提交工作者尚不存在。

- [ ] **步骤 3：实现领取、提交、查询与补偿顺序。**

`DeliverOnce` 先以随机租约 token 领取事件。它只处理 `generation.submission.requested`，调用 V2 客户端时不发送任何账务数据。

- `Accepted`：在事务内 `MarkSubmitted`，Outbox 结案为 `delivered`。
- `Rejected`：在同一事务内 `MarkRejected`、`ledger.ReverseInTx(..., "submission_rejected", frozenNow)`、`MarkFailed`。
- `Unknown`：在事务内写入 `reconciling`，之后仅用原稳定幂等键查询；查询接受则按 `Accepted` 结案，查询确认未受理则按 `Rejected` 补偿，仍未知则带退避时间重新排队。

不得在请求超时或任何未知结果后生成新的事件 ID、步骤 ID、创建 ID 或中台幂等键。若条件更新表明回调或其他工作者已收敛，读取并返回既有事实，不能再冲正。

- [ ] **步骤 4：运行工作者与关联领域测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/worker ./internal/integrations/generation ./internal/biz/generation ./internal/biz/ledger -count=1
```

预期：PASS；所有外部交互均由 `httptest` 模拟。

- [ ] **步骤 5：仅装配依赖，不启动后台循环也不注册路由。**

把生成提交 Usecase、Outbox Repository、V2 Client 与工作者构造器放入相应 `ProviderSet`。`main.go` 可显式接收未使用的工作者依赖以强制 Wire 校验图，但不得调用循环、写数据库或注册 HTTP/gRPC Handler。运行：

```bash
cd /Users/huangnaiwen/project/ai-business-service && go generate ./cmd/ai-business-service && go test ./cmd/ai-business-service -count=1 && go build ./cmd/ai-business-service
```

预期：PASS；应用仍无生成提交业务路由，运行时也不会自动向任何地址投递。

## 任务 6：完成本地质量门禁与范围复核

- [ ] **步骤 1：运行完整测试、竞态检查、静态检查和语义契约。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./... -count=1
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test -race ./... -count=1
cd /Users/huangnaiwen/project/ai-business-service && go vet ./...
cd /Users/huangnaiwen/project/ai-business-service && go build ./cmd/ai-business-service
cd /Users/huangnaiwen/project/ai-business-service && go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json
```

预期：所有测试、竞态检查、静态检查、构建和现有语义案例均通过。

- [ ] **步骤 2：复核范围。**

确认新代码只连接本地 MongoDB 和 `httptest`；不存在真实中台 URL、真实密钥、Node/JS 改动、对外业务路由、后台循环启动、支付改动或 Animate 字符串分支。确认日志和测试输出不包含提示词、素材 URL、签名、nonce 或密钥。

## 规格覆盖自检

| 设计要求 | 覆盖任务 |
| --- | --- |
| 占位、预扣和首次 Outbox 同事务提交 | 任务 3。 |
| 只允许三个技术原子，文生视频只投递首帧 | 任务 1、任务 3。 |
| V2 仅含技术字段且使用主站回调地址 | 任务 1、任务 5。 |
| 明确失败立即冲正，未知结果先查询 | 任务 4、任务 5。 |
| 并发领取、重放和回调竞争不重复结算 | 任务 2、任务 4、任务 5。 |
| 仅本地 MongoDB、无路由、无真实外部调用 | 全部任务与任务 6。 |
