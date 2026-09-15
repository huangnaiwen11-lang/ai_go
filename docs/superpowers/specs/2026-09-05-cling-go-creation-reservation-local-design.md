# Cling Go 本地创作占位与预留编排设计

## 目标

在独立本地 MongoDB `cling_main` 中建立“创作占位先于结算”的最小原子闭环。用户选择模板后，服务在同一个 MongoDB 事务内校验账号和权益、创建用户可见创作事实与内部执行步骤，再占用 VIP 日免或预扣本服务自有钻石。

提交生成中台、接收生成回调、发件箱投递、支付入账和每日 50 钻发放不属于本阶段。它们必须消费本阶段已提交的稳定创作事实，不能反向修改本阶段的原子边界。

## 范围与不变量

### 本阶段实现范围

- 创建 `creations` 中的用户可见创作占位。
- 创建 `creation_steps` 中的内部原子步骤计划。
- 在同一事务内执行账本日免占用或钻石预扣。
- 通过幂等键和请求指纹收敛用户重试与 MongoDB 事务重试。
- 在领域层拒绝 Animate 和未知原子；仅允许文生图、图编辑、图生视频。
- 使用现有身份、权益和账本模块的领域能力，不新增 HTTP 或 gRPC 业务路由。

### 明确不做

- 不调用生成中台，不写入中台地址、Token、钻石、VIP、余额或现网钱包字段。
- 不创建回调接收器、不校验 HMAC、不写 `callback_receipts`，不实现回调状态迁移。
- 不创建 Outbox 事件或后台投递器，不把 `pending_submission` 误标为已提交。
- 不修改 Node/JS、PayCores、商店内购、生成中台、现网 MongoDB、现网钱包或生产配置。
- 不实现每日 50 钻发放、充值入账、订阅写入、账户开户、余额查询或账单查询。
- 不注册面向客户端的 T2I、I2I、I2V 或 Animate 路由；用户面仍然只选择模板。

### 必须保持的不变量

1. 创建占位、步骤、日免占用或余额预扣、预留事实和账本分录必须一起提交或一起回滚。
2. 不得先读取日免或余额，再据此决定是否创建任务。日免与余额仅能通过账本仓储的条件更新占用。
3. 同一幂等键的同一请求最多生成一份创作、一组步骤、一条预留和一条预留账本分录。
4. 同一幂等键对应不同业务命令时必须冲突，不能把不同请求收敛为成功。
5. 余额为 0 但 VIP 日免仍可用时，必须允许创建；余额不足只在日免未命中时返回 `INSUFFICIENT_FUNDS`（HTTP 402）。
6. 游客必须在原用户上完成绑定后才能创建；不能因为创建请求新建用户。
7. 封禁和删除用户不能创建；登录态即时失效仍由身份模块的会话版本和撤销机制负责。

## 模块边界

```text
creations.Usecase（唯一的本地创建编排入口）
  ├── identity.UserRepository：读取当前用户状态、绑定状态与已固化时区
  ├── entitlement.Usecase：执行产品门禁、固定计价与每日额度计算
  ├── creations.Repository：写入创作占位和步骤计划
  ├── ledger.Usecase：在既有事务中占用日免或预扣钻石
  └── shared.TxRunner：建立整个创建命令的 MongoDB 事务
```

- `creations` 只负责用户可见创作事实、内部步骤计划和总事务编排；不认识 MongoDB Collection、HTTP、支付或中台协议。
- `ledger` 继续拥有预留、冲正和没收状态机。它不知道模板、步骤、中台地址、VIP 身份或支付渠道。
- `entitlement` 继续只计算规则，不读取余额、不执行扣款，也不暴露技术原子名称。
- `identity` 继续拥有账号状态、游客绑定、时区和会话状态。
- `generation` 后续只接受已经创建的内部步骤，不接收钻石、余额、VIP 或用户可见的模板模式。

