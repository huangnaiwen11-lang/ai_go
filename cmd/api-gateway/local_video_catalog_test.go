package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/transport/sessionauth"
)

func TestLocalVideoTemplateCatalog只返回公开展示字段(t *testing.T) {
	handler := newLocalVideoTemplateCatalogHandler(
		staticCatalogAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1", ContentAccess: "review_restricted"}},
		staticCatalogReader{manifest: &catalog.Manifest{Templates: []catalog.TemplateSummary{{
			TemplateID: "video-safe", ProductMode: catalog.ProductModeTemplateVideo, ContentSurface: catalog.ContentSurfaceSFW,
			Title: "安全视频模板", CoverURL: "https://assets.example.test/video-safe.webp", PreviewVideoURL: "https://assets.example.test/video-safe.mp4",
		}}}},
		http.NotFoundHandler(),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/homepage/video-templates", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"video-safe"`) || !strings.Contains(recorder.Body.String(), `"title":"安全视频模板"`) || !strings.Contains(recorder.Body.String(), `"coverUrl":"https://assets.example.test/video-safe.webp"`) || !strings.Contains(recorder.Body.String(), `"previewVideoUrl":"https://assets.example.test/video-safe.mp4"`) || strings.Contains(recorder.Body.String(), "model_sku") {
		t.Fatalf("视频模板目录响应错误：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// 未登录用户可以浏览公开 SFW 视频模板，但仍不能绕过创建接口的账号与权益门禁。
func TestLocalVideoTemplateCatalog未登录时只返回SFW模板(t *testing.T) {
	reader := &recordingCatalogReader{manifest: &catalog.Manifest{Templates: []catalog.TemplateSummary{
		{TemplateID: "video-safe", ProductMode: catalog.ProductModeTemplateVideo, ContentSurface: catalog.ContentSurfaceSFW, Title: "安全视频模板"},
	}}}
	handler := newLocalVideoTemplateCatalogHandler(
		staticCatalogAuthenticator{},
		reader,
		http.NotFoundHandler(),
	)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/homepage/video-templates", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if reader.request == nil || !reader.request.Reviewed {
		t.Fatalf("未登录目录必须按 SFW 内容面读取：%+v", reader.request)
	}
	if !strings.Contains(recorder.Body.String(), `"id":"video-safe"`) {
		t.Fatalf("未登录目录应只公开 SFW 模板：%s", recorder.Body.String())
	}
}

func TestLocalVideoTemplateCatalog为旧本地Fixture补齐站内媒体(t *testing.T) {
	handler := newLocalVideoTemplateCatalogHandler(
		staticCatalogAuthenticator{},
		staticCatalogReader{manifest: &catalog.Manifest{Templates: []catalog.TemplateSummary{{
			TemplateID: "local-video-5-legacy", ProductMode: catalog.ProductModeTemplateVideo, ContentSurface: catalog.ContentSurfaceSFW,
		}}}},
		http.NotFoundHandler(),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/homepage/video-templates", nil))
	body := recorder.Body.String()
	for _, expected := range []string{"城市漫游", "/legacy/templates/covers/bridge-tile-1.jpg", "/legacy/templates/videos/bridge-tile-1.mp4"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("旧本地 fixture 未补齐 %q：%s", expected, body)
		}
	}
}

// 原用户端会附带 catalog 缓存旁路、分类与分页参数；本地 Go 目录必须保持同一读合同，
// 否则 Gateway 会错误回退到已移除的 Node 服务。
func TestLocalVideoTemplateCatalog接受原用户端查询参数(t *testing.T) {
	handler := newLocalVideoTemplateCatalogHandler(
		staticCatalogAuthenticator{},
		staticCatalogReader{manifest: &catalog.Manifest{Templates: []catalog.TemplateSummary{{
			TemplateID: "local-video-5-legacy", ProductMode: catalog.ProductModeTemplateVideo, ContentSurface: catalog.ContentSurfaceSFW,
		}}}},
		http.NotFoundHandler(),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/homepage/video-templates?category=all&limit=24&offset=0&maxRating=sfw&catalog=cache-bypass", nil))

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"category":"photoToVideo"`) || !strings.Contains(recorder.Body.String(), `"limit":24`) {
		t.Fatalf("带原用户端查询参数的视频目录响应错误：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestLocalVideoTemplateCatalog按分类返回正确总数(t *testing.T) {
	handler := newLocalVideoTemplateCatalogHandler(
		staticCatalogAuthenticator{},
		staticCatalogReader{manifest: &catalog.Manifest{Templates: []catalog.TemplateSummary{{
			TemplateID: "local-video-5-legacy", ProductMode: catalog.ProductModeTemplateVideo, ContentSurface: catalog.ContentSurfaceSFW,
		}}}},
		http.NotFoundHandler(),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/homepage/video-templates?category=animate&limit=24", nil))

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"items":[]`) || !strings.Contains(recorder.Body.String(), `"total":0`) {
		t.Fatalf("空分类应返回空分页结果：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// 原用户端的生图页通过 homepage/content 读取 imagePresets；该兼容投影也要允许
// catalog 查询参数，并与视频模板共享同一套本地模板事实。
func TestLocalVideoTemplateCatalog提供带查询参数的HomepageContent(t *testing.T) {
	handler := newLocalVideoTemplateCatalogHandler(
		staticCatalogAuthenticator{},
		staticCatalogReader{manifest: &catalog.Manifest{Templates: []catalog.TemplateSummary{{
			TemplateID: "local-video-5-legacy", ProductMode: catalog.ProductModeTemplateVideo, ContentSurface: catalog.ContentSurfaceSFW,
		}}}},
		http.NotFoundHandler(),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/homepage/content?catalog=cache-bypass", nil))

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"imagePresets"`) || !strings.Contains(body, `"photoToVideo"`) || !strings.Contains(body, `"local-image-edit-dress-up"`) {
		t.Fatalf("homepage/content 兼容投影错误：status=%d body=%s", recorder.Code, body)
	}
}

type staticCatalogAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
}

func (auth staticCatalogAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return auth.identity, nil
}

type staticCatalogReader struct{ manifest *catalog.Manifest }

func (reader staticCatalogReader) BuildManifest(context.Context, catalog.ManifestRequest) (*catalog.Manifest, error) {
	return reader.manifest, nil
}

type recordingCatalogReader struct {
	manifest *catalog.Manifest
	request  *catalog.ManifestRequest
}

func (reader *recordingCatalogReader) BuildManifest(_ context.Context, request catalog.ManifestRequest) (*catalog.Manifest, error) {
	reader.request = &request
	return reader.manifest, nil
}
