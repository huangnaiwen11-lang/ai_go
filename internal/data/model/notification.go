package model

import "time"

type NotificationDocument struct {
	ID        string         `bson:"_id"`
	UserID    string         `bson:"user_id"`
	Type      string         `bson:"type"`
	Title     string         `bson:"title"`
	Body      string         `bson:"body"`
	Data      map[string]any `bson:"data,omitempty"`
	Read      bool           `bson:"read"`
	ReadAt    *time.Time     `bson:"read_at,omitempty"`
	CreatedAt time.Time      `bson:"created_at"`
}

// NotificationPreferencesDocument 为每个 Go 用户保存一份通知偏好快照。
// _id 固定等于 user_id，从存储层就阻止为同一用户产生多条互相竞争的配置。
type NotificationPreferencesDocument struct {
	ID                         string    `bson:"_id"`
	PushEnabled                bool      `bson:"push_enabled"`
	EmailEnabled               bool      `bson:"email_enabled"`
	GenerationCompletedEnabled bool      `bson:"generation_completed_enabled"`
	UpdatedAt                  time.Time `bson:"updated_at"`
}