`creations` 依赖其他模块的窄领域接口，而非 `data` 实现。`data` 只实现这些接口并把同一个事务 `context` 传给每次 MongoDB 调用。

## 领域模型与持久化补充

### 创作状态

本阶段只创建以下初始状态：

| 对象 | 初始状态 | 含义 |
| --- | --- | --- |
| `creation` | `pending_submission` | 占位和预留已提交，尚未向生成中台发送请求。 |
| 首个可执行 `creation_step` | `ready` | 后续提交器可选择该步骤发起中台请求。 |
| 依赖首帧的后续 `creation_step` | `blocked` | 必须等待前序步骤产出首帧资产后才可提交。 |

本阶段不允许把上述状态改为 `submitted`、`processing`、`succeeded`、`failed` 或审核终态；这些状态只能由后续提交与回调编排阶段通过版本化 CAS 推进。

### 创作请求

`creations.CreateReservedRequest` 是内部应用命令，不能直接等同于未来 HTTP 请求。它包含：

- `user_id`、`idempotency_key`；
- `template_id`、`template_version`；
- 已由服务端模板解析器生成的内部步骤计划；
- 用户可见的图片或视频能力参数，例如视频时长、音频开关和引用图数量；

`CreateReservedRequest` 不包含订阅、VIP、余额或任何可提升权益的字段。创作用例在事务内通过窄 `subscriptionReader` 从服务端自有订阅投影读取快照；缺失快照表示普通用户，读取错误原样返回。任务 4 只会在本地 MongoDB 的自有订阅读模型中实现该 reader，绝不回退为由请求参数或客户端携带订阅快照。

### 请求指纹

`CreationDocument` 增加 `request_fingerprint` 字段。它保存由以下稳定字段按固定顺序序列化后计算的 SHA-256 摘要：用户 ID、模板 ID、模板版本、用户可见能力参数、内部步骤计划，以及提示词和输入素材引用各自的摘要。

指纹不保存提示词、图片 URL、Cookie、Authorization、支付信息或原始请求正文。提示词和素材引用先单独摘要，再参与请求指纹计算；这样同一幂等键携带不同提示词或不同素材时会冲突，同时不会把敏感载荷复制到创作索引。

`idempotency_key` 继续使用既有全局唯一索引。重复键发生后，仓储必须重新读取既有创作并由领域层比较用户和指纹：完全一致时返回既有创作；任一字段不同则返回冲突。

### 原子步骤计划

步骤计划由服务端模板版本静态决定，不由客户端传入：

| 模板产品结果 | 合法内部计划 |
| --- | --- |
| 图片模板 | 单步 `text_to_image` 或单步 `image_edit`。 |
| 有输入首帧的视频模板 | 单步 `image_to_video`。 |
| 文生视频模板 | 第 1 步 `text_to_image`，第 2 步 `image_to_video`；第 2 步初始为 `blocked`。 |

`image_edit` 是模板图编辑能力，例如换装、脱衣等模板，不是泛化的图生图入口。`Animate`、空计划、重复序号、非连续序号、未知原子及不符合上述依赖关系的计划都必须在领域层拒绝。

`CreationStepDocument` 继续使用现有 `sequence` 表达顺序，初始 `external_execution_id` 为空，`callback_version` 为 `0`。本阶段不产生任何外部执行 ID 或回调写入。

## 原子创建流程

### 创建成功路径

```text
CreateReservedRequest
  → 读取同 idempotency_key 的既有创作
  → 命中且指纹一致：直接返回既有事实
  → 未命中：在 shared.TxRunner.WithinTx 内执行
      → 读取用户并确认 normal
      → 从自有订阅投影读取快照，再用绑定状态、时区执行 entitlement 门禁与计价
      → 解析当天权益，生成 quota_kind / local_date / quota_limit / quota_units
      → 写入 creation（pending_submission，version=1）
      → 写入 creation_steps（ready 或 blocked）
      → 调用 ledger.ReserveInTx 条件占用日免或预扣钻石
  → 事务提交后返回创作与按序步骤；预留是同一事务内的结算事实
```

