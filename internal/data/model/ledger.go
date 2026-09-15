package model

import "time"

// AccountDocument 表示用户自有钻石余额聚合。
// ID 直接等于 user_id，避免重复保存 user_id 产生两套条件更新定位事实。
type AccountDocument struct {
	ID             string    `bson:"_id"`
	DiamondBalance int64     `bson:"diamond_balance"`
	CreatedAt      time.Time `bson:"created_at"`
	UpdatedAt      time.Time `bson:"updated_at"`
}

// ReservationDocument 表示可被精确冲正的预留持久化对象。
type ReservationDocument struct {
	ID               string    `bson:"_id"`
	CreationID       string    `bson:"creation_id"`
	UserID           string    `bson:"user_id"`
	PriceDiamonds    int64     `bson:"price_diamonds"`
	ReservedDiamonds int64     `bson:"reserved_diamonds"`
	BenefitSource    string    `bson:"benefit_source"`
	QuotaKind        string    `bson:"quota_kind"`
	LocalDate        string    `bson:"local_date"`
	QuotaLimit       int32     `bson:"quota_limit"`
	QuotaUnits       int32     `bson:"quota_units"`
	Status           string    `bson:"status"`
	CreatedAt        time.Time `bson:"created_at"`
	UpdatedAt        time.Time `bson:"updated_at"`
}

// LedgerEntryDocument 表示账本分录持久化对象。
type LedgerEntryDocument struct {
	ID             string    `bson:"_id"`
	IdempotencyKey string    `bson:"idempotency_key"`
	AccountID      string    `bson:"account_id"`
	CreationID     string    `bson:"creation_id"`
	DeltaDiamonds  int64     `bson:"delta_diamonds"`
	Reason         string    `bson:"reason"`
	ReservationID  string    `bson:"reservation_id"`
	CreatedAt      time.Time `bson:"created_at"`
}
