package model

import "time"

type NotificationDocument struct {
	ID string `bson:"_id"`
	UserID string `bson:"user_id"`
	Type string `bson:"type"`
	Title string `bson:"title"`
	Body string `bson:"body"`
	Data map[string]any `bson:"data,omitempty"`
	Read bool `bson:"read"`
	ReadAt *time.Time `bson:"read_at,omitempty"`
	CreatedAt time.Time `bson:"created_at"`
}
