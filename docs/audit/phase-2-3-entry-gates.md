# 阶段 2 与阶段 3 准入门禁

## 目的

本文件把 Catalog、Realtime 与 Generation Orchestrator 的静态审计结论转化为可验证的准入门禁。它不授予任何 Go 路由迁移权限；`docs/audit/api-migration-matrix.json` 中仍为“待核实”的条目必须继续由 Node 处理。

若后续确需创建独立服务，服务名固定为 `catalog-service`、`realtime-service` 与 `generation-orchestrator`。每个服务均沿用 `ai-business-service` 的 Kratos 分层：`api` 只定义 transport 合同，`internal/service` 只做协议适配，`internal/biz` 承担用例和领域错误，`internal/data` 封装数据与外部服务访问。Gateway 继续是浏览器 `/api` 的唯一入口。

## 阶段 2：Catalog 与 Realtime

### Catalog 候选

首批候选是 `GET /api/homepage/video-templates` 与兼容别名 `GET /api/v1/homepage/video-templates`。前端的主请求为 `category=all&limit=24`，但 `/homepage/content` 是专用视频模板请求失败后的业务回退，不能在迁移时遗漏。静态证据位于：

- `frontend/src/api/homepage.ts` 与 `frontend/index.html`：客户端请求和首屏参数；
- `backend/src/bootstrap/registerApiRoutes.js`：`/api` 与 `/api/v1` 双挂载；
- `backend/src/modules/homepage/homepage.routes.js`：`optionalAuth`、query 归一、策略、缓存和 HTTP 响应；
- `backend/src/modules/homepage/homepage.content.js`：公开投影，禁止泄露内部 prompt、LoRA 和 `videoGenerationSpec`；
- `cmd/catalog-contract-replay` 与 `internal/catalogcontract`：现有匿名读取的离线比较器。

在将任一候选改为“确认迁移”之前，必须同时得到以下证据：

1. `category` 的默认/非法值、`limit` 的 1–100 边界、`offset`、`maxRating`、web/iOS/Android 平台与匿名/有效 Bearer/App scope 冲突的 Node 捕获样本；
2. `public`/`private` Cache-Control、Vary、X-Cache-Status、X-Data-Stale、视频接口无 ETag/304 与 content 接口 ETag/304 的独立契约；
3. `HomepageContentConfig`、`PlatformConfig`、User 审核字段以及 content 额外依赖的 `FaceSwapTemplateConfig` 的明确数据所有者和只读访问协议；
4. Node 全局限流 `429/RATE_LIMITED`、Redis 不可用 `503` 与公开投影防泄露的运行或端到端基线；
5. Go 候选与 Node 对同一脱敏请求的 contract replay 通过，且 Gateway 文件开关能够逐条回退两个精确路径。

### 已完成：匿名 Catalog GET 的离线契约回放

当前已完成的仅是匿名 Catalog GET 的离线契约回放工具。它覆盖 `/api` 与 `/api/v1` video templates 的公开响应契约；同时覆盖 `/api/homepage/content?catalog=contract-replay` 的条件缓存协议：先读取 baseline ETag，再向两个 upstream 发起条件 GET，并比较 `304` 的空正文与唯一 ETag。

回放器已在本地合同测试中锁定以下能力：所有 video 与 `homepage-content-etag` 用例都要求唯一非空 `X-Cache-Status`；video 用例禁止 ETag，`homepage-content-etag` 额外要求唯一非空且初始两端相同的 ETag，并要求双方第二阶段都返回空正文 `304`。`X-Data-Stale` 必须在两端同时缺失或同时为唯一值 `1`；独立缓存的 `X-Cache-Status` 可分别为 `hit` 或 `miss`，不要求值相同。回放只比较首次 HTTP 响应，不跟随重定向，也会清空调用方 Cookie Jar、Authorization、Cookie 与 Proxy-Authorization。这些是工具的本机自动化证据，不是 Node 与未来 Catalog 服务的真实回放结果。

若首次响应是重定向，`Location` 必须在两端同时缺失，或各自恰好出现一次且值完全一致；回放失败只报告字段类别，不回显跳转目标。这样可避免候选服务把匿名调用导向不同地址，却被误判为等价响应。

该工具不验证 `optionalAuth`、审核策略、限流、真实 MongoDB 数据、SSE 或任何写操作。它不构成 Gateway 对 Catalog、Realtime 或 Generation 的分流、迁移许可；迁移矩阵中仍为“待核实”的条目必须继续由 Node 处理。

