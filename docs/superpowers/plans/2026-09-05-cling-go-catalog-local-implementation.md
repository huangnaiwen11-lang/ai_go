# Cling Go 本地模板目录模块实现计划

> **面向 AI 代理的工作者：** 本项目没有 Git 仓库。必须使用 TDD，不能创建提交、分支或工作树。

**目标：** 为独立本地 MongoDB 的模板目录实现 SFW/NSFW 筛选、审核用户安全清单与 Web/iOS/Android 一致性。

**架构：** `biz/catalog` 持有领域对象、仓储接口和清单编译用例；`data` 只查询已启用模板并转换 PO。最终清单不携带中台原子或模板技术参数。

**技术栈：** Go 1.25、MongoDB Go Driver v2、独立本地副本集 `rs0`。

---

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/biz/catalog/model.go` | 模板内容面、产品模式、客户端平台和清单领域对象。 |
| `internal/biz/catalog/repository.go` | 已启用模板只读仓储接口。 |
| `internal/biz/catalog/usecase.go` | 审核过滤、非法模板淘汰和确定性排序。 |
| `internal/biz/catalog/usecase_test.go` | 不依赖 MongoDB 的领域规则测试。 |
| `internal/data/catalog_repository.go` | `templates` 集合查询与 PO/DO 转换。 |
| `internal/data/catalog_repository_test.go` | 本地 `rs0` 查询、筛选和精确清理测试。 |
| `internal/biz/catalog/provider.go`、`internal/biz/biz.go`、`internal/data/data.go` | 依赖注入注册，不新增传输层。 |

## 任务 1：建立失败的领域测试

- [ ] 在 `usecase_test.go` 定义内存模板仓储，编写 `BuildManifest` 的三个失败测试：审核用户只见 SFW、三个平台的结果一致、禁用或非法模板不泄露。
- [ ] 运行 `go test ./internal/biz/catalog -count=1`，确认因缺少 Catalog 用例而失败。

## 任务 2：实现纯领域清单编译

- [ ] 在 `model.go` 定义 `ContentSurface`（`sfw`、`nsfw`）、`ProductMode`（`template_image`、`template_video`）、`ClientPlatform`（`web`、`ios`、`android`）、`Template`、`TemplateSummary`、`ManifestRequest` 与 `Manifest`。
- [ ] 在 `repository.go` 声明 `TemplateRepository.ListEnabled(context.Context)`；不暴露 MongoDB 类型。
- [ ] 在 `usecase.go` 实现 `BuildManifest`：校验平台、读取已启用模板、审核用户仅保留 SFW、忽略非法内容面与模式、以 `sort_order`、`template_id`、`version` 排序并生成摘要。
- [ ] 运行 `go test ./internal/biz/catalog -count=1`，确认领域测试通过。

## 任务 3：实现 MongoDB 只读仓储

- [ ] 在 `catalog_repository.go` 以 `enabled: true` 查询 `schema.CollectionTemplates`，并按 `content_surface`、`sort_order`、`template_id`、`version` 读取。
- [ ] 使用自由转换函数把 `model.TemplateDocument` 转为 `catalog.Template`；不向 `biz` 泄露 BSON 字段或模板参数。
- [ ] 在 `provider.go`、`biz.go`、`data.go` 注册构造器；不修改 `internal/service`、`internal/server` 或 `api`。

## 任务 4：验证独立本地 MongoDB

- [ ] 在 `catalog_repository_test.go` 插入随机 ID 的 SFW、NSFW、禁用和非法模板，验证仓储只返回启用记录，审核编译结果只含 SFW。
- [ ] 用 Web、iOS、Android 三次调用比较清单摘要，确认顺序和内容完全一致。
- [ ] 使用 `CLING_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true' go test ./internal/data -count=1` 验证；每条清理语句必须带随机 `_id`。

## 任务 5：全量复核

- [ ] 运行 `go generate ./cmd/ai-business-service`，不手工编辑 `wire_gen.go`。
- [ ] 运行 `go test ./... -count=1`、`go test -race ./... -count=1`、`go vet ./...`、`go build ./cmd/ai-business-service` 与语义契约检查。
- [ ] 用静态检索确认 Catalog 新代码没有 HTTP/gRPC 路由、Node 修改、生产连接或技术原子字段。
