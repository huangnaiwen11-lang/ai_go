package adminapps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	biz "ai-business-service/internal/biz/adminapps"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

// fakeRepository 记录写调用，让测试能断言「传输层把哪些东西交给了业务层」。
type fakeRepository struct {
	createdWith  map[string]any
	updatedWith  map[string]any
	updatedID    string
	deletedID    string
	writeErr     error
	managedErr   error
	managedCalls int
}

func (f *fakeRepository) List(context.Context, biz.Query) (biz.Page, error) {
	return biz.Page{Apps: []biz.App{{ID: "507f1f77bcf86cd799439011", Fields: map[string]any{"id": "507f1f77bcf86cd799439011", "name": "Demo"}}}, Total: 1}, nil
}

func (f *fakeRepository) Get(context.Context, string) (biz.App, error) {
	return biz.App{ID: "507f1f77bcf86cd799439011", Fields: map[string]any{"id": "507f1f77bcf86cd799439011", "name": "Demo"}}, nil
}

func (f *fakeRepository) Overview(context.Context) (biz.Overview, error) {
	return biz.Overview{Total: 1, Active: 1, ByPlatform: map[string]int64{"android": 1}}, nil
}

func (f *fakeRepository) Create(_ context.Context, actor biz.Actor, payload map[string]any) (biz.App, error) {
	if f.writeErr != nil {
		return biz.App{}, f.writeErr
	}
	f.createdWith = payload
	f.createdWith["_actor"] = actor.ID
	return biz.App{ID: "507f1f77bcf86cd799439011", Fields: map[string]any{"id": "507f1f77bcf86cd799439011", "name": "Created"}}, nil
}

func (f *fakeRepository) Update(_ context.Context, actor biz.Actor, id string, payload map[string]any) (biz.App, error) {
	if f.writeErr != nil {
		return biz.App{}, f.writeErr
	}
	f.updatedID = id
	f.updatedWith = payload
	f.updatedWith["_actor"] = actor.ID
	return biz.App{ID: id, Fields: map[string]any{"id": id, "name": "Updated"}}, nil
}

func (f *fakeRepository) Delete(_ context.Context, actor biz.Actor, id string) (biz.App, error) {
	if f.writeErr != nil {
		return biz.App{}, f.writeErr
	}
	f.deletedID = id
	return biz.App{ID: id}, nil
}

func (*fakeRepository) PackageConfig(context.Context, string) (biz.Document, error) {
	return biz.Document{
		"appKey": "demo",
		"config": biz.Document{"android": biz.Document{}, "ios": biz.Document{}},
		"files":  biz.Document{},
	}, nil
}

func (*fakeRepository) UpdatePackageConfig(context.Context, biz.Actor, string, map[string]any) (biz.App, biz.Document, error) {
	return biz.App{Fields: map[string]any{"id": "507f1f77bcf86cd799439011"}}, biz.Document{}, nil
}
func (*fakeRepository) NativeConfigTemplate(context.Context, string) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) SetReviewMode(context.Context, biz.Actor, string, map[string]any) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) IntegrationCheck(context.Context, string) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) CreateBuildRequest(context.Context, biz.Actor, string, map[string]any) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) APIDomainCandidates(context.Context, string, map[string]any) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) RegisterAPIDomain(context.Context, biz.Actor, string, map[string]any) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) LegalPages(context.Context, string) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) UpdateLegalPublishing(context.Context, biz.Actor, string, map[string]any) (biz.Document, error) {
	return biz.Document{}, nil
}
func (f *fakeRepository) EnableManagedLegalPublishing(context.Context, biz.Actor, string) (biz.Document, error) {
	f.managedCalls++
	return biz.Document{}, f.managedErr
}
func (*fakeRepository) LegalAction(context.Context, biz.Actor, string, string, string, map[string]any) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) ListPlatformConfigs(context.Context, bool) ([]biz.Document, error) {
	return nil, nil
}
func (*fakeRepository) GetPlatformConfig(context.Context, string, string) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) UpdatePlatformConfig(context.Context, biz.Actor, string, string, map[string]any) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) DeletePlatformConfig(context.Context, biz.Actor, string, string) error {
	return nil
}
func (*fakeRepository) PatchPlatformConfig(context.Context, biz.Actor, string, string, string, map[string]any) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) ClonePlatformConfig(context.Context, biz.Actor, string, map[string]any) (biz.Document, error) {
	return biz.Document{}, nil
}
func (*fakeRepository) PlatformPreview(context.Context, string, string, string) (biz.Document, error) {
	return biz.Document{}, nil
}

