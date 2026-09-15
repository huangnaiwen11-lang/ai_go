# Growth Ping Gateway 首个只读灰度实现计划

> **面向 AI 代理的工作者：** 使用测试驱动开发（TDD）执行。此目录当前没有 Git 元数据，且用户已要求不使用 Git、worktree、commit 或 push；因此不创建提交，只保留最小可审查补丁。

**目标：** 由 Go Gateway 直接响应 `GET /api/growth/ping`，同时让所有其他请求继续回退 Node backend。

**架构：** `Gateway.Handler()` 在转发前调用精确 matcher。`internal/gateway/growth_ping.go` 只负责固定兼容响应，`internal/gateway/proxy.go` 保持请求 ID 与默认代理职责。未命中时不改变既有代理路径。

**技术栈：** Go 标准库 `net/http`、`net/http/httptest`、现有 Gateway 单元测试。

---

## 文件结构

- 创建：`internal/gateway/growth_ping.go` —— 精确 matcher 与只读响应 handler。
- 创建：`internal/gateway/growth_ping_test.go` —— 首个灰度接口的边界测试。
- 修改：`internal/gateway/proxy.go` —— 在默认代理前调用 matcher。

### 任务 1：用失败测试冻结灰度边界

**文件：**

- 创建：`internal/gateway/growth_ping_test.go`
- 参考：`internal/gateway/proxy_test.go`

- [x] **步骤 1：编写精确命中测试**

```go
func TestHandlerServesGrowthPingCanary(t *testing.T) {
    nodeHits := 0
    node := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
        nodeHits++
    }))
    defer node.Close()

    recorder := httptest.NewRecorder()
    request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
    request.Header.Set("X-Request-Id", "growth-ping-test")
    New(Config{DefaultUpstream: mustURL(t, node.URL)}).Handler().ServeHTTP(recorder, request)

    if recorder.Code != http.StatusOK { t.Fatalf("status = %d, want 200", recorder.Code) }
    if recorder.Header().Get("X-Request-Id") != "growth-ping-test" { t.Fatal("request id was not preserved") }
    if got := recorder.Body.String(); got != `{"success":true,"data":{"ok":true}}` { t.Fatalf("body = %q", got) }
    if nodeHits != 0 { t.Fatalf("nodeHits = %d, want 0", nodeHits) }
}
```

- [x] **步骤 2：编写回退边界测试**

```go
func TestHandlerFallsBackOutsideGrowthPingCanary(t *testing.T) {
    for _, request := range []*http.Request{
        httptest.NewRequest(http.MethodPost, growthPingPath, nil),
		httptest.NewRequest(http.MethodGet, "/api/growth/ping/", nil),
        httptest.NewRequest(http.MethodGet, "/api/growth/ping/extra", nil),
        httptest.NewRequest(http.MethodGet, "/api/v1/growth/ping", nil),
		httptest.NewRequest(http.MethodGet, "/api/growth%2Fping", nil),
		httptest.NewRequest(http.MethodGet, "/api/growth/%70ing", nil),
    } {
        // 上游返回 {"success":true,"data":{"source":"node"}}，每个请求必须收到该正文。
    }
}
```

- [x] **步骤 3：运行测试确认红灯**

运行：`go test ./internal/gateway -run 'GrowthPingCanary|FallsBackOutsideGrowthPingCanary' -count=1`

预期：FAIL，因为 `growthPingPath` 与本地 handler 尚未定义。

### 任务 2：实现最小本地只读 handler

**文件：**

- 创建：`internal/gateway/growth_ping.go`
- 修改：`internal/gateway/proxy.go`
- 测试：`internal/gateway/growth_ping_test.go`

- [x] **步骤 1：定义精确匹配与响应函数**

```go
const growthPingPath = "/api/growth/ping"

func servesGrowthPingCanary(r *http.Request) bool {
    return r.Method == http.MethodGet &&
        r.URL.Path == growthPingPath &&
        r.URL.EscapedPath() == growthPingPath
}

func serveGrowthPingCanary(w http.ResponseWriter) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write([]byte(`{"success":true,"data":{"ok":true}}`))
}
```

- [x] **步骤 2：在代理前接入精确 handler**

```go
if servesGrowthPingCanary(r) {
    serveGrowthPingCanary(w)
    return
}
g.proxyFor(r.URL.Path).ServeHTTP(w, r)
```

在此分支前保留请求 ID 生成逻辑。注释必须说明：不能用 `/api/growth` 前缀匹配，不能提前接管 `/api/v1/growth/ping`，且需要同时检查 `EscapedPath()`，防止编码路径在解码后误命中。

- [x] **步骤 3：运行目标测试确认绿灯**

运行：`go test ./internal/gateway -run 'GrowthPingCanary|FallsBackOutsideGrowthPingCanary' -count=1`

预期：PASS。

### 任务 3：执行回归与本地运行验证

**文件：**

- 修改：无。

- [x] **步骤 1：格式化并运行 Gateway 全量测试**

运行：`gofmt -w internal/gateway/growth_ping.go internal/gateway/growth_ping_test.go && go test ./internal/gateway -count=1`

预期：PASS。

- [x] **步骤 2：运行服务全量测试**

运行：`go test ./...`

预期：PASS。

- [x] **步骤 3：运行本地接口验证**

运行：`curl -i http://127.0.0.1:8080/api/growth/ping`

预期：`200 OK`、单个 `X-Request-Id` 和无尾随换行的固定成功 envelope。

- [x] **步骤 4：确认方法回退**

运行：`curl -i -X POST http://127.0.0.1:8080/api/growth/ping`

预期：响应来自 Node backend，而非固定成功 envelope。
