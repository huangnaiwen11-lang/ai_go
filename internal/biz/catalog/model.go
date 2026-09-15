// Package catalog 管理内容面、模板目录与模板编译。
package catalog

// ContentSurface 表示模板可见内容面。
type ContentSurface string

const (
	// ContentSurfaceSFW 表示安全内容面。
	ContentSurfaceSFW ContentSurface = "sfw"
	// ContentSurfaceNSFW 表示受限内容面。
	ContentSurfaceNSFW ContentSurface = "nsfw"
)

// ProductMode 是用户可选择的模板产品类型。
// 技术中台原子不属于用户的选择项，因此不在本枚举中出现。
type ProductMode string

const (
	// ProductModeTemplateImage 表示图片模板，包括文生图和模板图编辑的产品入口。
	ProductModeTemplateImage ProductMode = "template_image"
	// ProductModeTemplateVideo 表示视频模板，包括由主站编排的文生视频入口。
	ProductModeTemplateVideo ProductMode = "template_video"
)

// ClientPlatform 表示请求模板清单的客户端。
// 平台不参与目录筛选或排序，只用于拒绝未知调用方。
type ClientPlatform string

const (
	// ClientPlatformWeb 表示 Web 客户端。
	ClientPlatformWeb ClientPlatform = "web"
	// ClientPlatformIOS 表示 iOS 客户端。
	ClientPlatformIOS ClientPlatform = "ios"
	// ClientPlatformAndroid 表示 Android 客户端。
	ClientPlatformAndroid ClientPlatform = "android"
)

// Template 是目录模块从持久化层读取的完整模板领域对象。
// TemplateID 与 Version 是后续生成编排定位模板版本的稳定标识。
type Template struct {
	ID             string
	TemplateID     string
	Version        int64
	ContentSurface ContentSurface
	ProductMode    ProductMode
	SortOrder      int32
	Enabled        bool
	// 以下字段是跨端一致的用户可见投影；不承载模型、提示词、工作流或结算信息。
	Title           string
	CoverURL        string
	VideoURL        string
	PreviewVideoURL string
	Tag             string
	Badge           string
}

// TemplateSummary 是向上层交付的用户可见模板摘要。
// 它刻意不包含技术原子、提示词、模板参数或任何结算字段。
type TemplateSummary struct {
	TemplateID      string
	Version         int64
	ContentSurface  ContentSurface
	ProductMode     ProductMode
	SortOrder       int32
	Title           string
	CoverURL        string
	VideoURL        string
	PreviewVideoURL string
	Tag             string
	Badge           string
}

// ManifestRequest 是编译模板清单的调用上下文。
// Reviewed 为 true 时必须执行 SFW 限制。
type ManifestRequest struct {
	Platform ClientPlatform
	Reviewed bool
}

// Manifest 是跨端稳定的模板清单。
// 结果不回显调用平台，避免平台字段本身造成三端响应差异。
type Manifest struct {
	Templates []TemplateSummary
}