// fakeAuthenticator 按需返回身份或错误。
type fakeAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (f fakeAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return f.identity, f.err
}

func superAdmin() fakeAuthenticator {
	return fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: "super_admin"}}
}

func plainAdmin() fakeAuthenticator {
	return fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-2", Role: "admin"}}
}

const appPath = "/api/admin/apps/507f1f77bcf86cd799439011"

func TestHandlerServesReadContract(t *testing.T) {
	h := NewHandler(&fakeRepository{}, superAdmin())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/apps?page=1&limit=20", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"apps"`) || !strings.Contains(w.Body.String(), `"pagination"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// 匿名与无权限必须可区分：前端只在 401 时清理令牌并跳转登录。
func TestHandlerDistinguishesUnauthenticatedFromForbidden(t *testing.T) {
	cases := []struct {
		name     string
		fake     fakeAuthenticator
		wantCode int
	}{
		{"匿名", fakeAuthenticator{err: shared.ErrUnauthenticated}, http.StatusUnauthorized},
		{"非管理员", fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "u-1", Role: "user"}}, http.StatusForbidden},
		{"缺少身份", fakeAuthenticator{}, http.StatusUnauthorized},
	}
	for _, testCase := range cases {
		h := NewHandler(&fakeRepository{}, testCase.fake)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil))
		if w.Code != testCase.wantCode {
			t.Errorf("%s: 状态码 = %d，期望 %d", testCase.name, w.Code, testCase.wantCode)
		}
	}
}

// 写入口比读严：普通 admin 能读，但不能建/改/删 App。
func TestHandlerRequiresSuperAdminForWrites(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/admin/apps"},
		{http.MethodPatch, appPath},
		{http.MethodDelete, appPath},
	}
	for _, testCase := range cases {
		repository := &fakeRepository{}
		h := NewHandler(repository, plainAdmin())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(`{}`)))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: 状态码 = %d，期望 403", testCase.method, testCase.path, w.Code)
		}
		if repository.createdWith != nil || repository.updatedWith != nil || repository.deletedID != "" {
			t.Errorf("%s %s: 被拒的请求不应触达仓储", testCase.method, testCase.path)
		}
	}
}

// 普通 admin 读仍然放行 —— 收紧只针对写。
func TestHandlerStillAllowsPlainAdminToRead(t *testing.T) {
	h := NewHandler(&fakeRepository{}, plainAdmin())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
}

func TestHandlerCreatesAppWithCreatedStatus(t *testing.T) {
	repository := &fakeRepository{}
	h := NewHandler(repository, superAdmin())
	body := `{"name":"Demo","platform":"web","domain":"https://example.com/"}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/admin/apps", strings.NewReader(body)))

	// Node 的 POST /admin/apps 成功是 201 且响应体形如 {app}。
	if w.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201；body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"app"`) {
		t.Fatalf("响应体缺少 app 包装: %s", w.Body.String())
	}
	if repository.createdWith["name"] != "Demo" {
		t.Fatalf("仓储收到的 payload = %#v", repository.createdWith)
	}
	if repository.createdWith["_actor"] != "admin-1" {
		t.Fatalf("审计主体 = %v，期望 admin-1", repository.createdWith["_actor"])
	}
}

func TestHandlerUpdatesAppAndPassesPathID(t *testing.T) {
	repository := &fakeRepository{}
	h := NewHandler(repository, superAdmin())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, appPath, strings.NewReader(`{"status":"suspended"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	if repository.updatedID != "507f1f77bcf86cd799439011" {
		t.Fatalf("仓储收到的 id = %q", repository.updatedID)
	}
	if repository.updatedWith["status"] != "suspended" {
		t.Fatalf("仓储收到的 payload = %#v", repository.updatedWith)
	}
}

// DELETE 是软删除，响应体与 Node 一致：`{success: true}`。
func TestHandlerSoftDeletesApp(t *testing.T) {
	repository := &fakeRepository{}
	h := NewHandler(repository, superAdmin())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, appPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatalf("响应体 = %s，期望含 success:true", w.Body.String())
	}
	if repository.deletedID != "507f1f77bcf86cd799439011" {
		t.Fatalf("仓储收到的 id = %q", repository.deletedID)
	}
}

// 冲突必须带上 Node 的具体文案，前端直接把它显示给管理员。
func TestHandlerSurfacesConflictMessage(t *testing.T) {
	repository := &fakeRepository{writeErr: &biz.ConflictError{Message: "Bundle ID already registered"}}
	h := NewHandler(repository, superAdmin())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/admin/apps", strings.NewReader(`{"name":"Demo","platform":"web"}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Bundle ID already registered") {
		t.Fatalf("响应体 = %s，期望含具体冲突文案", w.Body.String())
	}
}

