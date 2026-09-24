package model

import "time"

// AdminReviewItemDocument is the Go-owned moderation read model.  It is not a
// mirror of the generation lifecycle document; review_status is written only
// by the moderation projection writer/importer.
type AdminReviewItemDocument struct {
	ID               string    `bson:"_id"`
	MediaType        string    `bson:"media_type"`
	Source           string    `bson:"source"`
	LegacySourceID   string    `bson:"legacy_source_id,omitempty"`
	CreationID       string    `bson:"creation_id,omitempty"`
	UserID           string    `bson:"user_id,omitempty"`
	AssetID          string    `bson:"asset_id,omitempty"`
	OutputRef        string    `bson:"output_ref,omitempty"`
	ReviewStatus     string    `bson:"review_status"`
	VisibilityStatus string    `bson:"visibility_status,omitempty"`
	GenerationStatus string    `bson:"generation_status"`
	Prompt           string    `bson:"prompt,omitempty"`
	NegativePrompt   string    `bson:"negative_prompt,omitempty"`
	TemplateID       string    `bson:"template_id,omitempty"`
	TemplateTitle    string    `bson:"template_title,omitempty"`
	ReviewedBy       string    `bson:"reviewed_by,omitempty"`
	ReviewedAt       time.Time `bson:"reviewed_at,omitempty"`
	RejectReason     string    `bson:"reject_reason,omitempty"`
	Version          int64     `bson:"version"`
	CreatedAt        time.Time `bson:"created_at"`
	OutputAt         time.Time `bson:"output_at,omitempty"`
	UpdatedAt        time.Time `bson:"updated_at"`
}

// AdminReviewProjectionStateDocument is the readiness fence for readers.  A
// freshly-created empty collection is not evidence that the projection is
// usable; the importer/new-generation writer must explicitly set Ready=true.
type AdminReviewProjectionStateDocument struct {
	ID        string    `bson:"_id"`
	Ready     bool      `bson:"ready"`
	UpdatedAt time.Time `bson:"updated_at"`
}
