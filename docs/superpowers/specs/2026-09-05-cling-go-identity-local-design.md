# Cling Go 本地身份模块设计

## 目标

在独立本地 MongoDB `cling_main` 中实现身份领域的最小闭环：游客升级为已绑定用户、账号状态变更以及会话立即失效。该闭环只供本地单元测试和集成测试调用，不创建 HTTP 或 gRPC 业务路由，也不迁移任何现网认证协议。

## 范围与边界

本阶段实现以下业务规则：

- 用户状态只有 `normal`、`banned`、`deleted` 三种合法值。
- 游客绑定会在原 `user_id` 上新增外部身份并把绑定状态改为 `bound`；不会创建第二个用户，也不会写入注册奖励、余额或账本。
- `provider + subject` 在全局唯一。已归属其他用户的外部身份不能再次绑定。
- 用户首次写入的时区不可修改；会话保存签发时的 `session_version`。
- 封禁或删除用户会在同一 MongoDB 事务内递增 `session_version` 并撤销全部有效会话，因此旧会话即使没有被逐条读取，也会因版本不一致立即失效。

以下内容明确不在本阶段实现：密码校验、JWT 签发或兼容、OAuth 回调、登录限流、Cloudflare Guard、Push Token、HTTP 响应映射、Node 用户迁移、生产数据访问和流量切换。

## 分层设计

```text
identity.Usecase
  ├── UserRepository       用户和账号状态
  ├── IdentityRepository   外部身份唯一绑定
  ├── SessionRepository    会话签发、撤销和校验
  └── shared.TxRunner      原子绑定与账号状态变更

MongoDB 仓储（data）
  └── model.UserDocument / IdentityDocument / SessionDocument
```

- `internal/biz/identity` 仅声明领域对象、仓储接口、领域错误和用例；不导入 MongoDB Driver、PO 或传输层类型。
- `internal/data` 负责 BSON 与领域对象转换，以及将 MongoDB 重复键、缺失文档映射为身份领域错误。
- 账号状态变更和会话校验由领域层决定，MongoDB 只执行条件更新、查询和事务。

## 关键流程

### 游客绑定

1. 在事务中读取原用户，要求账号状态为 `normal` 且绑定状态为 `guest`。
2. 查询 `provider + subject`。若已绑定当前用户，则返回同一用户，保证重复请求幂等；若归属其他用户，返回领域冲突错误。
3. 写入身份文档，并用条件更新把同一用户标为 `bound`。
4. 唯一索引遇到并发冲突时，重新读取已写入身份；如果仍归属同一用户，按幂等成功处理。

### 封禁与删除

1. 在事务中以条件更新写入新状态，并递增 `session_version`。
2. 撤销该用户所有尚未撤销的会话。
3. 校验会话时同时检查撤销时间、过期时间、用户状态及版本号。任一条件不满足即为无效会话。

## 验证要求

- 单元测试覆盖游客原地升级、外部身份冲突、时区不可改、封禁和删除后的会话拒绝。
- 本地 MongoDB 集成测试覆盖同一游客的并发绑定与封禁后的会话失效。
- 测试只使用 `CLING_TEST_MONGO_URI` 指定的本地 `rs0`，并按随机生成的精确 `_id` 清理测试文档。
- 最终运行 `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./cmd/ai-business-service` 和语义契约校验。

## 迁移限制

本设计不改变 `docs/audit/identity-static-contract.json` 中 `goRouteEnabled: false` 与 `migrationPermission: 未授予` 的结论。完成本地实现不代表可以创建身份路由、接管登录或连接生产环境。
