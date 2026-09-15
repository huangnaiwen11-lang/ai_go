# Cling Go 本地生成回调与终态编排设计

> 状态：已确认，待实施。本设计只在本地 `cling_main` / `rs0` 中实现等价的回调处理能力；不修改 Node/JS、PayCores、生成中台、现网钱包、生产配置，也不注册真实 HTTP 路由或启动后台投递循环。

## 1. 目标与业务语义

本阶段承接已完成的「创建、预扣、首次提交与 Outbox」本地闭环，接收生成中台的 V2 终态回调，并让创作安全地收敛。

- 回调合同以现有 Node 生成中台的 `generation-callback-v2` 为唯一基线：只接受现有两个 V2 `POST` 路径（`/api/v1/internal/generation-callback`、`/api/internal/generation-callback`）、版本 `2`、原始 Body HMAC、时间戳和 nonce。
- `execution.completed` 只能推进对应的已提交步骤；`execution.failed` 与 `execution.cancelled` 都只能使对应预留走一次技术失败冲正。
- 文生视频必须先完成 `text_to_image` 首帧，再提交 `image_to_video`。第二步的模型、提示词和参数在创建时冻结，不能因模板更新而变化。
- 审核没收不属于生成中台当前 V2 回调合同。本阶段不伪造审核事件；未来独立审核入口只能调用 `ledger.Confiscate`，不得退款。

## 2. 固定范围

### 包含

- 可由 `httptest` 直接调用的本地回调 `http.Handler`，但不在 `main`、`server` 或 Gateway 注册路由。
- V2 原始报文验签、时间窗口校验和 nonce 幂等回执。
- 回调终态的 MongoDB CAS、结果资产记录、技术失败冲正。
- 文生视频首帧完成后，原子激活第二步骤并创建第二个 Outbox 事件。

### 不包含

- 真实 callback Origin 切换、Nginx、部署、生产密钥或任何真实中台请求。
- 回调进度事件、轮询循环、用户查询 API、媒体下载或转存。
- 支付入账、每日赠钻、审核入口或审核规则。
- Animate，以及 `text_to_image`、`image_edit`、`image_to_video` 之外的原子。

## 3. 模块边界

```text
transport/generationcallback
  └─ 保留原始 Body，做 HTTP 映射；不认识账本与 MongoDB

integrations/generation
  └─ 校验现有 V2 回调报文、HMAC、时间戳、nonce 格式和受控终态字段

biz/generation
  └─ 编排回调回执、步骤 CAS、结果资产、延迟配方、Outbox 与技术失败语义

ledger
  └─ 只通过事务内 ReverseInTx 处理技术失败退款

data
  └─ 在同一 MongoDB Transaction 中实现回执、步骤、创作、资产、配方和 Outbox 写入
```

路由层只将原始 HTTP 请求交给 Handler。验签通过后，业务用例只接收受控的 `CallbackEvent`；它永远不会接触签名、密钥、完整原始报文或任意中台错误文本。

## 4. 回调安全合同

### 4.1 请求与签名

沿用现有 Node 合同：

- HTTP 方法固定为 `POST`，路径只允许现有的 `/api/v1/internal/generation-callback` 或 `/api/internal/generation-callback`，不接受查询参数；签名必须使用本次请求的精确允许路径。
- 必需 Headers：`X-Generation-Callback-Version`、`X-Generation-Callback-Timestamp`、`X-Generation-Callback-Nonce`、`X-Generation-Callback-Signature`。
- 签名载荷固定为 `generation-callback-v2`、时间戳、`POST`、固定路径、nonce 与原始 Body 的 SHA-256，以换行拼接后使用 `generation_callback_hmac_key` 计算 HMAC-SHA256。
- nonce 只允许现有格式 `[A-Za-z0-9._:-]{16,128}`；时间戳必须在受控的 5 分钟窗口内；签名使用常量时间比较。

验签、时间窗口或 JSON 合同失败时直接拒绝，不写 `callback_receipts`、不更新任务、不冲正。错误响应只使用固定类别，不能回显 URL、提示词、nonce、签名、密钥或中台错误消息。

### 4.2 受控事件

只接受现有 V2 回调的下列字段：`contractVersion`、`eventId`、`deliveryId`、`fingerprint`、`eventType`、`jobId`、`externalRef`、`status`、`outputs`、`error`、`usage`、`timestamps`、`executionRef`、`metadata`；未知字段、重复键、非对象 JSON 和超限 Body 都拒绝。

