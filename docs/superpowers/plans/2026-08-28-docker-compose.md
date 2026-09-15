# Kratos AI Business Service Docker Compose 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 使用 Docker Compose 一次启动 Kratos AI Business Service 和隔离 MySQL。

**架构：** 多阶段 Dockerfile 构建 `cmd/ai-business-service` 静态二进制，Compose 在内部网络连接 `app` 与 `mysql`。MySQL 不映射主机端口，Kratos对外发布 8000/9000，并以 HTTP 接口作为健康检查。

**技术栈：** Go 1.25、Kratos v3、Docker Compose、MySQL 8.4

---

### 任务 1：验证现有 Docker 启动缺口

**文件：**
- 检查：`Dockerfile`
- 缺失：`compose.yaml`

- [x] **步骤 1：执行失败验证**

运行：`docker compose config`

预期：FAIL，因为项目不存在 Compose 配置。

### 任务 2：实现容器镜像和编排

**文件：**
- 修改：`Dockerfile`
- 创建：`.dockerignore`
- 创建：`compose.yaml`

- [x] **步骤 1：修正镜像构建和入口**

将 `./cmd/ai-business-service` 构建为 `/app/ai-business-service`，容器入口使用：

```dockerfile
CMD ["./ai-business-service", "-conf", "/data/conf"]
```

- [x] **步骤 2：编排应用和数据库**

配置 `mysql:8.4` 健康检查、命名数据卷和内部连接串：

```yaml
KRATOS_DATABASE_SOURCE: root:root@tcp(mysql:3306)/test?timeout=5s&parseTime=True&loc=Local&charset=utf8mb4
```

- [x] **步骤 3：验证配置**

运行：`docker compose config`

预期：PASS，输出包含 `app`、`mysql` 和 `mysql-data`。

### 任务 3：更新使用文档

**文件：**
- 修改：`README.md`

- [x] **步骤 1：修正本机入口**

记录 `go run ./cmd/ai-business-service -conf ./configs`。

- [x] **步骤 2：增加 Compose 命令**

记录启动、查看状态/日志和停止命令。

### 任务 4：端到端验证

**文件：**
- 验证：`compose.yaml`
- 验证：Go 包测试

- [x] **步骤 1：停止本机服务并构建启动**

运行：`docker compose up -d --build`

预期：镜像构建完成，`app` 和 `mysql` 启动。

- [x] **步骤 2：检查服务健康**

运行：`docker compose ps`

预期：两个服务均为 `Up` 且健康。

- [x] **步骤 3：请求 HTTP API**

运行：`curl -i 'http://127.0.0.1:8000/v1/todos/list?page_size=10'`

预期：HTTP 200。

- [x] **步骤 4：运行 Go 测试**

运行：`go test ./...`

预期：全部包通过。

