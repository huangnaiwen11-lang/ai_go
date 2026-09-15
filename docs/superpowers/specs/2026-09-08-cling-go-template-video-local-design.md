# Cling Go 视频模板双路径本地重构设计

> 状态：已确认，待实施。本设计只改动本地 `ai-business-service` 的 Go 代码和独立 MongoDB `cling_main` / `rs0`；不修改 Node/JS、前端、PayCores、现网钱包、生成中台、生产配置或部署。

## 1. 目标与不变语义

将用户「选择视频模板」后的两条现网业务路径平移到本地 Go Gateway。用户始终选择模板，不会选择 `text_to_image`、`image_to_video` 或其他生成原子。

| 用户输入 | Go 内部编排 | 用户可见语义 |
| --- | --- | --- |
| `templateId + imageUrl` | 单步骤 `image_to_video` | 选视频模板并以用户图片生成视频。 |
| `templateId + prompt`，且没有 `imageUrl` | `text_to_image` 首帧 → `image_to_video` | 选视频模板并以文本生成视频。第二步只在可信首帧回调成功后投递。 |

两条路径共同遵循以下规则：

- 模板唯一决定模型、服务端提示词、负面提示词、技术参数、参考素材及两步骤配方；客户端不能提交模型、Provider、回调、LoRA、工作流、价格、钻石、余额、VIP 或支付字段。
- 创建、预扣、账本、日免费额度、Outbox、投递失败冲正、审核没收及可信回调复用既有 Go 领域能力；不能先查余额再创建任务。
- 视频计价与现有权益合同一致：5/10/15 秒分别是 50/100/150 钻；VIP 日免费额度尚有剩余时，即使余额为 0 也可以创建。
- 审核受限用户只能读取 SFW 模板；普通用户可读取 SFW 和 NSFW 模板。
- 仅接管结构完全匹配的已选模板请求。多图、工作流模式、LoRA、聊天关联、Provider、模型、未选模板及其他相邻 `/video` 请求继续代理 Node。
- 不新增 Animate 入口、独立「技术 I2V」入口或真实 Worker 循环。

## 2. 入口与请求分流

本地只审查并接管下列无查询参数、未编码路径的精确路由：

| HTTP 路由 | 用途 | 本地接管条件 |
| --- | --- | --- |
| `POST /api/chat/video` | 创建视频模板任务 | `templateId` 非空，且恰好满足「一张 HTTPS `imageUrl`」或「非空 `prompt` 且没有 `imageUrl`」之一。 |
| `POST /api/chat/videos/status` | 批量读取视频状态 | 所有 ID 都是规范 Go UUID，数量在既有批量上限内。 |
| `GET /api/chat/video/:taskId` | 读取单条视频状态 | `taskId` 是规范 Go UUID。 |

创建候选只允许 `templateId`、`imageUrl`、`prompt`、`negativePrompt`、`aspectRatio`、`durationSeconds` 与 `enableAudio`。`durationSeconds` 仅允许 5、10、15；`imageUrl` 必须是无空白的 HTTPS URL。文本与图片不能同时出现，避免把双路径混成不可验证的第三种编排。

Gateway 只做无状态分类并恢复原始请求体。候选检查、公开路由开关和会话开关全部通过后才交给 Go；Go Handler 一旦接管，错误直接返回，不会回退 Node，避免重复预扣。任一开关关闭时，不读取 Go MongoDB，所有请求继续代理 Node。

## 3. 视频模板与冻结编译器

模板仍存于本地 `templates` 集合，筛选条件是 `template_id + enabled + mode=template_video + content_surface`。同一模板存在多个启用版本、配方不完整、内容访问级别未知或技术字段非法时失败关闭。

`parameters` 使用服务端受控配方。形状如下：

```json
{
  "kind": "template_video",
  "i2v": {
    "model_sku": "ps-auto",
    "prompt": "服务端动作提示词",
    "negative_prompt": "服务端负面提示词",
    "parameters": { "durationSeconds": 5 }
  },
  "t2i": {
    "model_sku": "ps-image-v1",
    "prompt": "服务端首帧提示词",
    "negative_prompt": "服务端首帧负面提示词",
    "parameters": { "aspectRatio": "9:16" }
  }
}
```

