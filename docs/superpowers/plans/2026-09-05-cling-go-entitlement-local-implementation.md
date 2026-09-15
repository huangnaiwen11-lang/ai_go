# Cling Go 本地权益策略模块实现计划

> **面向 AI 代理的工作者：** 本项目没有 Git 仓库。必须使用 TDD，不能创建提交、分支或工作树。

**目标：** 实现独立的权益策略核心，稳定计算生成门禁、固定钻石价格、VIP 每日权益和用户本地日期。

**架构：** `biz/entitlement` 是无仓储的纯领域模块。上层传入绑定状态、订阅快照、已固化时区和产品请求；模块返回门禁错误或可供后续预留事务使用的价格与权益快照。

**技术栈：** Go 1.25、Kratos Wire、标准库 `time`；本阶段不连接 MongoDB。

---

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/biz/entitlement/model.go` | 产品输出、视频选项、订阅快照、请求、决策与每日权益领域对象。 |
| `internal/biz/entitlement/errors.go` | 非法产品、时长和时区的本模块领域错误。 |
| `internal/biz/entitlement/usecase.go` | 绑定/VIP 门禁、固定计价、首月日免倍率和本地日期计算。 |
| `internal/biz/entitlement/usecase_test.go` | 不依赖基础设施的门禁、价格、VIP 与时区规则测试。 |
| `internal/biz/entitlement/provider.go` | Wire 依赖注入入口。 |
| `internal/biz/biz.go` | 将权益用例加入业务模块装配，不新增传输路由。 |

## 任务 1：建立失败的领域测试

- [x] 在 `internal/biz/entitlement/usecase_test.go` 为未绑定用户、免费用户视频限制、图片与 5/10/15 秒视频固定价格、年付首月双倍（缺失开始时间时回退创建时间）与月付不双倍编写表驱动测试。
- [x] 增加时区日界测试：同一 UTC 时刻在 `Asia/Shanghai` 与 `America/Los_Angeles` 返回各自正确的 `LocalDate`；拒绝空值、`Local` 和非法 IANA 时区；首月双倍不放大每日 50 钻。
- [x] 运行 `go test ./internal/biz/entitlement -count=1`，确认因缺少领域类型和用例实现而失败。

## 任务 2：实现纯领域权益策略

- [x] 在 `model.go` 定义仅面向产品的图片/视频输出枚举、视频选项、绑定与订阅快照、生成决策和每日权益；不得引入技术中台原子、余额、MongoDB 或支付字段。
- [x] 在 `errors.go` 定义非法输出、非法视频时长和非法时区错误。
- [x] 在 `usecase.go` 实现 `EvaluateGeneration`：先校验绑定，再校验产品与时长，随后执行免费用户视频 VIP 门禁，并返回固定价格。
- [x] 在 `usecase.go` 实现 `ResolveDailyBenefits`：根据已固化时区计算本地日期；仅有效年付订阅前 30 天把图片/视频日免从 10/3 加倍为 20/6，日赠钻石保持 50。
- [x] 运行 `go test ./internal/biz/entitlement -count=1`，确认领域测试通过。

## 任务 3：接入业务装配但不暴露接口

- [x] 更新 `provider.go` 以注册 `NewUsecase`。
- [x] 更新 `internal/biz/biz.go`，把 `entitlement.ProviderSet` 纳入 Wire；不修改 `api`、`internal/service`、`internal/server` 或 Node 文件。
- [x] 运行 `go generate ./cmd/ai-business-service`，只由生成器更新 `cmd/ai-business-service/wire_gen.go`。

## 任务 4：全量复核

- [x] 运行 `go test ./... -count=1`、`go test -race ./... -count=1`、`go vet ./...`、`go build ./cmd/ai-business-service` 和 `go run ./cmd/semantic-contract-check --cases docs/contracts/cling-main/semantic-cases.json`。
- [x] 用静态检索确认权益模块未新增 HTTP/gRPC 路由、MongoDB 访问、余额判断、预扣写入、Node 修改或中台技术原子字段。
