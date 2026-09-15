# Go 本地收银台与商店内购入口实施计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `executing-plans` 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 在不改 Node/JS、不连接真实 PayCores、Apple 或 Google 的前提下，补全 Go 自有账本的本地收银台订单创建与受控 IAP 回执验证入口，并保持默认透明代理。

**架构：** 支付领域只保存服务器发布的商品版本、订单快照和已验证的商店交易；传输层只解析 HTTP 和会话，实际验签由可替换的 IAP 验证器完成。Gateway 仅在三个独立开关和精确路由开关均开启时接管 Node 的既有 URL，未接管的请求逐字节代理给 Node。测试使用本地确定性验证器，绝不调用真实商店或支付平台。

**技术栈：** Go、Kratos 配置、MongoDB 副本集事务、`net/http`、`httptest`。

---

## 文件职责

- `internal/biz/payments/checkout.go`：创建商品冻结订单、IAP 已验签交易模型与领域用例。
- `internal/biz/payments/checkout_test.go`：验证服务端商品快照、交易归属、双渠道幂等和客户端不可指定钻石数。
- `internal/data/payment_repository.go`：以既有事务和唯一索引持久化订单、回执与余额。
- `internal/integrations/appstore/verifier.go`：仅定义 Apple/Google 回执验证抽象与本地受控验证器；不含生产 URL 或密钥。
- `internal/transport/localpayment/handler.go`：处理精确的现网收银台与 `verify-purchase` URL，输出 Node 风格成功信封。
- `internal/transport/localpayment/handler_test.go`：拒绝非法会话、未知商品、伪造数量和跨用户回放；验证成功与重放。
- `internal/gateway/payment_entry.go`：定义精确本地支付入口路由匹配。
- `internal/gateway/proxy.go`、`internal/gateway/proxy_test.go`：增加显式支付入口处理器，保证默认代理和编码/查询路径回退。
- `cmd/api-gateway/payment_entry.go`、`cmd/api-gateway/main.go`：只在三重显式开关开启时装配本地 Mongo 与测试验证器。
- `cmd/api-gateway/payment_entry_e2e_test.go`：使用本地 Mongo `rs0` 验证下单 → 已验证 IAP 入账 → 重放不二次入账。
- `configs/config.local.yaml.example`：只记录本地开关与受控验证器说明，不填真实商店配置。

### 任务 1：先固定 IAP 的领域契约与渠道隔离

- [x] **步骤 1：编写失败的领域测试**

在 `internal/biz/payments/checkout_test.go` 写入：

```go
func TestSettleVerifiedStorePurchase只接受已验证交易且按冻结商品入账(t *testing.T) {
    // 输入没有 diamondAmount；交易与商品由服务端传入。
}

func TestSettleVerifiedStorePurchase苹果与Google同交易号不互相冲突(t *testing.T) {
    // 两个商店渠道必须是不同的幂等命名空间。
}
```

- [x] **步骤 2：运行测试验证红灯**

运行：

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache go test ./internal/biz/payments -run 'TestSettleVerifiedStorePurchase' -count=1
```

预期：FAIL，提示 `SettleVerifiedStorePurchase` 未定义。

- [x] **步骤 3：最少实现领域用例**

在 `internal/biz/payments/checkout.go` 定义 `StoreProvider`（`apple`、`google`）、`VerifiedStorePurchase` 和 `CheckoutService`；其结算入口只接受验证器产出的交易 ID 与服务器已发布商品，不接受请求金额、钻石、余额或 VIP。

- [x] **步骤 4：运行领域测试验证通过**

运行与步骤 2 相同的命令。

预期：PASS。

### 任务 2：实现本地 IAP 验证器与 HTTP 契约

- [x] **步骤 1：编写失败的传输测试**

在 `internal/transport/localpayment/handler_test.go` 写入：

```go
func TestHandler验证购买拒绝客户端钻石数量并只信任验证器(t *testing.T) {
    // body 中即使有 diamonds: 999999，也不影响服务器商品快照。
}

