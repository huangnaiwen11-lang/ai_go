// Package works 定义用户本人可查看的作品历史读取模型。
//
// 本包不暴露生成技术步骤、模型配方、外部任务标识或支付事实；这些字段不属于用户作品列表合同。
package works

import "time"

// Kind 是用户作品的产品输出类型。
type Kind string

const (
	// KindImage 表示图片作品，包括文生图和模板图编辑。
	KindImage Kind = "image"
	// KindVideo 表示图生视频，以及先出首帧再图生视频的最终视频作品。
	KindVideo Kind = "video"
)

// Work 是作品页允许返回的最小安全投影。
type Work struct {
	ID              string
	Kind            Kind
	Status          string
	TemplateID      string
	TemplateVersion int64
	DurationSeconds int32
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// ResultURL 仅能由已成功的最终步骤可用结果资产给出。
	ResultURL string
	// Error 是面向用户的安全错误描述，不包含供应商、模型或技术调用细节。
	Error string
}

// ListQuery 描述按创建时间倒序的作品分页。
// Cursor 非空时优先级高于 Skip，以复合位置避免相同时间戳下的遗漏。
type ListQuery struct {
	UserID string
	Kind   Kind
	Limit  int
	Skip   int
	Cursor string
}

// Page 是作品历史的分页读取结果。
type Page struct {
	Items      []Work
	NextCursor string
}
