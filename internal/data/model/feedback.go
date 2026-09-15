package model

import "time"

// FeedbackDocument 保存用户反馈原始内容，管理后台状态字段暂不由用户端写入。
type FeedbackDocument struct {
	ID          string               `bson:"_id"`
	UserID      string               `bson:"user_id"`
	Type        string               `bson:"type"`
	Message     string               `bson:"message"`
	Email       string               `bson:"email,omitempty"`
	Attachments []FeedbackAttachment `bson:"attachments,omitempty"`
	CreatedAt   time.Time            `bson:"created_at"`
}

type FeedbackAttachment struct {
	ID          string `bson:"id"`
	DownloadURL string `bson:"download_url"`
}
