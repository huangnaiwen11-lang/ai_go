# Cling Go 本地生成回调与终态编排实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `subagent-driven-development`（推荐）或 `executing-plans` 逐任务实施本计划。项目不是 Git 仓库：不得初始化 Git、创建分支、工作树、提交或推送。

**目标：** 在本地 `cling_main` / `rs0` 完成生成中台 V2 终态回调的验签、去重、终态 CAS、技术失败冲正，以及「首帧成功后才提交 I2V」编排。

**架构：** `internal/integrations/generation` 冻结现有 Node 的 `generation-callback-v2` 原始报文合同；`internal/executionv2` 冻结并绑定第二步 I2V 配方；`biz/generation` 只表达回调状态机；`data` 在一个 MongoDB Transaction 中写回执、步骤、创作、资产、配方和 Outbox。Handler 可由 `httptest` 调用，但不注册到 `main`、`server` 或 Gateway。

**技术栈：** Go 1.25、标准库 `crypto/hmac` / `crypto/sha256` / `net/http`、MongoDB Go Driver v2、本地 `cling_main` / `rs0`、`httptest`。

---

## 固定边界

- 不修改 Node/JS、PayCores、生成中台、现网钱包、生产配置、部署、Nginx 或真实 callback Origin。
- 不启动工作者循环、不注册 HTTP/gRPC 路由、不发送任何真实网络请求。
- 只接受现网 V2 的 `POST /api/v1/internal/generation-callback` 与 `/api/internal/generation-callback`；签名始终覆盖实际允许路径。
- 只支持 `text_to_image`、`image_edit`、`image_to_video`；不新增 Animate。
- 技术失败或取消立即且仅一次冲正；审核没收不属于本计划，不能借用技术失败或退款路径模拟。
- 生成请求、回调日志和错误不得携带用户 ID、模板 ID、钻石、余额、VIP、额度、价格、支付、账本、风控、工作流、提示词、素材 URL、完整 JSON、nonce、签名或密钥。
- Mongo 集成测试只允许 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true'`，且仅连接 `cling_main`。清理仅能对随机精确 `_id` 使用 `DeleteOne`；禁止 `DeleteMany`、删索引、删集合和删库。
- 所有新增 Go 注释均使用简体中文。

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/executionv2/deferred_recipe.go` | 冻结 I2V 的无首帧配方，注入首帧后生成完整快照。 |
| `internal/integrations/generation/callback.go` | 解析、验签和验证现有 V2 回调合同，不认识 MongoDB 或账本。 |
| `internal/biz/generation/callback.go` | 回调事件、终态命令、回执和仓储反转接口。 |
| `internal/biz/creations/model.go` | 增加最终状态与创建时的第二步冻结配方命令。 |
| `internal/biz/creations/usecase.go` | 与占位、预扣、首个 Outbox 同事务保存冻结第二步配方。 |
| `internal/data/model/creations.go` | 增加步骤终态字段和延迟配方 BSON PO。 |
| `internal/data/callback_repository.go` | 同事务回调回执、步骤 CAS、资产、配方消费与第二步 Outbox 写入。 |
| `internal/transport/generationcallback/handler.go` | 将原始 HTTP 请求映射为已验签事件；不注册路由。 |
| 相邻 `*_test.go` | 固定签名、重放、乱序、退款、首帧激活与 Mongo 原子性。 |

## 任务 1：冻结 V2 回调与延迟 I2V 配方合同

**文件：**

- 创建：`internal/integrations/generation/callback.go`
- 创建：`internal/integrations/generation/callback_test.go`
- 创建：`internal/executionv2/deferred_recipe.go`
- 创建：`internal/executionv2/deferred_recipe_test.go`

- [ ] **步骤 1：先写 V2 回调红灯测试。**

```go
func TestVerifyCallback接受现网V2已签名完成事件(t *testing.T) {
	raw := []byte(`{"contractVersion":"execution.callback.v2","eventId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","deliveryId":"delivery-1","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","eventType":"execution.completed","jobId":"job-1","externalRef":"step-1","status":"completed","outputs":[{"role":"result","mediaType":"image","url":"https://assets.example.test/result.png"}],"usage":{"processingMs":1,"outputCount":1},"timestamps":{},"executionRef":{"tenantId":"cling-ai","tenantSignupSource":"internal","capability":"text_to_image","modelSku":"ps-image-v1"},"metadata":{"site":"main","traceId":"trace-1"}}`)
	verifier := generation.NewCallbackVerifier(testCallbackKey, fixedNow)
	event, err := verifier.Verify(http.MethodPost, "/api/v1/internal/generation-callback", signedCallbackHeaders(raw), raw)
	if err != nil || event.ExternalRef != "step-1" { t.Fatalf("event=%#v err=%v", event, err) }
}

