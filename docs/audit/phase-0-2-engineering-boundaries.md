# 阶段 0 至阶段 2 工程边界

## 结论

阶段 0 至阶段 2 交付的是审计、兼容转发与匿名读取契约工具。它们用于收集和验证迁移证据，不是 Catalog、Realtime 或 Generation 的业务迁移实现。

## 阶段 0：审计 CLI 与库

`scripts/phase0-audit.mjs` 是命令行入口，负责解析参数、定位受控输入、编排静态扫描或矩阵校验，并在生成模式下原子写入审计产物。

`scripts/lib/phase0-audit-lib.mjs` 承担可复用的纯逻辑：静态调用提取、候选分类、迁移矩阵校验和不暴露正文的报告格式化。静态提取同时覆盖 `api.*` / `client.*` 与可静态确认的同源 `/api` `fetch`、`window.fetch`、`globalThis.fetch`、`EventSource` 调用形态；动态 URL、外部地址或动态请求配置仍故意不猜测。该库不做 TypeScript 作用域绑定解析，故名称形态只是一项静态候选证据，不能证明调用者实际绑定到浏览器全局对象。CLI 依赖库；库不依赖 CLI 的进程参数、文件路径或写入行为。

这项职责分离已经满足当前需求：CLI 保持薄层，库保持可单测的确定性逻辑。除非出现新的重复职责或依赖方向问题，不对这两个文件做无意义重构。

已复核：CLI 的生成模式使用同目录临时文件加原子 `rename` 写入审计产物；校验模式保持只读，不会生成审计文件。

## 阶段 1：兼容网关

`internal/gateway/` 只处理浏览器 `/api` 的兼容边界。它把未确认或未启用的请求转发到 Node；对已确认的精确低风险路由，先执行 Node 准入，再决定是否本地重放。默认 fail-closed 回退 Node，网关不访问业务数据库，也不承载领域业务。

`cmd/api-gateway/` 仅负责网关进程的启动与配置装配，不承载路由语义或业务决策。

## 阶段 2：匿名 Catalog 契约回放

`internal/catalogcontract/` 只比较匿名 GET 的外部 HTTP 契约。条件回放先获得 baseline ETag，再向两个 upstream 发送条件 GET；`304` 仅比较空正文和唯一 ETag。回放会过滤 Authorization、Cookie 与 Proxy-Authorization，禁用调用方 Cookie Jar，不注册路由、不读取 MongoDB，也不生成写流量。

`cmd/catalog-contract-replay/` 只装配固定、已审计的匿名回放用例和 upstream 参数。它根据用例类型调用普通或条件回放，不承担数据查询、用户身份伪造或 Gateway 分流。

`docs/audit/` 保存审计产物、迁移矩阵与门禁说明。静态清单只记录源码中的 API 调用候选；黄金用例分别记录证据状态、迁移资格和精确矩阵引用。只有矩阵中的“确认迁移”精确 method/path 才能进入路由放行，文档本身不是可执行的迁移开关。

## 注释原则

- 包级注释说明职责、依赖方向和禁止事项。
- 代码注释解释兼容、隐私、条件缓存或 fail-closed 等设计边界，不逐行复述实现。
- 审计文档只写已验证范围和未满足门禁，不记录生产地址、凭据、Token 或请求正文。

## 阶段 3 之前的命名约束

在阶段 3 的写入方、数据边界、回调与结算语义完成验证前，不能把阶段 0 至阶段 2 的工具称为业务迁移。迁移矩阵中仍为“待核实”的条目继续由 Node 处理；Gateway、Catalog 回放器和审计 CLI 都不构成 Catalog、Realtime 或 Generation 的分流或迁移许可。
