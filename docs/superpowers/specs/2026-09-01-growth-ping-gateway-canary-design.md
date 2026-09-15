# Growth Ping Gateway 首个只读灰度设计

## 目标

让 Go Gateway 在不访问数据库、不调用外部服务且不改变浏览器调用方式的前提下，直接处理首个低风险接口：`GET /api/growth/ping`。

## 范围

- 仅处理精确组合：HTTP `GET`，且解码后路径和原始转义路径都为 `/api/growth/ping`。
- 返回与 Node 基线一致的 JSON：`{"success":true,"data":{"ok":true}}`。
- 复用 Gateway 已创建或透传的 `X-Request-Id`。
- 所有其他请求继续由 `DefaultUpstream` 代理给 Node backend。

## 非目标

- 不处理 `/api/v1/growth/ping`。该路径继续走 Node 回退，避免在没有独立版本契约验证前扩大灰度范围。
- 不迁移首页、鉴权、钱包、生成、SSE 或任何包含持久化读写的接口。
- 不引入数据库连接、缓存、配置开关、监控系统或新的 Go 服务进程。

## 架构与数据流

请求先进入 `Gateway.Handler()`。Gateway 负责建立 `X-Request-Id`，随后调用精确路由匹配函数：

1. 仅当方法为 `GET`、解码后路径为 `/api/growth/ping`、原始转义路径也为 `/api/growth/ping` 时，由本地只读 handler 返回固定兼容 envelope。双重判断能阻止 `%2F` 或 `%70` 等编码变体解码后误命中。
2. 任何未命中的请求都继续使用原有反向代理，因此保留 Node 的路由、鉴权、状态码、SSE 刷新和错误语义。

本地 handler 独立放在 `internal/gateway/growth_ping.go`。该文件只承载静态兼容响应；`proxy.go` 继续负责路由选择、请求 ID 和上游代理。注释只解释兼容边界和回退原因，避免重复解释 Go 语法。

## 兼容性契约

| 项目 | 要求 |
| --- | --- |
| 方法与路径 | 仅 `GET /api/growth/ping` 命中本地 handler；解码路径与原始转义路径均须完全一致 |
| 响应状态 | `200 OK` |
| 响应头 | `Content-Type: application/json`，保留 `X-Request-Id` |
| 响应正文 | `{"success":true,"data":{"ok":true}}`，无尾随换行，与 Node 基线逐字节一致 |
| 相邻路径、其他方法、`/api/v1/...`、编码路径变体 | 必须回退 Node，不能误命中 |
| 无上游时的回退错误 | 普通上游失败返回 `502/UPSTREAM_BAD_RESPONSE`；可明确识别的超时返回 `504/UPSTREAM_TIMEOUT`。这仅是本机精确金丝雀契约，尚未证明生产超时、客户端取消、XFF 或通用灰度，不能开放通用流量 |

## 验收与回退

- 单元测试覆盖精确命中、请求 ID、方法不匹配、相邻路径、`/api/v1` 和编码路径变体回退。
- `go test ./internal/gateway` 与 `go test ./...` 均通过。
- 本地运行时请求 `GET /api/growth/ping` 返回兼容 envelope；`POST /api/growth/ping` 不由新 handler 接管。
- 回退只需删除精确 matcher 分支；未命中时所有流量天然继续使用 Node。

## 可维护性约束

- 路径常量、匹配函数和响应函数职责单一，便于将来审查或移除。
- 测试以外不使用魔法字符串重复定义接口路径。
- 代码注释解释为何匹配必须精确、为何版本化路径不包含在灰度范围内。
