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
	ID                  string `bson:"_id"`
	CreationID          string `bson:"creation_id"`
	Sequence            int32  `bson:"sequence"`
	Atom                string `bson:"atom"`
	Provider            string `bson:"provider,omitempty"`
	AccountRef          string `bson:"account_ref,omitempty"`
	ContractVersion     string `bson:"contract_version,omitempty"`
	MappingVersion      string `bson:"mapping_version,omitempty"`
	ExternalExecutionID string `bson:"external_execution_id,omitempty"`
	// TerminalResultRef is an internal, transient provider locator. It is never
	// returned as an owned result asset; the R2 materializer replaces it with a
	// validated object reference before publication.
	TerminalResultRef      string `bson:"terminal_result_ref,omitempty"`
	SubmitStatus           string `bson:"submit_status"`
	CallbackVersion        int64  `bson:"callback_version"`
	ProviderRejectionCause string `bson:"provider_rejection_cause,omitempty"`
	// R2UploadPending records that the provider confirmed completion while the
	// result had not yet been copied into owned storage.
	//
	// It lives on the step row rather than on an asset: no asset row exists yet
	// (the insert only happens inside the successful publication transaction),
	// and TerminalResultRef already holds the transient provider locator in an
	// internal-only field. These fields are never projected into a user-facing
	// read path, so the provider URL still cannot become a user-visible key.
	R2UploadPending       bool      `bson:"r2_upload_pending,omitempty"`
	R2UploadPendingAt     time.Time `bson:"r2_upload_pending_at,omitempty"`
	R2UploadPendingReason string    `bson:"r2_upload_pending_reason,omitempty"`
	CreatedAt             time.Time `bson:"created_at"`
}

// DeferredRecipeDocument 表示等待首帧绑定的图生视频冻结技术配方。
type DeferredRecipeDocument struct {
	ID         string `bson:"_id"`
	StepID     string `bson:"step_id"`
	CreationID string `bson:"creation_id"`
	Atom       string `bson:"atom"`
	Protocol   string `bson:"protocol,omitempty"`
	// The deferred recipe is a strict protocol union. Legacy execution.v2 uses
	// these fields; b2b.job.v2 deliberately omits them rather than persisting
	// empty legacy members beside its canonical public-product payload.
	ModelSKU      string    `bson:"model_sku,omitempty"`
	InputTemplate []byte    `bson:"input_template,omitempty"`
	B2BRecipe     []byte    `bson:"b2b_recipe,omitempty"`
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
