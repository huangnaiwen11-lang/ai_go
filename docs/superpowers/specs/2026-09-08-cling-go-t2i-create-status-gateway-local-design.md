# Cling Go 首个公开 T2I 创建与轮询 Gateway 本地接入设计

> 状态：已确认，待编写实施计划。本设计只定义首个「自由文生图」平移切片的本地边界；不启动真实 Worker，不调用真实生成中台，不切生产流量。

## 1. 目标与不变量

将一个受控的自由文生图（T2I）切片平移到 Go，同时让既有前端仍能通过创建响应和图片轮询接口获知任务结果。

本设计必须保持以下不变量：

- 用户只使用产品入口，不选择 `text_to_image`、`image_edit`、`image_to_video` 等中台原子。
- Go 只接管由内部模板 `t2i-freeform` 编译出的单步骤 T2I；不新增 Animate，不接管 I2I，也不接管文生视频。
- 游客未绑定返回 `403 ACCOUNT_BINDING_REQUIRED`；需 VIP 返回 `403 VIP_REQUIRED`；无日免且钻石不足返回 `402 INSUFFICIENT_FUNDS`；会话无效返回 `401 UNAUTHORIZED`。这些错误不得合并为「不能生成」。
- 创建始终先写创作占位、预留与 Outbox，再异步提交中台。确定未受理时立即冲正；中台技术失败或取消时由回调冲正；审核没收不退款。
- 中台请求只包含受控技术快照与本服务回调地址，绝不包含钻石、VIP、余额、账本、支付或 Node 钱包事实。
- Go 自有会话、Go 用户库、Go 回调和 Go 账本保持独立；不读取或改写 Node 登录态、Node/JS、前端、PayCores、现网钱包和生产配置。

## 2. 已核实的现网兼容合同

前端图片轮询的实际路径是：

```text
POST /api/chat/image/async
  -> 响应 imageId
  -> POST /api/images/statuses
  -> 若批量状态接口不支持，再 GET /api/images/:id
```

现网 Node 的 `POST /api/images/statuses` 只接受 `imageIds`，每次最多 20 个，且每个 ID 必须是 24 位 MongoDB ObjectId。Go `creations._id` 是 UUID，不能通过该 Node 校验。因此，只接管创建而不接管下面两条状态读取路径，会导致前端无法轮询 Go 任务。

Go 创建响应保持 Node 成功 envelope 的字段形状：

```json
{
  "success": true,
  "data": {
    "imageId": "<Go creation UUID>",
    "jobId": "<Go first-step UUID>",
    "status": "generating",
    "cost": 20,
    "coinsCharged": 0,
    "creditsUsed": {
      "imageCreditsUsed": 0,
      "videoCreditsUsed": 0
    },
    "coinsRemaining": 0,
    "billing": {}
  }
}
```

其中 `cost` 固定表示图片标价 20 钻；`coinsCharged` 取本次预留实际扣减的钻石，日免时为 0；`coinsRemaining` 取 Go 自有账户的事务后余额；`billing` 由本次 Go 预留来源投影，不使用 Node 钱包数据。`jobId` 是第一步骤的稳定内部任务标识，不等同于尚未产生的中台执行 ID。

状态读取保持现网图片状态字段：

```json
{
  "id": "<Go creation UUID>",
  "imageUrl": "https://...",
  "generationStatus": "generating | completed | failed",
  "generationErrorCode": "...",
  "generationErrorMessage": "...",
  "generationErrorDetail": null
}
```

成功图片地址只能从已验证回调写入的结果资产读取：`assets(owner_type=creation_step, owner_id=<final step>, asset_kind=result, status=available)`。不能从请求、模板参数或中台回调未验证字段读取 URL。

## 3. 路由归属与精确匹配

Gateway 只考虑下列 3 个精确路由：

| 路由 | Go 接管条件 | 其他请求 |
| --- | --- | --- |
| `POST /api/chat/image/async` | 已开启公开 T2I 路由开关、有效 Go 自有会话、路径无编码差异且请求符合第 4 节。 | 原样代理 Node。 |
| `POST /api/images/statuses` | 已开启开关、有效 Go 自有会话、Body 合法且全部为 Go UUID。 | 原样代理 Node。 |
| `GET /api/images/:id` | 已开启开关、有效 Go 自有会话、`id` 为规范 Go UUID。 | 原样代理 Node。 |