func TestHandlerMapsValidationAndNotFound(t *testing.T) {
	cases := []struct {
		name     string
		writeErr error
		wantCode int
	}{
		{"非法输入", biz.ErrInvalid, http.StatusBadRequest},
		{"不存在", biz.ErrNotFound, http.StatusNotFound},
		{"依赖不可用", errors.New("boom"), http.StatusServiceUnavailable},
	}
	for _, testCase := range cases {
		h := NewHandler(&fakeRepository{writeErr: testCase.writeErr}, superAdmin())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, appPath, strings.NewReader(`{"status":"active"}`)))
		if w.Code != testCase.wantCode {
			t.Errorf("%s: 状态码 = %d，期望 %d", testCase.name, w.Code, testCase.wantCode)
		}
	}
}

// 未实现的子资源仍要如实报 501，不能伪装成资源不存在。
func TestHandlerReportsUnknownSubresourcesAsNotImplemented(t *testing.T) {
	paths := []string{
		"/api/admin/apps/507f1f77bcf86cd799439011/unsupported-child",
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch} {
			h := NewHandler(&fakeRepository{}, superAdmin())
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
			if w.Code != http.StatusNotImplemented {
				t.Errorf("%s %s: 状态码 = %d，期望 501", method, path, w.Code)
			}
			if !strings.Contains(w.Body.String(), "ADMIN_API_NOT_MIGRATED") {
				t.Errorf("%s %s: 响应体 = %s，期望含 ADMIN_API_NOT_MIGRATED", method, path, w.Body.String())
			}
		}
	}
}

func TestHandlerServesPackageConfigContract(t *testing.T) {
	h := NewHandler(&fakeRepository{}, superAdmin())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, appPath+"/package-config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, fragment := range []string{`"appKey"`, `"config"`, `"android"`, `"ios"`, `"files"`} {
		if !strings.Contains(w.Body.String(), fragment) {
			t.Errorf("body=%s, want %s", w.Body.String(), fragment)
		}
	}
}

func TestHandlerServesAppsCompatibilityEndpoints(t *testing.T) {
	for _, testCase := range []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{http.MethodGet, appPath + "/native-config-template", "", http.StatusOK},
		{http.MethodPatch, appPath + "/review-mode", `{"mode":"strict"}`, http.StatusOK},
		{http.MethodGet, appPath + "/integration-check", "", http.StatusOK},
		{http.MethodPost, appPath + "/build-request", `{"platform":"android"}`, http.StatusAccepted},
		{http.MethodPost, appPath + "/api-domain/candidates", `{}`, http.StatusOK},
		{http.MethodPost, appPath + "/api-domain/register", `{"confirmation":"REGISTER_DOMAIN","domain":"demo.app"}`, http.StatusAccepted},
		{http.MethodGet, appPath + "/legal-pages", "", http.StatusOK},
		{http.MethodPut, appPath + "/legal-pages/publishing-binding", `{"approvedBrandDomain":"demo.app","publisherBinding":{"status":"pending","provider":"cloudflare_r2"}}`, http.StatusOK},
		{http.MethodPost, appPath + "/legal-pages/publishing-binding/managed", `{}`, http.StatusAccepted},
		{http.MethodPost, appPath + "/legal-pages/privacy", `{}`, http.StatusServiceUnavailable},
		{http.MethodGet, "/api/admin/config/platforms", "", http.StatusOK},
		{http.MethodPut, "/api/admin/config/platforms/web?clientId=demo.app", `{"displayName":"Demo"}`, http.StatusOK},
		{http.MethodGet, "/api/admin/config/platforms/web/preview?clientId=demo.app", "", http.StatusOK},
	} {
		t.Run(testCase.method+" "+testCase.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			NewHandler(&fakeRepository{}, superAdmin()).ServeHTTP(w, httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(testCase.body)))
			if w.Code != testCase.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, testCase.want, w.Body.String())
			}
		})
	}
}