func TestVerifyCallback拒绝过期签名错误路径重复键和非技术字段(t *testing.T) { /* table test */ }
```

- [ ] **步骤 2：运行红灯。**

运行：`go test ./internal/integrations/generation -run TestVerifyCallback -count=1`

预期：编译失败，因为 `CallbackVerifier` 和 `CallbackEvent` 尚不存在。

- [ ] **步骤 3：实现最小回调验证器。**

实现 `CallbackVerifier.Verify(method, path, headers, rawBody)`：

```go
payload := strings.Join([]string{
	"generation-callback-v2", timestamp, method, path, nonce, sha256Hex(rawBody),
}, "\n")
expected := hmac.New(sha256.New, callbackKey)
_, _ = expected.Write([]byte(payload))
if !hmac.Equal(expected.Sum(nil), suppliedSignature) { return CallbackEvent{}, ErrInvalidCallback }
```

只允许两个既有路径、`POST`、版本 `2`、16–128 位 nonce、5 分钟窗口和严格 JSON。`executionRef` 必须验证现网主站租户身份，`metadata` 只允许 `site`、`traceId`，但二者都不进入受控事件。完成事件只接受一个无用户信息的 HTTPS `result` 输出；为兼容现网中台签名资源地址，允许查询参数和片段。失败和取消事件均不得带 `result`，且不暴露 `error` 内容，只分别在受控事件中保留固定 `Failed` 或 `Cancelled` 事实。验证器返回 `NonceHash` 与 `PayloadDigest`，供领域层持久化。

- [ ] **步骤 4：先写延迟配方红灯测试。**

```go
func TestDeferredImageToVideoRecipe只能绑定一次HTTPS首帧(t *testing.T) {
	recipe, err := executionv2.CompileDeferredImageToVideo("ps-auto", []byte(`{"prompt":"move","parameters":{"durationSeconds":5}}`))
	if err != nil { t.Fatal(err) }
	snapshot, err := recipe.BindOpeningFrame("https://assets.example.test/frame.png")
	if err != nil || snapshot.Capability != executionv2.CapabilityImageToVideo { t.Fatalf("snapshot=%#v err=%v", snapshot, err) }
}
```

- [ ] **步骤 5：运行延迟配方红灯。**

运行：`go test ./internal/executionv2 -run TestDeferredImageToVideoRecipe -count=1`

预期：编译失败，因为延迟配方合同尚不存在。

- [ ] **步骤 6：实现冻结与绑定。**

`CompileDeferredImageToVideo` 只接受 `prompt`、可选 `negativePrompt` 和 `parameters`，不接受 assets 或任何商业字段；它复制原始字节并生成摘要。`BindOpeningFrame` 只接受安全 HTTPS URL，构造 `assets:[{"role":"opening_frame","url":...}]` 后必须调用现有 `Compile(CapabilityImageToVideo, ...)`。任何输入错误只返回共享固定哨兵。

- [ ] **步骤 7：运行合同回归。**

运行：`go test ./internal/executionv2 ./internal/integrations/generation -count=1`

预期：PASS；不建立 HTTP Server，不访问网络。

## 任务 2：创建时与预扣事务一起保存第二步冻结配方

**文件：**

- 修改：`internal/biz/creations/model.go`
- 修改：`internal/biz/creations/repository.go`
- 修改：`internal/biz/creations/usecase.go`
- 修改：`internal/biz/creations/usecase_test.go`
- 修改：`internal/data/model/creations.go`
- 修改：`internal/data/creation_repository.go`
- 修改：`internal/data/creation_repository_test.go`
- 修改：`internal/data/schema/collections.go`
- 修改：`internal/data/schema/indexes.go`
- 修改：`internal/data/schema/indexes_test.go`
- 修改：`internal/data/migrate/local_schema_test.go`

- [ ] **步骤 1：为双步骤视频写红灯测试。**

```go
func TestCreateReserved文生视频同事务冻结第二步配方(t *testing.T) {
	request := validVideoRequest()
	request.DeferredImageToVideo = &creations.DeferredImageToVideo{ModelSKU: "ps-auto", InputTemplate: json.RawMessage(`{"prompt":"move","parameters":{"durationSeconds":5}}`)}
	result, err := fixture.usecase.CreateReserved(context.Background(), request)
	if err != nil { t.Fatal(err) }
	fixture.assertDeferredRecipe(result.Steps[1].ID, "ps-auto")
}
```

再覆盖：图片任务携带延迟配方、视频缺配方、第一步不是 T2I、第二步不是 I2V、配方带 assets、配方含支付或模板字段，均在事务前拒绝且不写创作、预留、分录、配方或 Outbox。

- [ ] **步骤 2：运行红灯。**

运行：`go test ./internal/biz/creations -run 'TestCreateReserved.*冻结第二步配方' -count=1`

预期：编译失败，因为命令与仓储边界尚未定义。

- [ ] **步骤 3：定义最小领域事实和 BSON 文档。**

增加 `DeferredImageToVideo` 与 `DeferredRecipe`：仅包含第二步骤 ID、创作 ID、`image_to_video`、SKU、冻结配方字节、摘要、状态和业务时间。创作模块只依赖下列窄写入边界：

```go
type DeferredRecipeWriter interface {
	Create(context.Context, *DeferredRecipe) error
}
```

新增 `generation_step_recipes` 集合与 `ux_generation_step_recipes_step_id` 唯一索引；配方集合不保存用户、模板、计费或账本事实。

- [ ] **步骤 4：在既有总事务中写入配方。**

在 `CreateReserved` 进入事务前调用 `executionv2.CompileDeferredImageToVideo`。只在合法两步骤视频的第二步骤 ID 上，以调用方事务 context 调用 `DeferredRecipeWriter.Create`；写入顺序保持「创作与步骤 → 预留 → 首步骤 Outbox → 延迟配方」。任一写入失败必须令全部事实回滚。

- [ ] **步骤 5：补真实 MongoDB 红绿测试。**

运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
go test ./internal/biz/creations ./internal/data -run 'TestMongoCreateReserved.*配方' -count=1
```

