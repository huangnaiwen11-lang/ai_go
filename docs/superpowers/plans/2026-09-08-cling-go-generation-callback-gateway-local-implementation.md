# Cling Go 生成回调 Gateway 本地接入实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `subagent-driven-development` 或 `executing-plans` 逐任务实现。项目不是 Git 仓库：不得初始化 Git、创建工作树、提交或推送。

**目标：** 本地 Gateway 仅将两条既有内部生成回调路径交给 Go 回调 Handler，其他请求继续代理 Node。

**架构：** `gateway` 在默认代理之前作精确 method/path/query/编码路径匹配；命中时调用已存在的 `generationcallback.Handler`，未命中保持透明代理。回调 Handler、Mongo 事务和业务状态机不重复实现，也不改变其职责。

**技术栈：** Go 1.25、`net/http`、`httptest`、现有 `internal/gateway`、本地 MongoDB rs0。

---

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/gateway/generation_callback.go` | 回调精确路由匹配和 Gateway 装配边界。 |
| `internal/gateway/generation_callback_test.go` | 命中、拒绝与 Node 回退测试。 |
| `internal/gateway/proxy.go` | 将可选回调 Handler 注入 Gateway，保持默认代理。 |
| `internal/gateway/proxy_test.go` | 验证未提供 Handler 时维持现有代理行为。 |
| `internal/transport/generationcallback/handler_test.go` | 经 Gateway 的本地 rs0 回归。 |
| `cmd/ai-business-service/wire.go`、`wire_gen.go` | 仅装配本地 Handler 和 Gateway，不注册额外 Server 或路由。 |

## 固定边界

- 不修改 Node/JS、PayCores、钱包、生产配置、Nginx、回调 Origin 或部署。
- 不接入公开生成 API，不启动 Worker 或循环，不新增 Animate。
- 只接管 `POST /api/v1/internal/generation-callback` 与 `POST /api/internal/generation-callback`；任何 query、编码路径、点路径或重复斜杠均不命中本地路由，继续现有代理。
- 本地 Mongo 测试只连接 `cling_main/rs0`；清理只允许随机精确 `_id` 的 `DeleteOne`。

## 任务 1：为 Gateway 定义可选回调装配与精确匹配

**文件：**

- 创建：`internal/gateway/generation_callback.go`
- 创建：`internal/gateway/generation_callback_test.go`
- 修改：`internal/gateway/proxy.go`
- 修改：`internal/gateway/proxy_test.go`

- [ ] **步骤 1：先写红灯测试。**

```go
func TestHandler将精确生成回调交给本地处理器(t *testing.T) {
	called := false
	gateway := New(Config{DefaultUpstream: upstreamURL, GenerationCallback: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})})
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/internal/generation-callback", nil))
	if !called || recorder.Code != http.StatusNoContent { t.Fatal("callback was not dispatched") }
}
```

另写表驱动测试：legacy 路径命中；`GET`、query、`%2f`、`%2e`、`..`、重复斜杠和其他路径均到 Node upstream；未提供 `GenerationCallback` 时两个路径也到 Node upstream。

- [ ] **步骤 2：运行红灯。**

运行：`go test ./internal/gateway -run 'TestHandler.*生成回调' -count=1`

预期：编译失败，因为 `Config.GenerationCallback` 与回调 matcher 尚不存在。

- [ ] **步骤 3：实现最小匹配与分流。**

```go
type Config struct {
	DefaultUpstream *url.URL
	RouteSwitch RouteSwitch
	AdmissionTimeout time.Duration
	GenerationCallback http.Handler
}

func matchesGenerationCallback(r *http.Request) bool {
	return r != nil && r.Method == http.MethodPost && r.URL != nil &&
		!r.URL.ForceQuery && r.URL.RawQuery == "" &&
		r.URL.EscapedPath() == r.URL.Path &&
		(r.URL.Path == "/api/v1/internal/generation-callback" || r.URL.Path == "/api/internal/generation-callback")
}
```

在 `Gateway.Handler()` 分配 request ID 后、原 `confirmedLocalRoutes` 循环前检查 matcher；仅当 `generationCallback != nil` 且 matcher 成立时直接 `ServeHTTP` 并返回。不得调用 Node 准入逻辑，不得改变未命中代理。

- [ ] **步骤 4：运行绿色回归。**

运行：`go test ./internal/gateway -count=1`

预期：PASS，原 growth ping 与代理测试仍通过。

## 任务 2：受控 Wire 装配与 Gateway 端到端回归

**文件：**

- 修改：`cmd/ai-business-service/wire.go`
- 修改：`cmd/ai-business-service/wire_gen.go`
- 修改：`internal/transport/generationcallback/handler_test.go`

- [ ] **步骤 1：先写装配和端到端红灯测试。**

在 Gateway 测试中构造真实 `generationcallback.NewHandler`，以 Node V2 原始 HMAC 报文经 `gateway.Handler()` 调用；断言响应为 `{"ok":true}`。本地 rs0 fixture 必须沿用已有流程：`CreateReserved` → 首步 Outbox → `MarkSubmitted` → Gateway 回调 → 第二步 Outbox。断言第二步载荷可经 `ExecutionFromSubmissionPayload` 解析，且只含 `capability`、`model_sku`、`input`。

- [ ] **步骤 2：运行红灯。**

运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
go test ./internal/gateway ./internal/transport/generationcallback -run 'Gateway.*Callback' -count=1
```

预期：Gateway 尚未被真实 Handler 装配，测试失败。

- [ ] **步骤 3：最小 Wire 改动。**

让 Wire 仅构造并传递回调 `http.Handler` 给 `gateway.Config.GenerationCallback`。不得在 Kratos HTTP Server、`main.go`、服务注册函数或 Gateway 以外位置注册路径；不得新增监听器。

- [ ] **步骤 4：运行 rs0 绿色回归。**

运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
go test ./internal/gateway ./internal/transport/generationcallback ./internal/biz/generation ./internal/data -count=1
```

预期：PASS；无外网请求、无真实中台 POST。

## 任务 3：最终门禁与范围审查

- [ ] **步骤 1：运行本地全量验证。**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./... -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test -race ./... -count=1
go vet ./...
go build -o /private/tmp/ai-business-service-gateway-callback ./cmd/ai-business-service
go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json
```

预期：全部通过。

- [ ] **步骤 2：范围扫描。**

运行：

```bash
rg -n 'generationcallback|generation-callback' cmd internal/server internal/gateway
rg -n 'ListenAndServe|HandleFunc|Handle\(' cmd internal/server internal/gateway
```

预期：回调引用只位于 Gateway 受控装配和测试；无额外 Server 路由、Worker、Node/JS、支付或生产改动。
