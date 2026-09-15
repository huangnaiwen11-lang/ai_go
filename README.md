# AI Business Service

Cling 主站的 Go 模块化单体骨架。当前阶段只建立清晰的分层、协议和装配边界；不接生产环境、不切换流量，也不改动 Node/JS、生成中台或 PayCores 的业务写路径。

## 当前范围

- 提供 Kratos HTTP 与 gRPC 传输层，以及 `GET /healthz` 存活探针。
- 使用 `api/cling/v1` 作为新的公共协议根目录，默认 Todo 示例已完全移除。
- 固化 `identity`、`catalog`、`entitlement`、`ledger`、`creations`、`outbox`、`generation`、`contentreview`、`payments` 9 个业务边界。
- 冻结脱敏业务语义案例，作为后续业务实现和集成测试的前置门禁。
- 本地 MongoDB `cling_main` 声明用户、模板、创作、账本、支付、反馈和通知等集合及索引；`accounts` 以用户 ID 为主键保存钻石余额，不额外建立二级索引。
- `LocalSchemaInitializer` 必须在 HTTP 与 gRPC 开始监听前完成上述初始化；初始化失败时应用拒绝启动。
- 本地已实现 Go 自有会话验证、创作占位、VIP/游客/余额门禁、账本预扣、审核没收、生成提交 Outbox、可信生成回调，以及图片和模板视频的状态投影。
- 本地支付已实现商品版本、订单冻结、PayCores V2 可信回调、Apple/Google 独立交易命名空间以及受控 IAP 回执入账；全部只写 Go 自有账户、账本、订单和回执。
- 本地 Gateway 可在显式开关下接管自由文生图、模板图编辑和模板视频的受控候选；默认关闭，未命中候选一律透明代理 Node。
- 本地 Gateway 可在独立开关下接管通知列表、未读数、已读和删除；通知始终使用 Go 会话和 Go 自有 Mongo 用户隔离。
- 不读取或改写 Node 登录态、Node 钱包、PayCores 账本、生成中台或任何生产配置；主服务默认不启动 Worker、审核服务或生成中台调用。`internal/worker.Runner` 仅供明确的本地命令或测试装配使用。
- 本地 MongoDB 集成测试仅连接 `CLING_TEST_MONGO_URI` 指定的 `127.0.0.1` 上 `rs0`，并仅按随机测试 ID 精确清理测试文档。

游客绑定本地联调可用 `go run ./cmd/local-auth-credential -provider google -subject local-user@example.test` 生成签名凭据；该命令只适用于本地 `LocalBindingVerifier`，不产生真实 OAuth 或短信凭据。

商店回执本地联调可用 `go run ./cmd/local-store-receipt -provider apple -transaction local-1 -store-product com.example.coins100` 生成受控回执；它只适用于 `LocalVerifier`，不连接 Apple/Google。

## 业务模块

| 模块 | 职责 |
| --- | --- |
| `identity` | 用户、游客绑定、会话和账号状态。 |
| `catalog` | SFW / NSFW 内容面、模板目录和模板编译。 |
| `entitlement` | 价格、VIP、每日额度和用户时区。 |
| `ledger` | 自有账本、预扣、确认、冲正和审核没收。 |
| `creations` | 用户可见的创作投影与状态事件。 |
| `outbox` | 生成提交事件、领取租约、重试与结案。 |
| `generation` | 三个技术原子、提交状态机、可信回调与文生视频父子编排。 |
| `contentreview` | 审核受限用户的最小提示词审核合同与没收分流。 |
| `payments` | Go 自有商品、订单、支付回调验签与幂等入账，不接管现网钱包。 |

用户选择模板，而不是直接选择 T2I、I2I 或 I2V。主站只会向生成中台编译并提交 `text_to_image`、`image_edit`、`image_to_video` 三个技术原子；文生视频由主站编排为首帧和视频两个步骤，对用户只结算一次。

## 分层约束

```text
客户端 → DTO → service → DO → biz → DO → data → PO → 存储
```

- `service`：只处理 DTO 与 DO 的转换、参数校验和传输协议适配。
- `biz`：拥有领域对象（DO）、Usecase、仓储接口和业务错误；不得依赖 `data`。
- `data`：拥有持久化对象（PO）与仓储实现；不得依赖 `service` 或 DTO。
- `cmd`：唯一允许同时装配各层的入口。

跨模块协作优先通过明确接口和事务内 Outbox 事件完成，不能绕过模块边界直接读写别的模块表。

## 目录

```text
api/cling/v1/                     公共协议及生成代码
cmd/ai-business-service/          应用入口与 Wire 装配
internal/biz/<module>/            9 个领域模块
internal/data/                    本地 MongoDB 仓储与持久化层
internal/service/                 HTTP / gRPC 适配层（后续落位）
internal/server/                  传输层与通用中间件
docs/contracts/cling-main/        脱敏业务语义案例与校验说明
```

## 本地验证

生成协议和配置：

```bash
make api
make config
go generate ./...
```

运行语义门禁：

