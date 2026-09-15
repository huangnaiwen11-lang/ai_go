# Cling Go 本地身份模块实现计划

> **面向 AI 代理的工作者：** 本计划在无 Git 仓库的本地项目中执行。必须使用测试驱动开发（TDD），不得创建提交、分支或工作树。

**目标：** 在不新增业务路由的前提下，为独立本地 MongoDB 实现游客绑定、账号状态和会话失效的身份领域闭环。

**架构：** `biz/identity` 持有领域对象、仓储接口和用例；`data` 以 MongoDB 实现仓储并完成 DO/PO 转换。用例通过 `shared.TxRunner` 保证绑定和状态变更的原子性，传输层保持空白。

**技术栈：** Go 1.25、Kratos、MongoDB Go Driver v2、独立本地副本集 `rs0`。

---

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/biz/identity/model.go` | 用户、外部身份、会话和枚举值等纯领域对象。 |
| `internal/biz/identity/repository.go` | 领域层的仓储接口和可识别错误。 |
| `internal/biz/identity/usecase.go` | 游客绑定、账号状态变更和会话校验规则。 |
| `internal/biz/identity/usecase_test.go` | 使用内存仓储验证领域规则，不依赖 MongoDB。 |
| `internal/data/identity_repository.go` | MongoDB 仓储及 DO/PO 转换。 |
| `internal/data/identity_repository_test.go` | 使用本地 `rs0` 验证并发唯一绑定和会话撤销。 |
| `internal/biz/identity/provider.go` | 仅注册身份用例的依赖注入入口。 |
| `internal/biz/biz.go`、`internal/data/data.go` | 将身份用例与仓储构造器纳入已有装配集合，不注册传输接口。 |

## 任务 1：冻结身份领域契约

- [ ] 在 `model.go` 声明 `User`、`ExternalIdentity`、`Session`、账号状态和绑定状态常量；所有对象仅使用 Go 类型，不带 BSON 或 DTO 标签。
- [ ] 在 `repository.go` 定义三个仓储接口：用户读取和条件更新、外部身份查询和写入、会话创建/查询/撤销；声明用户不存在、状态非法、身份冲突和会话无效等领域错误。
- [ ] 在 `usecase_test.go` 编写游客原地绑定、已占用外部身份拒绝、账号封禁和删除后会话失效的失败测试。
- [ ] 运行 `go test ./internal/biz/identity -count=1`，确认测试因尚无用例实现而失败。

## 任务 2：实现最小身份用例

- [ ] 在 `usecase.go` 实现 `BindGuest`：在事务内读取同一用户、写入外部身份、条件更新绑定状态；重复请求保留同一用户并返回成功。
- [ ] 实现 `ChangeAccountStatus`：仅允许目标状态为 `banned` 或 `deleted`，在同一事务内递增版本并撤销有效会话。
- [ ] 实现 `ValidateSession`：拒绝已撤销、已过期、账号非正常或版本不一致的会话。
- [ ] 重新运行 `go test ./internal/biz/identity -count=1`，确认领域测试通过。

## 任务 3：实现 MongoDB 仓储

- [ ] 在 `identity_repository.go` 实现三个仓储，并使用 `schema.CollectionUsers`、`schema.CollectionIdentities`、`schema.CollectionSessions`。
- [ ] 使用自由函数将 `model` 中的 BSON PO 转换为 `identity` 中的领域对象；不得把 MongoDB 类型泄露到 `biz`。
- [ ] 将 `_id` 精确缺失、唯一索引重复和条件更新不匹配映射为身份领域错误。
- [ ] 更新 `ProviderSet`，但不新增 `service`、`server` 或 `api` 文件。

## 任务 4：验证真实本地并发行为

- [ ] 在 `identity_repository_test.go` 创建随机游客和两条并发绑定协程；两次调用都必须返回同一个 `user_id`，且数据库中只有一条对应身份。
- [ ] 创建会话后封禁用户；验证数据库中的会话已撤销、用户版本加 1，且 `ValidateSession` 返回会话无效。
- [ ] 测试清理只按每个随机 `_id` 调用精确删除，禁止空条件删除、删除集合或删除数据库。
- [ ] 使用 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -count=1` 验证。

## 任务 5：全量复核

- [ ] 运行 `go generate ./cmd/ai-business-service`，只更新 Wire 自动生成文件。
- [ ] 运行 `go test ./... -count=1`、`go test -race ./... -count=1`、`go vet ./...` 和 `go build ./cmd/ai-business-service`。
- [ ] 运行 `go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json`，确认冻结语义未漂移。
- [ ] 复核不存在新业务路由、生产连接、Node/JS 修改或现网钱包调用。