写入顺序体现“先占位、再预扣”的业务语义；由于操作位于同一事务，外部读取方只能看到完整提交后的事实，绝不会看到未结算的孤立占位。

### 账本事务适配

现有 `ledger.Usecase.Reserve` 自行开启事务，无法与创作写入构成同一原子边界。账本模块将拆出事务内入口：

```go
// ReserveInTx 只使用传入的事务 context，不另行创建事务。
func (usecase *Usecase) ReserveInTx(ctx context.Context, request ReserveRequest) (*Reservation, error)
```

`Reserve` 保持对外行为不变：它仍以 `WithinTx` 包装并调用 `ReserveInTx`。`creations.Usecase` 则拥有外层事务并在相同 `txCtx` 中调用 `ReserveInTx`。这样账本的条件占用、预留和账本分录仍由账本模块维护，但能与创作事实一起提交。

MongoDB 可能重试事务回调，因此创作 ID、步骤 ID、请求指纹和业务时刻必须在进入事务前稳定生成；账本预留 ID 与账本幂等键必须在事务前固定，或由事务前固定的 `CreationID` 确定性派生。回调内不得生成随机 ID 或重新读取系统时间。创作用例只读取一次 UTC 业务时刻，并将它同时传给创作时间戳、权益门禁和每日额度计算，保证重试不跨本地日界或订阅到期边界。

### 并发与重复键收敛

两个相同命令并发时都可能在第一次读取中未命中。两者随后竞争 `ux_creations_idempotency_key`：

1. 获胜事务提交创作、步骤和账本事实。
2. 另一事务因唯一键失败并整体回滚，不能留下日免、钻石或步骤写入。
3. 用例在事务外重新读取获胜创作，比较用户和请求指纹。
4. 一致则返回同一创作；不一致则返回创建命令冲突。

不得使用“已创建但预留稍后补偿”的两阶段模式，也不得依赖读取余额或日免判断并发胜负。

## 门禁、计价与额度映射

创建用例在事务内读取当前用户和自有订阅投影，并构造权益模块所需的快照：

- 用户不是 `normal`：拒绝创建，不写任何事实。
- 用户仍是游客：返回 `ACCOUNT_BINDING_REQUIRED`（HTTP 403）。
- 视频为 10 秒或 15 秒、开启音频或使用多张引用图且用户不是有效 VIP：返回 `VIP_REQUIRED`（HTTP 403）。
- 图片固定价格为 20 钻；视频 5 / 10 / 15 秒分别为 50 / 100 / 150 钻。
- 基础 VIP 每日图片上限为 10、视频上限为 3 个单位；有效年付订阅首月按现有权益规则翻倍为图片 20、视频 6 个单位。
- 日免种类分别为 `vip_daily_image` 和 `vip_daily_video`。图片每次占用 1 个单位；视频保持现网按时长单位结算的语义，5 / 10 / 15 秒分别占用 1 / 2 / 3 个单位。视频日免上限 3 表示每天最多 3 个 5 秒单位，不表示所有时长都一律占用 1 个单位。日免不足时才尝试条件预扣全额钻石，绝不拆分为部分日免加部分钻石。

每日赠钻固定为 50 钻，是独立的权益发放和账户入账职责，年付首月不翻倍。本阶段只消费 `accounts` 中已经存在的自有余额；VIP 在日免仍可用时即使余额为 0 也可创建。

## 错误与副作用

| 情形 | 领域结果 | HTTP 语义 | 持久化副作用 |
| --- | --- | --- | --- |
| 游客未绑定 | `ACCOUNT_BINDING_REQUIRED` | 403 | 无。 |
| 免费用户请求 VIP 视频能力 | `VIP_REQUIRED` | 403 | 无。 |
| 日免耗尽且余额不足 | `INSUFFICIENT_FUNDS` | 402 | 无。 |
| 封禁、删除或不存在用户 | 账号不可用错误 | 后续鉴权合同定义 | 无。 |
| 模板计划非法或含 Animate | 非法创作命令错误 | 后续路由合同定义 | 无。 |
| 同幂等键但命令不同 | 创作命令冲突错误 | 409 | 无新增事实。 |
| MongoDB 任意写入失败 | 原始错误向上返回 | 后续路由合同定义 | 整体回滚。 |

