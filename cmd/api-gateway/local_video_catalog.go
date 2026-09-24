package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/transport/sessionauth"
)

const (
	localVideoTemplateCatalogPath = "/api/homepage/video-templates"
	localHomepageContentPath      = "/api/homepage/content"
)

type videoCatalogAuthenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type videoCatalogReader interface {
	BuildManifest(context.Context, catalog.ManifestRequest) (*catalog.Manifest, error)
}

// newLocalVideoTemplateCatalogHandler 读取 Go 自有模板目录，并按审核身份收口 SFW 可见性。
// 响应不暴露模型、提示词、工作流或计费事实，三端都应使用这份相同目录。
func newLocalVideoTemplateCatalogHandler(authenticator videoCatalogAuthenticator, reader videoCatalogReader, next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request == nil || request.URL == nil || request.Method != http.MethodGet || request.URL.EscapedPath() != request.URL.Path {
			next.ServeHTTP(writer, request)
			return
		}
		switch request.URL.Path {
		case localVideoTemplateCatalogPath:
			handlerLocalVideoTemplateCatalog(writer, request, authenticator, reader)
		case localHomepageContentPath:
			handlerLocalHomepageContent(writer, request, authenticator, reader)
		default:
			next.ServeHTTP(writer, request)
		}
	})
}

func handlerLocalVideoTemplateCatalog(writer http.ResponseWriter, request *http.Request, authenticator videoCatalogAuthenticator, reader videoCatalogReader) {
	params := parseLocalVideoCatalogParams(request)
	manifest := loadLocalVideoTemplateManifest(writer, request, authenticator, reader)
	if manifest == nil {
		return
	}
	allItems := buildLocalVideoTemplateCatalogItems(manifest)
	items := filterAndPageLocalVideoTemplates(allItems, params)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"success": true,
		"data": map[string]any{
			"items":      items,
			"total":      countLocalVideoTemplateMatches(allItems, params.category),
			"limit":      params.limit,
			"offset":     params.offset,
			"categories": []string{"photoToVideo", "animate"},
		},
	})
}

func handlerLocalHomepageContent(writer http.ResponseWriter, request *http.Request, authenticator videoCatalogAuthenticator, reader videoCatalogReader) {
	manifest := loadLocalVideoTemplateManifest(writer, request, authenticator, reader)
	if manifest == nil {
		return
	}
	items := buildLocalVideoTemplateCatalogItems(manifest)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"success": true,
		"data": map[string]any{
			"sections": map[string]any{
				"imagePresets": map[string]any{
					"title": "Image Templates", "emoji": "✨", "columns": 2,
					"items": []map[string]string{{
						"id": "local-image-edit-dress-up", "title": "本地换装测试模板", "coverUrl": "/legacy/images/homepage/lux-black-marble-portrait.webp", "type": "image", "contentRating": "sfw", "imageGenerationMode": "image_edit",
					}},
				},
				"photoToVideo": map[string]any{"title": "Dream Video", "emoji": "", "columns": 2, "items": items},
			},
			"tagConfigs":      []any{},
			"prefetchEnabled": true,
		},
	})
}

type localVideoCatalogParams struct {
	category string
	limit    int
	offset   int
}

func parseLocalVideoCatalogParams(request *http.Request) localVideoCatalogParams {
	params := localVideoCatalogParams{category: "all", limit: 20}
	if request == nil || request.URL == nil {
		return params
	}
	query := request.URL.Query()
	if category := query.Get("category"); category == "photoToVideo" || category == "animate" || category == "all" {
		params.category = category
	}
	if limit, err := strconv.Atoi(query.Get("limit")); err == nil && limit > 0 {
		params.limit = min(limit, 100)
	}
	if offset, err := strconv.Atoi(query.Get("offset")); err == nil && offset > 0 {
		params.offset = offset
	}
	return params
}

func loadLocalVideoTemplateManifest(writer http.ResponseWriter, request *http.Request, authenticator videoCatalogAuthenticator, reader videoCatalogReader) *catalog.Manifest {
	if authenticator == nil || reader == nil {
		writeVideoCatalogError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
		return nil
	}
	// 模板浏览是公开能力：没有可信 Go 会话时只按 SFW 内容面编译。
	// 创建视频仍由独立创建接口校验账号绑定、封禁状态、权益与预扣，不能被目录读取绕过。
	authenticated, err := authenticator.Authenticate(request)
	reviewed := err != nil || authenticated == nil || authenticated.UserID == ""
	if !reviewed {
		reviewed = authenticated.ContentAccess == identity.ContentAccessReviewRestricted
	}
	manifest, err := reader.BuildManifest(request.Context(), catalog.ManifestRequest{
		Platform: catalog.ClientPlatformWeb, Reviewed: reviewed,
	})
	if err != nil || manifest == nil {
		writeVideoCatalogError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
		return nil
	}
	return manifest
}

