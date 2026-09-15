// Package identitycontract 冻结 Identity 阶段在本地静态审计中已经确认的公开边界。
//
// 本包不是 Identity Service，不校验 JWT，也不注册 HTTP 路由。它的唯一用途是防止
// 静态审计资料被误当成迁移或 Go 分流许可；真实登录、OAuth、账号状态与旧令牌兼容性
// 仍需受控运行证据后才能进入后续阶段。
package identitycontract
