package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本机图片模板目录只能暴露固定的公开展示字段，不能泄露冻结技术配方。
func TestLocalImageTemplateCatalog只返回本地SFW换装模板(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/homepage/image-templates", nil)

	newLocalImageTemplateCatalogHandler(http.NotFoundHandler()).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, expected := range []string{"local-image-edit-dress-up", "本地换装测试模板", "\"coverUrl\":\"/legacy/images/homepage/lux-black-marble-portrait.webp\"", "\"contentRating\":\"sfw\""} {
		if !strings.Contains(body, expected) {
			t.Fatalf("目录响应缺少 %q：%s", expected, body)
		}
	}
	for _, forbidden := range []string{"model_sku", "negative_prompt", "reference_assets"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("目录响应泄露技术字段 %q：%s", forbidden, body)
		}
	}
}

func TestLocalImageTemplateCatalog只接管精确目录路径(t *testing.T) {
	called := false
	fallback := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		called = true
		writer.WriteHeader(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()

	newLocalImageTemplateCatalogHandler(fallback).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/homepage/image-templates/extra", nil))

	if !called || recorder.Code != http.StatusNoContent {
		t.Fatalf("相邻路径应继续交给下游：called=%t status=%d", called, recorder.Code)
	}
}

// 原用户端会在模板目录请求上附带 catalog 缓存旁路参数；本地目录应忽略该读取参数，
// 而不是回退到已经移除的 Node 服务。
func TestLocalImageTemplateCatalog接受Catalog查询参数(t *testing.T) {
	recorder := httptest.NewRecorder()
	newLocalImageTemplateCatalogHandler(http.NotFoundHandler()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/homepage/image-templates?catalog=cache-bypass", nil))

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"local-image-edit-dress-up"`) {
		t.Fatalf("带 catalog 的图片目录响应错误：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
