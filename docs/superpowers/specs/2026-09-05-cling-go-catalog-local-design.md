# Cling Go 本地模板目录模块设计

## 目标

在独立本地 MongoDB `cling_main` 中实现模板目录（Catalog）的只读领域闭环。用户只会得到可选模板，不会看到或选择 `text_to_image`、`image_edit`、`image_to_video` 等生成中台原子。

本阶段不创建 HTTP 或 gRPC 业务路由，不连接 Node、生产 MongoDB 或生成中台，也不修改现有首页接口。

## 业务规则

- 模板内容面只有 `sfw` 与 `nsfw`。
- 审核用户只能获得 `sfw` 模板；未审核用户可获得两个内容面的已启用模板。
- Web、iOS、Android 对相同审核状态必须返回完全相同的模板清单和顺序。平台参数只用于校验调用方类型，不参与筛选或排序。
- 对用户公开的模板模式只有 `template_image` 与 `template_video`。`I2I` 的换装、脱衣等能力仍是模板内部配置，不作为用户选择的技术模式。
- 未启用、内容面非法、模式非法的模板一律不进入清单。宁可少展示，也不能把未审核的配置暴露给用户。

## 分层与对象边界

```text
CatalogUsecase
  └── TemplateRepository.ListEnabled
        └── MongoDB templates 集合

CatalogManifest
  └── TemplateSummary（模板 ID、版本、内容面、产品模式、排序号）
```

- `internal/biz/catalog` 仅定义模板领域对象、清单编译规则和仓储接口；不依赖 MongoDB、身份模块、传输层或生成模块。
- `internal/data/catalog_repository.go` 仅查询 `enabled: true` 的模板并转换 BSON PO，不解释审核规则。
- 清单中的 `TemplateSummary` 不含中台原子、提示词、钻石、VIP、余额、账本或支付字段。

## 稳定排序

业务层统一按 `sort_order`、`template_id`、`version` 排序。即使数据库返回顺序变化、不同平台调用路径不同，最终清单仍保持确定性。

## 测试策略

- 领域单元测试覆盖审核用户 SFW 过滤、三端同清单、禁用或非法模板不泄露。
- 本地 MongoDB 集成测试写入随机 ID 的 SFW、NSFW、禁用与非法模板，验证仓储只读到启用记录，编译器按业务规则筛选和排序。
- 清理仅按测试随机 `_id` 精确删除；不得清空集合、删除数据库或连接生产环境。

## 明确不做

- 不实现首页 API、分页、缓存头、ETag、限流、认证、App scope 或内容偏好协议兼容。
- 不创建模板管理后台、模板写入接口、素材上传、生成任务或中台调用。
- 不变更 `docs/audit/api-migration-matrix.json` 中任何待核实 Catalog 路由，也不把本地清单实现作为流量迁移许可。
