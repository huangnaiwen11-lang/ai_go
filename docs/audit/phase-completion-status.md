# Go API 重构阶段状态台账

## 判定口径

2026-09-14 补充核验见 [本地 Go 独立处理验证](2026-09-14-local-go-validation.md)。本地门禁通过
不等于全部代码完成：真实身份/商店验证器、Redis 适配及完整浏览器联调仍存在缺口，不能统一归为“只差授权”。

本台账严格采用主设计文档的退出条件，而不是只看某个测试是否通过。当前状态为：

- **本地工程门禁已完成**：可离线验证的审计、合同、回放与保护测试已经具备。
- **正式迁移验收未完成**：设计要求的真实运行验证、灰度观察和回滚演练尚未执行。
- 当前任务仅允许本地重构；**不得连接、配置或切换生产环境**。

因此，“本地绿色”只证明 Go 不会在证据不足时提前接管业务，不代表某阶段已完成流量切换。

## 分阶段状态

| 阶段 | 本地已完成 | 正式关闭前仍需的证据 |
|---|---|---|
| 0 使用面审计与契约冻结 | 静态候选、精确迁移矩阵、黄金用例、候选淘汰清单和矩阵校验。 | 为拟迁移业务接口补齐受控运行证据、数据所有者、语义基线及负责人。 |
| 1 Go Gateway 双路由 | 精确 `GET /api/growth/ping` 金丝雀、Node 准入、回退开关、SSE 透传和错误 envelope 合同。 | 按真实边缘头链、超时策略完成受控灰度、指标观察和一键回退演练。 |
| 2 Catalog 与 Realtime | 匿名 Catalog 回放、缓存字段比较、SSE transcript 合同和首帧竞争窗口记录；通知读写合同已在 Go 本地 Mongo、用户索引、Handler 单测与 Gateway 动态 exact route 中落地；新增默认关闭的本地生成 SSE Hub/Handler，覆盖会话鉴权、用户隔离、ready 首帧、心跳、断开清理和 `Last-Event-ID` 有界重放。 | Node 与 Go 候选服务真实对照、Redis 跨进程事件、跨进程断线补偿、跨用户隔离及缓存/排序/可见性验证。 |
| 3 Generation Orchestrator | T2I、模板 I2I、模板 I2V 的本地 Mongo 创建/预扣/审核/回调闭环，以及 Outbox 一次投递 Worker 与显式本地轮询器；默认仍代理 Node，主服务也不会自动投递。 | 真实回调签名、幂等、状态机、结算与唯一写入方的受控验证。 |
| 4 Media Service | 上传 presign、签名 TTL、访问策略和票据合同。 | 使用真实文件验证上传、重试、过期、可见性和删除语义。 |
| 5 Identity Service | Go 自有注册、密码登录、移动端 Guest、账号读取、封禁/删除会话立刻失效，以及带 HMAC 签名的本地 binding verifier、原游客绑定合同与幂等测试；magic link 和真实 OAuth/短信仍未接入。 | 真实 OAuth/短信 verifier、旧 token 兼容、账号状态、OAuth 回调、401 和会话行为的受控验证。 |
| 6 Wallet/Billing | Go 自有商品版本、订单冻结、账本/余额事务、PayCores V2 回调、受控 Apple/Google IAP 回执和重放防护的本地 Mongo 闭环；默认仍代理 Node。 | 真实分账、余额守恒、退款、支付回调、商店验签、并发、对账与受控灰度验证。 |
| 7 Admin | 本次用户端重构明确排除管理后台；现有清单仅作为旧项目审计材料保留。 | 不属于本次交付，后续若启动后台迁移需另立范围和授权。 |

## 路由与产品边界

- Gateway 默认将所有 `/api` 请求代理 Node；仅当独立环境开关与 exact route-switch 同时开启时，才会接管已审查的本地账号、生成、生成回调、支付回调或支付/IAP精确路由。
- 公共生成能力仅覆盖 **T2I、I2I、I2V**，并在本机完成预扣、审核、回调和状态读取闭环；未获真实依赖授权时不得接入真实生成中台或切换流量。
- **Animate 已废弃**：新前端不提供入口，Go Gateway 不提供新任务创建路由；旧 Animate 深链只能兼容跳转到视频页且丢弃参数，旧历史数据仍按既有数据迁移策略处理。
- **Animate 继续 Node-only**：历史任务、历史回调、查询、取消和结算仍由 Node 维护，本次不新增 Go 写入方。

