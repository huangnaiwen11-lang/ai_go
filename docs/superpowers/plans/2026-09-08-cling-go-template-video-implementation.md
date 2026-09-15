# Cling Go 视频模板双路径实现计划

> **面向 AI 代理的工作者：** 使用 `executing-plans` 逐任务实施本计划。步骤使用复选框（`- [ ]`）跟踪进度。本项目不是 Git 仓库：禁止初始化 Git、创建工作树、提交或推送。

**目标：** 在本地 Go Gateway 接管已选视频模板的单图 I2V 与文本首帧 → I2V 创建、状态读取及完整可信回调闭环，同时保持其他 Node `/video` 请求语义不变。

**架构：** `biz/video` 编译服务端视频模板配方为单步骤或两步骤 `creations.CreateReservedRequest`；`data` 读取唯一可见配方并按产品输出读取状态；`transport/video` 只解析严格 DTO；`gateway/video_route.go` 仅分类精确视频候选。既有 creations、entitlement、ledger、Outbox、worker 和 generation callback 保持各自边界。

**技术栈：** Go、MongoDB rs0、本地 `cling_main`、现有 execution.v2、net/http、httptest。

---

## 固定边界

- 只接管 `POST /api/chat/video`、`POST /api/chat/videos/status`、`GET /api/chat/video/:taskId` 的精确无 query 路由；仅当 Go 会话开关、公开视频开关和精确路由开关同时启用。
- 创建请求必须有 `templateId`，并且二选一：一张 HTTPS `imageUrl`（单步 I2V）或非空 `prompt` 且无 `imageUrl`（T2I 首帧 → I2V）。
- 创建候选只允许 `templateId`、`imageUrl`、`prompt`、`negativePrompt`、`aspectRatio`、`durationSeconds`、`enableAudio`；任何其他字段继续代理 Node。
- 不改 Node/JS、前端、PayCores、现网钱包、生产配置；不启动 Worker、不访问真实生成中台。
- 所有 MongoDB 测试仅使用 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true'`，并只对随机精确 `_id` 调用 `DeleteOne` 清理。

## 文件结构

| 文件 | 职责 |
| --- | --- |
| `internal/biz/video/template.go` | 视频模板配方、单图 I2V 与文本双步骤编译器、创建用例。 |
| `internal/biz/video/template_test.go` | 锁定路径选择、配方冻结、输入隔离和权益请求。 |
| `internal/biz/video/status.go` | 视频状态读模型和安全投影。 |
| `internal/biz/video/status_test.go` | 状态枚举、UUID 与最终视频资产投影。 |
| `internal/data/video_template_repository.go` | 基于内容访问级别读取唯一启用视频模板配方。 |
| `internal/data/video_template_repository_test.go` | rs0 配方、SFW/NSFW 和版本冲突测试。 |
| `internal/data/video_status_repository.go` | 按用户与 `product_output=video` 查询视频任务。 |
| `internal/data/video_status_repository_test.go` | MongoDB 归属、输出类型与最终视频资产测试。 |
| `internal/biz/creations/model.go`、`internal/data/model/creations.go`、`internal/data/creation_repository.go` | 持久化产品输出，允许单步骤 I2V 视频计划。 |
| `internal/transport/video/handler.go` | 严格视频 DTO、认证、创建与状态 HTTP 映射。 |
| `internal/gateway/video_route.go` | 精确视频路由和 Node 回退候选分类。 |
| `internal/gateway/proxy.go`、`cmd/api-gateway/*.go` | 独立视频 Handler 注入与三层开关装配。 |

### 任务 1：补齐视频创作的产品输出与单步骤 I2V 计划

**文件：**

- 修改：`internal/biz/creations/model.go`
- 修改：`internal/biz/creations/usecase.go`
- 修改：`internal/biz/creations/usecase_test.go`
- 修改：`internal/data/model/creations.go`
- 修改：`internal/data/creation_repository.go`
- 修改：`internal/data/creation_repository_test.go`
- 修改：`internal/data/model/model_test.go`

- [x] **步骤 1：先写失败测试。**

