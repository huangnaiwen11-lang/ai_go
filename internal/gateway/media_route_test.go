package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMatchLocalMediaRoute只接管两个规范素材路径(t *testing.T) {
	validID := "550e8400-e29b-41d4-a716-446655440000"
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/media/images", nil),
		httptest.NewRequest(http.MethodGet, "/api/media/images/"+validID, nil),
	} {
		if route := matchLocalMediaRoute(request); route == mediaRouteNone {
			t.Fatalf("规范素材路由未匹配：%s %s", request.Method, request.URL)
		}
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/media/images?x=1", nil),
		httptest.NewRequest(http.MethodGet, "/api/media/images/not-a-uuid", nil),
		httptest.NewRequest(http.MethodDelete, "/api/media/images/"+validID, nil),
	} {
		if route := matchLocalMediaRoute(request); route != mediaRouteNone {
			t.Fatalf("非规范素材路由错误接管：%s %s", request.Method, request.URL)
		}
	}
}
