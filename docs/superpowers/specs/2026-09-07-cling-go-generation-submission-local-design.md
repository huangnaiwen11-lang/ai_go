# Cling Go 本地生成提交与 Outbox 设计

> 状态：待施工。本设计只覆盖本地「任务已占位且已预扣」之后的首次技术提交。生成回调、终态结算、支付入账、HTTP 业务路由、生产配置和流量切换均不在本阶段实施。

## 1. 目标与边界

在独立本地 MongoDB 数据库 `cling_main` 中，为已创建的首个 `ready` 步骤写入可持久重试的 Outbox 事件，并通过 `httptest` 模拟的生成中台提交 `execution.v2` 技术请求。

本阶段以已确认的主站业务基线和现网只读合同为准：

- 中台只有 `text_to_image`、`image_edit`、`image_to_video` 三个原子；不新增 Animate 入口或兼容分支。
- 文生视频仅投递首个 `text_to_image` 步骤；其 `image_to_video` 后续步骤保持 `blocked`，必须由下一阶段的成功回调持久化首帧后再激活。
- 生成中台请求使用现网 `execution.v2` 合同，目标路径为 `POST /api/v2/executions`；请求和查询均使用稳定的步骤级幂等键。
- 生成中台请求不得出现用户 ID、模板 ID、钻石、余额、VIP、每日额度、价格、支付、账本、风控、工作流内部标识或旧版 `callbackUrl` 字段。
- 主站回调地址由本服务配置的独立回调基地址加固定路径组成；本地测试只使用 `httptest` 地址，绝不访问真实中台或默认的 `cling-ai.com` 地址。
- Node/JS、PayCores、生成中台、现网钱包、生产配置与对外 HTTP/gRPC 业务路由均不修改。

## 2. 现状与必须补齐的事实

当前 `creations.Usecase.CreateReserved` 已在同一 MongoDB 事务中写入创作、步骤、日免或账户预扣、预留和账本分录。它只保存 `InputDigest`，故意不把提示词或素材 URL 写入 `creations` 文档。

这意味着工作者不能从创作索引重建技术请求。为保证异步投递可重试，又不扩大创作索引的敏感字段，本阶段把经模板编译后的最小技术请求快照仅写入 Outbox 的 `payload`。该快照是提交所必需的技术数据，不包含产品计费和用户商业事实。

每一个可提交的首步骤对应一个稳定事件 ID：`generation.submission:<step_id>`。MongoDB 的 `_id` 唯一性和创作总事务共同保证同一预留事实最多产生一个首次提交事件。

## 3. 模块与数据职责

```text
creations
  └─ 在占位与预扣事务中生成首次提交技术快照，调用 outbox.Writer 入队

outbox
  └─ 定义持久事件、领取租约和投递状态；不理解模板、价格或中台协议

generation
  └─ 定义 execution.v2 请求、响应分类与步骤提交状态；不读取余额、VIP 或账本

worker
  └─ 领取事件、调用受控生成客户端；明确失败时在同一事务内更新任务、冲正预留并结案事件

ledger
  └─ 提供事务内 ReverseInTx；仍独占余额、日免、预留与账本分录
```

`data` 是上述反转接口的唯一 MongoDB 实现。它可以在同一 Session Transaction 内操作 `creations`、`creation_steps`、`reservations`、`ledger_entries` 与 `outbox_events`，但业务层不导入 MongoDB Client 或集合。

## 4. Outbox 与状态机

Outbox 事件拥有以下最小状态：

| 状态 | 含义 | 可达后续状态 |
| --- | --- | --- |
| `pending` | 已和占位、预扣一起提交，尚未被工作者领取。 | `dispatching` |
| `dispatching` | 某个工作者已取得带过期时间的租约。 | `delivered`、`reconciling`、`failed`、`pending` |
| `reconciling` | POST 结果未知，必须先用原幂等键查询中台。 | `delivered`、`failed`、`pending` |
| `delivered` | 中台已接受同一技术任务；等待后续回调阶段处理终态。 | 无 |
| `failed` | 已确认中台未受理，主站任务已标记提交失败且预留已冲正。 | 无 |

同一时刻只有持有未过期租约的工作者可以处理事件。领取、重新排队、结案和失败处理全部使用条件更新；不得先读取事件状态再无条件写入。

