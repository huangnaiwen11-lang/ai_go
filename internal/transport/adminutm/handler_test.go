package adminutm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	biz "ai-business-service/internal/biz/adminutm"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

const testLinkID = "6aacecb5a5366433dcd75029"

type stubAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (s stubAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return s.identity, s.err
}

type stubRepository struct {
	page      biz.Page
	sources   []biz.Source
	link      biz.Link
	listErr   error
	writeErr  error
	gotCreate biz.Input
	gotUpdate biz.Input
	gotID     string
	deleted   bool
}

func (s *stubRepository) List(context.Context, biz.Query) (biz.Page, error) {
	return s.page, s.listErr
}

func (s *stubRepository) Sources(context.Context, bool) ([]biz.Source, error) {
	return s.sources, s.listErr
}

func (s *stubRepository) Create(_ context.Context, _ biz.Actor, in biz.Input) (biz.Link, error) {
	s.gotCreate = in
	if s.writeErr != nil {
		return biz.Link{}, s.writeErr
	}
	return s.link, nil
}

func (s *stubRepository) Update(_ context.Context, _ biz.Actor, id string, in biz.Input) (biz.Link, error) {
	s.gotID, s.gotUpdate = id, in
	if s.writeErr != nil {
		return biz.Link{}, s.writeErr
	}
	return s.link, nil
}

func (s *stubRepository) Delete(_ context.Context, _ biz.Actor, id string) error {
	s.gotID, s.deleted = id, true
	return s.writeErr
}

func superAdmin() stubAuthenticator {
	return stubAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: "super_admin"}}
}

func newHandler(repository biz.Repository, auth stubAuthenticator) http.Handler {
	return NewHandler(repository, auth)
}

func call(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

func dataOf(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload["success"] != true {
		t.Fatalf("success 不是 true: %s", recorder.Body.String())
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data 对象: %s", recorder.Body.String())
	}
	return data
}

// 前端 UtmLinksPage 读的是 res.items / r.slug / r.shortUrl。
// 这条用例锁死线格式是 camelCase —— 直接序列化 biz 结构体会得到 PascalCase，
// 接口 200 但表格恒为空。
func TestHandlerUTM列表返回camelCase线格式(t *testing.T) {
	lastClick := time.Date(2026, time.September, 18, 7, 0, 0, 0, time.UTC)
	repository := &stubRepository{page: biz.Page{Total: 1, Items: []biz.Link{{
		ID: testLinkID, Slug: "probe-link", Label: "探针", TargetPath: "/photo-to-video",
		UtmSource: "probe_src", UtmMedium: "banner", ExtraParams: map[string]string{"from": "probe"},
		Enabled: true, Clicks: 7, ShortURL: "https://cling-ai.com/api/v1/r/probe-link",
		FullURL:     "https://cling-ai.com/photo-to-video?utm_source=probe_src",
		LastClickAt: &lastClick, CreatedAt: lastClick, UpdatedAt: lastClick,
	}}}}
	recorder := call(t, newHandler(repository, superAdmin()), http.MethodGet, "/api/admin/utm-links?limit=5", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	data := dataOf(t, recorder)
	if data["total"] != float64(1) {
		t.Fatalf("total=%v", data["total"])
	}
	items, ok := data["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items=%v", data["items"])
	}
	item := items[0].(map[string]any)
	for _, key := range []string{"id", "slug", "label", "targetPath", "utmSource", "utmMedium", "utmCampaign", "utmContent", "utmTerm", "extraParams", "enabled", "notes", "clicks", "lastClickAt", "createdAt", "updatedAt", "shortUrl", "fullUrl"} {
		if _, present := item[key]; !present {
			t.Fatalf("缺少 camelCase 字段 %q，实际 keys=%v", key, keysOf(item))
		}
	}
	if _, present := item["Slug"]; present {
		t.Fatalf("仍然返回 PascalCase 字段: %v", keysOf(item))
	}
	if item["id"] != testLinkID || item["shortUrl"] != "https://cling-ai.com/api/v1/r/probe-link" {
		t.Fatalf("字段值不对: %v", item)
	}
	if extra, ok := item["extraParams"].(map[string]any); !ok || extra["from"] != "probe" {
		t.Fatalf("extraParams 丢失: %v", item["extraParams"])
	}
}

func TestHandlerUTM列表空集合返回空数组而不是null(t *testing.T) {
	recorder := call(t, newHandler(&stubRepository{}, superAdmin()), http.MethodGet, "/api/admin/utm-links", "")
	data := dataOf(t, recorder)
	items, ok := data["items"].([]any)
	if !ok {
		t.Fatalf("items 应为数组，实际 %T", data["items"])
	}
	if len(items) != 0 {
		t.Fatalf("items=%v", items)
	}
}

func TestHandlerUTM来源聚合返回camelCase(t *testing.T) {
	repository := &stubRepository{sources: []biz.Source{{
		UtmSource: "probe_src", Enabled: true, TotalClicks: 7,
		Links: []biz.SourceLink{{ID: testLinkID, Slug: "probe-link", Label: "探针", Enabled: true, UtmMedium: "banner"}},
	}}}
	recorder := call(t, newHandler(repository, superAdmin()), http.MethodGet, "/api/admin/utm-links/sources?onlyEnabled=true", "")
	data := dataOf(t, recorder)
	sources, ok := data["sources"].([]any)
	if !ok || len(sources) != 1 {
		t.Fatalf("sources=%v", data["sources"])
	}
	source := sources[0].(map[string]any)
	if source["utmSource"] != "probe_src" || source["totalClicks"] != float64(7) {
		t.Fatalf("source=%v", source)
	}
	link := source["links"].([]any)[0].(map[string]any)
	if link["id"] != testLinkID || link["utmMedium"] != "banner" {
		t.Fatalf("link=%v", link)
	}
}

// Node 端 utm-links 五条路由全部走 requireSuperAdmin。
// 只放行 admin 会让同一路径在两个实现下可访问性不同。
func TestHandlerUTM全路由要求super_admin(t *testing.T) {
	for _, role := range []string{"admin", "user", "editor", ""} {
		auth := stubAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "u-1", Role: role}}
		recorder := call(t, newHandler(&stubRepository{}, auth), http.MethodGet, "/api/admin/utm-links", "")
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("role=%q status=%d 期望 403", role, recorder.Code)
		}
	}
	anonymous := stubAuthenticator{identity: &sessionauth.AuthenticatedIdentity{}}
	if recorder := call(t, newHandler(&stubRepository{}, anonymous), http.MethodGet, "/api/admin/utm-links", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 status=%d 期望 401", recorder.Code)
	}
}

