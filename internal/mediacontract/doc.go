// Package mediacontract 校验调用方已脱敏的直传媒体票据。
//
// 输入只应来自调用方已脱敏保留的 Node POST /api/upload/ugc/presign 票据字段；本包
// 不校验 HTTP 方法或路径。本包不发起网络请求、不访问 R2 或 MongoDB、不注册 HTTP
// 路由；它只检查请求摘要与票据字段。该离线校验不构成 Media Service 实现、迁移或
// Gateway 分流许可，也不替代真实上传、读取、删除与权限验证。
package mediacontract