预期：PASS；随机精确配方 `_id` 仅由 `DeleteOne` 清理，失败时不留任何半成品。

## 任务 3：定义回调终态用例与事务内状态机

**文件：**

- 创建：`internal/biz/generation/callback.go`
- 创建：`internal/biz/generation/callback_test.go`
- 修改：`internal/biz/creations/model.go`
- 修改：`internal/biz/ledger/usecase.go`

- [ ] **步骤 1：写领域状态机红灯测试。**

```go
func TestHandleCallback技术失败或取消在同一事务只冲正一次(t *testing.T) {
	for _, event := range []generation.CallbackEvent{failedEvent("step-1", "job-1"), cancelledEvent("step-1", "job-1")} {
		fixture := newCallbackFixture(t)
		fixture.addSubmittedImageStep("creation-1", "step-1", "job-1")
		if err := fixture.usecase.Handle(context.Background(), event); err != nil { t.Fatal(err) }
		fixture.assertGenerationFailedAndReversed("creation-1", "step-1")
	}
}

func TestHandleCallback首帧完成原子激活第二步骤(t *testing.T) { /* 断言步骤 ready、唯一 Outbox 和保留预留 */ }
```

必须覆盖：图片完成、I2V 完成、首次步骤失败或取消、第二步骤失败或取消、重复 nonce、不同 nonce 的乱序终态、已 `reversed` / `confiscated` 预留，以及 job ID、capability、媒体类型或外部引用不匹配。

- [ ] **步骤 2：运行红灯。**

运行：`go test ./internal/biz/generation -run 'TestHandleCallback' -count=1`

预期：编译失败，因为 `CallbackUsecase`、`CallbackStore` 和终态命令尚不存在。

