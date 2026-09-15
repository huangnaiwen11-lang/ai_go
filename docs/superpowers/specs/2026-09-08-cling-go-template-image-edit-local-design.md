# Cling Go 模板图编辑（I2I）本地重构设计

## 1. 目标与范围

在独立本地 MongoDB `cling_main` 中，把已选图片模板的编辑创建平移到 Go。用户仍然选择产品模板并上传素材；Go 将模板冻结为生成中台的 `image_edit` 技术快照。用户不选择 I2I、模型、Provider、工作流或回调地址。

本切片只接管 `POST /api/chat/image/async` 中已带合法 `templateId` 和 `inputImages` 的模板图编辑请求。未知模板、未选模板的输入图请求、`operation=remove`、旧 Provider/技术字段、聊天关联字段、非规范路径或其他不兼容形态，保持完整代理 Node。

不修改 Node/JS、前端、PayCores、现网钱包、生产配置或生产数据；不启动真实 Worker，不请求真实生成中台，也不进行线上切流。

## 2. 保持的产品语义

- 模板图编辑是图片模板能力，例如脱衣、换装；不是通用图生图入口。
- 同一模式下，Web、iOS、Android 共享同一份模板定义。审核受限用户只能选择 SFW 模板；普通用户可以使用启用的 SFW 或 NSFW 模板。
- Go 会话必须有效；游客未绑定、需 VIP、钻石不足继续分别返回 `403 ACCOUNT_BINDING_REQUIRED`、`403 VIP_REQUIRED`、`402 INSUFFICIENT_FUNDS`。不得在创建前读取余额作预检查。
- 创建事务继续先写创作占位、预扣、账本和 Outbox。提交确定未受理时冲正；可信技术失败或取消时冲正；审核没收不退款。
- 中台出站载荷只含 `image_edit` 的冻结技术快照和 Go 自有回调地址，不含用户、模板、钻石、余额、VIP、额度、支付或账本字段。

## 3. 模块边界

| 模块 | 职责 |
| --- | --- |
| `internal/gateway` | 精确识别模板图编辑候选；无法完整理解的请求继续代理 Node。 |
| `internal/transport/t2i` | 保持既有三条图片 HTTP 路由与响应 envelope；新增严格 I2I DTO。 |
| `internal/transport/sessionauth` | 在已验证的 Go 会话身份中返回用户内容访问级别，不能信任请求头。 |
| `internal/biz/t2i` | 新增模板图编辑编译器与创建用例；复用创作预留和状态投影。 |
| `internal/data` | 读取唯一启用模板版本及其服务端编辑配方；按内容面过滤，不让客户端指定版本或技术字段。 |
| `internal/biz/creations`、`ledger`、`outbox` | 继续承担同事务预扣、账本、Outbox 和幂等事实。 |

不增加服务间调用，不引入微服务或新的公开 API。

## 4. 模板配方与输入规则

Go 模板文档的 `parameters` 为仅服务端读取的编辑配方：

```json
{
  "kind": "image_edit",
  "model_sku": "ps-edit-apparel-v1",
  "prompt": "服务端模板提示词",
  "negative_prompt": "服务端负面提示词",
  "parameters": {"steps": 28},
  "input_rule": {"user_image_count": 1, "user_role": "source_image"},
  "reference_assets": [
    {"role": "guide_image", "url": "https://assets.example/template-guide.png"}
  ]
}
```

`kind` 必须为 `image_edit`；模型 SKU、提示词、负面提示词、参数、用户图片数量、素材角色和模板参考图均由模板决定。首个切片的输入规则固定为 1 张用户 HTTPS 图片，可选 1 张服务端 HTTPS 参考图，最多 2 张素材；这与现网“1 张用户图片 + 1 张服务端参考图”的模板语义一致。

客户端请求只能提交 `templateId`、`inputImages`、可选用户补充提示词和 `X-Request-Id`。补充提示词只作为模板提示词后的产品文案追加，不可覆盖服务端提示词或负面提示词。`inputImages` 必须是只有 1 个无首尾空白的 HTTPS URL 的数组，不能由客户端伪造 `role`。

## 5. 身份、模板选择与创建流程

`identity.Usecase.ValidateSession` 已读取用户状态和内容访问级别。会话认证器将该受控值放入 `AuthenticatedIdentity`，作为模板配方查询条件；客户端不得提交 `contentSurface`、`reviewed` 或用户 ID。

```text
HTTP 请求
  → Go 会话验证（用户 ID + 内容访问级别）
  → Gateway 精确候选判断
  → 读取唯一启用且对该用户可见的图片模板版本
  → 编译 image_edit 快照并冻结输入摘要
  → CreateReserved：占位 + 预扣 + 账本 + Outbox
  → 后续受控 Worker 投递 /api/v2/executions
  → 已验签回调写最终素材、状态或冲正
```

同一 `gateway:i2i:<X-Request-Id>` 对相同冻结快照重放既有创作；快照不同返回冲突。状态查询继续复用现有的归属约束和可信最终素材投影。

## 6. 错误与回退规则

| 情况 | 行为 |
| --- | --- |
| 有效 Go I2I 候选，但会话无效 | Go 返回 `401 UNAUTHORIZED`。 |
| 有效 Go I2I 候选，但门禁或预扣失败 | Go 返回既有 `403` / `402` 业务错误。 |
| 有效 Go I2I 候选，但模板不存在、不可见、未启用或配方无效 | Go 返回 `400 INVALID_REQUEST`，不创建任何事实。 |
| 非候选、旧语义或未知请求形态 | Gateway 原样代理 Node。 |
| Go 已接管后的任意错误 | 不回退 Node，避免双创建和双预扣。 |

## 7. 验收

必须新增单元、Mongo 集成与 HTTP 端到端测试，覆盖：内容面过滤、唯一启用模板、客户端技术字段拒绝、输入图与参考图编译、幂等冲突、门禁错误、预扣/Outbox、模拟投递、可信回调、批量与单条状态读取，以及非候选请求的 Node 透明代理。

验证命令固定为：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
  GOCACHE=/private/tmp/ai-business-service-go-cache go test ./... -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' \
  GOCACHE=/private/tmp/ai-business-service-go-cache go test -race ./... -count=1
GOCACHE=/private/tmp/ai-business-service-go-cache go vet ./...
GOCACHE=/private/tmp/ai-business-service-go-cache go build -o /private/tmp/ai-business-service-template-image-edit ./cmd/api-gateway
```
