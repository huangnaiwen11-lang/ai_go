# Cling Go 模板图编辑（I2I）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `executing-plans` 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法跟踪进度。本项目不是 Git 仓库，禁止初始化 Git、创建 worktree、提交或推送。

**目标：** 在不改变 Node 旧请求语义的前提下，将已选模板的图片编辑创建、预扣、Outbox、可信回调与状态读取接入本地 Go Gateway。

**架构：** Gateway 只精确放行 `templateId + inputImages` 的模板图编辑候选；transport 根据已验证 Go 会话分派 T2I 或 I2I 用例。I2I 用例只从唯一启用模板读取 `image_edit` 配方，冻结 1 张用户素材和可选服务端参考图后复用 `creations.CreateReserved`。

**技术栈：** Go、MongoDB rs0、既有 identity、creations、ledger、outbox、generation callback、net/http。

---

## 文件结构

| 文件 | 职责 |
| --- | --- |
| `internal/biz/t2i/edit.go` | 模板图编辑配方、编译器和创建用例。 |
| `internal/biz/t2i/edit_test.go` | 锁定服务端配方、素材规则和客户端字段隔离。 |
| `internal/data/i2i_repository.go` | 从唯一启用、内容面可见的模板读取 I2I 配方。 |
| `internal/data/i2i_repository_test.go` | MongoDB 模板读取、内容面过滤和精确清理。 |
| `internal/transport/sessionauth/authenticator.go` | 将验证后的内容访问级别交给已接管的 HTTP handler。 |
| `internal/transport/t2i/handler.go` | 严格 I2I DTO 与 T2I/I2I 创建分派。 |
| `internal/gateway/t2i_route.go` | 区分纯 T2I、模板 I2I 与 Node 回退请求。 |
| `cmd/api-gateway/t2i.go` | 注入 I2I 用例，保持双开关默认关闭。 |

### 任务 1：会话身份补充内容访问级别

- [x] 先在 `internal/transport/sessionauth/authenticator_test.go` 写失败测试：正常会话返回 `UserID` 和用户已保存的 `ContentAccess`；请求头不能覆盖该值。
- [x] 在 `internal/biz/identity/model.go` 为运行时验证结果增加非持久化 `ContentAccess`，并在 `ValidateSession` 已读取用户后填充它。
- [x] 在 `internal/transport/sessionauth/authenticator.go` 将该受控字段映射到 `AuthenticatedIdentity`；未知值安全失败。
- [x] 运行 `GOCACHE=/private/tmp/ai-business-service-go-cache go test ./internal/biz/identity ./internal/transport/sessionauth -count=1`。

### 任务 2：服务端模板图编辑配方与编译器

- [x] 先在 `internal/biz/t2i/edit_test.go` 写失败测试：仅 `kind=image_edit`、1 张 HTTPS 用户图、可选 HTTPS 服务端参考图、受支持编辑模型和参数可以编译为单步骤 `image_edit`。
- [x] 写失败测试：空图、多图、HTTP/带空白 URL、客户端模型/回调/技术字段、非法模板配方都被拒绝且不会调用预扣用例。
- [x] 在 `internal/biz/t2i/edit.go` 定义 `ImageEditRecipe`、`ImageEditRecipeReader`、`ImageEditCommand`、`CompileImageEdit` 与 `ImageEditUsecase`；用 `executionv2.Compile` 冻结快照并调用 `CreateReserved`。
- [x] 运行 `GOCACHE=/private/tmp/ai-business-service-go-cache go test ./internal/biz/t2i -count=1`。

### 任务 3：MongoDB 配方读取与内容面过滤

- [x] 先在 `internal/data/i2i_repository_test.go` 写 rs0 失败测试：审核受限用户只读取 SFW，普通用户可读取启用的 NSFW；同一 `template_id` 多个启用版本或非法参数失败关闭。
- [x] 在 `internal/data/i2i_repository.go` 解析模板 `parameters` 中的 `kind/model_sku/prompt/negative_prompt/parameters/input_rule/reference_assets`，并使用 `template_id + enabled + mode + content_surface` 精确过滤。
- [x] 所有本机 Mongo 测试种入随机 `_id`，仅用 `DeleteOne({_id: ...})` 清理。
- [x] 运行 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' GOCACHE=/private/tmp/ai-business-service-go-cache go test ./internal/data -run 'TestMongo.*ImageEdit' -count=1`。

### 任务 4：HTTP 分派与 Gateway 精确候选

- [x] 先在 `internal/gateway/t2i_route_test.go` 写失败测试：只有 `operation` 缺失或 `generate`、合法 `templateId`、严格 1 项 `inputImages`、无 Provider/模型/聊天字段的请求可进入 I2I handler；相邻请求继续代理 Node。
- [x] 先在 `internal/transport/t2i/handler_test.go` 写失败测试：I2I 使用受控会话内容访问级别，创建响应保持现有图片 envelope；本地模板错误返回 `400 INVALID_REQUEST`，接管后不回退 Node。
- [x] 修改 `internal/gateway/t2i_route.go` 与 `internal/transport/t2i/handler.go`，以独立 I2I 候选和严格 DTO 分派 `ImageEditUsecase`，不改变现有 T2I 判定。
- [x] 修改 `cmd/api-gateway/t2i.go` 装配 Mongo 配方读取器和 I2I 用例；双开关任一关闭时仍不连接 MongoDB、不接管 Node。
- [x] 运行 `GOCACHE=/private/tmp/ai-business-service-go-cache go test ./internal/gateway ./internal/transport/t2i ./cmd/api-gateway -count=1`。

### 任务 5：本地端到端验收与范围审计

- [x] 在 `internal/transport/t2i/handler_e2e_test.go` 增加模板 I2I 场景：真实 Go 会话、SFW 模板、1 张用户图和 1 张模板参考图，经创建、预扣、Outbox、模拟提交、可信回调后由批量和单条读取返回最终图片。
- [x] 断言 Outbox 中仅有 `capability/model_sku/input` 三个技术字段，不包含用户、模板、钻石、余额、VIP、支付或账本字段。
- [x] 运行全量验证：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' GOCACHE=/private/tmp/ai-business-service-go-cache go test ./... -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' GOCACHE=/private/tmp/ai-business-service-go-cache go test -race ./... -count=1
GOCACHE=/private/tmp/ai-business-service-go-cache go vet ./...
GOCACHE=/private/tmp/ai-business-service-go-cache go build -o /private/tmp/ai-business-service-template-image-edit ./cmd/api-gateway
```

- [x] 审计确认未修改 Node/JS、前端、PayCores、现网钱包或生产配置，且未启动真实 Worker 或线上切流。
