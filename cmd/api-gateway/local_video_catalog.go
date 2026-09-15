package main

import (
	"context"
	"encoding/json"
	"net/http"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/transport/sessionauth"
)

const localVideoTemplateCatalogPath = "/api/homepage/video-templates"

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
		if request == nil || request.URL == nil || request.Method != http.MethodGet || request.URL.Path != localVideoTemplateCatalogPath || request.URL.RawQuery != "" {
			next.ServeHTTP(writer, request)
			return
		}
		if authenticator == nil || reader == nil {
			writeVideoCatalogError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
			return
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
			return
		}
		items := make([]map[string]string, 0)
		for _, template := range manifest.Templates {
			if template.ProductMode != catalog.ProductModeTemplateVideo {
				continue
			}
			items = append(items, map[string]string{"id": template.TemplateID, "title": template.TemplateID, "type": "video", "contentRating": string(template.ContentSurface)})
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": map[string]any{"items": items, "total": len(items)}})
	})
}

func writeVideoCatalogError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "code": code, "message": "请求未完成", "details": nil})
}
