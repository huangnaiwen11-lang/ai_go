package model

import "time"

// PaymentProductDocument 表示可版本化的本地支付商品持久化对象。
type PaymentProductDocument struct {
	ProductID     string `bson:"product_id"`
	Version       int64  `bson:"version"`
	DiamondAmount int64  `bson:"diamond_amount"`
	AmountCents   int64  `bson:"amount_cents,omitempty"`
	Currency      string `bson:"currency,omitempty"`
	Label         string `bson:"label,omitempty"`
	PublishStatus string `bson:"publish_status"`
}

// PaymentOrderDocument 表示支付订单持久化对象。
type PaymentOrderDocument struct {
	ID              string    `bson:"_id"`
	UserID          string    `bson:"user_id"`
	Provider        string    `bson:"provider"`
	ProductID       string    `bson:"product_id"`
	ProductVersion  int64     `bson:"product_version"`
	DiamondAmount   int64     `bson:"diamond_amount"`
	AmountCents     int64     `bson:"amount_cents,omitempty"`
	Currency        string    `bson:"currency,omitempty"`
	Status          string    `bson:"status"`
	ProviderOrderID string    `bson:"provider_order_id,omitempty"`
	CreatedAt       time.Time `bson:"created_at"`
	UpdatedAt       time.Time `bson:"updated_at"`
}

// PaymentReceiptDocument 表示支付回执持久化对象。
type PaymentReceiptDocument struct {
	ID                    string    `bson:"_id"`
	Provider              string    `bson:"provider"`
	ExternalTransactionID string    `bson:"external_transaction_id"`
	PaymentOrderID        string    `bson:"payment_order_id"`
	UserID                string    `bson:"user_id"`
	DiamondAmount         int64     `bson:"diamond_amount"`
	LedgerEntryID         string    `bson:"ledger_entry_id,omitempty"`
	ReceivedAt            time.Time `bson:"received_at"`
}

// PayCoresCallbackNonceDocument 表示 Go 自有 PayCores 回调防重放记录。
// 它只持久化 nonce 摘要和已验签 HTTP 边界，绝不保存原 nonce、签名、报文或支付事实。
type PayCoresCallbackNonceDocument struct {
	NonceHash string    `bson:"nonce_hash"`
	Method    string    `bson:"method"`
	Path      string    `bson:"path"`
	ExpiresAt time.Time `bson:"expires_at"`
}

// CallbackReceiptDocument 表示回调回执持久化对象。
type CallbackReceiptDocument struct {
	ID              string    `bson:"_id"`
	Source          string    `bson:"source"`
	NonceHash       string    `bson:"nonce_hash"`
	ReceivedAt      time.Time `bson:"received_at"`
	PayloadDigest   string    `bson:"payload_digest"`
	CreationID      string    `bson:"creation_id"`
	StepID          string    `bson:"step_id"`
	ExternalRef     string    `bson:"external_ref"`
	JobID           string    `bson:"job_id"`
	Capability      string    `bson:"capability"`
	Terminal        string    `bson:"terminal"`
	MediaType       string    `bson:"media_type"`
	ResultURL       string    `bson:"result_url"`
	CallbackVersion string    `bson:"callback_version"`
}
