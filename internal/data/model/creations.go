package model

import "time"

// CreationDocument 表示创作持久化对象。
type CreationDocument struct {
	ID                   string    `bson:"_id"`
	IdempotencyKey       string    `bson:"idempotency_key"`
	UserID               string    `bson:"user_id"`
	TemplateID           string    `bson:"template_id"`
	TemplateVersion      int64     `bson:"template_version"`
	ProductOutput        string    `bson:"product_output"`
	VideoDurationSeconds int32     `bson:"video_duration_seconds,omitempty"`
	RequestFingerprint   string    `bson:"request_fingerprint"`
	ParentID             string    `bson:"parent_id,omitempty"`
	Status               string    `bson:"status"`
	Version              int64     `bson:"version"`
	CreatedAt            time.Time `bson:"created_at"`
	UpdatedAt            time.Time `bson:"updated_at"`
}

// CreationStepDocument 表示创作步骤持久化对象。
type CreationStepDocument struct {
	ID                  string    `bson:"_id"`
	CreationID          string    `bson:"creation_id"`
	Sequence            int32     `bson:"sequence"`
	Atom                string    `bson:"atom"`
	ExternalExecutionID string    `bson:"external_execution_id,omitempty"`
	SubmitStatus        string    `bson:"submit_status"`
	CallbackVersion     int64     `bson:"callback_version"`
	CreatedAt           time.Time `bson:"created_at"`
}

// DeferredRecipeDocument 表示等待首帧绑定的图生视频冻结技术配方。
type DeferredRecipeDocument struct {
	ID            string    `bson:"_id"`
	StepID        string    `bson:"step_id"`
	CreationID    string    `bson:"creation_id"`
	Atom          string    `bson:"atom"`
	ModelSKU      string    `bson:"model_sku"`
	InputTemplate []byte    `bson:"input_template"`
	Digest        string    `bson:"digest"`
	Status        string    `bson:"status"`
	CreatedAt     time.Time `bson:"created_at"`
	UpdatedAt     time.Time `bson:"updated_at"`
}

// DailyQuotaDocument 表示每日配额持久化对象。
type DailyQuotaDocument struct {
	ID        string    `bson:"_id"`
	UserID    string    `bson:"user_id"`
	QuotaKind string    `bson:"quota_kind"`
	LocalDate string    `bson:"local_date"`
	UsedCount int32     `bson:"used_count"`
	Limit     int32     `bson:"limit"`
	UpdatedAt time.Time `bson:"updated_at"`
}
