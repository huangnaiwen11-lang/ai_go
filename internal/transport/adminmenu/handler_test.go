package adminmenu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	biz "ai-business-service/internal/biz/adminmenu"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

type stubAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (s stubAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return s.identity, s.err
}

type stubRepository struct {
	stored   map[string]bool
	updated  *time.Time
	by       string
	readErr  error
	writeErr error
	gotSave  map[string]bool
	gotActor biz.Actor
}

func (s *stubRepository) Get(context.Context) (biz.Visibility, error) {
	if s.readErr != nil {
		return biz.Visibility{}, s.readErr
	}
	return biz.Visibility{Overrides: s.stored, UpdatedAt: s.updated, UpdatedBy: s.by}, nil
}

func (s *stubRepository) Save(_ context.Context, actor biz.Actor, overrides map[string]bool) (biz.Visibility, error) {
	s.gotActor, s.gotSave = actor, overrides
	if s.writeErr != nil {
		return biz.Visibility{}, s.writeErr
	}
	return biz.Visibility{Overrides: overrides, UpdatedAt: s.updated, UpdatedBy: actor.ID}, nil
}

func identity(role string) stubAuthenticator {
	return stubAuthenticator{
		identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: role},
	}
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

func TestHandlerGetReturnsOverrides(t *testing.T) {
	repository := &stubRepository{stored: map[string]bool{"/loras": true, "/users": false}}
	handler := NewHandler(repository, identity("super_admin"))

	recorder := call(t, handler, http.MethodGet, "/api/admin/system/menu-visibility", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200, body=%s", recorder.Code, recorder.Body.String())
	}
	overrides, ok := dataOf(t, recorder)["overrides"].(map[string]any)
	if !ok {
		t.Fatalf("overrides 不是对象: %s", recorder.Body.String())
	}
	if overrides["/loras"] != true || overrides["/users"] != false {
		t.Fatalf("overrides = %#v, want {/loras:true, /users:false}", overrides)
	}
}

// 没配置过时必须返回 {} 而不是 null —— null 会让前端把「还没人配过」
// 当成异常状态处理。
func TestHandlerGetWithoutDocumentReturnsEmptyObject(t *testing.T) {
	handler := NewHandler(&stubRepository{}, identity("super_admin"))

	recorder := call(t, handler, http.MethodGet, "/api/admin/system/menu-visibility", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", recorder.Code)
	}
	overrides, ok := dataOf(t, recorder)["overrides"].(map[string]any)
	if !ok {
		t.Fatalf("overrides 应为空对象而不是 null: %s", recorder.Body.String())
	}
	if len(overrides) != 0 {
		t.Fatalf("overrides = %#v, want 空", overrides)
	}
}