已登记的请求边界语料只用于未来在受控环境中对同一匿名请求执行 Node 与候选服务的回放比较；它不是已验证的 Node 生产结果。未取得真实样本、`optionalAuth`/App scope、缓存、`429`/`503`、数据所有权和精确回退证据前，仍不得分流。

当前本机密钥本显示 `mongodb-backend` 仅能在 backend runtime 注入。不得猜测、复制或以 Go 服务直连同一 MongoDB；数据边界必须通过受管配置和独立只读验证明确后才可实现 `internal/data` 仓储。

### Realtime 候选

Realtime 候选为 `GET /api/users/me/generations/stream`。浏览器 `EventSource` 用 query token，Node 仅在没有 Authorization 时把它改写成 Bearer；因此 header 优先级、JWT、账号状态和 App scope 都属于对外合同。

静态代码证明正常路径会发送 `snapshot`，事件会发送 `change`，并在 change 后绕过缓存刷新 snapshot。但订阅在首个 snapshot 之前创建，所以刚建连时可能先收到 change。当前 Node 没有 snapshot-first barrier，不能将“首帧恒为 snapshot”当作已验证事实。

Realtime Service 的准入条件为：

1. 以路由级测试或受控运行捕获 header token、query token、二者同时存在、无效 token、账号禁用和 App scope 冲突的 HTTP/SSE 行为；
2. 捕获实际 framing：`text/event-stream`、`data: <JSON>\n\n`、keep-alive、`X-Accel-Buffering: no`、心跳、snapshot 读取错误的 transient error；
3. 验证正常 `snapshot → change → 新 snapshot`、建连竞争窗口、重复 snapshot 抑制、断线关闭、重连以及 HTTP overview/12 秒轮询补偿；
4. 验证 Redis 可用与不可用时的跨进程 fan-out、去重和从持久化 snapshot 重建，Redis Pub/Sub 不得承担持久队列职责；
5. 对首帧竞态作出显式业务决定：保持当前 Node 语义，或另立产品变更后实施 snapshot barrier。未完成决定前，Gateway 必须回退 Node。

### 已完成：离线 SSE 帧结构校验

`internal/realtimecontract` 仅对调用方已捕获的 HTTP 状态、四个公开响应头和 SSE 帧结构做离线校验，可接受 LF/CRLF 与任意读取分片拼接后的字节流，并明确允许 `change` 先于 `snapshot`。该工具不创建网络连接、不访问 Redis/Mongo、不注册路由，也不比较事件载荷；它不构成 Realtime 服务迁移、Gateway 分流或任何生产兼容性的许可。

## 阶段 3：Generation Orchestrator

### Animate 切片已取消

本文件最初以 Animate 作为最小迁移候选。后续产品已决定停止 Animate 新建和重试，并取消该 Go 迁移切片；这项决定不等同于现有源码已经下线。当前静态扫描仍能发现 Web `animateApi.startAnimate`、Android `startAnimate`、Node `POST /api/animate/start` 以及 `POST /api/creations/retry` 的 Animate 分支。关闭这些入口需要在可提交的主站 Git 工作树中完成，并以路由、前端和 Native 回归测试验证；在此之前，所有新建路径仍由 Node 处理，不能宣称产品已停止创建任务。以下历史说明只用于保护已有任务的 Node 只读查询、已签名 callback、去重、退款和结算收敛，绝不构成新建 `generation-orchestrator`、新增 Go 路由或切换写入方的授权。

仍在售的 T2I、I2I 与 I2V 已登记为独立的“待核实”矩阵条目：T2I/I2I 共用 `POST /api/chat/image/async`，I2V 仅限 `POST /api/chat/video` 的 `imageUrl` 分支。它们在进入任何 Go 编排迁移前，仍必须冻结数据所有者、唯一写入方、状态版本、精确路由和回滚门槛；不得复用 Animate 的历史切片，也不得把静态登记误作 Go 路由放行。

### 已完成：三项基础创作能力的离线证据门禁

`internal/generationcontract` 是纯离线的准入材料校验包，只接收调用方提供的脱敏证据清单。它不创建 HTTP 路由、不访问数据库，也不提交生成任务。当前只允许以下三种明确的产品入口与分支：