- 图片路径只使用 `i2v`：用户图以 `source_image` 写入技术输入，编译成单步骤 `image_to_video` 快照。
- 文本路径要求同时有 `t2i` 与 `i2v`：首步由 `t2i` 编译；第二步以既有 `DeferredImageToVideo` 冻结 `i2v` 的模型、提示词与参数，不伪造首帧 URL。
- 用户补充提示词只能追加到模板服务端提示词；客户端负面提示词、比例、时长和音频仅可在模板明确允许的产品输入范围内覆盖。当前首个切片固定由模板配方决定技术参数，客户端值只用于与模板合同一致性校验，避免技术配方漂移。
- `CreateReserved` 在一个 MongoDB 事务中写创作、步骤、预扣、账本、首步 Outbox 与延迟配方。文本路径第二步初始为 `blocked`；图片路径只有一个 `ready` 的 I2V 步骤。

## 4. 回调、状态与安全边界

既有可信回调能力是本设计的唯一终态入口：

1. 图片路径 I2V 成功后写入视频资产，步骤与创作变为 `succeeded`。
2. 文本路径首帧成功后写入首帧资产、消费创建时冻结的配方、绑定 `opening_frame`、写第二步 Outbox，并把第二步从 `blocked` 原子推进到 `ready`。
3. 第二步 I2V 成功后才将整条创作标记为 `succeeded`。
4. 任一步骤的可信技术失败或取消按既有账本路径自动且仅一次冲正；审核没收不退款，且本阶段不新增审核入口。

视频状态读取只基于本地创作、步骤和最终视频资产，不查询 Node 或生成中台。响应延续现网视频任务字段：`taskId`、`status`、`durationSeconds`、`videoUrl` 与固定错误码；不返回技术配方、素材 URL、回调信息、账本或权益内部事实。

中台收到的 Outbox 载荷永远只包含 `capability`、`model_sku` 与 `input`。其中 `input` 只允许 `prompt`、`negativePrompt`、`assets`、`parameters` 四类技术字段；不得带用户、模板、钻石、余额、VIP、额度、价格、支付、账本、回调 URL 或审核信息。

## 5. 工程模块与测试

新增 `internal/biz/video` 负责视频模板配方、双路径编译和状态读取；`internal/data` 只负责 MongoDB 模板与状态投影；`internal/transport/video` 负责严格 HTTP DTO、认证和响应映射；`internal/gateway/video_route.go` 只处理精确候选分类；`cmd/api-gateway` 负责依赖装配。不得将视频逻辑塞进现有 T2I Handler。

实现必须按测试先行完成：

- 编译器测试覆盖单图 I2V、文本双步骤、配方冻结、非法输入和客户端技术字段拒绝。
- MongoDB 测试覆盖 SFW/NSFW 内容面、唯一启用版本与精确 `DeleteOne` 清理。
- Gateway 与 Handler 测试覆盖双路径准确接管、相邻 Node 回退、403 绑号/VIP、402 余额不足及会话内容访问级别。
- 本地端到端测试覆盖「创建 → 预扣 → Outbox → 模拟提交 → 可信首帧回调 → 第二步 Outbox → I2V 回调 → 视频状态」和单图 I2V 闭环；不启动 Worker，不请求真实中台。
- 最终运行 `go test ./...`、`go test -race ./...`、`go vet ./...` 与 API Gateway 构建。

## 6. 明确不包含

- Node 旧业务、前端调用、PayCores、现网钱包、生产 MongoDB、生产环境变量、部署、Nginx、真实回调 Origin、真实中台请求。
- 多图、首尾帧、运动控制、参考图视频、LoRA、音频编排、视频高清、流式状态、公开视频流、聊天关联、Provider 或模型选择。
- Animate、用户直接选择生成原子、通用未选模板的 I2V、支付入账、注册奖励、审核入口与生产切流。
