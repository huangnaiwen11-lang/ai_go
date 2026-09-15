# Cling Go 自有会话 Gateway 本地接入设计

## 目标

为后续纯文生图（T2I）公开创建接口建立最小、可验证的身份前置能力：Gateway 在显式本地开关开启时，仅对已确认的 T2I 候选请求验证 **Go 自有会话**，并把经过验证的 `user_id` 交给后续本地 Handler。

本切片不迁移登录、注册、OAuth、游客绑定或任何 Node 会话协议。它只复用已经完成的 `identity.ValidateSession` 规则，确认被封禁、删除、撤销或过期的 Go 会话会被立即拒绝。这样，后续 T2I 创建不会为了读取用户身份而复用 Node 钱包、Node 登录态或 Node 的用户数据。

## 范围

- 新增一个窄的会话令牌解析器：只接受 `Authorization: Bearer <Go session ID>`。
- 令牌值是 Go `sessions._id` 的不透明标识；不解析 JWT、不读取 Cookie、不接受 query token，也不兼容 Node Token。
- 解析器调用 `identity.Usecase.ValidateSession`，并在成功时输出最小的 `AuthenticatedIdentity{UserID}`。
- Gateway 的公开 T2I 本地 Handler 后续通过该解析器接收身份；本阶段不注册、切流或改写 `POST /api/chat/image/async`。
- 增加一个独立开关 `GATEWAY_GO_SESSION_AUTH_ENABLED`。默认关闭；关闭时不读取 Go MongoDB、不验证请求、不改变任何 Node 代理请求。
- 仅允许本机 `rs0` MongoDB 配置，与已完成的生成回调本地装配采用同一受控连接与清理方式。

## 非目标

- 不修改 Node/JS、前端、PayCores、现网钱包、生成中台、Nginx、生产配置或任何生产回调地址。
- 不签发会话，不实现密码、短信、OAuth、游客创建或游客绑定 HTTP 接口。
- 不接受 Node JWT、Node Cookie、`X-User-Id`、反向代理注入身份头或客户端传入的用户 ID。
- 不创建 T2I、I2I、I2V、Animate 的公开路由，不写创建、账本、额度或 Outbox 数据。
- 不启动 Worker、后台循环或额外 HTTP Server。

## 方案与取舍

已选择「Go 自有不透明会话 ID + MongoDB 会话实时校验」。它与已有 `sessions` 集合和 `session_version` 失效规则直接对应；每次本地候选请求都可发现封禁、删除、撤销与过期，满足旧登录态立即失效的要求。

未选择以下方案：

1. **直接信任 Node 登录态**：会重新耦合 Node 会话和用户库，违反“自己的库、自己的登录态”。
2. **只验签 JWT、不读取会话**：无法在请求时可靠识别已撤销或因封禁而版本失效的会话。
3. **从 `X-User-Id` 传递身份**：该头可由客户端伪造，不能作为公开写接口的身份来源。

## 分层与接口

```text
Gateway（仅后续确认的 T2I 精确候选）
  └── sessionauth.Authenticator
        └── identity.SessionValidator（适配 identity.Usecase.ValidateSession）
              └── data.NewSessionRepository + data.NewUserRepository
                    └── 本机 cling_main / rs0
```

- `internal/transport/sessionauth` 只处理 HTTP `Authorization` 头、固定错误和 `context.Context` 内的最小身份；不导入 Gateway、MongoDB 或创建业务。
- `internal/biz/identity` 继续只处理会话与用户领域规则。为避免传输层依赖完整 Usecase，新增窄接口 `SessionValidator`，由适配器原样转发 `ValidateSession`。
- `cmd/api-gateway` 仅在开关为 `true` 时构造 MongoDB、身份用例和验证器；无论成功或失败，监听结束都只执行一次 cleanup。
- `internal/gateway` 不自己解析令牌，也不定义用户身份协议；它只保存可选的候选 Handler。实际 T2I 路由接入留给下一份规格。

## 令牌合同

| 项目 | 规则 |
| --- | --- |
| Header | 只接受一个 `Authorization` 值，格式为 `Bearer <session-id>`。方案名大小写不敏感。 |
| `session-id` | 去除首尾空白后必须非空、长度不超过 128；不做 JWT 解码或 URL 解码。 |
| 其他来源 | Cookie、query、Body、`X-User-Id`、`X-Forwarded-*` 均不读取。 |
| 成功身份 | 仅得到 Go `Session.UserID`，不从客户端接收或回显用户 ID。 |
| 时间 | 验证器使用服务端 `UTC now`，不信任客户端时间。 |
| 账号状态 | `normal` + 会话未撤销 + 未过期 + `session_version` 一致才可通过。 |

重复 `Authorization` 值、缺失 Bearer 值、带控制字符、超过长度上限、无效会话和内部读取失败必须走不同的受控路径；不得回显令牌、会话 ID、用户 ID、MongoDB 错误或上游错误。

## 错误合同

后续公开 T2I Handler 应保持现网根层 envelope：

```json
{
  "success": false,
  "code": "UNAUTHORIZED",
  "message": "Authentication required",
  "details": null,
  "requestId": "..."
}
```

- 令牌缺失、格式错误、重复、无效、撤销、过期、账号封禁或删除均返回 `401 UNAUTHORIZED`，不区分外部可见原因。
- 会话仓储或数据库故障返回 `503 SERVICE_UNAVAILABLE`，不应误判为无效会话。
- 后续生成权益门禁仍独立映射：未绑号为 `403 ACCOUNT_BINDING_REQUIRED`、需 VIP 为 `403 VIP_REQUIRED`、余额不足为 `402 INSUFFICIENT_FUNDS`。本会话层不把它们混成“不能生成”。

## Gateway 开关与回退

`GATEWAY_GO_SESSION_AUTH_ENABLED` 只控制本地会话依赖的装配，不是路由切流开关。

| 状态 | 行为 |
| --- | --- |
| 未设置或非 `true` | 不建立 MongoDB 连接；所有请求按当前规则代理 Node。 |
| `true` 且本地配置有效 | 构造验证器，但仍不新增公开路由；现有 Node 请求不受影响。 |
| `true` 且配置无效 | Gateway 拒绝启动，避免以部分身份依赖运行。 |

生成回调开关保持独立。两者可以分别启用，且都不能使用 `RouteSwitch` 或前缀匹配绕过各自的安全边界。

## 测试与验收

- `sessionauth` 单元测试覆盖缺失、重复、错误格式、非法字符、超长令牌、成功、过期、撤销、版本失配、封禁、删除及仓储失败。
- Gateway 装配测试覆盖默认关闭不建库、显式开启仅接受本地 rs0、监听返回与监听报错各清理一次。
- 通过本机 `rs0` 集成测试：随机创建 Go 用户与会话，验证 `normal` 用户成功；调用封禁后，用同一会话立即得到 `401`。清理只允许随机精确 `_id` 的 `DeleteOne`。
- 回归运行 `go test ./... -count=1`、`go test -race ./... -count=1`、`go vet ./...`、Gateway 构建与语义契约检查。
- 范围扫描确认没有 Node/JS、前端、PayCores、生产配置、公开生成路由或 Worker 变更。

## 后续边界

本规格完成后，下一切片才可设计 `POST /api/chat/image/async` 的纯 T2I 精确分流。该切片必须再次冻结：请求 DTO、模板解析、内容分级、幂等键、T2I 技术快照、根层成功 envelope、`GeneratedImage`/轮询兼容策略，以及 Go 自有创建数据与现网历史读取的边界。
