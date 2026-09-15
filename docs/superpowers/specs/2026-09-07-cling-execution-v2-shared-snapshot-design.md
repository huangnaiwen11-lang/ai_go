# Cling `execution.v2` 共享技术快照设计

> 状态：本地实现已完成。本设计用于修复创作预扣链路与生成客户端对技术快照规则漂移的问题；本阶段不增加业务路由、后台投递或生产配置。

## 问题

当前 `creations.InitialSubmission` 和 `integrations/generation.BuildExecution` 分别校验模型 SKU、素材、参数和 JSON。两套规则已经发生偏差：创作事务可能完成占位与预扣，并把后续生成客户端必定拒绝的快照写入 Outbox。

技术快照还会被重复解析，且创作侧在大小限制生效前已解析原始 JSON。重复键、业务字段和不合规素材必须在预扣之前收敛。

## 目标

- 以一份纯 Go、版本化的 `execution.v2` 技术合同校验创作和生成客户端。
- 在创作事务开始前一次性校验并冻结首步骤快照；指纹与 Outbox 复用同一份冻结字节。
- 让成功写入 Outbox 的快照必定能通过生成客户端的 `BuildExecution` 校验。
- 保持现有三原子：`text_to_image`、`image_edit`、`image_to_video`；不引入 Animate。

## 模块边界

新增 `internal/executionv2`，只依赖标准库。它不认识用户、模板、价格、钻石、VIP、账本、MongoDB、HTTP、HMAC 或 Outbox 状态。

```text
creations
  └─ executionv2.Compile：在预扣前得到冻结 Snapshot
       ├─ request fingerprint 使用 Snapshot.Digest
       └─ outbox payload 使用 Snapshot.MarshalSubmissionPayload

integrations/generation
  └─ executionv2.Compile：在 BuildExecution 前验证同一 SKU、原子与输入规则

outbox
  └─ 保存 Snapshot 生成的原始 JSON 字节，不解析业务含义
```

`internal/generationcontract` 继续保存迁移准入证据；它与底层 `execution.v2` 技术合同职责不同，保持不变。

## 共享合同

`executionv2.Compile(capability, modelSKU, rawInput)` 返回不可变 `Snapshot`。它按以下顺序处理：

1. 先限制输入字节数，再以 token 解析器检查完整 JSON、深度、节点数和所有对象层级的重复键。
2. 只接受输入对象的 `prompt`、`negativePrompt`、`assets`、`parameters`。`negativePrompt` 可省略，冻结后的类型值为空字符串。
3. 以 capability 校验精确模型 SKU、非空 prompt、素材角色、HTTPS 素材地址、素材数量和视频首帧约束。
4. 使用一套技术参数白名单和标量/结构类型规则；未知键、产品、账户、支付、额度、账本、风控和工作流语义均因不在白名单而拒绝。
5. 复制原始输入字节，计算包含合同版本、capability、SKU 与输入字节的摘要；不把提示词或素材 URL 放入错误文本。

`Snapshot.MarshalSubmissionPayload` 只输出 `{ "capability", "model_sku", "input" }`。这三个字段都是生成中台所需的技术事实；工作者必须用同一共享合同解析它们，再结合稳定的步骤 ID 构造 `execution.v2` 请求。它验证最终 payload 仍不超过 Outbox 的 1 MiB 限制，并返回独立字节副本。

## 创作事务

`CreateReserved` 在校验计划后定位第一个 `ready` 步骤，并对 `InitialSubmission` 调用一次 `executionv2.Compile`。它用冻结快照完成请求指纹和 Outbox payload，再进入已有 MongoDB 事务。

同一幂等键的不同 capability、SKU 或输入摘要返回冲突。文生视频只编译并入队首个 `text_to_image` 步骤；后续 `image_to_video` 仍为 `blocked`。模板图编辑使用步骤已决定的 `image_edit` capability。

## 客户端适配

生成客户端保留既有公开 Go 类型与 HTTP 签名行为，但将 capability、SKU、输入、素材和参数验证委托给 `executionv2`。`BuildExecution` 从冻结快照构建请求体；最终请求 JSON 的字段白名单、相同字节校验、签名和发送边界保持不变。

## 错误与资源边界

共享包只返回固定哨兵错误，不回显模型、提示词、素材 URL、参数名或原始 JSON。输入在任何 `json.Unmarshal` 前受字节限制；token 扫描器还限制 JSON 深度和节点数，防止异常快照耗尽内存或栈空间。

## 验收

- 对每个三原子，合法共享快照都能被 `BuildExecution` 接受。
- 非法 SKU、空 prompt、错误素材、HTTP URL、未知参数、业务字段、重复键、超限字节/深度/节点均在预扣前拒绝，且没有 Outbox 事件。
- 相同冻结快照的指纹与 Outbox payload 稳定；不同快照不能重放。
- Task 1 合同测试、Task 2 Outbox Mongo 测试、Task 3 创作 Mongo 回滚与 I2I/T2V 测试均通过。
