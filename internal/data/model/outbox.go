package model

import (
	"time"
)

// OutboxEventDocument 表示发件箱事件持久化对象。
type OutboxEventDocument struct {
	ID             string    `bson:"_id"`
	AggregateID    string    `bson:"aggregate_id"`
	EventType      string    `bson:"event_type"`
	Payload        []byte    `bson:"payload"`
	DeliveryStatus string    `bson:"delivery_status"`
	AttemptCount   int32     `bson:"attempt_count"`
	NextAttemptAt  time.Time `bson:"next_attempt_at"`
	LeaseToken     string    `bson:"lease_token,omitempty"`
	LeaseUntil     time.Time `bson:"lease_until,omitempty"`
	LeaseOwner     string    `bson:"lease_owner,omitempty"`
	JobID          string    `bson:"job_id,omitempty"`
	LastError      string    `bson:"last_error,omitempty"`
	CreatedAt      time.Time `bson:"created_at"`
	UpdatedAt      time.Time `bson:"updated_at"`
}
