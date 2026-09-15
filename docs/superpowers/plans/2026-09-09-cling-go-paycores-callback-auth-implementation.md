# Go 本地 PayCores 回调验签实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 `subagent-driven-development` 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 在不启用支付 HTTP 路由的前提下，实现 Node V2 等价的 PayCores 回调验签与 Go 自有 nonce 防重放。

**架构：** `integrations/paycores` 负责无状态验签和受控关联投影，`payments` 只接收不含金额的结算关联，`data` 只持久化本地 nonce。验签成功后由未来的受控 transport 调用既有订单结算；本计划不创建该 transport。

**技术栈：** Go 标准库 HMAC-SHA256、MongoDB 唯一索引和 TTL 索引、Go 单元测试及本地副本集集成测试。

---

### 任务 1：冻结 Node V2 验签与关联投影合同

**文件：**

- 创建：`internal/integrations/paycores/callback.go`
- 创建：`internal/integrations/paycores/callback_test.go`

- [x] **步骤 1：编写失败的验签测试**

编写 Node 现网格式的固定测试向量，断言成功时只返回 `userId`、本地渠道订单号和外部交易号；测试改路径、改 body、过期、无效 nonce 和错误 HMAC 均返回固定领域错误。

- [x] **步骤 2：运行测试验证失败**

运行：`go test ./internal/integrations/paycores -run TestVerifyPaymentConfirmed -count=1`

预期：FAIL，原因是 `NewCallbackVerifier` 未定义。

- [x] **步骤 3：实现最小验签器**

实现 `NewCallbackVerifier(key, now)` 和 `Verify(method, path, headers, rawBody)`：校验 V2 信封、60 秒时间窗、允许路径、HMAC 常量时间比较和受控 JSON 投影。禁止定义 `credits`、价格或余额字段。

- [x] **步骤 4：运行验签测试验证通过**

运行：`go test ./internal/integrations/paycores -run TestVerifyPaymentConfirmed -count=1`

预期：PASS。

### 任务 2：持久化本地 nonce 防重放

**文件：**

- 修改：`internal/data/model/payments.go`
- 修改：`internal/data/schema/collections.go`
- 修改：`internal/data/schema/indexes.go`
- 创建：`internal/data/paycores_callback_nonce_repository.go`
- 创建：`internal/data/paycores_callback_nonce_repository_test.go`

- [x] **步骤 1：编写失败的本地 Mongo 集成测试**

测试成功验签后同一 nonce 的并发消费只成功一次；首次记录只保存 SHA-256 nonce 摘要、路径、方法与过期时间；事务外存储可用，但数据库不可用必须返回错误。

- [x] **步骤 2：运行测试验证失败**

运行：`CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -run TestMongoPayCoresNonce -count=1`

预期：FAIL，原因是 nonce 集合、索引和仓储尚不存在。

- [x] **步骤 3：实现最小 nonce 仓储**

声明 `payment_callback_nonces`，添加 `nonce_hash` 唯一索引和 `expires_at` TTL 索引。实现 `Consume(ctx, nonceHash, method, path, expiresAt)`；重复键映射为支付回调重放领域错误，绝不保存原 nonce 或签名。

- [x] **步骤 4：运行 nonce 测试验证通过**

运行：`CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -run TestMongoPayCoresNonce -count=1`

预期：PASS。

### 任务 3：组装已验证回调用例，但不注册路由

**文件：**

- 创建：`internal/biz/payments/callback.go`
- 创建：`internal/biz/payments/callback_test.go`

- [x] **步骤 1：编写失败的编排测试**

使用内存 nonce store 与验签后的受控关联，断言成功 nonce 才会调用 `SettleVerifiedOrder`；重放、验签失败与 nonce 存储失败不会触发订单结算。

- [x] **步骤 2：运行测试验证失败**

运行：`go test ./internal/biz/payments -run TestConfirmedCallback -count=1`

预期：FAIL，原因是 `ConfirmedCallbackUsecase` 未定义。

- [x] **步骤 3：实现最小编排器**

实现只依赖 `VerifiedPaymentConfirmation`、nonce store 和 `SettleVerifiedOrder` 的用例。它不读取 HTTP 请求、不保存原始 body、不接受金额，也不在 server 或 gateway 注册路由。

- [x] **步骤 4：运行测试验证通过**

运行：`go test ./internal/biz/payments -run TestConfirmedCallback -count=1`

预期：PASS。

### 任务 4：路由门禁与全量回归

**文件：**

- 修改：`internal/walletcontract/contract_test.go`（仅在需要增强未注册断言时）

- [x] **步骤 1：执行受控回归**

运行：`CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/integrations/paycores ./internal/data ./internal/biz/payments ./internal/walletcontract -count=1`

预期：PASS。

- [x] **步骤 2：执行全量验证**

运行：`go vet ./internal/integrations/paycores ./internal/data ./internal/biz/payments && go test ./... -count=1`

预期：PASS，且 Go 支付回调路由保持未注册。
