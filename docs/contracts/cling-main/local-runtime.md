# Cling 主站本地运行环境

本地环境只使用独立 MongoDB 数据库 `cling_main`。它与现有 Node 的 MongoDB 数据库、连接串、集合和业务数据完全隔离。

## 为什么必须使用副本集

任务占位、每日额度或钻石预扣、账本记录和 Outbox 事件会跨多个集合写入。为防止并发双免和部分成功，本地 MongoDB 必须作为单节点副本集 `rs0` 启动，不能使用 standalone 模式。

## 启动与检查

```bash
docker compose up -d mongo mongo-init
docker compose ps
docker compose up -d --build app
curl http://127.0.0.1:18000/healthz
```

`mongo-init` 只初始化本地副本集，不创建或迁移 Node 的任何集合。应用后续只允许连接 `cling_main`；配置校验会拒绝其他数据库名、缺失副本集或关闭事务的配置。

宿主机运行 Go 测试或 `go run` 时使用 `configs/config.yaml`，它通过
`127.0.0.1:27017` 的本机绑定和 `directConnection=true` 访问副本集。运行
`docker compose up app` 时，Compose 会自动挂载 `config.docker.yaml`，并改用
Docker 网络内的 `mongo:27017`。两种配置均只连接 `cling_main`。

配置参数必须明确指向单个文件，例如：

```bash
go run ./cmd/ai-business-service -conf ./configs/config.yaml
```

不要把整个 `configs/` 目录传给 `-conf`，目录中包含示例文件，不属于运行时配置。

## 本地配置

从 `configs/config.local.yaml.example` 复制本地配置时，替换两个本地开发密钥。不得将真实会话密钥、回调密钥、支付凭证、生产 URL 或 Node MongoDB 连接串写入仓库。
