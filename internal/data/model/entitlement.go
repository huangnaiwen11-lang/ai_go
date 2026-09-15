package model

import "time"

// SubscriptionDocument 表示仅供服务端读取的订阅权益快照投影。
// 该投影不保存支付渠道、订单、钻石或余额等结算事实。
type SubscriptionDocument struct {
	UserID        string    `bson:"_id"`
	Status        string    `bson:"status"`
	BillingPeriod string    `bson:"billing_period"`
	StartsAt      time.Time `bson:"starts_at"`
	ExpiresAt     time.Time `bson:"expires_at"`
	CreatedAt     time.Time `bson:"created_at"`
	UpdatedAt     time.Time `bson:"updated_at"`
}