在 `usecase_test.go` 增加 `TestCreateReserved接受单步骤图生视频`：构造 `ProductOutputVideo`、时长 5 秒、`Plan: []StepPlan{{Sequence: 1, Atom: AtomImageToVideo}}` 与 `InitialSubmission{ModelSKU: "ps-auto", Input: ...source_image...}`，断言创建成功、唯一步骤为 `ready` 且只写一个首步 Outbox。增加 `TestCreateReserved拒绝图片单步I2V和视频缺技术快照`，断言非法命令不产生创作、预留、账本或 Outbox。

- [x] **步骤 2：运行红灯。**

运行：

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/biz/creations -run 'TestCreateReserved(接受单步骤图生视频|拒绝图片单步I2V和视频缺技术快照)' -count=1
```

预期：失败，因为 `ValidatePlan` 尚未接受视频单步骤 `image_to_video`。

- [x] **步骤 3：实施最小领域与持久化改动。**

将 `Creation.Output entitlement.ProductOutput` 与 `CreationDocument.ProductOutput string \`bson:"product_output"\`` 写入创建模型；`CreateReserved` 用 `request.Product.Output` 填充领域与 BSON 文档。把 `ValidatePlan` 的视频分支扩展为下列唯一合法集合：

```go
case entitlement.ProductOutputVideo:
	if len(plan) == 1 && plan[0].Atom == AtomImageToVideo {
		return nil
	}
	if len(plan) == 2 && plan[0].Atom == AtomTextToImage && plan[1].Atom == AtomImageToVideo {
		return nil
	}
```

保留 `compileInitialSubmission` 的首步骤原子推导；不允许 HTTP 调用方直接指定原子。更新现有 BSON 字段断言与仓储 round-trip 测试，保证图片输出仍存为 `image`、视频输出存为 `video`。

- [x] **步骤 4：运行绿色回归。**

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/biz/creations ./internal/data ./internal/data/model -count=1
```

预期：通过；现有两步骤文本视频语义不变。

### 任务 2：实现服务端视频模板双路径编译器

**文件：**

- 创建：`internal/biz/video/template.go`
- 创建：`internal/biz/video/template_test.go`

- [x] **步骤 1：先写编译器红灯测试。**

定义内存 `TemplateRecipeReader`，分别覆盖：

```go
func TestCompileTemplateVideo图片路径创建单步I2V(t *testing.T) { /* imageUrl → AtomImageToVideo */ }
func TestCompileTemplateVideo文本路径冻结首帧与I2V(t *testing.T) { /* prompt → T2I + blocked I2V */ }
func TestCompileTemplateVideo拒绝混合输入与客户端技术字段(t *testing.T) { /* 两者同时、HTTP、空值和非法配方均失败 */ }
```

成功断言图片路径的 `InitialSubmission` 可经 `executionv2.ParseSubmissionPayload` 的输入合同验证为 `image_to_video`，并含一个 `source_image`；文本路径的首步是 `text_to_image`，第二步是 `image_to_video`，且 `DeferredImageToVideo` 可经 `CompileDeferredImageToVideo` 重新验证。

- [x] **步骤 2：运行红灯。**

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/biz/video -run TestCompileTemplateVideo -count=1
```

预期：编译失败，因为 `video` 包和编译器尚不存在。

- [x] **步骤 3：实施配方与创建用例。**

在 `template.go` 定义 `TemplateRecipe`、`TechnicalRecipe`、`TemplateRecipeReader`、`CreateCommand`、`CompileTemplateVideo` 与 `Usecase`。配方只包含 `templateID/version/i2v/t2i`，技术配方固定为 `modelSKU/prompt/negativePrompt/parameters`。

`CompileTemplateVideo` 只接受下列互斥输入：

```go
type CreateInput struct {
	UserImageURL string
	UserPrompt   string
	Duration     int32
	EnableAudio  bool
}
```

图片路径将用户图写入 `source_image` 并生成单步 I2V；文本路径把用户提示追加到服务端首帧提示词，编译首步 T2I，并冻结 I2V 模板。两种路径均以 SHA-256 技术输入摘要形成 `InputDigest`，使用 `ProductOutputVideo`、用户请求的时长、音频及对应引用图数量调用 `CreateReserved`。幂等键分别是 `gateway:video:image:<requestID>` 与 `gateway:video:text:<requestID>`。

