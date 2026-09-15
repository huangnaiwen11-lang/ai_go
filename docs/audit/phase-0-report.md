# Phase 0：使用面审计与契约冻结报告

## 当前状态

- 审计对象：同级项目 `ai-host-v2-platform-main` 的主站、管理后台及其源码中的 API 调用候选。
- Go 基座：本项目已存在，为 Kratos 服务模板；`api/todo/v1` 仅为参考示例，不属于业务迁移对象。
- 生产实现：审计期间仍以 Node backend 为唯一事实实现。
- Git：本阶段不使用 Git、worktree、commit 或 push。

## 已验证基线

当前 HTTP 默认监听 `0.0.0.0:8000`，gRPC 默认监听 `0.0.0.0:9000`；这些端口仅属于本地模板基线，不得直接替代主站 `/api` 入口。

执行 `scripts/phase0-audit.mjs` 后，静态扫描得到 479 个 API 候选、54 个后台页面。三份生成清单中的条目全部标记为“待核实”：静态调用、路由注册或页面存在只能构成候选证据，不能证明鉴权、数据依赖、错误语义、业务责任或生产使用量。扫描器识别既有 `api.*` / `client.*` 调用，以及 URL 和 method 均可静态确认的同源 `/api` `fetch`、`window.fetch`、`globalThis.fetch` 与 `EventSource` 调用形态。该轻量词法扫描不做 TypeScript 作用域绑定解析，因此名称形态匹配不等同于运行时全局对象证明；动态 URL、动态请求配置、外部地址和运行时环境变量拼接仍须人工核验后再登记到权威矩阵，不能以不完整的文本解析冒充完整调用面。管理端页面额外记录了当前页面文件的直接 API import 与统一权限守卫，仍不含子组件或运行时调用。生成结果位于 `docs/audit/api-inventory.generated.json`、`docs/audit/admin-page-inventory.generated.json` 和 `docs/audit/admin-page-api-permission-inventory.generated.json`。

## 入口契约

- 用户端和管理后台继续从 `/api` 进入。
- 管理后台浏览器入口保持 `/admin/`。
- 成功 envelope、错误字段 `code/message/details/requestId`、鉴权头和 SSE 事件语义必须保持不变。

## 放行条件

阶段 1 仅条件放行矩阵中的 `GET /api/growth/ping`：该接口具备 Node 路由、Node 全局限流准入、Gateway 契约测试和本机冻结成功响应字节级基线证据。开关启用时，Gateway 仍先请求 Node，精确消耗 Node 全局限流配额；只有 Node 返回未编码 200 且正文逐字节等于冻结基线时才本地回放。Node 返回 429/RATE_LIMITED、响应编码或正文偏离基线时，均保留 Node 响应。开关为 false，或文件不可用、畸形、含未知键时，Gateway fail-closed 回退 Node。Gateway 只能消费矩阵中“确认迁移”的精确 method/path；当前仅为该 Ping 接口，不得使用路径前缀、通配或自动 `/api/v1` 扩展，所有未确认接口均回退 Node。

阶段 2 的 Catalog 两条精确路径、Realtime SSE 与内部 generation callback 仍为“待核实”。Catalog 读取尚未冻结审核策略、缓存/限流语义和 Go 侧只读数据边界；Realtime SSE 尚存在首个 snapshot 与订阅事件的竞争窗口；终态 callback 仍须完成真实 HMAC、outbox、结算和唯一写入方验证。用户明确保留的 T2I/I2I 与 I2V 新建入口也已登记为“待核实”：T2I/I2I 共用 `POST /api/chat/image/async`，I2V 仅限 `POST /api/chat/video` 的 `imageUrl` 分支，均尚未完成鉴权、扣费、状态机、回调与唯一写入方基线。

以下 7 条 `/api/animate/` 历史路由已因后续产品决策标记为“候选淘汰”：

- `GET /api/animate/cost`
- `POST /api/animate/start`
- `GET /api/animate/status/:taskId`
- `GET /api/animate/history`
- `POST /api/animate/cancel/:taskId`
- `DELETE /api/animate/:taskId`
- `POST /api/animate/refund/:taskId`

这 7 条路由不进入 Go 迁移或分流清单，也不构成 Go 放行或迁移完成；存量 Animate 继续 Node-only，Node 必须保护已有任务的历史读取、签名 callback、去重、退款与结算收敛。`POST /api/creations/retry` 仅 `type=animate` 分支为候选淘汰，其余分支不受此决定影响。上述待核实条目只用于排定审计与契约工作，绝不构成 Gateway 分流许可。

候选淘汰坚持“不删除”原则：在负责人确认、数据影响评估和可恢复方案齐备前，不删除任何 Node、React 或 Go 文件。

## 当前结论

本阶段已登记静态候选采集、入口契约冻结和候选淘汰规则；这些审计产物不等同于全部业务接口完成迁移准备。当前 API 矩阵只确认放行 growth ping 给阶段 1，任何其他业务迁移继续由契约门禁阻止。

## 验证记录

以下命令均须从 `ai-business-service` 根目录运行。

- `node --test scripts/phase0-audit.test.mjs`：验证静态候选归一、矩阵字段门禁与无正文校验报告。
- `node scripts/phase0-audit.mjs`：扫描前端和管理后台静态候选，并原子写入两个生成清单。
- `node scripts/phase0-audit.mjs --validate-matrix`：只读校验 API 迁移矩阵，不扫描源码、不重写生成清单或报告。
- `cd ../ai-host-v2-platform-main/backend && npm test -- --runInBand src/modules/chat/image-generation-submission.service.test.js src/modules/chat/image-generation-convergence.arch.test.js src/modules/chat/video-start.service.test.js src/modules/chat/chat.routes.video.batch.test.js`：验证 T2I/I2I 与 I2V 的本机提交、收敛、订阅限制和状态读取合同。
- 审计 JSON：API 候选清单、后台页面清单、API 迁移矩阵和语义黄金用例均须通过 Node JSON parser 解析。
