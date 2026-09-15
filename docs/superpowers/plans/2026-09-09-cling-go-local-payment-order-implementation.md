# Go 本地支付订单冻结实现计划

**目标：** 实现 Go 自有商品版本、订单冻结和已验证订单结算核心，不接入支付 HTTP 路由。

**架构：** `payments` 领域负责商品、订单与冻结结算；`data` 负责 Mongo 条件写入和事务。结算从本地订单取得钻石数，并复用已有支付回执入账逻辑。

### 任务 1：商品与订单领域模型

- 创建 `internal/biz/payments/order.go`：定义商品版本、订单状态、创建命令和已验证结算命令。
- 测试 `internal/biz/payments/order_test.go`：未发布商品拒绝；创建订单冻结钻石数。
- 先运行 `go test ./internal/biz/payments -run TestCreateOrder -count=1` 确认红灯，再实现最小模型与服务。

### 任务 2：冻结订单结算

- 修改 `internal/biz/payments/service.go`：增加事务内入账方法和按订单结算方法。
- 测试 `internal/biz/payments/order_test.go`：商品更新后旧订单按冻结数入账；信号的用户或渠道不匹配时拒绝。
- 先运行定向测试确认红灯，再实现事务内订单状态迁移与回执入账。

### 任务 3：Mongo 持久化

- 修改 `internal/data/model/payments.go`、`internal/data/schema/collections.go`、`internal/data/schema/indexes.go`。
- 修改 `internal/data/payment_repository.go`：实现商品、订单、条件完成状态和事务内结算写入。
- 测试 `internal/data/payment_order_repository_test.go`：本机副本集验证商品版本冻结和结算原子回滚。

### 任务 4：回归与路由门禁

- 运行 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data ./internal/biz/payments ./internal/walletcontract -count=1`。
- 运行 `go vet ./internal/data ./internal/biz/payments && go test ./... -count=1`。
- 保持 `walletcontract` 的 Go 支付路由关闭断言通过。
