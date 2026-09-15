// Package walletcontract 冻结 Wallet/Billing 阶段在本地静态审计中已经确认的边界。
//
// 本包不实现余额、账本、退款、支付或 Webhook，也不会连接 MongoDB、PayCores 或其他
// 外部服务。它只防止静态资料遗漏高风险副作用后被错误用作 Go 分流许可。
package walletcontract