func TestHandler验证购买同交易重放只返回一次入账(t *testing.T) {
    // 同 provider + transactionId 对应同一交易事实。
}
```

- [x] **步骤 2：运行测试验证红灯**

运行：

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache go test ./internal/transport/localpayment -count=1
```

预期：FAIL，包或处理器不存在。

- [x] **步骤 3：实现受控验证器与传输处理器**

创建 `internal/integrations/appstore/verifier.go`，只暴露 `Verifier` 接口和显式 `LocalVerifier`；创建 `internal/transport/localpayment/handler.go`，精确支持：

```text
POST /api/wallet/create-external-checkout
POST /api/wallet/verify-purchase
```

两条路由都要求 Go 自有会话。首条路由只创建 pending 本地订单，返回本地测试收银台状态而不向真实 PayCores 发请求；第二条路由必须先调用验证器、再按服务端商品版本冻结并事务入账。

- [x] **步骤 4：运行传输测试验证通过**

运行与步骤 2 相同的命令。

预期：PASS。

### 任务 3：Gateway 三重门禁和本机 Mongo 端到端验收

- [x] **步骤 1：编写失败的 Gateway 测试**

在 `internal/gateway/proxy_test.go` 写入：

```go
func TestPaymentEntry只在精确路由和开关开启时接管(t *testing.T) {
    // 无 handler、开关未放行、带 query 或编码 URL 都继续请求 Node。
}
```

在 `cmd/api-gateway/payment_entry_e2e_test.go` 写入：

```go
func TestGateway本地支付与IAP闭环(t *testing.T) {
    // 随机 ID，精确清理：发布商品 → 下单 → 本地验签 → 余额和账本各增加一次。
}
```

- [x] **步骤 2：运行测试验证红灯**

运行：

```bash
GOCACHE=/private/tmp/ai-business-service-go-cache go test ./internal/gateway ./cmd/api-gateway -run 'TestPaymentEntry|TestGateway本地支付与IAP闭环' -count=1
```

预期：FAIL，支付入口未被 Gateway 识别或本地装配不存在。

- [x] **步骤 3：实现三重门禁装配**

Gateway 增加 `PaymentEntryHandler`，但仅在：

```text
GATEWAY_GO_SESSION_AUTH_ENABLED=true
GATEWAY_LOCAL_PAYMENT_ENTRY_ENABLED=true
GATEWAY_LOCAL_IAP_TEST_VERIFIER_ENABLED=true
```

且 route-switch JSON 精确放行该方法与 URL 时才调用它。其余所有情况继续代理 Node；验证器仅接受本地 `local:` 测试回执，任何真实或未知格式都失败关闭。

- [x] **步骤 4：运行本机 Mongo 端到端测试验证通过**

运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./internal/gateway ./cmd/api-gateway -run 'TestPaymentEntry|TestGateway本地支付与IAP闭环' -count=1
```

预期：PASS；测试只按随机 ID `DeleteOne` 清理。

### 任务 4：全量回归

- [x] **步骤 1：格式化与静态检查**

运行：

```bash
gofmt -w internal/biz/payments internal/integrations/appstore internal/transport/localpayment internal/gateway cmd/api-gateway
GOCACHE=/private/tmp/ai-business-service-go-cache go vet ./...
```

- [x] **步骤 2：执行完整本地回归**

运行：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test ./... -count=1

CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
GOCACHE=/private/tmp/ai-business-service-go-cache \
go test -race ./... -count=1

GOCACHE=/private/tmp/ai-business-service-go-cache \
go build -o /private/tmp/ai-business-service-api-gateway ./cmd/api-gateway
```

- [x] **步骤 3：完成审计**

确认代码中不存在真实 PayCores、Apple 或 Google 访问；确认 Node/JS 文件零改动；确认新 Go 注释均为简体中文。