```bash
go test ./cmd/semantic-contract-check -count=1
go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json
```

运行本地回归：

```bash
go test ./...
```

也可以使用统一本地门禁（会自动检查相邻的新前端）：

```bash
make verify-local
```

启动本地服务：

```bash
go run ./cmd/ai-business-service -conf ./configs/config.yaml
curl http://127.0.0.1:18000/healthz
```

## 本地模板视频联调数据

在启用 `GATEWAY_GO_SESSION_AUTH_ENABLED`、`GATEWAY_PUBLIC_VIDEO_ENABLED` 和
`GATEWAY_GENERATION_CALLBACK_ENABLED` 的隔离 Gateway 后，可运行以下命令创建一批新的本地数据：

```bash
go run ./cmd/local-video-fixture
```

命令只接受 `127.0.0.1` / `localhost` 上 `cling_main`、`rs0` 的 MongoDB 连接，且所有写入使用新 UUID，
不会覆盖旧文档。它输出两条 Go 自有 Bearer 会话和一个 SFW 5 秒视频模板：

- 预扣用户：初始 50 钻，用来确认创建后余额为 0；
- VIP 用户：初始余额为 0，同时预置当天 3 条免费视频额度，用来确认日免优先于余额。

该命令不会读取 Node 登录态、启动 Worker、提交生成中台或访问 PayCores。

## 本地图片联调数据

在启用 `GATEWAY_GO_SESSION_AUTH_ENABLED`、`GATEWAY_PUBLIC_T2I_ENABLED` 和
`GATEWAY_GENERATION_CALLBACK_ENABLED` 的隔离 Gateway 后，可运行以下命令创建一批新的本地数据：

```bash
go run ./cmd/local-image-fixture > /private/tmp/ai-business-service-local-image-fixture.txt
```

命令只接受 `127.0.0.1` / `localhost` 上 `cling_main`、`rs0` 的 MongoDB 连接；所有用户、会话、
账户、额度和模板文档都使用新的随机 ID，写入冲突会回滚，绝不覆盖既有文档。其输出文件只供本地
Vite 代理读取，不应提交或打印到终端。fixture 包含：

- 预扣用户：初始 20 钻，用于验证一次图片创建会完整预扣；
- VIP 用户：初始余额为 0，同时预置当天 10 张免费图片额度，用于验证日免优先于余额；
- SFW `t2i-freeform` 服务端自由文生图配方，以及单图输入的 SFW 换装图编辑模板。

该命令不会读取 Node 登录态、启动 Worker、提交生成中台、访问 PayCores 或使用现网钱包。

## 本地支付与 IAP 联调边界

支付入口默认继续代理 Node。只有以下三个环境变量都显式设为 `true`，且
`GATEWAY_EXACT_ROUTE_SWITCH_FILE` 精确放行对应 POST 路由时，Gateway 才会接管
`/api/wallet/create-external-checkout` 与 `/api/wallet/verify-purchase`：

```bash
GATEWAY_GO_SESSION_AUTH_ENABLED=true
GATEWAY_LOCAL_PAYMENT_ENTRY_ENABLED=true
GATEWAY_LOCAL_IAP_TEST_VERIFIER_ENABLED=true
```

本地收银台仅创建 Go 自有 `pending` 订单，响应会标识 `integrationMode: local_only`，不会请求
PayCores。IAP 验证器仅接受受控格式 `local:v1:<apple|google>:<transaction>:<storeProduct>`，
任何真实或未知回执都失败关闭。真实商店验签、真实收银台与流量切换仍必须单独完成测试环境授权、
回滚演练和灰度验收。

## 本地生成中台合同模拟器

没有真实生成中台授权时，可启动仅监听回环地址的 HTTP 模拟器完成提交、签名和查询联调：

```bash
make local-generation-platform
```

默认地址为 `127.0.0.1:18000`。模拟器只接受本地请求 HMAC，不访问外部网络；需要验证回调时，
可额外传入 `--callback=true --callback-hmac-key <本地回调密钥>`。它用于代码和协议验证，不能替代
真实中台的灰度、回调、对账和回滚验收。

## 业务语义门禁

详细规则见 [业务语义基线说明](docs/contracts/cling-main/semantic-baseline-guide.md)。其中包含游客绑定、VIP 与余额错误码、每日免费额度、并发预扣、失败冲正、审核没收、文生视频父子任务、支付幂等、封禁会话和跨端模板一致性等案例。

外部依赖授权后的验收步骤见 [外部依赖验收门禁](docs/audit/external-validation-gates.md)。

门禁、预扣、回调、VIP、游客绑定和支付入账均已具备本地验证；但公开路由和支付回调路由仍默认关闭，项目不得据此进行生产部署或流量迁移。生产切换必须另行完成真实依赖验收、灰度方案和回滚方案。

本地 SSE 状态流可通过 `GATEWAY_LOCAL_GENERATION_STREAM_ENABLED=true` 显式装配；它使用进程内
事件 Hub，适合单进程联调，不宣称 Redis 跨进程兼容。未开启时该路径继续代理 Node。
