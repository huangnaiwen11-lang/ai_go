package model

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TemplateDocument 表示模板持久化对象。
type TemplateDocument struct {
	ID             string `bson:"_id"`
	TemplateID     string `bson:"template_id"`
	Version        int64  `bson:"version"`
	ContentSurface string `bson:"content_surface"`
	Mode           string `bson:"mode"`
	SortOrder      int32  `bson:"sort_order"`
	Enabled        bool   `bson:"enabled"`
	// 展示元数据独立于 Parameters，避免向客户端投影时误泄露冻结技术配方。
	Title           string    `bson:"title,omitempty"`
	CoverURL        string    `bson:"cover_url,omitempty"`
	VideoURL        string    `bson:"video_url,omitempty"`
	PreviewVideoURL string    `bson:"preview_video_url,omitempty"`
	Tag             string    `bson:"tag,omitempty"`
	Badge           string    `bson:"badge,omitempty"`
	Parameters      bson.Raw  `bson:"parameters"`
	CreatedAt       time.Time `bson:"created_at"`
	UpdatedAt       time.Time `bson:"updated_at"`
}

// AssetDocument 表示资产持久化对象。
type AssetDocument struct {
	ID         string `bson:"_id"`
	OwnerType  string `bson:"owner_type"`
	OwnerID    string `bson:"owner_id"`
	AssetKind  string `bson:"asset_kind"`
	StorageKey string `bson:"storage_key"`
	Status     string `bson:"status"`
	// ContentType 与 ByteSize 是用户上传素材的内容合同；生成结果资产可保持零值以兼容旧记录。
	ContentType string `bson:"content_type,omitempty"`
	ByteSize    int64  `bson:"byte_size,omitempty"`
	// ContentSHA256 is recorded for B2B materialized results so the database
	// asset can be tied back to the immutable R2 object without retaining the
	// transient provider URL as a user-visible reference.
	ContentSHA256 string    `bson:"content_sha256,omitempty"`
	CreatedAt     time.Time `bson:"created_at"`
}
