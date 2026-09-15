package main

import (
	"encoding/json"
	"net/http"
)

const localImageTemplateCatalogPath = "/api/homepage/image-templates"

// newLocalImageTemplateCatalogHandler 仅为完整本地图片 fixture 提供可验收的公开目录。
// 模板技术配方、参考图和模型选择始终留在 Go 服务端，浏览器只取得展示和选择所需字段。
func newLocalImageTemplateCatalogHandler(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL == nil || request.URL.Path != localImageTemplateCatalogPath || request.URL.RawQuery != "" {
			next.ServeHTTP(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"items": []map[string]string{{
					"id": "local-image-edit-dress-up", "title": "本地换装测试模板", "coverUrl": "/cling-ai-icon.png", "type": "image", "contentRating": "sfw",
				}},
				"total": 1,
			},
		})
	})
}
