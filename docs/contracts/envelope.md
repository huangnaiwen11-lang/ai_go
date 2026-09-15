# API 契约冻结记录

## HTTP

- 入口：`/api`；内部 Go 服务不得直接暴露给浏览器。
- 成功响应：保持现有 `{ "success": true, "data": ... }` 结构及原字段类型、默认值、分页和排序语义。
- 错误响应：保持 `code`、`message`、`details`、`requestId`；HTTP 状态码和可重试性按 Node 基线执行。
- 请求上下文：透传 Bearer Token、`X-Request-Id`、`X-Language`、`X-Device-Id`。

## SSE

- 路径和事件名保持现有前端使用方式。
- 正常链路的目标顺序是先发送权威 `snapshot`，再发送 `change`；前端以新的 snapshot 作为断线、页面恢复和后台挂起后的权威收敛依据。
- 当前 Node 实现会先登记事件订阅、再异步获取首个 snapshot。因此在刚建连的竞争窗口中，事件可先触发 `change`；现有实现没有 snapshot-first barrier。该窗口尚未完成运行时基线采样，Realtime Service 不得因本文件而假定“首帧必为 snapshot”。若要消除该竞态，必须作为独立的产品/语义变更先冻结并验收，不能借迁移顺带改变。
- Redis Pub/Sub 只做实时通知；断线时必须能从持久化事实重建 snapshot。

## 语义门禁

字段兼容但业务结果、权限、状态迁移、扣费/退款、副作用或幂等行为不同，均视为契约不兼容。