- `eventType=execution.completed` 与 `status=completed` 必须成对出现，并且只允许一个 `outputs` 项，角色为 `result`，媒体类型与步骤原子一致。
- `eventType=execution.failed` 与 `status=failed` 必须成对出现；`eventType=execution.cancelled` 与 `status=cancelled` 也必须成对出现。失败和取消均不得携带角色为 `result` 的输出；`error` 仅用于确认技术终态，不持久化其中的 code 或 message。
- `externalRef` 必须等于本服务的步骤 ID，`jobId` 必须与步骤已保存的 `external_execution_id` 完全一致，`executionRef.capability` 必须与步骤原子一致。

## 5. 回执、状态与幂等

`callback_receipts` 继续使用现有唯一键 `source + nonce_hash`。`source` 固定为 `generation.execution.v2`，`nonce_hash` 为 nonce 的 SHA-256，`payload_digest` 为原始 Body 的 SHA-256。

在同一个 MongoDB Transaction 内：先插入回执，再以 `callback_version`、步骤 ID、job ID 和当前状态执行条件更新，最后写入结果或后续 Outbox 事件。重复 nonce 命中唯一键时返回幂等确认，不重放任何业务写入；同一步骤的不同 nonce 乱序到达时，CAS 只允许首个合法终态胜出。

创作新增终态 `succeeded`、`generation_failed`；步骤新增终态 `succeeded`、`generation_failed`。技术失败或取消与提交失败不同：它们证明中台已经受理但未产生结果，仍必须立即冲正一次。

## 6. 终态编排

| 回调 | 前置状态 | 同事务动作 | 结算 |
| --- | --- | --- | --- |
| 图片步骤完成 | `submitted` | 写入 `assets` 的受控结果引用，步骤与创作改为 `succeeded` | 保持预留，不退款 |
| 文生视频首帧完成 | 第一步骤 `submitted`，第二步骤 `blocked` | 写入首帧资产；由冻结配方生成第二步快照；第二步骤改为 `ready`；写入稳定的第二步 Outbox 事件 | 保持预留，不退款 |
| I2V 完成 | 第二步骤 `submitted` | 写入视频资产，步骤与创作改为 `succeeded` | 保持预留，不退款 |
| 技术失败 | 对应步骤 `submitted` | 步骤与创作改为 `generation_failed`，`ledger.ReverseInTx`，记录回执 | 立即且仅一次冲正 |
| 技术取消 | 对应步骤 `submitted` | 与技术失败使用相同的步骤/创作失败状态、`ledger.ReverseInTx` 与回执路径 | 立即且仅一次冲正 |

结果 URL 必须是无用户信息的 HTTPS URL；为兼容现网中台签名资源地址，允许查询参数和片段。它仅作为 `assets.storage_key` 的受控外部资源标识，Handler 不下载、不转存，也不把 URL 记录到日志或错误响应。

## 7. 文生视频的冻结第二步配方

创建文生视频时，服务端模板编译器除首步骤快照外，还必须在同一创建与预扣事务中写入一个仅供内部使用的延迟配方。该配方单独保存于 `generation_step_recipes`，不能写进 `creations` 或 `creation_steps`：

- 主键为第二步骤 ID，唯一绑定创作 ID、`image_to_video` capability、模型 SKU、冻结的提示词和参数模板、创建时间与摘要。
- 配方不含用户 ID、模板 ID、余额、VIP、额度、价格、支付、账本、风控或工作流字段。
- 首帧回调只可把通过 URL 校验的首帧作为 `opening_frame` 素材注入该配方；随后必须重新调用 `executionv2.Compile`，得到完整、可提交且不可变的第二步骤快照。
- 同一事务以 `generation.submission:<second_step_id>` 创建 `pending` Outbox 事件；重复回调因回执唯一键和步骤 CAS 不会创建第二条事件。

这保证第二步的模板语义在创建时冻结，同时避免把尚未知晓的首帧 URL 伪造为创建时的完整 `image_to_video` 请求。

## 8. 验收与测试

所有测试只使用 `httptest` 与本地 `cling_main` / `rs0`。MongoDB 清理仅对随机精确 `_id` 使用 `DeleteOne`。

- Handler：正确签名、过期时间、错误 nonce、错误路径、错误签名、重复键与敏感字段均被安全处理。
- 回执：同 nonce 重放、不同 nonce 的相同终态、乱序完成/失败都不重复资产、Outbox 或退款。
- 终态：图片完成、I2V 完成、技术失败冲正、已冲正/已没收冲突均符合状态机。
- 文生视频：创建时冻结配方；首帧完成只创建一条第二步事件；第二步请求含首帧、保持冻结参数；完成后才收敛创作成功。
- 回归：`go test ./...`、`go test -race ./...`、`go vet ./...`、构建和现有语义契约均通过。

## 9. 上线前门槛

本设计完成仅表示本地语义闭环。真实接入前仍需独立审批并完成：回调 Origin 切换、真实密钥配置、生成中台信任配置、路由注册、部署、监控与线上回放验收。上述事项均不在本设计或后续本地实施计划中执行。