// 空 id 是「路由没匹配上」-> 404；格式不对的 id 是「参数非法」-> 400。
// 两者不能混：把参数写错说成「应用不存在」会让排障方向完全跑偏。
func TestHandlerDistinguishesMissingAppFromMalformedID(t *testing.T) {
	h := NewHandler(&fakeRepository{}, superAdmin())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/apps/", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("空 id 状态码 = %d，期望 404", w.Code)
	}

	for _, path := range []string{"/api/admin/apps/not-an-objectid", "/api/admin/apps/507f1f77bcf86cd79943901"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: 状态码 = %d，期望 400", path, w.Code)
		}
	}
}

// 请求体必须是 JSON 对象。数组/标量/空体都该是 400，而不是让业务层拿到奇怪的类型。
func TestHandlerRejectsNonObjectPayloads(t *testing.T) {
	for _, body := range []string{`[]`, `"text"`, ``, `null`, `{"name":`} {
		h := NewHandler(&fakeRepository{}, superAdmin())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/admin/apps", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%q: 状态码 = %d，期望 400", body, w.Code)
		}
	}
}

func TestHandlerRejectsTrailingJSONWithoutCallingRepository(t *testing.T) {
	repository := &fakeRepository{}
	w := httptest.NewRecorder()
	NewHandler(repository, superAdmin()).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/admin/apps", strings.NewReader(`{"name":"Demo"} {"other":"value"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=400 body=%s", w.Code, w.Body.String())
	}
	if repository.createdWith != nil {
		t.Fatalf("尾随 JSON 不应调用 Create，payload=%#v", repository.createdWith)
	}
}

func TestHandlerMapsManagedLegalPublishingUnavailable(t *testing.T) {
	repository := &fakeRepository{managedErr: biz.ErrExternalUnavailable}
	w := httptest.NewRecorder()
	NewHandler(repository, superAdmin()).ServeHTTP(w, httptest.NewRequest(http.MethodPost, appPath+"/legal-pages/publishing-binding/managed", strings.NewReader(`{}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=503 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"EXTERNAL_INTEGRATION_UNAVAILABLE"`) {
		t.Fatalf("body=%s, want EXTERNAL_INTEGRATION_UNAVAILABLE", w.Body.String())
	}
	if repository.managedCalls != 1 {
		t.Fatalf("managed calls=%d want=1", repository.managedCalls)
	}
}

// 未迁移的方法必须报 405 并给出 Allow，而不是静默落到别的分支。
func TestHandlerRejectsUnsupportedMethods(t *testing.T) {
	h := NewHandler(&fakeRepository{}, superAdmin())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, appPath, strings.NewReader(`{}`)))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, http.MethodPatch) {
		t.Fatalf("Allow = %q，期望含 PATCH", allow)
	}
}

func TestHandlerRejectsMalformedQuery(t *testing.T) {
	h := NewHandler(&fakeRepository{}, superAdmin())
	for _, query := range []string{"?page=abc", "?limit=1000", "?platform=desktop", "?page=1&page=2"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/apps"+query, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: 状态码 = %d，期望 400", query, w.Code)
		}
	}
}

// 依赖未接线时必须 503，而不是 panic 或 200。
func TestHandlerReportsUnavailableDependencies(t *testing.T) {
	w := httptest.NewRecorder()
	NewHandler(nil, superAdmin()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("空仓储状态码 = %d，期望 503", w.Code)
	}

	w = httptest.NewRecorder()
	NewHandler(&fakeRepository{}, nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("空鉴权器状态码 = %d，期望 503", w.Code)
	}
}

// 会话失效必须仍是 401，不能被写门禁改写成 403。
func TestHandlerKeepsSessionErrorsAsUnauthenticated(t *testing.T) {
	h := NewHandler(&fakeRepository{}, fakeAuthenticator{err: shared.ErrUnauthenticated})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/admin/apps", strings.NewReader(`{"name":"Demo","platform":"web"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "UNAUTHENTICATED") {
		t.Fatalf("响应体 = %s，期望含 UNAUTHENTICATED", w.Body.String())
	}
}
