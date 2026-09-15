# Kratos AI Business Service Docker Compose 设计

## 目标

通过一条 `docker compose up -d --build` 命令启动 Kratos HTTP/gRPC 服务和独立 MySQL，使运行不依赖本机 Go 或本机 MySQL。

## 架构

- `app` 服务从当前源码构建 Kratos 二进制，监听 HTTP 8000 和 gRPC 9000。
- `mysql` 服务使用 MySQL 8.4，仅暴露在 Compose 内部网络，不映射主机数据库端口。
- `app` 使用 `KRATOS_DATABASE_SOURCE` 将配置中的 `DATABASE_SOURCE` 占位符指向 `mysql:3306`。
- MySQL 健康后才启动 Kratos；Kratos HTTP 列表接口用于容器健康检查。

## 文件职责

- `Dockerfile`：构建 `cmd/ai-business-service` 并提供容器运行环境。
- `compose.yaml`：编排 Kratos、MySQL、网络、端口和持久化数据卷。
- `.dockerignore`：限制构建上下文，排除本地生成物和编辑器文件。
- `README.md`：记录本机与 Docker 两种启动方式。

## 验证

1. `docker compose config` 验证编排配置。
2. `docker compose up -d --build` 完成镜像构建和启动。
3. `docker compose ps` 显示两个服务运行且健康。
4. 请求 `http://127.0.0.1:8000/v1/todos/list?page_size=10` 返回 HTTP 200。
5. `go test ./...` 全部通过。