func TestHandlerPutSavesOverrides(t *testing.T) {
	repository := &stubRepository{}
	handler := NewHandler(repository, identity("super_admin"))

	recorder := call(t, handler, http.MethodPut, "/api/admin/system/menu-visibility",
		`{"overrides":{"/loras":true,"/images":false}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200, body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.gotSave["/loras"] != true || repository.gotSave["/images"] != false {
		t.Fatalf("落库内容 = %#v", repository.gotSave)
	}
	if repository.gotActor.ID != "admin-1" {
		t.Fatalf("审计主体 = %q, want admin-1", repository.gotActor.ID)
	}
}

// 空对象是合法输入，含义是「全部按默认清单」。
func TestHandlerPutAcceptsEmptyOverrides(t *testing.T) {
	repository := &stubRepository{}
	handler := NewHandler(repository, identity("super_admin"))

	recorder := call(t, handler, http.MethodPut, "/api/admin/system/menu-visibility", `{"overrides":{}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200, body=%s", recorder.Code, recorder.Body.String())
	}
	if len(repository.gotSave) != 0 {
		t.Fatalf("落库内容 = %#v, want 空", repository.gotSave)
	}
}

// 缺 overrides 与「显式传空」是两件事，前者按非法输入处理。
func TestHandlerPutRejectsMissingOverrides(t *testing.T) {
	handler := NewHandler(&stubRepository{}, identity("super_admin"))

	recorder := call(t, handler, http.MethodPut, "/api/admin/system/menu-visibility", `{}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400", recorder.Code)
	}
}

func TestHandlerPutRejectsUnknownField(t *testing.T) {
	handler := NewHandler(&stubRepository{}, identity("super_admin"))

	recorder := call(t, handler, http.MethodPut, "/api/admin/system/menu-visibility",
		`{"overrides":{},"extra":1}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400", recorder.Code)
	}
}

// 键必须是绝对路径 —— 相对路径在前端永远匹配不上任何节点，
// 属于安静失效的脏数据，必须在入口挡下。
func TestHandlerPutMapsInvalidOverrideToBadRequest(t *testing.T) {
	repository := &stubRepository{writeErr: biz.ErrInvalid}
	handler := NewHandler(repository, identity("super_admin"))

	recorder := call(t, handler, http.MethodPut, "/api/admin/system/menu-visibility",
		`{"overrides":{"loras":true}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerRequiresSuperAdmin(t *testing.T) {
	for _, role := range []string{"admin", "user", "editor", ""} {
		handler := NewHandler(&stubRepository{}, identity(role))
		recorder := call(t, handler, http.MethodGet, "/api/admin/system/menu-visibility", "")
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("角色 %q 状态码 = %d, want 403", role, recorder.Code)
		}
	}
}

func TestHandlerEmptyUserIDIsUnauthenticated(t *testing.T) {
	auth := stubAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "  ", Role: "super_admin"}}
	handler := NewHandler(&stubRepository{}, auth)

	recorder := call(t, handler, http.MethodGet, "/api/admin/system/menu-visibility", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, want 401", recorder.Code)
	}
}

func TestHandlerMapsAuthErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"未认证", shared.ErrUnauthenticated, http.StatusUnauthorized},
		{"依赖不可用", shared.ErrServiceUnavailable, http.StatusServiceUnavailable},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			handler := NewHandler(&stubRepository{}, stubAuthenticator{err: item.err})
			recorder := call(t, handler, http.MethodGet, "/api/admin/system/menu-visibility", "")
			if recorder.Code != item.want {
				t.Fatalf("状态码 = %d, want %d", recorder.Code, item.want)
			}
		})
	}
}

func TestHandlerPathAndMethodBoundaries(t *testing.T) {
	handler := NewHandler(&stubRepository{}, identity("super_admin"))

	if code := call(t, handler, http.MethodGet, "/api/admin/system/other", "").Code; code != http.StatusNotImplemented {
		t.Fatalf("未迁移路径状态码 = %d, want 501", code)
	}
	if code := call(t, handler, http.MethodPost, "/api/admin/system/menu-visibility", "").Code; code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d, want 405", code)
	}
	if code := call(t, handler, http.MethodDelete, "/api/admin/system/menu-visibility", "").Code; code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE 状态码 = %d, want 405", code)
	}
}

func TestHandlerUnavailableWithoutDependencies(t *testing.T) {
	recorder := call(t, NewHandler(nil, identity("super_admin")),
		http.MethodGet, "/api/admin/system/menu-visibility", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, want 503", recorder.Code)
	}
	recorder = call(t, NewHandler(&stubRepository{}, nil),
		http.MethodGet, "/api/admin/system/menu-visibility", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, want 503", recorder.Code)
	}
}

func TestNormalizeOverridesRejectsRelativePaths(t *testing.T) {
	if _, err := biz.NormalizeOverrides(map[string]bool{"loras": true}); err == nil {
		t.Fatal("相对路径应被拒绝")
	}
	if _, err := biz.NormalizeOverrides(map[string]bool{"/loras": true}); err != nil {
		t.Fatalf("绝对路径应通过，得到 %v", err)
	}
	if _, err := biz.NormalizeOverrides(map[string]bool{}); err != nil {
		t.Fatalf("空覆盖项应通过，得到 %v", err)
	}
}