## 本地外部依赖模拟器

为便于在没有真实中台授权时完成代码级和 HTTP 级验证，新增了
`cmd/local-generation-platform`。该进程只监听回环地址，校验生成请求 HMAC，支持三种
原子能力的提交与查询，并可选发送本地完成回调。它不读取生产配置、不访问外部网络、不会
改变 Gateway 默认代理行为，也不能作为真实中台灰度验收证据。

本地启动示例：

```bash
go run ./cmd/local-generation-platform --addr 127.0.0.1:18000 --callback=false
```

对应的自动化合同测试位于 `internal/integrations/generation/local_platform_test.go`，覆盖
签名提交、幂等查询和未签名请求拒绝。
其中回调测试还会使用独立的本地 HTTP 传输验证完成回调的路径、载荷和 HMAC，避免只测内存函数而遗漏出站回调合同。

## 已留存的本机运行核验

2026-09-03 在临时回环端口启动当前源码构建，并使用不可达的本机上游验证精确相邻路径：

- `GET /api/growth/ping/` 返回 `502`、`UPSTREAM_BAD_RESPONSE`、既有错误 envelope，并保留调用方提供的 `X-Request-Id`。
- 临时进程在验证后已停止；没有连接生产或外部服务。
- 原有 `127.0.0.1:8080` Gateway 进程为旧构建，返回过历史 `UPSTREAM_UNAVAILABLE`。它未被停止或改动，也不能作为当前源码或正式阶段的验收依据。

这条核验只证明当前本机构建的错误映射和精确路由回退，不覆盖真实 Node、边缘转发头、SSE、灰度或回滚演练。

## 最近一次本地复核（2026-09-03）

本次复核先修复了两项会削弱本地门禁的缺口：

- 生成任务的终态回调现在仅允许从 `processing` 进入终态；`draft`、`submitted` 与零值任务均被拒绝，已终态的重放和冲突语义保持不变。
- 静态合同解析现在要求所有冻结字段显式出现，并拒绝任意对象层级的重复键、未知字段与拼接文档；失败时仍只输出固定文本，不回显清单内容。

随后已在本机完成以下复核，且没有连接生产或外部业务服务：

```bash
go test -race -count=1 ./...
go vet ./...
node --test scripts/phase0-audit.test.mjs
node scripts/phase0-audit.mjs --validate-matrix
go run ./cmd/static-contract-check --stage identity --manifest docs/audit/identity-static-contract.json
go run ./cmd/static-contract-check --stage wallet --manifest docs/audit/wallet-static-contract.json
go run ./cmd/static-contract-check --stage admin --manifest docs/audit/admin-static-contract.json
```

独立复审未发现必须修复的问题。以上结果仍只证明本地门禁有效，不改变“阶段 0～7
尚未经过真实运行验证、灰度观察和回滚演练”的正式状态。

## 本地复核命令

在本项目根目录运行：

```bash
go test -race -count=1 ./...
go vet ./...
go run ./cmd/static-contract-check --stage identity --manifest docs/audit/identity-static-contract.json
go run ./cmd/static-contract-check --stage wallet --manifest docs/audit/wallet-static-contract.json
go run ./cmd/static-contract-check --stage admin --manifest docs/audit/admin-static-contract.json
node --test scripts/phase0-audit.test.mjs
node scripts/phase0-audit.mjs --validate-matrix
```

这些命令只验证本地工程状态；它们不能替代任何阶段所需的真实运行验证、灰度观察和回滚演练。

## 本轮本地复核（2026-09-12）

- `go test -run '^$' ./...` 全部 Go 包编译通过。
- 生成中台本地提交、查询和签名回调合同测试通过。
- 本地 SSE Hub/Handler 已接入 Gateway 可选装配，默认关闭；鉴权、用户隔离、首帧、心跳和断开清理测试通过。Handler 依赖 `StreamSource` 最小合同，进程内 Hub 可在不改 HTTP/前端协议的前提下替换为 Redis 适配器；当前仍未宣称 Redis 跨进程验收完成。
- 阶段 0 审计测试 41 项全部通过，迁移矩阵 46 条全部通过。
- 新前端 Vitest 138 项、TypeScript 和 Go-only API 审计全部通过。

上述证据仍属于本地工程验证，不改变真实依赖和生产灰度的授权前置条件。