- [ ] **步骤 3：定义窄接口与状态。**

外部验签事件不含 `CreationID`。在它和 Store 之间定义可信 `CallbackLinker`：它只能用已保存的 `StepID` 关联补全内部创作归属，并返回已关联终态命令；不得从原始请求、URL、metadata 或其他外部字段推导。`CallbackStore` 的单事务方法只接收该已关联命令与固定 `businessAt`：

```go
type CallbackStore interface {
	Complete(context.Context, CompletedCallback) error
	Fail(context.Context, FailedCallback) error
}
```

`CompletedCallback` 固定包含由 `CallbackLinker` 补全的内部 `CreationID`，以及 `StepID`、`JobID`、`Capability`、`ResultURL`、`NonceHash`、`PayloadDigest`、`CallbackVersion` 与 `At`；`FailedCallback` 固定包含相同的内部创作、步骤、任务、能力、回执摘要与时间，并以固定终态事实区分失败或取消，但不含中台错误文本。创作增加 `succeeded`、`generation_failed`，步骤增加 `succeeded`、`generation_failed`；它们只能经回调 CAS 到达。

- [ ] **步骤 4：实现用例的最小事务编排。**

`Handle` 在事务前冻结 UTC 时间。完成事件只调用 `CallbackStore.Complete`。失败或取消事件都严格执行同一冲正路径：

```go
return tx.WithinTx(ctx, func(txCtx context.Context) error {
	if err := store.Fail(txCtx, command); err != nil { return err }
	_, err := ledgerUsecase.ReverseInTx(txCtx, command.CreationID, ledger.ReversalReasonGenerationFailed, businessAt)
	return err
})
```

重复回执和 CAS 冲突只读取既有事实并返回已确认，不允许再次写资产、Outbox 或账本。`Confiscate` 不在此用例中出现。

- [ ] **步骤 5：运行领域回归。**

运行：`go test ./internal/biz/generation ./internal/biz/ledger -count=1`

预期：PASS；用例不导入 MongoDB 或 HTTP。

## 任务 4：实现 MongoDB 回执、终态 CAS、资产和首帧激活

**文件：**

- 创建：`internal/data/generation_callback_repository.go`
- 创建：`internal/data/generation_callback_repository_test.go`
- 修改：`internal/data/data.go`
- 修改：`internal/data/model/creations.go`
- 修改：`internal/data/model/catalog.go`
- 修改：`internal/data/model/payments.go`
- 修改：`internal/data/schema/indexes.go`

- [ ] **步骤 1：写 MongoDB 原子红灯测试。**

```go
func TestMongoCallback首帧完成只创建一次第二步事件(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createTwoStepVideoWithRecipe()
	fixture.markFirstStepSubmitted("job-frame-1")
	err := fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.store.Complete(txCtx, fixture.firstFrameCompleted())
	})
	if err != nil { t.Fatal(err) }
	fixture.assertSecondStepReadyAndOnePendingEvent()
}
```

再实现并发或连续重放：第二个 nonce 的相同完成事件必须命中 CAS，不能新增 `assets` 或第二步 Outbox；失败或取消事件对同一预留只得到一条反向事实；已没收预留不被反向冲正。

- [ ] **步骤 2：运行红灯。**

运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
go test ./internal/data -run 'TestMongoCallback' -count=1
```

预期：编译失败，因为 Mongo 回调仓储尚不存在。

- [ ] **步骤 3：实现条件写入。**

`Complete` 在一个传入事务 context 中依序完成：插入 `callback_receipts`；以步骤 ID、job ID、`submitted` 和 `callback_version` 条件更新步骤；插入确定性 `asset:<step_id>:result`；对于终步骤把创作设为 `succeeded`，对于首帧步骤读取并消费冻结配方、绑定首帧、写入 `generation.submission:<second_step_id>`，并将第二步骤改为 `ready`。任一重复键或条件未命中均映射为领域冲突，不能覆盖赢家。

`Fail` 对失败或取消事件同样先写回执，再以提交状态条件将步骤和创作设为 `generation_failed`；它不直接改余额、日免、预留或 Outbox，账本冲正只由任务 3 的外层用例完成。

- [ ] **步骤 4：运行 MongoDB 绿色回归。**

运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
go test ./internal/biz/generation ./internal/data -run 'TestMongoCallback|TestMongo账本' -count=1
```