精确路由必须同时满足：HTTP method 一致、`URL.Path` 与 `URL.EscapedPath()` 均等于固定字面量、没有重复斜杠、没有路径编码、没有裸 query 或 query 参数。相似路径、版本别名、尾部斜杠、编码路径、错误 method 和任何未列入表中的路由都必须继续代理 Node。

公开 T2I 路由开关独立于 `GATEWAY_GO_SESSION_AUTH_ENABLED`：

- 会话开关关闭时，不装配 Go 认证器，3 条路由全部代理 Node。
- 会话开关开启但公开 T2I 路由开关关闭时，仍全部代理 Node。
- 两个开关均开启后，只有先满足精确路由与请求分类的请求才验证 Go 会话并进入本地处理。

本地 Handler 一旦开始执行，任何验证、门禁、创建、读取或内部故障都必须直接按 Go 合同返回；不得把同一请求重放给 Node，避免重复预扣或双任务。

## 4. 创建请求分类与兼容边界

`POST /api/chat/image/async` 仅在下列条件全部成立时视为 Go T2I 候选：

1. `inputImages` **字段完全缺失**。`inputImages: []`、`null`、非数组、带图片的数组均不属于本切片，全部代理 Node。
2. `operation` 缺失或为 `generate`。
3. `provider`、`templateId`、`templateTitle` 缺失；`presetTags` 缺失或为空数组；`optimizePrompt` 缺失或为 `false`。这些字段一旦携带非默认语义，全部代理 Node，不能被 Go 静默丢弃。
4. `prompt`、`negativePrompt`、`aspectRatio`、`width`、`height` 满足冻结的兼容 DTO。`messageId` 与 `agentId` 不参与 Go 幂等或模板选择，携带时代理 Node，避免误改变聊天关联语义。
5. 请求不含未知 JSON 字段、重复 JSON 键或无法完整解码的 Body。

Go DTO 会保留现网长度、比例和尺寸上限。用户可输入提示词、负向提示词和允许的画幅；它们是产品输入，不是技术原子选择。客户端不能传模型、LoRA、工作流、回调地址、费用、VIP、余额、审核结论或任意技术参数。

仅凭请求形状不可以决定用户身份。候选请求先通过 Go 自有 `Authorization: Bearer <session-id>` 实时验证；Node Token、Cookie、query token、`X-User-Id` 均不能进入 Go。无效、撤销、过期、封禁或删除会话固定返回 `401 UNAUTHORIZED`；会话或 MongoDB 依赖故障返回 `503 SERVICE_UNAVAILABLE`。

## 5. 模板、内容面与技术快照

`t2i-freeform` 是内部系统模板，不出现在用户可选择的技术原子列表。它必须具备以下持久化事实：

| 字段 | 固定规则 |
| --- | --- |
| `template_id` | `t2i-freeform` |
| `version` | 创建时读取的单个已启用版本，并写入 `creations.template_version`。 |
| `mode` | `template_image`。 |
| `content_surface` | `sfw`。 |
| `parameters` | 仅服务端可读的模型 SKU、受控输入模板、允许画幅与固定生成参数。 |

必须新增一个专用模板编译器，职责是读取 `TemplateDocument.Parameters`，按 `template_id + version` 编译出 `creations.StepPlan{sequence:1, atom:text_to_image}` 与 `InitialSubmission`。编译后通过既有 `executionv2.Compile` 冻结模型 SKU、技术 JSON 和摘要；创建事务只保存模板版本、请求摘要与冻结技术快照，不接受客户端技术配方。

`content_surface=sfw` 只描述该系统模板本身，不把普通用户的自由提示词错误收窄为「全站仅 SFW」。现网语义是：普通用户不因自由提示词被统一拦截；审核受限用户才必须只生成安全内容。

创建前必须按 Go 用户的 `content_access` 运行以下策略，任一审核依赖缺失时公开路由不得开启：

