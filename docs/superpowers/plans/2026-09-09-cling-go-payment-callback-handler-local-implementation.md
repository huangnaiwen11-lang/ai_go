# Go 本地 PayCores 支付回调 Handler 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `subagent-driven-development` 逐任务实现本计划。步骤使用复选框（`- [ ]`）语法跟踪进度。本项目不是 Git 仓库：不得创建工作树、提交或推送。

**目标：** 新增未注册的本地 PayCores 回调 HTTP Handler，以 `httptest` 验证 Node V2 验签、防重放、渠道订单定位与 Go 自有冻结订单结算的完整链路。

**架构：** `integrations/paycores` 继续拥有 V2 HMAC 和原始 Header 解析；`transport/paymentcallback` 只负责 HTTP 合同与受控映射；`payments` 继续拥有 nonce、防重放、订单定位与入账。Handler 不加入 server、Gateway 或 `cmd`，因此现有 Node 回调路径不变。

**技术栈：** Go 标准库 `net/http` / `httptest`、HMAC-SHA256、本机 MongoDB `cling_main` / `rs0`。

---

## 固定边界

- 只处理 Go 本地一次性钻石订单；不处理 VIP、游戏、退款、收银台创建或商店内购。
- 不读取生产密钥、不访问 PayCores、不修改 Node/JS、现网钱包、生产配置或 Gateway。
- 不注册 HTTP/gRPC 路由；`internal/walletcontract` 的 Go 路由关闭断言必须保持通过。
- 业务层只能收到用户 ID、PayCores 渠道订单号、渠道交易号和 `PaymentCallbackNonceHash`；不能收到原 nonce、签名、Body、金额、钻石、VIP、余额或价格。
- 成功响应严格为 Node 同形 JSON：`{"success":true,"data":{"newBalance":<Go 本地余额>,"duplicate":<bool>}}`。
- 失败响应严格为 Node 同形 JSON：`{"success":false,"error":"<固定文本>}`；不得回显内部错误、密钥、签名、Body 或订单金额。

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/integrations/paycores/callback.go` | 增加仅供已验签 transport 调用的 nonce 提取方法，复用同一 Header 解析规则，避免 Handler 重写大小写变体与重复 Header 处理。 |
| `internal/integrations/paycores/callback_test.go` | 固定“验签成功后得到同一原 nonce”的测试；重复/大小写 Header 仍拒绝。 |
| `internal/transport/paymentcallback/handler.go` | 未注册 HTTP Handler、精确路径与 Body 限制、受控映射、Node 同形响应和错误映射。 |
| `internal/transport/paymentcallback/handler_test.go` | `httptest` 单元测试和本机 Mongo 端到端测试；不启动真实服务。 |
| `internal/walletcontract/contract_test.go` | 仅在现有断言无法证明 paymentcallback 未注册时增强门禁。 |

### 任务 1：冻结“验签后取 nonce”的窄集成边界

**文件：**

- 修改：`internal/integrations/paycores/callback.go`
- 修改：`internal/integrations/paycores/callback_test.go`

- [x] **步骤 1：编写失败测试，锁定已验签请求的 nonce 读取。**

新增测试使用现有固定 Node V2 请求头，先调用 `Verify`，再断言新方法返回原样 nonce；同时用 `X-Request-Nonce` 与 `x-request-nonce` 两个 map key 构造重复 Header，断言新方法拒绝。

```go
func TestVerifiedNonce仅接受与验签相同的单值Header(t *testing.T) {
	verifier := newPaymentVerifier(t)
	headers := nodePaymentHeaders()
	if _, err := verifier.Verify(http.MethodPost, "/api/v1/internal/payment-confirmed", headers, []byte(nodeCanonicalPaymentBody)); err != nil {
		t.Fatal(err)
	}
	nonce, err := verifier.VerifiedNonce(headers)
	if err != nil || nonce != nodePaymentNonce {
		t.Fatalf("VerifiedNonce() = %q, %v", nonce, err)
	}
}
```

- [x] **步骤 2：运行红灯测试。**

```bash
cd /Users/huangnaiwen/project/ai-business-service && \
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/integrations/paycores -run TestVerifiedNonce -count=1
```

预期：编译失败，提示 `VerifiedNonce` 未定义。

- [x] **步骤 3：实现最小 nonce 提取方法。**

在 `CallbackVerifier` 增加：

```go
// VerifiedNonce 仅供已成功完成 Verify 的 transport 立即构造摘要。
// 它不代表调用方已完成 HMAC 验签，原 nonce 不得进入 payments。
func (verifier *CallbackVerifier) VerifiedNonce(headers http.Header) (string, error) {
	_, nonce, _, ok := callbackHeaders(headers)
	if verifier == nil || !ok {
		return "", ErrInvalidCallback
	}
	return nonce, nil
}
```

不得复制 `exactHeader`、正则或 Header 大小写处理；必须复用 `callbackHeaders`。

- [x] **步骤 4：运行定向回归。**

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/integrations/paycores -count=1
```

预期：PASS，既有 Node V2 固定向量和新增 nonce 测试均通过。

### 任务 2：实现未注册的 paymentcallback Handler 与 HTTP 合同测试

**文件：**

- 创建：`internal/transport/paymentcallback/handler.go`
- 创建：`internal/transport/paymentcallback/handler_test.go`

- [x] **步骤 1：编写失败的 Handler 合同测试。**

定义窄用例接口：

```go
type confirmedCallbackUsecase interface {
	Handle(context.Context, payments.VerifiedPaymentConfirmation) (payments.ApplyResult, error)
}
```

先用测试密钥和固定 Node 签名构造请求，覆盖：两条精确路径成功、非 POST 为 405、编码路径为 404、任意 query 为 400、超过 1 MB 为 413、签名错误为 401。成功断言精确响应：

