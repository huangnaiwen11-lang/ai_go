# Cling Go 本地账本与预留模块设计

## 目标

在独立本地 MongoDB `cling_main` 中实现创作结算的最小原子闭环：先占用 VIP 日免或预扣自有钻石，再创建一条预留事实和一条不可变账本分录。模块还提供失败冲正与审核没收的领域操作，供后续生成编排和回调模块调用。

本阶段不创建创作占位、不提交生成中台、不接收回调、不发放每日钻石、不对接 PayCores 或商店内购，也不注册 HTTP/gRPC 业务路由。

## 现网语义基线

- 不能先读取余额或日免后再决定是否创建任务。日免条件占用、余额预扣、预留事实和账本分录必须在同一个 MongoDB 事务中完成。
- 余额不足必须返回既有 `INSUFFICIENT_FUNDS`（HTTP 402），且不能留下预留、账本或额度写入。
- VIP 在剩余日免存在时，即使自有钻石余额为 0，也能完成日免预留。
- 最后一份日免只能被一个并发请求占用；另一请求须回退到自有钻石预扣，余额不足时返回 402。
- 中台明确提交失败、以及后续技术失败回调，都必须调用同一冲正操作。已预扣钻石立刻返还；已占日免释放；冲正幂等，不能重复返还。
- 审核没收只将预留标记为 `confiscated`，不返钻、不释放日免、不新增冲正分录。
- 当前 Node 的视频日免以时长单位处理。为保持这个扩展点，预留请求显式携带 `quota_units`；必须一次完整占用，不能把同一次创作拆成“部分日免 + 部分钻石”。图片通常传入 1，视频由后续生成编排按现网策略传入。

## 余额聚合与账本分录

原有 14 个集合不足以并发安全地保存可用钻石余额：若只对 `ledger_entries` 求和，两个并发事务都可能读到同一余额并各自插入扣款分录，导致透支；把余额字段写入 `users` 又会让账本模块侵入身份模块的持久化边界。

因此新增本服务自有的第 15 个集合 `accounts`：

```text
accounts（可用余额聚合，_id = user_id）
  ├── diamond_balance：仅用于条件扣减和展示投影
  ├── created_at / updated_at
  └── 不记录 Node 钱包 ID、Node 余额或支付渠道信息

ledger_entries（不可变资金/权益审计）
  └── 每次预扣、日免占用、冲正各写一条幂等分录
```

`accounts` 只依赖 MongoDB 自带的 `_id` 唯一性，因此声明的二级索引仍为 17 个。账户的创建与充值入账属于后续身份初始化和支付模块职责；本阶段不懒创建并充值。日免预留不依赖账户文档，付费预留则要求本服务账户已存在且余额足够。

## 分层与原子流

```text
后续创作编排
  └── ledger.Usecase.Reserve
        └── shared.TxRunner.WithinTx
              ├── 尝试 daily_quotas 条件递增（完整 quota_units）
              ├── 未命中日免时，对 accounts 条件扣减 diamond_balance
              ├── 插入 reservations（每 creation_id 唯一）
              └── 插入 ledger_entries（预留分录）

后续提交失败 / 技术失败回调
  └── ledger.Usecase.Reverse
        └── 同一事务内反向释放来源并追加冲正分录

后续审核回调
  └── ledger.Usecase.Confiscate
        └── 仅迁移 reservation 状态，不退款、不释放额度
```

- `internal/biz/ledger` 拥有预留命令、状态机、仓储接口和用例；它不导入 MongoDB、身份、权益、生成或支付模块。
- `internal/data/ledger_repository.go` 实现条件更新、BSON 转换和存储错误映射；所有操作必须使用事务 context。
- `shared.TxRunner` 是业务层发起事务的唯一入口。固定的 reservation ID 与账本幂等键在进入事务前生成，保证 MongoDB 重试回调不会产生重复事实。
- 上层必须传入已由权益模块计算的 `price_diamonds`、`quota_kind`、`local_date`、`quota_limit`、`quota_units`；账本模块不自行解析模板、VIP 或视频时长。

## 状态与幂等性

| 操作 | 前置状态 | 结果状态 | 资金与额度影响 |
| --- | --- | --- | --- |
| 预留 | 无同 `creation_id` 预留 | `reserved` | 日免：条件递增并写 0 钻分录；付费：条件扣减余额并写负钻分录。 |
| 重复预留 | 已有同 `creation_id` 预留且命令一致 | 保持原状 | 返回既有预留，不再扣费或占额。 |
| 冲正 | `reserved` | `reversed` | 日免递减原 `quota_units`；付费加回 `reserved_diamonds`；均追加一次冲正分录。 |
| 重复冲正 | `reversed` | 保持原状 | 返回既有预留，不新增分录。 |
| 审核没收 | `reserved` | `confiscated` | 不修改余额、日免或账本。 |
| 已冲正后没收 / 已没收后冲正 | 非 `reserved` | 拒绝 | 不产生额外资金变动。 |

预留命令重复但用户、价格、额度来源上下文不一致时返回冲突错误，而不是把不同请求错误收敛为成功。

## 持久化字段

在现有 `ReservationDocument` 基础上补充 `user_id`、`quota_kind`、`local_date`、`quota_units`、`created_at`、`updated_at`，使冲正能够只依赖预留事实精确释放原始日免，且不重新计算当前时区或当前 VIP 额度。

`accounts` 文档只包含 `_id`、`diamond_balance`、`created_at`、`updated_at`。所有支付回调、注册奖励和现网钱包字段均明确不属于该文档。

## 测试策略

- 领域单元测试使用内存仓储和事务模拟，覆盖零余额日免、余额不足无副作用、已存在预留幂等、付费冲正一次、日免冲正一次、审核没收不退款及非法状态迁移。
- 本地 `rs0` 集成测试用随机 `_id` 写入 `accounts`、`daily_quotas`、`reservations`、`ledger_entries`；并发抢最后日免时断言恰好一份预留和一条日免分录，另一个请求返回 `INSUFFICIENT_FUNDS`。
- 清理仅对每个随机 `_id` 执行 `DeleteOne`，不得清空集合、删除数据库或连接生产 MongoDB。

## 明确不做

- 不把本模块作为对外钱包 API，不迁移 `/api/wallet/*`，不读取或修改 Node 钱包。
- 不实现充值、支付回调、每日 50 钻发放、账户开户路由、账户余额查询路由或账单列表。
- 不创建 `creations`、`creation_steps`、Outbox 或生成请求；后续创作模块将把预留与创作占位组合成更大的原子事务。
- 不调用中台，不携带钻石、VIP 或余额给中台。
