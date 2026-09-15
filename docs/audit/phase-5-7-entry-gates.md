# 阶段 5～7 准入门禁

本文冻结 Phase 5 Identity、Phase 6 Wallet/Billing 与 Phase 7 Admin 的当前静态证据、已知边界及缺失的运行证据。静态证据不是迁移许可：在每个阶段的门禁全部满足并留存可复核的真实运行证据前，不得据此创建 Go 路由、服务或流量分流。

## Phase 5：Identity

### 已完成的静态证据

- `frontend/src/api/auth.ts` 的注册、密码登录、Apple 登录和 Google 登录均依赖 `/api/auth/register` 或 `/api/auth/login` 返回认证结果；`frontend/src/api/client.ts` 随后以 Bearer 鉴权头发送该结果，并处理 `401`。
- `backend/src/modules/auth/auth.routes.js` 的注册与登录包含 Cloudflare guard、限流与锁定、请求元数据和审计；`GET /auth/me` 需要认证。Guest 仅适用于移动端；magic link、OAuth 与原生登录另有重定向、加密和账号状态语义。
- `backend/src/middleware/auth.js` 要求生产环境的 JWT 签名密钥长度至少为 `32` 个字符。本文不记录任何凭据或密钥内容。

### 仍未满足的门禁

- 留存旧认证凭据校验兼容性的真实运行 capture。
- 验证账号禁用、角色与 App scope 在所有相关入口的实际拒绝行为。
- 验证 OAuth callback 域名约束、密码策略与登录锁定的真实运行结果。
- 验证 `401` 的响应 envelope、`requestId` 关联和前端处理结果。
- 验证注册唯一性、重复请求处理和审计记录的实际一致性。
- 以上验证必须来自受管环境的真实运行 capture；当前尚无可用证据。

### 当前禁止事项

- 不得创建 Identity Service、Go 路由、写路径或任何 Gateway 分流。
- 不得以静态路由、前端调用或 JWT 配置推断线上兼容性、账号状态或审计已验证。

## Phase 6：Wallet/Billing

### 已完成的静态证据

- `backend/src/modules/wallet/wallet.routes.js` 提供 `/balance`、`/ledger` 与 `/verify-purchase`；余额、账本和退款是单一事实来源。
- PayCores 继续独占支付渠道与 webhook 边界。
- 余额读取接口具有 `private`、`no-store`、`Vary`、`X-Cache-Status` 语义，并可能返回 `X-Data-Stale`。

### 仍未满足的门禁

- 验证金额以分为单位的端到端表示，以及账本借贷、余额与退款之间的不变量。
- 验证 CAS、并发竞争、重试与幂等键下的真实写入结果。
- 验证退款、支付回调及其与 PayCores 外部边界的责任划分和失败恢复。
- 验证缓存语义、数据陈旧标识及错误 `402`、`429` 的 envelope 和客户端处理。
- 以上项目必须有真实运行验证；当前仅有静态证据。

### 当前禁止事项

- 不得创建 Wallet/Billing 服务、Go 写路径或 Gateway 分流。
- 不得从余额读取接口推断支付回调、退款、并发控制或账本一致性已经通过验证。

## Phase 7：Admin

### 已完成的静态证据

- `admin/src/auth/adminAccess.ts` 的 `canAccessAdminPath` 有以下静态分支：
  - `super_admin`：全部权限。
  - 无任何 permission scope 的 legacy `admin`：允许访问全部路径。
  - 有 permission scope 的 `admin`：才按菜单 scope 限制访问。
- `backend/src/modules/audit/audit.service.js` 的现有语义是：审计写入失败不会中断业务；本阶段不得擅自改变该语义。
- 设计顺序要求先审计 React 页面、API、权限与访问证据，保持 `/admin/` 和 Gateway `/api`，最后才评审 Vue 框架或迁移。
- 当前没有真实页面访问、E2E、Admin API 精确矩阵或高风险写入基线。

### 仍未满足的门禁

- 以上三个权限分支均须取得真实运行 capture 才能迁移；静态权限代码不构成 Admin 迁移许可。
- 冻结 React 页面、对应 API、permission scope 与实际访问证据的精确矩阵。
- 对高风险写入逐项确认授权依赖、审计可见性、真实运行结果与可执行回滚路径。
- 取得页面访问与 E2E 证据，并确认现有 `/admin/` 与 Gateway `/api` 行为不被破坏。
- 在上述证据、许可和依赖均冻结后，才可单独评审 Vue 框架或迁移方案；当前不存在此项许可。

### 当前禁止事项

- 在许可、依赖、页面证据、高风险确认、审计和回滚均冻结前，不得创建 Vue 壳层、Admin Go 路由或界面分流。
- 不得以静态权限代码或“审计失败不中断业务”的既有语义，宣称 Admin 页面、API 或高风险写入已经获得迁移许可。

## 离线证据清单工具

`cmd/migration-evidence-check` 统一校验上述三个阶段的最小脱敏运行证据清单。它只读取本地 JSON，既不访问生产环境、MongoDB、R2 或 PayCores，也不创建 HTTP 路由或写入任何业务数据。

材料引用只能使用以下两种不可逆格式：

- `sha256:<64 位小写十六进制摘要>`
- `audit:<UUID>`

不得把 Authorization、Cookie、token、签名、支付回执、请求/响应正文、用户标识或完整 Webhook 载荷放进清单。未知字段、多段 JSON、重复场景、缺失场景及非脱敏引用都会被拒绝，错误输出不会回显材料内容。

三个可验证模板分别是：

- `docs/audit/identity-migration-evidence.example.json`
- `docs/audit/wallet-billing-migration-evidence.example.json`
- `docs/audit/admin-migration-evidence.example.json`

本地执行示例：

```bash
go run ./cmd/migration-evidence-check --manifest docs/audit/identity-migration-evidence.example.json
```

校验通过只代表离线清单的格式和阶段场景完整，不能替代受管环境的真实运行证据，也不构成 Go 服务、Gateway 分流、Vue 壳层或任何流量切换的许可。

## 静态合同检查

`cmd/static-contract-check` 将当前三阶段的静态源码边界固化为可执行的 JSON 合同。它只
读取本地文件，不访问 HTTP、数据库、PayCores、R2 或生产环境。

```bash
go run ./cmd/static-contract-check --stage identity --manifest docs/audit/identity-static-contract.json
go run ./cmd/static-contract-check --stage wallet --manifest docs/audit/wallet-static-contract.json
go run ./cmd/static-contract-check --stage admin --manifest docs/audit/admin-static-contract.json
```

三个清单都固定为「待核实」和「未授予」：

- Identity 保留 `/api` 与 `/api/v1` 双前缀，覆盖注册、登录、Guest、账号读取、Push Token、注册奖励修复、magic link 与 OAuth 路由；登出仍仅清理本地 Token。
- Wallet 固定余额读取可能创建 Wallet、账本 cursor 优先于 skip、`applyMutation` 不变量，以及 PayCores 独占支付与 Webhook；同时逐条冻结 6 个主站 PayCores 代理入口、`/api` 与 `/api/v1` 的 `payment-confirmed` 回调别名、V2 HMAC 和 nonce 重放拒绝语义。
- Admin 固定 `/admin/`、`/api`、三条权限分支和「审计写入失败不阻断业务」语义。

清单中若出现未知字段、额外 JSON、Go 路由开关、Vue 壳层开关或迁移许可，命令都会
失败，且不会回显清单中的字段值。通过只表示静态材料没有漂移，不能替代阶段运行证据。