```go
want := `{"success":true,"data":{"newBalance":42,"duplicate":false}}`
if recorder.Code != http.StatusOK || recorder.Body.String() != want {
	t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
}
```

模拟用例必须断言它收到的确认对象仅含 `UserID`、`OrderID`、`ProviderTxnID` 与有效 nonce 摘要，且 nonce 摘要不等于原 nonce。

- [x] **步骤 2：运行红灯测试。**

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/transport/paymentcallback -run TestHandler -count=1
```

预期：编译失败，提示 `paymentcallback.NewHandler` 未定义。

- [x] **步骤 3：实现最小 Handler。**

实现结构与 `internal/transport/generationcallback/handler.go` 一致，但放在独立包：

```go
type handler struct {
	verifier *paycores.CallbackVerifier
	usecase  confirmedCallbackUsecase
}

func NewHandler(verifier *paycores.CallbackVerifier, usecase confirmedCallbackUsecase) http.Handler
```

`ServeHTTP` 必须先检查 method、`EscapedPath()==Path`、固定路径和空 query，再用 `io.LimitReader(request.Body, 1024*1024+1)` 读取。调用 `verifier.Verify` 后，调用 `verifier.VerifiedNonce` 并立即执行：

```go
nonceHash, err := payments.NewPaymentCallbackNonceHash(nonce)
confirmation, err := payments.NewVerifiedPaymentConfirmation(
	verified.UserID, verified.OrderID, verified.ProviderTxnID, nonceHash,
)
result, err := usecase.Handle(request.Context(), confirmation)
```

成功时 `duplicate` 固定为 `!result.Applied`，`newBalance` 固定取 `result.DiamondBalance`。

错误映射只识别以下领域错误：

```go
ErrPaymentCallbackReplayed                 -> 409, "Request replayed"
ErrConfirmedCallbackDependenciesUnavailable -> 503, "Authentication unavailable"
```

验签与 nonce 摘要构造错误映射 `401, "Invalid signature"`；其余领域错误映射 `500, "Failed to process payment"`。所有失败使用：

```go
func writeFailure(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"success":false,"error":`+strconv.Quote(message)+`}`)
}
```

不得 import `data`、MongoDB Driver、`server`、`gateway` 或任何 Node 代码。

- [x] **步骤 4：补齐错误映射与重放绿灯测试。**

为 fake usecase 分别返回 `payments.ErrPaymentCallbackReplayed`、`payments.ErrConfirmedCallbackDependenciesUnavailable` 和普通错误，断言 409、503、500 及精确 Node 同形失败 JSON。再让同一 signed nonce 连续请求两次，断言第二次为 409；Handler 会两次进入用例，fake 用例只在首次成功结算，第二次拒绝为回放且不再次结算。

- [x] **步骤 5：运行 Handler 包回归。**

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/transport/paymentcallback -count=1
```

预期：PASS，且 `rg -n "paymentcallback" internal/server internal/gateway cmd` 无输出。

### 任务 3：本机 Mongo 端到端验证与路由关闭门禁

**文件：**

- 修改：`internal/transport/paymentcallback/handler_test.go`
- 修改：`internal/walletcontract/contract_test.go`（仅当现有门禁不能证明未注册时）

- [x] **步骤 1：编写失败的本机 Mongo 端到端测试。**

复用现有支付商品、订单和数据层初始化模式，在随机精确 ID 下创建：已发布商品、`pending` 的 Go 本地 PayCores 订单、Go 本地用户账户。通过 `httptest.NewRecorder` 调用 Handler，渠道订单号必须不同于本地订单 ID。

首次请求断言：订单变为 `paid`、仅生成一条 `payment_receipts`、账户余额增加冻结钻石数、仅生成一条 `payment_credit`。同 nonce 第二次请求断言 409，所有上述数量与余额不变。测试清理只对记录的精确 `_id` 调用 `DeleteOne`。

- [x] **步骤 2：运行红灯测试。**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/transport/paymentcallback -run TestMongoHandler -count=1
```

预期：在 Handler 或真实装配缺失时失败；不得使用 mock 替代该验收。

- [x] **步骤 3：完成最小真实装配并转绿。**

测试中只装配以下已有本地依赖：`data.NewPaymentRepository`、`data.NewPayCoresCallbackNonceRepository`、`payments.NewService`、`payments.NewConfirmedCallbackUsecase` 与测试密钥构造的 `paycores.NewCallbackVerifier`。不得向生产 ProviderSet 或任何 server 增加该 Handler。

- [x] **步骤 4：运行支付边界回归。**

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/integrations/paycores ./internal/transport/paymentcallback ./internal/data ./internal/biz/payments ./internal/walletcontract -count=1
```

预期：PASS；支付回调 Handler 仅由测试直接构造，未在任何实际路由注册。

### 任务 4：全量验证与范围扫描

**文件：**

- 修改：`docs/superpowers/plans/2026-09-09-cling-go-payment-callback-handler-local-implementation.md`（完成后勾选步骤）

- [x] **步骤 1：执行静态检查与全量测试。**

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache \
go vet ./internal/integrations/paycores ./internal/transport/paymentcallback ./internal/biz/payments ./internal/data

GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./... -count=1
```

预期：PASS。

- [x] **步骤 2：复核不扩大路由和外部副作用。**

```bash
rg -n "paymentcallback" internal/server internal/gateway cmd || true
rg -n "PayCores|paycores" internal/transport/paymentcallback -g '*.go'
```

预期：第一条没有输出；第二条只引用 Go 本地 `integrations/paycores` 与 `biz/payments`。检查文件改动，确认未修改 Node/JS、PayCores、生产配置、现网钱包或任何路由装配。
