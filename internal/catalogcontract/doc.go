// Package catalogcontract 提供匿名 GET 的离线 HTTP 契约回放。
//
// 304 条件阶段只能由 baseline ETag 驱动；Authorization、Cookie 与
// Proxy-Authorization 会在每次回放前过滤。本包不注册路由、不读取 MongoDB，
// 也不生成写流量。
package catalogcontract
