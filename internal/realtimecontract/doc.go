// Package realtimecontract 校验已捕获的 Node SSE 响应的公开结构。
//
// 本包仅处理调用方提供的状态码、响应头和响应正文，不创建网络连接，也不访问
// Redis、MongoDB 或 HTTP 路由。Parse 与 Validate 只供未来受控捕获逐份校验，
// 不能作为兼容、迁移或流量分流的证明。
package realtimecontract