现有三种生成门禁错误的 HTTP 语义不得改变或合并。中台提交失败、技术失败回调的冲正和审核没收仍由后续阶段实现：失败必须调用账本 `Reverse`，审核没收必须调用 `Confiscate`，两者不能在本阶段提前模拟为本地完成。

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/biz/creations/model.go` | 创作命令、创作状态、步骤计划、指纹输入和返回领域对象。 |
| `internal/biz/creations/errors.go` | 创作命令冲突、非法计划和账号不可用等领域错误。 |
| `internal/biz/creations/repository.go` | 创作和步骤的反转依赖接口。 |
| `internal/biz/creations/usecase.go` | 门禁、计划校验、事务编排、幂等收敛。 |
| `internal/biz/creations/usecase_test.go` | 内存仓储和事务模拟下的核心行为测试。 |
| `internal/biz/creations/provider.go` | 创建用例的 Wire 注册。 |
| `internal/biz/ledger/usecase.go` | 拆出 `ReserveInTx`，保留原有 `Reserve` 行为。 |
| `internal/data/creation_repository.go` | MongoDB 的创作、步骤读取与事务写入实现。 |
| `internal/data/creation_repository_test.go` | 本地 `rs0` 的事务、幂等和回滚集成测试。 |
| `internal/data/model/creations.go` | 增加创作请求指纹的 BSON 字段。 |
| `internal/data/model/model_test.go` | 锁定新增 BSON 字段名。 |
| `internal/data/data.go`、`internal/biz/biz.go` | 注册仓储和用例，不注册业务传输层。 |

不新增集合或二级索引：`creations.idempotency_key`、`creation_steps.creation_id + sequence` 和账本既有唯一索引已覆盖本阶段的幂等需求。

## 测试与本地验证

### 领域单元测试

- 已绑定 VIP 且余额为 0、日免可用时：创作、首步、预留和 0 钻账本分录一起出现。
- 日免耗尽、账户余额 20、图片价格 20 时：一次创建后余额为 0；同键重试不再扣款、不重复创建步骤。
- 日免耗尽、余额不足、游客未绑定、非 VIP 超能力视频、封禁用户和非法 Animate 计划：均不留下任何创作、步骤、预留、额度或账本事实。
- 同一幂等键不同模板版本、视频能力参数或步骤计划：必须冲突且不改动首个命令的事实。
- 文生视频计划创建 `text_to_image` 的 `ready` 首步与 `image_to_video` 的 `blocked` 后续步骤。
- 模拟步骤插入或账本预留失败：验证此前写入的创作和步骤随事务回滚。

### MongoDB 集成测试

测试只连接本地单节点副本集 `rs0`：

```bash
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true'
```

每个测试使用随机创作、用户、预留和步骤 ID。清理仅对这些精确 `_id` 执行逐条 `DeleteOne`；禁止 `DeleteMany`、空过滤删除、删除集合、删除数据库或连接生产环境。

最终实现完成后执行：

```bash
go generate ./cmd/ai-business-service
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./... -count=1
CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test -race ./... -count=1
go vet ./...
go build ./cmd/ai-business-service
```

## 后续衔接

下一阶段才可在本设计之上定义中台提交与自有回调编排：提交器只读取 `ready` 步骤；请求必须带本服务回调地址；请求不携带钻石、VIP 或余额；中台明确提交失败和技术失败回调立即调用 `ledger.Reverse`；审核没收调用 `ledger.Confiscate` 且不退款。支付和每日钻石发放继续独立于生成提交与回调。