- [x] **步骤 4：运行绿色回归。**

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/biz/video ./internal/biz/creations ./internal/executionv2 -count=1
```

预期：通过；编译器不连接数据库或访问网络。

### 任务 3：实现 MongoDB 视频模板读取与视频状态投影

**文件：**

- 创建：`internal/data/video_template_repository.go`
- 创建：`internal/data/video_template_repository_test.go`
- 创建：`internal/biz/video/status.go`
- 创建：`internal/biz/video/status_test.go`
- 创建：`internal/data/video_status_repository.go`
- 创建：`internal/data/video_status_repository_test.go`

- [x] **步骤 1：先写 MongoDB 与状态红灯测试。**

为模板读取器种入随机 `TemplateDocument`：审核受限用户读取 SFW `template_video`，拒绝 NSFW；普通用户可读取 NSFW；同 `template_id` 的两个启用版本、`kind` 非 `template_video`、缺 `i2v` 或无 `t2i` 的文本配方均失败关闭。所有 fixture 仅记录随机 `_id` 并用 `DeleteOne` 清理。

为状态投影种入三条随机 `CreationDocument`：当前用户视频成功（带最终 `result` 视频资产）、当前用户图片成功、其他用户视频。断言只返回当前用户且 `product_output=video` 的任务；成功视频返回 URL，`pending_submission` 返回 `generating`，技术失败返回固定 `GENERATION_FAILED`。

- [x] **步骤 2：运行红灯。**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/data ./internal/biz/video -run 'TestMongo.*Video|TestVideoStatus' -count=1
```

预期：编译失败，因为视频模板和状态仓储尚不存在。

- [x] **步骤 3：实施受控读取与状态。**

读取器使用 `template_id + enabled=true + mode=template_video + content_surface`，受控内容访问级别决定表面集合；最多读取两条并要求恰好一条。BSON 配方只解码 `kind/i2v/t2i` 与技术字段，并将参数重新编码为 JSON；任何未知、空或非法技术形态均返回 `video.ErrInvalidTemplateVideoRequest`。

状态仓储以 `_id in ids + user_id + product_output=video` 单次查询创作，按最后步骤读取 `creation_step/result/available` 资产；`biz/video.StatusUsecase` 保持请求顺序、校验 UUID 去重与数量上限，并将内部状态映射到 `generating/completed/failed`。成功但无最终资产必须报数据不一致，不得伪造完成结果。

- [x] **步骤 4：运行绿色回归。**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/data ./internal/biz/video -count=1
```

预期：通过；测试不会删除集合、索引或非 fixture 数据。

### 任务 4：严格 HTTP、Gateway 分流与独立开关装配

**文件：**

- 创建：`internal/transport/video/handler.go`
- 创建：`internal/transport/video/handler_test.go`
- 创建：`internal/gateway/video_route.go`
- 创建：`internal/gateway/video_route_test.go`
- 修改：`internal/gateway/proxy.go`
- 修改：`internal/gateway/proxy_test.go`
- 创建：`cmd/api-gateway/video.go`
- 修改：`cmd/api-gateway/main.go`
- 修改：`cmd/api-gateway/main_test.go`

- [x] **步骤 1：先写 Gateway 与 Handler 红灯测试。**

Gateway 表驱动测试覆盖精确路由、图片候选、文本候选、文本与图片混合、空模板、多图、HTTP URL、Provider、模型、LoRA、工作流、聊天字段、未知字段与 query；仅两种合法请求进入本地 `VideoHandler`，其他均返回 Node fixture 响应。

Handler 测试用受控认证器和 fake `video.Creator` 验证：图片与文本请求使用已认证 `UserID/ContentAccess`、稳定幂等键和视频响应信封；缺少绑定返回既有 403、需要 VIP 返回 403、余额不足返回既有 402；本地非法模板请求返回 `400 INVALID_REQUEST`，已接管的请求不得回退 Node。

- [x] **步骤 2：运行红灯。**

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/gateway ./internal/transport/video ./cmd/api-gateway -run 'Test.*Video' -count=1
```

预期：编译失败，因为视频路由、Handler 与注入尚不存在。

- [x] **步骤 3：实施最小传输与装配。**

实现 `transport/video.NewHandler(authenticator, creator, statusReader)`；`POST /api/chat/video` 以严格 JSON 解码成互斥路径命令，`X-Request-Id` 缺失时生成 UUID。创建成功返回：

