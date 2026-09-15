# Go 本地 PayCores 回调验签设计

## 目标

在 Go 服务中实现与 Node 现网一致的 PayCores V2 回调验签与 nonce 防重放基础能力，为已冻结的本地钻石订单提供受控的已验证关联信号。

## 边界

- 仅处理一次性钻石订单，不处理 VIP 订阅、游戏订单、退款或支付渠道创建收银台。
- 不注册 HTTP 路由，不读取生产密钥，不调用 PayCores，也不修改 Node 支付业务。
- 通过验签后的结果只保留本地结算所需的订单关联；不把 `credits`、余额、VIP、价格、原始签名或请求体送入 payments 领域。
- nonce 使用 Go 自有的 `payment_callback_nonces` 集合，不能复用生成中台的 callback receipt，也不能接触 Node 的 `PaycoresCallbackNonce`。

## 兼容合同

Node V2 签名载荷固定为：

```text
METHOD\nPATH\nTIMESTAMP\nNONCE\nSHA256(JSON.stringify(body))
```

- 仅允许 `POST /api/internal/payment-confirmed` 与 `/api/v1/internal/payment-confirmed`。
- 读取 `x-timestamp`、`x-request-nonce`、`x-signature-v2`，时间窗为 60 秒。
- nonce 格式为 16 至 128 位十六进制或连字符；同一 nonce 只能消费一次，保留 2 分钟。
- 签名无效、过期或字段不合法时不得写 nonce；签名有效但 nonce 已使用时拒绝；nonce 库不可用时失败关闭。

## 组件与数据流

```text
HTTP 原始输入（尚不注册路由）
  → PayCores V2 验签器
  → Go 自有 nonce 仓储（唯一写入）
  → VerifiedPaymentConfirmation（无金额）
  → SettleVerifiedOrder（既有本地订单结算）
```

验签器只负责密码学合同和最小关联字段校验。订单存在性、用户/渠道/渠道订单号一致性、冻结钻石数与入账事务，继续由 `payments.Service.SettleVerifiedOrder` 负责。

## JSON 兼容策略

Go 不能把 `map[string]any` 的默认序列化当作 Node `JSON.stringify` 的等价物。实现必须以 Node 测试向量固定“解析后 JSON.stringify”的体摘要，并只接受 Go 本地一次性钻石回调的受控字段集合及嵌套层级。任何无法无歧义复现 Node 序列化的输入一律拒绝，不能退化为签原始请求字节。

## 验收

- Node 生成的固定 V2 签名向量可被 Go 接受；改路径、改 body、过期、错误签名、缺少 V2 信封均拒绝。
- 成功验签仅输出本地订单结算关联，不输出或接受金额与 `credits`。
- nonce 唯一性和 TTL 索引由本地 schema 声明；并发相同 nonce 仅一个成功。
- 未注册任何支付 HTTP 路由，walletcontract 路由门禁仍通过。
