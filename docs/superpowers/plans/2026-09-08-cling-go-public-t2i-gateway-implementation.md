# Cling Go 公开 T2I Gateway 本地接入实施计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `executing-plans` 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法跟踪进度。本项目不是 Git 仓库，禁止初始化 Git、创建 worktree、提交或推送。

**目标：** 将 Go 自有会话下的纯 T2I 创建、批量状态与单条详情精确接入 Gateway，同时保证其他请求原样代理 Node。

**架构：** Gateway 只负责精确路由、开关、候选分类和透明代理；命中 Go 条件后直接交给本地 Handler，绝不先请求 Node。T2I transport 负责严格 DTO、根层 envelope 与认证；biz/data 负责模板编译、创建、归属读取和状态投影。

**技术栈：** Go、Kratos、MongoDB 单节点副本集 `rs0`、`net/http`、既有 creations/ledger/outbox/sessionauth。

---

## 文件结构

| 文件 | 职责 |
| --- | --- |
| `internal/gateway/t2i_route.go` | 三条精确路由、路径和请求候选分类，不读取 MongoDB。 |
| `internal/gateway/proxy.go` | 在 Go 处理和 Node 透明代理之间执行 fail-closed 分流。 |
| `internal/transport/t2i/handler.go` | 严格 DTO、Go 会话认证、根层 HTTP 响应。 |
| `internal/biz/t2i/usecase.go` | 自由 T2I 模板编译、请求指纹和创建编排。 |
| `internal/biz/t2i/status.go` | Go 创作状态到现网图片视图的只读投影。 |
| `internal/data/t2i_repository.go` | 模板版本、归属创作和最终结果资产的 MongoDB 读取。 |
| `cmd/api-gateway/main.go` | 仅在两个显式开关均开启时装配本地 T2I Handler。 |

### 任务 1：Gateway 精确匹配与纯 T2I 候选分类

**文件：** 创建 `internal/gateway/t2i_route.go`、`internal/gateway/t2i_route_test.go`；修改 `internal/gateway/proxy.go`、`internal/gateway/route_switch.go`。

- [x] 先写失败测试：仅 `POST /api/chat/image/async`、`POST /api/images/statuses`、`GET /api/images/:id` 可本地匹配；尾部斜杠、编码路径、query、错误 method、重复斜杠均必须代理 Node。
- [x] 写失败测试：创建请求只有 `inputImages` 字段完全缺失、`operation` 为缺失或 `generate`、没有模板/模型/技术字段时才为候选；`inputImages: []`、`null`、未知字段或 `messageId` 必须代理 Node。
- [x] 实现固定路径常量、严格 JSON 单次解码与重复键拒绝；分类器不得读取登录态、MongoDB 或 Node 响应。
- [x] 改造 Gateway：本地开关与 T2I Handler 均存在时才直达 Handler；Handler 未命中前代理 Node，Handler 一旦接管绝不回退 Node。
- [x] 运行 `GOCACHE=/private/tmp/ai-business-service-go-cache go test ./internal/gateway -count=1`。

### 任务 2：服务端自由 T2I 模板编译与创建命令

**文件：** 创建 `internal/biz/t2i/model.go`、`internal/biz/t2i/usecase.go`、对应测试；修改 `internal/data/model/catalog.go`、`internal/data/catalog_repository.go`、`internal/data/schema/*`。

- [x] 先写失败测试：仅 `t2i-freeform` 的启用版本、`mode=template_image`、`content_surface=sfw` 能编译为单步 `text_to_image`；客户端不能指定模型、回调、LoRA 或技术参数。
- [x] 写失败测试：`X-Request-Id` 合法时构造 `gateway:t2i:<request-id>`；同键同指纹重放既有创作，同键不同指纹返回冲突。
- [x] 实现只读模板配方结构与编译器，使用 `executionv2.Compile` 冻结输入；调用既有 `creations.CreateReserved`，不新增余额预检查。
- [x] 运行 `go test ./internal/biz/t2i ./internal/biz/creations -count=1`。

### 任务 3：Go 创作图片状态投影

**文件：** 创建 `internal/biz/t2i/status.go`、`internal/data/t2i_repository.go`、对应测试。

- [x] 先写失败测试：`pending_submission` 映射 `generating`；`succeeded` 仅从最终步骤可用 result asset 取 URL；失败、冲正、没收映射固定错误码与中文消息。
- [x] 写失败测试：批量 UUID 数量为 1 至 20、无重复、全部归属同一用户才本地处理；混入 ObjectId 或未知格式必须整体回退，不能拆分响应。
- [x] 实现按 `creation_id + user_id` 的 MongoDB 归属读取与最终资产查询；不存在或非本人不返回批量条目，单条返回统一 404。
- [x] 使用 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true'` 运行 data 集成测试，清理只用随机精确 `_id` 的 `DeleteOne`。

### 任务 4：T2I HTTP Handler 与 Gateway 装配

**文件：** 创建 `internal/transport/t2i/handler.go`、`handler_test.go`；修改 `cmd/api-gateway/main.go`、`cmd/api-gateway/session_auth.go` 与测试。

- [x] 先写失败测试：无效 Go 会话返回 401；游客未绑定、需 VIP、无资金分别保留 403、403、402；数据库依赖故障返回 503；任何本地错误均不得代理 Node。
- [x] 写失败测试：创建成功 envelope 包含 Go UUID `imageId`、内部步骤 `jobId`、图片标价 20 与本次预留投影；状态读取保持前端字段形状。
- [x] 实现显式 `GATEWAY_GO_SESSION_AUTH_ENABLED` 与公开 T2I 开关的双门控；默认不装配、不连 MongoDB、不影响 Node。
- [x] 运行 `go test ./cmd/api-gateway ./internal/transport/t2i ./internal/gateway -count=1`。

### 任务 5：本地端到端验证与范围审计

- [x] 以 Go 会话、`t2i-freeform` 本地模板、rs0 fixture 覆盖创建→预扣→Outbox→显式工作者投递→可信回调→三条状态读取；不得启动后台循环或请求真实审核/生成服务。
- [x] 运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
  GOCACHE=/private/tmp/ai-business-service-go-cache go test ./... -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
  GOCACHE=/private/tmp/ai-business-service-go-cache go test -race ./... -count=1
GOCACHE=/private/tmp/ai-business-service-go-cache go vet ./...
GOCACHE=/private/tmp/ai-business-service-go-cache go build -o /private/tmp/ai-business-service-public-t2i ./cmd/api-gateway
```

- [x] 审计修改范围，确认没有修改 Node/JS、前端、PayCores、现网钱包、生产配置，也没有启用真实 Worker 或线上切流。