- `text_to_image`：`POST /api/chat/image/async`，`inputImages=absent`；
- `image_to_image`：`POST /api/chat/image/async`，`inputImages=1-2`；
- `image_to_video`：`POST /api/chat/video`，`imageUrl=present`。

每一种模式都必须同时具备请求校验、鉴权、账号资格、敏感限流、内容策略、结算、任务落库、幂等提交、回调收敛和退款收敛的脱敏材料引用。校验器会拒绝缺失、重复或未知的证据类型；错误也不会回显材料引用。它明确拒绝 Animate 及任何其他未列入的能力。

本地校验通过只说明证据清单的结构完整，不验证材料内容，也不构成 Go 迁移或 Gateway 切流许可。T2I、I2I 与 I2V 在取得受控运行证据、冻结唯一写入方及完整回滚方案前，必须继续由 Node 处理。

### 已完成：三项基础能力的离线领域核心

`internal/biz/generation` 现在提供不依赖 transport、数据库、钱包或 GPU/provider 的纯 Go 状态机：

- 只接受 T2I、I2I、I2V 的静态输入分支；Animate 与其他模式在领域入口被拒绝；
- T2I 只允许零张输入图片，I2I 只允许一至两张输入图片，I2V 必须有首帧图片；
- 任务只能按 `draft → submitted → processing` 演进；成功、失败、取消的回调以幂等键收敛，同一事实安全重放，冲突终态保持原事实；
- 它不注册 HTTP 路由、不写数据库、不扣费、不调用 `generation-service`，也不改变 Node 的任务、callback、退款或结算职责。

该核心是未来阶段 3 在拿到受控证据后实现仓储和 transport 适配时的可测基础，不是已经启用的 `generation-orchestrator`。当前唯一写入方和所有业务流量仍是 Node。

#### 本地证据清单检查

可复制 `docs/audit/generation-evidence-manifest.example.json`，将其中示例哈希替换为受控捕获的脱敏材料引用，再执行：

```bash
go run ./cmd/generation-evidence-check --manifest <脱敏清单文件>
```

示例默认是 T2I。I2I 必须改为 `image_to_image` 与 `inputImages=1-2`；I2V 必须改为 `image_to_video`、`POST /api/chat/video` 与 `imageUrl=present`。清单不得写入 Authorization、Cookie、原始请求正文、用户标识或任务 ID。命令只做本地 JSON 结构和证据集合校验，拒绝未知字段，也不会在失败输出中回显材料引用。

#### 已废弃的 Animate 历史审计记录

原最小候选曾包含 `POST /api/animate/start`、`GET /api/animate/status/:taskId`、`GET /api/animate/history`、`POST /api/animate/cancel/:taskId` 与 `POST /api/creations/retry` 的 `type=animate` 分支。它们固定使用 `legacy.v1`，且 execution.v2 不包含 Animate capability；这些事实仅说明为什么不能把旧合同替换为新合同。

该记录不是阶段 3 准入条件。Animate 不得新增 Go 路由、服务、数据写入方或结算协调；`generation-service` 继续独占 GPU/provider 调度，Node 继续作为存量任务事实、签名 callback、去重、退款与结算的唯一处理路径。

`POST /api/v1/internal/generation-callback` 仍是独立“待核实”条目，不能因为它历史上处理 Animate 就被候选淘汰。它必须在任何未来迁移前单独冻结 V2 HMAC、租户与任务归属、delivery/fingerprint 去重、CAS 终态竞争、outbox 重试和唯一写入方；在此之前仍由 Node 处理。

## 生产验证状态（已暂停）

当前任务只推进本地重构、离线合同和自动化测试，不连接、配置、部署或切换生产环境。
因此，调用量、Redis 跨实例行为、回调重试、账务结果和数据集合状态均不作为本轮工作的
证据来源。

后续只有在负责人单独授权生产验收时，才可补齐受管只读观测与阶段性灰度验证。届时必须
使用项目既有的受控流程，并遵守以下边界：

- 不读取或复制私钥、环境变量、容器内密钥或生产数据库内容。
- 不把静态源码、离线测试或本地合同当作生产流量、账务或 Redis 行为的替代证据。
- 每个阶段先完成精确接口的受控验证、回滚演练和记录，再评估是否允许下一阶段分流。

当前证据只能说明源码调用链与本地单元测试意图，不能证明生产调用量、Redis 跨实例
行为、回调重试、账务结果或数据集合实际状态。