1. `standard` 用户保持现网普通用户语义，不执行全量 SFW 拦截，也不接受客户端伪造的内容评级。
2. `review_restricted` 用户在提交器领取生成 Outbox 后、向生成中台发送前，必须调用 Go 直连的、与现网 Grok 审核等价的内容审核端口。它只接收最小提示词审核载荷，不能调用 Node 审核 HTTP 接口，也不能读取 Node 会话或钱包。
3. 审核明确拒绝时，提交器在同一事务中把创作收敛为 `confiscated`、结束未发送的 Outbox，并将预留转为 `confiscated`；不得冲正或调用生成中台。
4. 审核服务不可用、超时或返回无效结构不是「内容不安全」。提交器必须把创作标记为 `submission_failed`、结束未发送的 Outbox 并冲正一次；对外状态读取投影为失败。Gateway 仅在会话或本地依赖故障时返回 `503 SERVICE_UNAVAILABLE`。

当前 Node 对审核受限请求会使用 Grok 审核并在审核不可用时拒绝请求；Go 必须直接实现等价的审核端口和失败关闭策略。审核用户只看 SFW 模板的目录过滤不能代替自由提示词审核，也不能代替审核没收账务规则。

## 6. 创建、预扣与提交状态机

```text
请求通过会话、内容与权益门禁
  -> 同一 MongoDB 事务：creation(pending_submission)
                         + step(ready)
                         + reservation
                         + ledger entry
                         + Outbox(pending)
  -> 返回 generating
  -> 提交器领取 Outbox；审核受限用户先执行 Go 直连内容审核
       ├─ 明确拒绝：creation=confiscated，预留没收且不退款，不发送中台
       ├─ 审核不可用：creation=submission_failed，立即冲正，不发送中台
       └─ 普通用户或审核允许：投递受控技术快照
       ├─ 已确认受理：step=submitted，等待回调
       ├─ 已确认未受理：creation=submission_failed，立即冲正
       └─ 结果未知：reconciling，沿用同一幂等键查询，禁止新建任务
  -> 可信回调
       ├─ 成功：写 result asset，creation=succeeded
       ├─ 技术失败/取消：creation=generation_failed，冲正一次
       └─ 审核没收：creation=confiscated，预留没收且不退款
```

创建幂等键必须由 Gateway 在通过 Go 会话后生成，格式固定为 `gateway:t2i:<request-id>`。只接受一个符合现网请求 ID 格式的 `X-Request-Id`；缺失时 Gateway 生成 UUID 并写回请求头。不得使用 Node `messageId`，不得让客户端直接指定 Go 幂等键。相同键且相同请求指纹返回原创作；相同键但指纹不同返回固定冲突错误，不触发再次预扣。

权益判断、预留与状态读取均走 Go 自有账本。图片标价 20 钻；VIP 每日 10 张图片、每日 3 条视频与每日 50 钻等既有规则继续由权益模块计算。余额为 0 但仍有日免额度时，必须允许创建。不能先单独检查余额再创建，因为并发请求会造成双免。

## 7. 图片状态投影

新增只读的「Go 创作图片视图」用例。它只能按已认证的 Go `user_id` 查询归属自己的创作，不能通过客户端传入 user ID。

| Go 创作状态 | `generationStatus` | `imageUrl` | 错误投影 |
| --- | --- | --- | --- |
| `pending_submission` | `generating` | 不返回 | 不返回。 |
| `succeeded` | `completed` | 最终步骤的已验证 result asset URL | 不返回。 |
| `submission_failed` | `failed` | 不返回 | `GENERATION_SUBMISSION_FAILED` / `生成服务暂不可用，请稍后重试` / `null`。 |
| `generation_failed` | `failed` | 不返回 | `GENERATION_FAILED` / `生成失败，请重试` / `null`。 |
| `confiscated` | `failed` | 不返回 | `CONTENT_CONFISCATED` / `内容审核未通过` / `null`。 |

状态读取规则：

