# Biz 层

`biz` 拥有领域对象（DO）、Usecase、仓储接口和业务错误。业务规则只能在此层或其明确的子模块中定义。

- 不依赖 `service`、`data`、DTO、PO 或存储客户端。
- 每个模块在 `internal/biz/<module>/` 下维护自己的 Usecase 与仓储接口。
- 跨模块协作通过接口或 Outbox 事件完成，不能直接访问其他模块的持久化实现。
