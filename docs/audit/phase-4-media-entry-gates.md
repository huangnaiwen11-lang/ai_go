# 阶段 4：Media 准入门禁

## 已完成的本机证据

`internal/mediacontract` 已提供纯离线的直传视频票据校验：仅允许 `kind=video`、
固定的 50 MiB 上限、三种视频 `Content-Type`、安全的 HTTP(S) URL、固定对象 key
形态与唯一的 `Content-Type` 请求头。它只处理调用方已脱敏的字段，不发网络请求，
不访问 R2 或 MongoDB，也不注册路由。

`cmd/media-ticket-check` 将这项离线合同封装为本地命令。它严格读取 JSON，拒绝未知字段和多段 JSON；无论失败原因是什么，都不会输出签名 URL、对象 key 或票据内容。可先用工程内的占位符示例验证命令：

```bash
go run ./cmd/media-ticket-check --manifest docs/audit/media-ticket-manifest.example.json
```

真实票据必须复制为受控的本地文件后再检查，不能把预签名 URL、对象 key、用户标识或任何有效凭据提交到工程。命令只验证本地字段结构，既不上传文件，也不访问 R2。

## 仍未满足的门禁

尚未取得真实 Node 捕获，且未验证 R2 权限、签名 TTL、真实 PUT、下载与删除的受控
文件流程。因此对象可见性、过期语义、回源策略和失败重试行为均未冻结。

在这些证据补齐前，禁止创建 Media 路由、Media Service 业务骨架或 Gateway 分流；
`POST /api/upload/ugc/presign` 仍由 Node 处理。该离线包不是阶段 4 已迁移的声明。
