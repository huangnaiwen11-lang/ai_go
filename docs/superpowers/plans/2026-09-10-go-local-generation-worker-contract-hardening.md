# Go 本地生成 Worker 合同加固实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `subagent-driven-development`（推荐）或 `executing-plans` 逐任务实施本计划。每步使用复选框跟踪。本项目不是 Git 仓库：不得初始化 Git、创建分支、工作树、提交或推送。

**目标：** 在不修改 Node/JS 语义、也不连接真实中台的前提下，使 Go 生成提交客户端与现网 V2 准入合同一致，并提供一个默认不运行的本地 Worker 进程入口。

**架构：** `integrations/generation` 负责 API Key、V2 签名和中台状态归类；`worker` 只负责既有 Outbox 投递循环；独立 `cmd/generation-submission-worker` 装配 Mongo、客户端和 Runner。主 HTTP 服务仍不启动 Worker，生产密钥与真实地址均不写入配置。

**技术栈：** Go、Kratos 配置、MongoDB 本地副本集、标准库 HTTP、现有 Wire 依赖图。

---

## 固定边界

- 不修改 Node/JS、现网钱包、PayCores、生成中台代码或生产配置。
- 不读取 Node 登录态、Node 钱包或生产密钥；配置只允许本机地址和本地占位密钥。
- 不在主 HTTP 服务中自动启动后台 Worker；只有显式运行独立命令才会轮询。
- 只处理 `text_to_image`、模板 `image_edit` 和 `image_to_video` 三个原子；不增加 Animate。

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/conf/conf.proto` | 声明生成中台独立 API Key，不与两方向 HMAC 混用。 |
| `internal/conf/validate.go` | 在启动前拒绝缺失的本地占位配置。 |
| `internal/integrations/generation/client.go` | 每次 submit / lookup 写入 API Key，并把已创建的中台状态视为受理。 |
| `internal/integrations/generation/*_test.go` | 冻结请求头与状态合同，防止回归。 |
| `cmd/generation-submission-worker/` | 唯一可显式启动的本地轮询入口，负责关闭信号和已有依赖装配。 |

### 任务 1：冻结生成中台 API Key 与状态合同

**文件：**

- 修改：`internal/integrations/generation/contract_client_test.go`
- 修改：`internal/integrations/generation/client.go`
- 修改：`internal/integrations/generation/provider.go`
- 修改：`internal/conf/conf.proto`
- 修改：`internal/conf/validate.go`
- 修改：`internal/conf/validate_test.go`
- 修改：`configs/config.yaml`
- 修改：`configs/config.local.yaml.example`
- 生成：`internal/conf/conf.pb.go`

- [x] **步骤 1：先增加失败测试。**

```go
func TestSubmit与Lookup都携带独立APIKey(t *testing.T) {
    // httptest 断言 X-API-Key 精确等于测试 API Key，且请求仍带原 V2 签名。
}

func TestSubmit已创建的中台状态应视为受理(t *testing.T) {
    // queued、dispatching、processing、completed 均带 jobId 时返回 nil；
    // rejected、failed、cancelled 仍按既有失败语义处理。
}
```

- [x] **步骤 2：运行红灯测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/integrations/generation -run 'Test(Submit与Lookup都携带独立APIKey|Submit已创建的中台状态应视为受理)' -count=1
```

预期：测试失败，因为客户端尚未发送 `X-API-Key`，且 `queued` 会被误判为未知。

- [x] **步骤 3：最小实现并生成配置代码。**

`Integrations.Generation` 增加 `api_key`；客户端只保存裁剪后的密钥，发往精确的 POST / GET 请求头。配置校验拒绝空值；状态归类只依据带 `jobId` 的中台执行事实，不改变 Worker 的账本冲正分支。

```bash
cd /Users/huangnaiwen/project/ai-business-service && make config && go test ./internal/integrations/generation ./internal/conf -count=1
```

### 任务 2：提供默认不运行的本地 Worker 入口

**文件：**

- 创建：`cmd/generation-submission-worker/main.go`
- 创建：`cmd/generation-submission-worker/main_test.go`
- 修改：`internal/worker/runner.go`
- 修改：`internal/worker/runner_test.go`

- [x] **步骤 1：先增加失败测试。**

```go
func TestNewRunnerBatch每轮最多投递指定次数(t *testing.T) {
    // 受控 deliverer 返回空队列，断言每个 tick 最多 batchSize 次，取消时正常退出。
}

func TestWorker入口不注册HTTP监听且仅装配Runner(t *testing.T) {
    // 静态验证入口不导入 server 或注册 HTTP 路由。
}
```

- [x] **步骤 2：运行红灯测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/worker ./cmd/generation-submission-worker -run 'Test(NewBatchRunner|Worker入口)' -count=1
```

预期：失败，因为批处理构造器和独立入口尚不存在。

- [x] **步骤 3：最小实现。**

保留现有 `NewRunner` 兼容调用，新增可验证的批量构造器；每轮顺序调用既有 `DeliverOnce`。`DeliverOnce` 的既有接口不区分空队列和成功投递，因此批量上限只约束单轮最多调用次数，不伪造空队列信号。入口直接作为小型组合根读取相同本地配置、调用 `conf.Validate`、构造 Mongo/客户端/Worker/Runner，并以 `signal.NotifyContext` 处理退出；它无需 Wire，且禁止接入 `cmd/ai-business-service`。

- [x] **步骤 4：绿色验证。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && go test ./internal/worker ./cmd/generation-submission-worker -count=1
GOCACHE=/private/tmp/ai-business-service-go-cache go build -o /private/tmp/generation-submission-worker ./cmd/generation-submission-worker
```

### 任务 3：补齐本地拒绝到冲正的组合回归

**文件：**

- 修改：`internal/transport/t2i/handler_e2e_test.go`
- 修改：`internal/data/creation_repository_test.go`

- [x] **步骤 1：先增加失败测试。**

以随机精确 ID 创建 T2I 和 I2I：模拟中台返回明确拒绝，调用既有 Worker，断言 `submission_failed`、Outbox `failed`、预留仅一次 `reversed`；再重放不得产生第二笔冲正。

- [x] **步骤 2：运行红灯测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/transport/t2i ./internal/data -run 'Test.*(明确拒绝|提交失败).*' -count=1
```

- [x] **步骤 3：只补必要实现或测试夹具。**

若现有 Worker 状态机已经满足断言，只增加组合测试；不复制业务逻辑。任何缺口只能在 Worker、Mongo 状态条件更新或测试专用 HTTP stub 中修复。

- [x] **步骤 4：完整本地回归。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' GOCACHE=/private/tmp/ai-business-service-go-cache go test ./... -count=1
GOCACHE=/private/tmp/ai-business-service-go-cache go vet ./...
```

## 外部验收前置条件

真实中台、PayCores 或商店验收必须由用户提供 Go 专属测试地址、API Key、两个方向 HMAC、独立 HTTPS 回调域名和测试商户；本计划不获取、不猜测、不复用现网凭据。