预期：PASS；所有清理由 fixture 保存的随机精确 `_id` 执行 `DeleteOne`。

## 任务 5：提供未注册的 HTTP Handler 与本地端到端测试

**文件：**

- 创建：`internal/transport/generationcallback/handler.go`
- 创建：`internal/transport/generationcallback/handler_test.go`
- 修改：`internal/biz/biz.go`
- 修改：`internal/data/data.go`

- [ ] **步骤 1：写 Handler 红灯测试。**

```go
func TestHandler验签后确认回调且不注册路由(t *testing.T) {
	handler := generationcallback.NewHandler(verifier, usecase)
	request := signedRequest(t, http.MethodPost, "/api/v1/internal/generation-callback", completedRawBody)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` { t.Fatalf("response=%d %s", recorder.Code, recorder.Body.String()) }
}
```

覆盖：错误方法、错误路径、带查询参数、验签失败、过期、未知事件、业务冲突和内部错误。完全相同的已确认终态重放（包括相同 nonce）返回幂等成功 `{"ok":true}`，不得重复写入资产、Outbox 或账本；nonce 或终态事实冲突才返回固定短错误码。所有失败响应只断言固定短错误码，不匹配或打印原始 Body。

- [ ] **步骤 2：运行红灯。**

运行：`go test ./internal/transport/generationcallback -count=1`

预期：编译失败，因为 Handler 尚不存在。

- [ ] **步骤 3：实现最小 HTTP 适配器。**

限制 Body 为 1 MiB，保留原始字节交给验证器，调用回调用例后返回固定 JSON 确认。Handler 不调用 `http.ListenAndServe`，不修改 `main.go`、`server` 或 Gateway。Provider 只导出构造器以便测试和未来受控装配，不能注册路由。

- [ ] **步骤 4：写真实本地 E2E。**

在 `httptest` Handler 上使用现有 Node 合同签名算法构造首帧完成回调；fixture 必须先经 `CreateReserved` 写入预留、首步骤 Outbox 与冻结第二步配方，再模拟首步骤 `submitted`。断言回调后第二步 Outbox payload 可经 `ExecutionFromSubmissionPayload` 构造 I2V 请求，且仅含技术字段和首帧资产。

- [ ] **步骤 5：运行端到端回归。**

运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
go test ./internal/transport/generationcallback ./internal/biz/generation ./internal/data -count=1
```

预期：PASS；不监听外部端口，不发送真实网络请求。

## 任务 6：完成本地质量门禁与语义复核

- [ ] **步骤 1：运行完整测试、竞态检查、静态检查、构建和语义契约。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && \
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./... -count=1
cd /Users/huangnaiwen/project/ai-business-service && \
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test -race ./... -count=1
cd /Users/huangnaiwen/project/ai-business-service && go vet ./...
cd /Users/huangnaiwen/project/ai-business-service && go build -o /private/tmp/ai-business-service-generation-callback ./cmd/ai-business-service
cd /Users/huangnaiwen/project/ai-business-service && go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json
```

预期：所有命令通过；不得启动工作者、注册回调路由或访问真实网络。

- [ ] **步骤 2：范围复核。**

确认没有 Node/JS、PayCores、生产配置、真实 URL、真实密钥、路由注册、循环、Animate、支付入账或审核入口改动。确认回调失败和测试输出不出现提示词、素材 URL、签名、nonce、密钥或中台错误正文。

## 规格覆盖自检

| 设计要求 | 覆盖任务 |
| --- | --- |
| 保留现有 V2 HMAC、nonce、路径与事件语义 | 任务 1、任务 5。 |
| 创建时冻结 I2V 配方，避免模板漂移 | 任务 1、任务 2。 |
| 回执去重、乱序 CAS 与不重复结算 | 任务 3、任务 4。 |
| 首帧成功后才生成第二步技术快照与 Outbox | 任务 4、任务 5。 |
| 技术失败或取消一次冲正，审核没收不退款且不在本阶段实现 | 任务 3、任务 4、任务 6。 |
| 仅本地测试、无真实路由、无后台循环 | 全部任务与任务 6。 |
