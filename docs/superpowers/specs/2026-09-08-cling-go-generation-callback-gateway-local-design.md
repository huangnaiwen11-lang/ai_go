# Cling Go 生成回调 Gateway 本地接入设计

## 目标

在本地 Go Gateway 中仅接入已完成的生成中台回调 Handler，使其处理两个既有内部回调路径。所有未命中请求继续按当前逻辑代理到 Node，保证现有业务语义不变。

## 范围

本阶段只接入以下精确请求：

- `POST /api/v1/internal/generation-callback`
- `POST /api/internal/generation-callback`

命中请求交给 `internal/transport/generationcallback.Handler`。该 Handler 已负责原始 Body 限制、V2 HMAC 验签、回调状态机和固定错误响应。

## 非目标

- 不接入公开生成 API，不替换 Node 的创建任务入口。
- 不修改 Node/JS、PayCores、现网钱包、生成中台、Nginx、生产配置或回调 Origin。
- 不启动 Worker、后台循环或额外 HTTP Server。
- 不处理 Animate，也不新增任意业务原子。

## 路由规则

Gateway 在默认 Node 代理之前进行精确匹配：

1. 路径必须是两条允许路径之一。
2. 方法必须是 `POST`。
3. 请求不得包含 query，也不得通过编码路径、重复斜杠、点路径或路径清理绕过精确匹配。
4. 命中后调用 Go 回调 Handler；未命中时保持既有代理行为。

Gateway 不重复实现验签和业务逻辑。回调 Handler 仍是唯一的 HTTP 适配器，业务状态、账本冲正和 Mongo 原子写入继续位于既有分层中。

## 错误与幂等

精确命中的请求由回调 Handler 返回固定 JSON 响应。相同受控终态的回放返回幂等成功；终态事实冲突返回固定短错误码。未命中的请求不由本功能改变响应，由现有 Node 代理处理。

## 本地验收

- Gateway 单元测试覆盖两个路径的命中、错误方法、query、编码路径和未命中回退代理。
- `httptest` 覆盖经 Gateway 到回调 Handler 的验签与固定响应。
- 本机 `rs0` 回归确认首帧回调仍可原子激活第二步 I2V Outbox。
- 全量测试、竞态检查、`go vet`、构建和语义契约全部通过。
- 范围扫描确认未修改 Node/JS、生产配置、公开 API 路由或 Worker 启动逻辑。