func buildLocalVideoTemplateCatalogItems(manifest *catalog.Manifest) []videoTemplateCatalogItem {
	items := make([]videoTemplateCatalogItem, 0)
	localFixtureCount := 0
	for _, template := range manifest.Templates {
		if template.ProductMode != catalog.ProductModeTemplateVideo {
			continue
		}
		localFixture := strings.HasPrefix(template.TemplateID, "local-video-5-")
		if localFixture && localFixtureCount >= len(localVideoPresentations) {
			// compose 的视频 fixture 可能在旧版本中重复写入随机 UUID；只展示
			// 与原站白名单等长的本地媒体池，避免重启后卡片无限增长。
			continue
		}
		title := strings.TrimSpace(template.Title)
		coverURL := strings.TrimSpace(template.CoverURL)
		videoURL := strings.TrimSpace(template.VideoURL)
		previewVideoURL := strings.TrimSpace(template.PreviewVideoURL)
		if localFixture {
			presentation := localVideoPresentation(localFixtureCount)
			// 本地 fixture 的模板 ID 只用于定位可执行配方；展示媒体按稳定顺序
			// 映射，确保历史 UUID、重复启动产生的记录都能覆盖完整素材池。
			title = presentation.title
			coverURL = presentation.coverURL
			videoURL = presentation.videoURL
			previewVideoURL = presentation.videoURL
		} else if title == "" {
			// 未知技术记录没有可展示标题时隐藏，避免把内部文档泄露到目录。
			continue
		}
		if previewVideoURL == "" {
			previewVideoURL = videoURL
		}
		items = append(items, videoTemplateCatalogItem{
			ID: template.TemplateID, Title: title, Type: "video", Category: "photoToVideo", ContentRating: string(template.ContentSurface),
			CoverURL: coverURL, VideoURL: videoURL, PreviewVideoURL: previewVideoURL, Tag: template.Tag, Badge: template.Badge,
		})
		if localFixture {
			localFixtureCount++
		}
	}
	return items
}

func filterAndPageLocalVideoTemplates(items []videoTemplateCatalogItem, params localVideoCatalogParams) []videoTemplateCatalogItem {
	filtered := items[:0]
	for _, item := range items {
		if params.category == "all" || item.Category == params.category {
			filtered = append(filtered, item)
		}
	}
	if params.offset >= len(filtered) {
		return []videoTemplateCatalogItem{}
	}
	end := min(params.offset+params.limit, len(filtered))
	return filtered[params.offset:end]
}

func countLocalVideoTemplateMatches(items []videoTemplateCatalogItem, category string) int {
	count := 0
	for _, item := range items {
		if category == "all" || item.Category == category {
			count++
		}
	}
	return count
}

type localVideoPresentationItem struct {
	title    string
	coverURL string
	videoURL string
}

var localVideoPresentations = [...]localVideoPresentationItem{
	{title: "城市漫游", coverURL: "/legacy/templates/covers/bridge-tile-1.jpg", videoURL: "/legacy/templates/videos/bridge-tile-1.mp4"},
	{title: "光影肖像", coverURL: "/legacy/templates/covers/bridge-tile-2.jpg", videoURL: "/legacy/templates/videos/bridge-tile-2.mp4"},
	{title: "镜头靠近", coverURL: "/legacy/templates/covers/bridge-tile-3.jpg", videoURL: "/legacy/templates/videos/bridge-tile-3.mp4"},
	{title: "轻舞转身", coverURL: "/legacy/templates/covers/bridge-tile-4.jpg", videoURL: "/legacy/templates/videos/bridge-tile-4.mp4"},
	{title: "柔光时刻", coverURL: "/legacy/templates/covers/bridge-tile-5.jpg", videoURL: "/legacy/templates/videos/bridge-tile-5.mp4"},
	{title: "城市夜色", coverURL: "/legacy/templates/covers/bridge-tile-6.jpg", videoURL: "/legacy/templates/videos/bridge-tile-6.mp4"},
	{title: "镜面回眸", coverURL: "/legacy/templates/covers/bridge-tile-7.jpg", videoURL: "/legacy/templates/videos/bridge-tile-7.mp4"},
	{title: "慢动作特写", coverURL: "/legacy/templates/covers/bridge-tile-8.jpg", videoURL: "/legacy/templates/videos/bridge-tile-8.mp4"},
	{title: "暖色片段", coverURL: "/legacy/templates/covers/bridge-tile-9.jpg", videoURL: "/legacy/templates/videos/bridge-tile-9.mp4"},
}

// localVideoPresentation 对原项目审核过的 SFW 白名单媒体做本地静态映射。
// 目录中的模板 ID 仍由 Mongo 配方决定，媒体只承担展示职责。
func localVideoPresentation(index int) localVideoPresentationItem {
	if index < 0 {
		index = 0
	}
	return localVideoPresentations[index%len(localVideoPresentations)]
}

// videoTemplateCatalogItem 是视频目录唯一允许出站的展示字段。
// 技术配方与计费状态在领域层冻结，不能通过这个 DTO 回显。
type videoTemplateCatalogItem struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Type            string `json:"type"`
	Category        string `json:"category"`
	ContentRating   string `json:"contentRating"`
	CoverURL        string `json:"coverUrl,omitempty"`
	VideoURL        string `json:"videoUrl,omitempty"`
	PreviewVideoURL string `json:"previewVideoUrl,omitempty"`
	Tag             string `json:"tag,omitempty"`
	Badge           string `json:"badge,omitempty"`
}

func writeVideoCatalogError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "code": code, "message": "请求未完成", "details": nil})
}
