package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// refresh-download-link 在过渡期内必须同时接受两种拼写：
//   - `/api/media/refresh-download-link` —— 前端实际请求（API 基址 `/api` + `/media/refresh-download-link`）
//   - `/media/refresh-download-link`     —— 历史拼写，`deploy/route-switch.local.json` 当前用的键
//
// 两者都不能只留一个：新增那个是为了让前端真正命中；保留旧的那个是因为 route-switch 是
// fail-closed 的 —— JSON 里若还有代码不认识的键，整份开关失效，全部受约束路由会回退 Node（502）。
func TestMatchR2MediaRoute刷新路径同时接受新旧两种拼写(t *testing.T) {
	for _, path := range []string{"/api/media/refresh-download-link", "/media/refresh-download-link"} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		route, ok := matchR2MediaRoute(request)
		if !ok {
			t.Fatalf("规范刷新路径未接管：POST %s", path)
		}
		// 每个拼写必须映射到**自身**的路由键，否则白名单开关会拿错键去查。
		if route.path != path || route.method != http.MethodPost {
			t.Fatalf("POST %s 命中路由 %s %s", path, route.method, route.path)
		}
		if !isConfirmedExactRoute(route) {
			t.Fatalf("POST %s 的键不在代码级白名单里", path)
		}
	}

	// 方法必须收敛到 POST：GET 刷新链接是另一个语义，不得被接管。
	for _, path := range []string{"/api/media/refresh-download-link", "/media/refresh-download-link"} {
		if _, ok := matchR2MediaRoute(httptest.NewRequest(http.MethodGet, path, nil)); ok {
			t.Fatalf("GET %s 不应被刷新路由接管", path)
		}
	}

	// 相邻路径不得被误接管。
	for _, path := range []string{
		"/api/media/refresh-download-link/extra",
		"/api/v1/media/refresh-download-link",
		"/media/refresh-download-link/",
	} {
		if route, ok := matchR2MediaRoute(httptest.NewRequest(http.MethodPost, path, nil)); ok {
			t.Fatalf("相邻路径被错误接管：POST %s → %s", path, route.path)
		}
	}
}

// 两条冻结媒体路径必须与前端调用完全一致（前缀不能漏）。
func TestR2MediaRoute两条冻结路径带api前缀(t *testing.T) {
	for _, testCase := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/media/object"},
		{http.MethodGet, "/api/media/image"},
	} {
		route, ok := matchR2MediaRoute(httptest.NewRequest(testCase.method, testCase.path, nil))
		if !ok || route.path != testCase.path {
			t.Fatalf("%s %s 未按原路径接管：%#v ok=%v", testCase.method, testCase.path, route, ok)
		}
	}
}