- `POST /api/images/statuses` 只在 `imageIds` 是 1 至 20 个不重复、规范 Go UUID 且全部属于同一 Go 路径时本地处理；按请求去重后的原始顺序返回已拥有的状态，非本人或不存在的 Go ID 不返回条目。
- 如果批量请求含任何 Node ObjectId、未知格式或混合 ID，整个请求代理 Node。Gateway 不拆分请求、不合并两套响应，避免在 Go 登录态下伪造或错用 Node 身份。
- `GET /api/images/:id` 仅针对规范 Go UUID 执行本地读取；归属不匹配或不存在返回 Go 的统一 `404`，不能借此探测其他用户创作。
- Go 会话用户的历史 Node 任务继续由 Node 会话与 Node 路径处理。实际切流前，Go T2I 候选只应面向没有活动 Node 图片轮询的会话；这是一条切流门禁，不通过时不开启公开路由。

## 8. 分层、错误与安全

```text
gateway 路由识别（仅分流与透明代理）
  -> sessionauth（Go 自有 Bearer 会话）
  -> transport/t2i（DTO、根层 envelope）
  -> biz/t2i 或 creations 编排（门禁、模板编译、创建）
  -> data（MongoDB 创作、资产、模板、账本读取）
```

- `internal/gateway` 不读取 MongoDB、不解析技术模板、不做余额或内容决策。
- `internal/transport` 只做严格 DTO 解码、调用会话认证器与业务用例、编码根层 envelope；不直接访问 MongoDB。
- `internal/biz` 拥有候选分类后的 T2I 门禁、模板配方编译接口、创建编排与状态投影规则。
- `internal/data` 实现按 `creation_id + user_id` 的归属读取、最终结果资产读取、模板版本读取和必要索引；不向上层泄露 BSON、MongoDB 错误或技术参数。
- 日志只记录请求 ID、创作 ID、步骤 ID、模板版本、状态和错误分类；不得记录 Bearer 值、提示词、负向提示词、资产 URL、账本余额、签名或完整 JSON。

所有 Go 本地错误沿用根层 envelope。除了已冻结的 `401`、`402`、`403`、`503` 外，DTO 无效返回 `400`，归属不存在返回 `404`，相同幂等键不同请求返回 `409`。错误响应不能回显敏感输入或内部中台错误。

## 9. 实施前置条件与验收

以下事项必须全部完成，才允许开启公开 T2I 路由：

1. `standard` 与 `review_restricted` 的 Go 内容策略、审核用户安全限制、审核没收不退款，以及审核服务不可用时的冲正，已经以 Go 本地测试证明。
2. `t2i-freeform` 固定模板版本及其服务端技术配方可读取、编译和冻结；客户端无法覆盖模型或回调。
3. 三条精确 Gateway 路由完成无编码路径、重复斜杠、query、method、开关关闭、Node 回退和「Go Handler 后绝不回退」测试。
4. 创建测试覆盖游客未绑定、VIP、余额不足、余额为 0 但有日免、并发日免、幂等重放、请求指纹冲突、确定提交失败立即冲正和未知提交结果不新建任务。
5. 读取测试覆盖 UUID 批量顺序、最多 20 个、非本人隐藏、成功资产读取、各失败投影、Node ID 回退与混合 ID 整体回退。
6. MongoDB 集成测试仅连接本机 `cling_main` / `rs0`；每个测试记录随机精确 `_id`，在 `t.Cleanup` 中逐条 `DeleteOne` 清理。
7. 全量运行 `go test ./... -count=1`、`go test -race ./... -count=1`、`go vet ./...`、Gateway 构建和语义契约检查；范围扫描确认未修改 Node/JS、前端、PayCores、现网钱包、生产配置或真实中台调用。

## 10. 非目标

- 不迁移带 `inputImages` 的模板图编辑 I2I；其全部业务继续由 Node 处理。
- 不迁移文生视频或独立 I2V；文生视频仍严格遵循「先 T2I 首帧，再 I2V」的既有 Go 编排。
- 不提供 Animate 入口、新任务、兼容分支或数据迁移。
- 不接入真实支付回调、生产 MongoDB、生产生成中台、生产回调 Origin 或任何线上开关。
- 不在本切片读取或迁移 Node 历史图片、Node 登录态、Node 钱包或 PayCores 账本。
