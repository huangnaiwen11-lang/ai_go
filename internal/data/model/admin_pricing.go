package model

import "time"

// AdminPricingConfigDocument mirrors the old SystemConfig key/value shape for
// the pricing keys that are owned by the Go admin gateway. Values are kept
// untyped so nested override documents remain forward compatible.
type AdminPricingConfigDocument struct {
	ID           string    `bson:"_id"`
	Key          string    `bson:"key"`
	Value        any       `bson:"value"`
	Category     string    `bson:"category,omitempty"`
	Description  string    `bson:"description,omitempty"`
	Environment  string    `bson:"environment"`
	Enabled      bool      `bson:"enabled"`
	UpdatedBy    string    `bson:"updated_by,omitempty"`
	ChangeReason string    `bson:"change_reason,omitempty"`
	Version      int64     `bson:"version"`
	CreatedAt    time.Time `bson:"created_at"`
	UpdatedAt    time.Time `bson:"updated_at"`
}
