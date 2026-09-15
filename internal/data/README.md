# Data 层

`data` 实现 `biz` 声明的仓储接口，并拥有持久化对象（PO）与存储客户端。

- 不依赖 `service` 或 API DTO。
- 每个仓储只操作其所属业务模块的表；不得跨模块直接读写数据。
- MongoDB 连接和 `shared.TxRunner` 已落位，且只连接独立数据库 `cling_main`。本地 MongoDB 必须以副本集运行，才能支持多文档事务。
- 业务层不能取得 MongoDB Client 或 Collection；跨集合的占位、预扣和 Outbox 写入必须经 `shared.TxRunner` 完成。
- `LocalSchemaInitializer` 在本地独立 `cling_main` 的启动前声明 16 个集合和 17 个二级索引；`accounts` 以 `_id=user_id` 保存钻石余额，不创建 Node 集合或钱包数据。
