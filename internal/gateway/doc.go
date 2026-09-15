// Package gateway 提供浏览器 /api 的兼容网关边界。
//
// 它只负责兼容转发、精确且低风险的本地路由，以及在本地重放前向 Node 发起准入。
// 默认策略为 fail-closed：未确认、未启用或准入不满足的请求继续回退 Node。
// 本包不访问业务数据库，也不承载领域业务逻辑。
package gateway