步骤新增内部提交状态 `dispatching`、`submitted`、`reconciling`、`submission_failed`。创作新增 `submission_failed` 状态。它们只描述首次提交的已知事实；技术成功、技术失败、取消、审核没收和媒体归属继续由回调阶段定义。

## 5. 生成中台 V2 技术合同

请求体严格使用共享 `generation-execution-v2.json` 的允许字段：

```json
{
  "contractVersion": "execution.v2",
  "idempotencyKey": "cling-step:<step_id>",
  "externalRef": "<step_id>",
  "capability": "text_to_image",
  "modelSku": "ps-image-v1",
  "input": {
    "assets": [],
    "prompt": "...",
    "parameters": {}
  },
  "delivery": { "callback": "tenant", "resultUrlPolicy": "permanent" },
  "priorityClass": "standard",
  "metadata": { "site": "cling-go-main" }
}
```

客户端序列化后使用现网方向性 V2 请求签名：`X-Service-Id=main-backend`、`X-Timestamp`、`X-Request-Nonce`、`X-Content-SHA256` 与 `X-Signature-V2`。签名内容固定为请求版本、源服务、目标服务、方法、精确目标路径、时间戳、nonce 与原始 Body SHA-256。出站密钥与未来入站回调密钥分离；配置文件只保留本地无效占位值，测试直接注入随机测试密钥。

收到 `201`、`200` 或经过查询确认的执行记录后，主站写入中台 `jobId` 到 `creation_steps.external_execution_id`，步骤迁移为 `submitted`，Outbox 迁移为 `delivered`。同一步骤的稳定幂等键不得改变。

## 6. 明确失败、未知结果与冲正

| 结果分类 | 主站动作 | 是否立即冲正 |
| --- | --- | --- |
| 本地构建或签名失败，尚未发送请求 | 标记提交失败。 | 是 |
| 中台明确拒绝且能证明未创建执行任务 | 标记提交失败。 | 是 |
| 请求超时、连接在发送后中断、`5xx`、重放冲突、响应格式不完整 | 写入 `reconciling`，使用原幂等键查询。 | 否 |
| 查询确认已受理 | 保存执行 ID，标记 `submitted`。 | 否 |
| 查询确认未受理 | 标记提交失败。 | 是 |

明确失败的事务顺序固定为：条件迁移步骤和创作状态 → `ledger.ReverseInTx` → 将 Outbox 标记为 `failed`。任何一步失败均回滚。`ReverseInTx` 只允许 `reserved` 预留迁移为 `reversed`；并发路径若已使预留收敛，不得再次退款。未来回调与本工作者的竞争由相同的状态条件和预留状态机收敛。

本阶段不把未知结果误判为失败，也不创建第二个技术任务。无论提交尝试次数多少，只能复用同一个 `step_id`、`externalRef` 与 `idempotencyKey`。

## 7. 配置与安全

`Integrations.Generation.CallbackBaseURL` 的含义调整为主站回调 Origin，而不是完整回调路径。工作者以固定路径 `/api/v1/internal/generation-callback` 生成回调 URL；本地测试可使用临时 HTTP Origin，真实接入前必须由独立受控配置把生成中台信任 Origin 切换到 Go 主站，且不在本阶段执行该操作。

配置新增独立的 `generation_request_hmac_key` 与 `generation_callback_hmac_key`。本阶段只使用前者签名出站请求；后者只完成结构和校验，为下一阶段的入站验签预留，不能用一个通用回调密钥替代两个方向。

日志只记录事件 ID、创作 ID、步骤 ID、状态、HTTP 分类和请求摘要。不得记录提示词、素材 URL、完整 JSON、签名、nonce 或密钥。

## 8. 验收与后续衔接

本阶段必须通过以下本地测试：

- 占位、预扣与首步骤 Outbox 事件同事务提交；预留失败时事件不存在。
- V2 请求只出现允许字段；任何商业字段或未知原子均在发送前被拒绝。
- 两个并发工作者只能领取一次事件；同一事件重放只产生一个外部执行 ID。
- 明确拒绝立即冲正一次；未知结果先查询，不创建第二个请求；查询确认未受理后才冲正。
- 所有 MongoDB 集成测试只连接本地 `cling_main` / `rs0`，并按随机精确 ID 使用 `DeleteOne` 清理。

完成后，下一份设计才定义入站回调的 V2 HMAC、nonce 回执、终态 CAS、首帧资产、第二步骤激活、技术失败/取消冲正以及审核没收不退款。支付入账继续保持独立。