```json
{"success":true,"data":{"taskId":"<creation UUID>","status":"generating","coinsUsed":50,"coinsRemaining":0,"durationSeconds":5}}
```

`video_route.go` 提供 `matchVideoLocalRoute`、`isVideoLocalCandidate` 与精确 route key；`proxy.go` 增加独立 `VideoHandler` 字段与先于默认代理的分派分支。`cmd/api-gateway/video.go` 在已有本地 Mongo 装配模式中注入 `video.NewUsecase`、视频模板仓储与状态仓储；`GATEWAY_PUBLIC_VIDEO_ENABLED` 只有显式 `true` 时才与 `GATEWAY_GO_SESSION_AUTH_ENABLED` 共同装配，关闭时不连接 MongoDB。更新 `newGateway`、清理函数和开关单测。

- [x] **步骤 4：运行绿色回归。**

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/gateway ./internal/transport/video ./cmd/api-gateway -count=1
```

预期：通过；默认全部视频请求仍代理 Node。

### 任务 5：双路径本地端到端回调闭环与质量门禁

**文件：**

- 创建：`internal/transport/video/handler_e2e_test.go`
- 修改：`docs/superpowers/plans/2026-09-08-cling-go-template-video-implementation.md`

- [x] **步骤 1：先写端到端红灯测试。**

使用真实本地 rs0 fixture 写入会话、用户、账户、VIP 与随机 SFW 视频模板，分别执行：

1. `templateId + imageUrl`：创建、预扣、单个 I2V Outbox、模拟 `MarkSubmitted`、可信 `image_edit` 不匹配回调拒绝、可信 `image_to_video` 回调完成、单条与批量视频状态返回最终 `videoUrl`。
2. `templateId + prompt`：创建、预扣、首步 T2I Outbox、模拟提交、可信首帧回调、第二步唯一 I2V Outbox、模拟第二步提交、可信 I2V 回调完成、状态返回最终 `videoUrl`。

两条测试都将 Outbox payload 解码为 map，断言顶层只有 `capability/model_sku/input`，而 `input` 只含技术字段。fixture 按随机精确 ID 清理创作、步骤、预留、账本、Outbox、延迟配方、资产及回执。

- [x] **步骤 2：运行红灯。**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/transport/video -run TestHandler视频模板 -count=1 -v
```

预期：在缺少视频端到端装配前失败。

- [x] **步骤 3：完成 E2E fixture 与回归断言。**

复用现有 callback verifier、callback usecase、Mongo transaction、`GenerationSubmissionRepository.MarkSubmitted` 与 session authenticator；测试不构造 Worker 或 generation HTTP client。为每个回调使用唯一 nonce，首帧回调必须携带 `text_to_image`，第二步回调必须携带 `image_to_video`，确保生产 capability CAS 未被放宽。

- [x] **步骤 4：运行最终验证。**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache go test ./... -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache go test -race ./... -count=1
GOCACHE=/private/tmp/ai-business-service-go-cache go vet ./...
GOCACHE=/private/tmp/ai-business-service-go-cache \
go build -o /private/tmp/ai-business-service-template-video ./cmd/api-gateway
```

- [x] **步骤 5：范围审计与计划收口。**

确认改动仅在本地 Go 项目及本计划；没有 Node/JS、前端、PayCores、现网钱包、生产配置、真实中台调用、Worker 循环、Animate 或支付改动。把通过验证的任务勾选为完成。

## 规格覆盖自检

| 规格要求 | 覆盖任务 |
| --- | --- |
| 选模板后的图片 I2V 与文本两步骤统一入口 | 任务 2、任务 4、任务 5。 |
| 单步骤 I2V 与两步骤计划均可预扣创建 | 任务 1、任务 2。 |
| SFW/NSFW 内容面和唯一启用模板 | 任务 3。 |
| 状态只读取本地视频创作与可信结果资产 | 任务 3、任务 4、任务 5。 |
| 精确候选分流与 Node 回退 | 任务 4。 |
| 首帧后才投递第二步、回调严格能力匹配 | 任务 5。 |
| 本地测试、无 Worker、无生产改动 | 全部任务，任务 5 最终审计。 |
