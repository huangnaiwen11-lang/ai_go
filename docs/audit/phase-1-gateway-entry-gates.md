# 阶段 1：Gateway 双路由准入门禁

## 当前允许范围

当前 Gateway 只具备本机兼容验证能力：当精确文件开关启用时，`GET /api/growth/ping` 会先经过 Node 准入，再按冻结成功响应决定是否本地回放。其他所有请求仍由默认代理交给 Node。

这不等同于 Gateway 已部署、已灰度或可承接通用 `/api` 流量。生产分流保持关闭，直到本文件全部门禁都有可复现证据。

## 必须冻结的 HTTP 契约

| 主题 | 当前源码证据 | 放行前必须补齐的证据 | 未完成时的规则 |
| --- | --- | --- | --- |
| 普通 API、上传与 SSE 超时 | `GATEWAY_ADMISSION_TIMEOUT` 可为精确 Growth Ping 的 Node 准入请求设置正数超时；超时会取消该请求并映射为 `504/UPSTREAM_TIMEOUT`。未设置时保持原本地行为；通用代理、上传和 SSE 仍没有经运行基线确认的超时策略。 | 分别捕获 Node 的普通 API、上传和 SSE 建连/持续传输超时行为；验证客户端断开会取消上游请求，且 SSE 不会被 Gateway `WriteTimeout` 提前切断。 | 该可选本地配置不构成通用路由灰度许可，不凭经验填写受控环境的超时值。 |
| 上游错误 envelope | Node 的 `ApiError.upstreamBadResponse` 为 `502/UPSTREAM_BAD_RESPONSE`，`ApiError.upstreamTimeout` 为 `504/UPSTREAM_TIMEOUT`；Gateway 本机契约将连接失败和本地冻结 ping 响应在写出前的前缀检查读取失败映射为前者，将 `context.DeadlineExceeded` 或任意 `net.Error.Timeout() == true` 映射为后者，均使用 Node message、`details: null` 和入口 `requestId`。状态已开始中继后发生的流中断不伪造新的 JSON error，仍需受控运行证据。 | 为真实连接失败、上游 response-header 超时和客户端取消分别捕获受控运行证据，冻结 HTTP 状态、`code`、`message`、`details` 与 `requestId`；不得将客户端取消协议推断为上游超时。 | 不以 `UPSTREAM_UNAVAILABLE` 作为对浏览器开放的兼容结论；没有生产超时与客户端取消证据时，不开放通用流量。 |
| Cloudflare 与转发头 | Node 仅在私网 socket、公开 `CF-Connecting-IP`，且 `X-Forwarded-For` 最右侧为 Cloudflare IP 时信任 Cloudflare 请求。Gateway 的准入路径会追加 `X-Forwarded-For`，默认反向代理也会追加转发 hop。 | 用脱敏请求捕获 Nginx → Gateway → Node 的 `remoteAddress`、`X-Real-IP`、`X-Forwarded-For`、`CF-Connecting-IP` 与 `CF-*` 头；在 Node 的 `requestNetworkTrust` 合同测试中复现同一链路。 | 未确认 Gateway 是否只接收可信反向代理流量前，不修改或启用通用转发策略。 |
| 灰度与回退装配 | 当前只有 `cmd/api-gateway` 本地启动入口和文件级精确开关；没有 Docker、反向代理或按接口/用户/租户/百分比的实际部署装配。 | 提供可审阅的受控部署清单、精确路由灰度配置、指标面板与一键回退演练记录；回退必须把流量恢复到 Node，不能删除服务或修改业务数据。 | 不将“文件开关可回退”表述为生产灰度回退已验证。 |

## 证据采集边界

- 只能使用受管只读审计入口或等价的脱敏运行捕获；禁止读取私钥、生产环境变量、MongoDB 凭据、容器密钥或数据库。
- 捕获材料不得包含 Authorization、Cookie、用户身份、请求正文、完整 IP 或生产地址；应以字段存在性、脱敏 hop 类型和 HTTP 合同表达。
- 不因缺少生产证据而猜测超时值、信任链或错误映射。所有未确认路由继续回退 Node。

## 阶段 1 退出条件

只有同时满足以下条件，才可以把 Gateway 阶段从“本机金丝雀工具”更新为“可灰度服务”：

1. `/api` 前端入口无需改动，且 requestId、鉴权头、语言和设备头、成功/失败 envelope 与 Node 基线一致；
2. 普通 API、上传、SSE、上游失败和客户端取消的超时合同都有自动化测试与受控运行证据；
3. Cloudflare/XFF 拓扑通过 Node 信任链测试，认证、限流、地理策略和审计不会因新增 hop 改变语义；
4. 仅由权威迁移矩阵中的精确确认路由参与灰度，并具备按接口关闭、观测告警和回到 Node 的演练记录。

在上述证据缺失时，Catalog、Realtime、Generation、Media、Identity、Billing 与 Admin 的路由全部不得交给 Gateway 或新增 Go 服务处理。
