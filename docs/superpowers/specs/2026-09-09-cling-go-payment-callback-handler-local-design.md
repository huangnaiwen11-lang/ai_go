# Go 本地 PayCores 支付回调 Handler 设计

## 目标

在 Go 服务中增加一个仅供受控装配或 `httptest` 调用的 PayCores 支付回调 HTTP Handler。它把已完成的 V2 验签、防重放和本地冻结订单结算组合为可验证的本地 HTTP 闭环，但不注册到 `server`、Gateway 或任何公开路由。

## 范围与红线

- 只处理 Go 自有的一次性钻石订单；不处理 VIP、游戏订单、退款、收银台创建或商店内购。
- 不读取生产密钥、不访问 PayCores、不修改 Node/JS、现网钱包、现网订单或生产配置。
- 不注册 HTTP 或 gRPC 业务路由，不改变 Gateway 分流；现网请求继续由 Node 处理。
- Handler 只把验签后的 `userId`、PayCores 渠道订单号、渠道交易号和受控 nonce 摘要交给 `payments`；不得向业务层传递原始 Body、签名、原 nonce、金额、钻石、VIP、余额或价格。
- 钱包余额只从 Go 自有账本读写；绝不读取 Node 钱包。

## 组件职责

```text
httptest HTTP 请求
  → paymentcallback.Handler
  → integrations/paycores.CallbackVerifier
  → payments.NewPaymentCallbackNonceHash
  → payments.NewVerifiedPaymentConfirmation
  → payments.ConfirmedCallbackUsecase
  → Go 本地 payment_orders / payment_receipts / accounts / ledger_entries
```

`internal/transport/paymentcallback` 只负责 HTTP 边界：精确路径、Body 上限、验签调用、领域对象映射和固定响应。它不拥有余额、订单、nonce 或入账规则。

`payments.ConfirmedCallbackUsecase` 保持既有顺序：先消费 nonce，再按渠道订单号定位本地订单，最后调用 `SettleVerifiedOrder`。本地订单 ID 与 PayCores 渠道订单号可以不同，金额和钻石数量只从本地冻结订单读取。

## HTTP 合同

仅接受以下精确路径：

- `POST /api/internal/payment-confirmed`
- `POST /api/v1/internal/payment-confirmed`

路径必须没有编码差异、重复斜杠或查询参数。请求 Body 上限为 1 MB。Handler 使用既有 Node V2 验签合同，不重新实现 HMAC 规则。

成功响应保持现网 Node 的响应外形：

```json
{
  "success": true,
  "data": {
    "newBalance": 120,
    "duplicate": false
  }
}
```

`newBalance` 是 Go 自有账户余额；`duplicate` 反映既有本地支付回执是否已入账。该 Handler 未注册，因此该响应目前只作为本地合同测试，不构成对外切流。

## 错误映射

| 条件 | HTTP 状态 | 固定响应含义 |
| --- | --- | --- |
| 非 POST、非法路径或查询参数 | 400、404 或 405 | 非法内部请求 |
| Body 超过 1 MB | 413 | 非法内部请求 |
| V2 信封、时间窗、签名或 Body 合同无效 | 401 | 回调认证失败 |
| nonce 已消费 | 409 | 回放请求 |
| nonce 存储不可用 | 503 | 认证/防重放不可用 |
| 订单关联、结算或未知内部错误 | 500 | 内部处理失败 |

不得把密钥、签名、原 nonce、订单金额或内部错误文本写入响应或日志。

## 测试与验收

- 使用测试注入的 HMAC 密钥、受控时钟和 `httptest`；不读取真实环境变量。
- 验证两条路径的成功链路，以及方法、编码路径、查询参数、超大 Body、缺失/篡改签名和过期时间戳。
- 使用本机 `cling_main` / `rs0` 验证成功结算只入账一次，回放返回 409，且本地订单 ID 与渠道订单号不同时仍可正确结算。
- 验证 nonce 存储错误不会结算；订单定位或结算错误不能泄露敏感字段。
- `internal/walletcontract` 必须继续证明 Go 支付路由未注册。

## 后续边界

本设计完成不代表允许开启支付路由。未来真实接入仍需要独立审批并完成：生产密钥受控注入、PayCores 回调目标切换、Gateway/Server 路由登记、Node 回退策略、线上回放验收、监控与回滚方案。
