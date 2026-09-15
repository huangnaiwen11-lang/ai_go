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
		staticCatalogReader{manifest: &catalog.Manifest{Templates: []catalog.TemplateSummary{{TemplateID: "video-safe", ProductMode: catalog.ProductModeTemplateVideo, ContentSurface: catalog.ContentSurfaceSFW}}}},
		http.NotFoundHandler(),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/homepage/video-templates", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"video-safe"`) || strings.Contains(recorder.Body.String(), "model_sku") {
		t.Fatalf("视频模板目录响应错误：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// 未登录用户可以浏览公开 SFW 视频模板，但仍不能绕过创建接口的账号与权益门禁。
func TestLocalVideoTemplateCatalog未登录时只返回SFW模板(t *testing.T) {
	reader := &recordingCatalogReader{manifest: &catalog.Manifest{Templates: []catalog.TemplateSummary{
		{TemplateID: "video-safe", ProductMode: catalog.ProductModeTemplateVideo, ContentSurface: catalog.ContentSurfaceSFW},
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