func TestHandlerUTM新建返回link包装并补默认值(t *testing.T) {
	repository := &stubRepository{link: biz.Link{ID: testLinkID, Slug: "new-link", Label: "新链接", UtmSource: "src", TargetPath: "/", UtmMedium: "banner"}}
	recorder := call(t, newHandler(repository, superAdmin()), http.MethodPost, "/api/admin/utm-links",
		`{"slug":"new-link","label":"新链接","utmSource":"src"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// 前端 api.post 用整个 data 体，必须包 link key。
	link, ok := dataOf(t, recorder)["link"].(map[string]any)
	if !ok || link["slug"] != "new-link" {
		t.Fatalf("响应缺少 link 包装: %s", recorder.Body.String())
	}
	if repository.gotCreate.TargetPath == nil || *repository.gotCreate.TargetPath != "/" {
		t.Fatalf("targetPath 默认值未补: %+v", repository.gotCreate.TargetPath)
	}
	if repository.gotCreate.UtmMedium == nil || *repository.gotCreate.UtmMedium != "banner" {
		t.Fatalf("utmMedium 默认值未补: %+v", repository.gotCreate.UtmMedium)
	}
}

// Node 端 slug 允许下划线（tg_community_2026q2）。早期 Go 版本的正则会把它判成非法。
func TestHandlerUTM新建接受下划线slug(t *testing.T) {
	repository := &stubRepository{link: biz.Link{ID: testLinkID}}
	recorder := call(t, newHandler(repository, superAdmin()), http.MethodPost, "/api/admin/utm-links",
		`{"slug":"tg_community_2026q2","label":"Telegram 社群","utmSource":"telegram"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerUTM新建缺少必填字段返回400(t *testing.T) {
	repository := &stubRepository{}
	recorder := call(t, newHandler(repository, superAdmin()), http.MethodPost, "/api/admin/utm-links", `{"slug":"only-slug"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.gotCreate.Slug != nil {
		t.Fatalf("校验失败却调用了仓储")
	}
}

func TestHandlerUTM拒绝未知字段(t *testing.T) {
	recorder := call(t, newHandler(&stubRepository{}, superAdmin()), http.MethodPost, "/api/admin/utm-links",
		`{"slug":"new-link","label":"新链接","utmSource":"src","unexpected":1}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// 启用开关只发 {enabled}，其余字段必须保持原值（nil = 未提供）。
func TestHandlerUTM更新只发enabled不覆盖其它字段(t *testing.T) {
	repository := &stubRepository{link: biz.Link{ID: testLinkID, Slug: "probe-link"}}
	recorder := call(t, newHandler(repository, superAdmin()), http.MethodPut, "/api/admin/utm-links/"+testLinkID, `{"enabled":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.gotID != testLinkID {
		t.Fatalf("id=%q", repository.gotID)
	}
	if repository.gotUpdate.Enabled == nil || *repository.gotUpdate.Enabled {
		t.Fatalf("enabled 未透传: %+v", repository.gotUpdate.Enabled)
	}
	if repository.gotUpdate.Slug != nil || repository.gotUpdate.Label != nil || repository.gotUpdate.Notes != nil {
		t.Fatalf("未提供的字段被改写: %+v", repository.gotUpdate)
	}
}

func TestHandlerUTM删除返回id(t *testing.T) {
	repository := &stubRepository{}
	recorder := call(t, newHandler(repository, superAdmin()), http.MethodDelete, "/api/admin/utm-links/"+testLinkID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if dataOf(t, recorder)["id"] != testLinkID || !repository.deleted {
		t.Fatalf("删除响应或调用不对: %s", recorder.Body.String())
	}
}

func TestHandlerUTM不存在与重复slug的错误语义(t *testing.T) {
	notFound := &stubRepository{writeErr: biz.ErrNotFound}
	if recorder := call(t, newHandler(notFound, superAdmin()), http.MethodDelete, "/api/admin/utm-links/"+testLinkID, ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("不存在 status=%d 期望 404", recorder.Code)
	}
	// Node 端重复 slug 是 ApiError.badRequest → 400，这里保持一致。
	conflict := &stubRepository{writeErr: biz.ErrConflict}
	if recorder := call(t, newHandler(conflict, superAdmin()), http.MethodPost, "/api/admin/utm-links",
		`{"slug":"dup-link","label":"重复","utmSource":"src"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("重复 slug status=%d 期望 400", recorder.Code)
	}
}

func TestHandlerUTM路径与方法边界(t *testing.T) {
	handler := newHandler(&stubRepository{}, superAdmin())
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/admin/utm-links/not-an-objectid", http.StatusBadRequest},
		{http.MethodGet, "/api/admin/utm-links/" + testLinkID, http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/admin/utm-links/sources", http.StatusMethodNotAllowed},
		{http.MethodPut, "/api/admin/utm-links/" + testLinkID + "/extra", http.StatusNotImplemented},
		{http.MethodGet, "/api/admin/utm-links-something-else", http.StatusNotImplemented},
	}
	for _, item := range cases {
		recorder := call(t, handler, item.method, item.path, "")
		if recorder.Code != item.want {
			t.Fatalf("%s %s status=%d 期望 %d body=%s", item.method, item.path, recorder.Code, item.want, recorder.Body.String())
		}
	}
}

func TestHandlerUTM认证失败按统一语义拒绝(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"会话无效", shared.ErrUnauthenticated, http.StatusUnauthorized},
		{"依赖不可用", shared.ErrServiceUnavailable, http.StatusServiceUnavailable},
	}
	for _, item := range cases {
		auth := stubAuthenticator{err: item.err}
		recorder := call(t, newHandler(&stubRepository{}, auth), http.MethodGet, "/api/admin/utm-links", "")
		if recorder.Code != item.want {
			t.Fatalf("%s status=%d 期望 %d", item.name, recorder.Code, item.want)
		}
	}
}

func keysOf(payload map[string]any) []string {
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	return keys
}
