package adminanalyticsga4

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	bizga4 "ai-business-service/internal/biz/adminanalyticsga4"
	"ai-business-service/internal/transport/sessionauth"
)

type fakeAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (f fakeAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return f.identity, f.err
}

// fakeReporter 记录收到的请求并回放预设结果，用来断言分派与失败映射。
//
// 必须加锁：Devices / Summary / conversion-compare / PWA 系列会经 biz 层的
// parallel* 并发扇出，同一请求内多条报表同时进来。不加锁既会让 -race 直接报，
// 也会让「第 n 条请求回放第 n 个脚本结果」这个序号语义在并发下错位——
// 那时测试失败的原因会指向业务代码，而实际错在替身。
type fakeReporter struct {
	mu       sync.Mutex
	requests []bizga4.ReportRequest
	reports  []bizga4.Report
	errors   []error
}

// record 原子地「登记请求 + 取回该请求对应的脚本结果」。
// append 与下标计算必须在同一个临界区内，否则两次并发调用可能拿到同一下标。
func (f *fakeReporter) record(request bizga4.ReportRequest) (bizga4.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, request)
	index := len(f.requests) - 1

	var report bizga4.Report
	if index < len(f.reports) {
		report = f.reports[index]
	}
	var err error
	if index < len(f.errors) {
		err = f.errors[index]
	}
	return report, err
}

func (f *fakeReporter) RunReport(_ context.Context, request bizga4.ReportRequest) (bizga4.Report, error) {
	return f.record(request)
}

func (f *fakeReporter) RunRealtimeReport(_ context.Context, request bizga4.ReportRequest) (bizga4.Report, error) {
	return f.record(request)
}

// snapshot 返回已登记请求的副本，供断言侧读取。
func (f *fakeReporter) snapshot() []bizga4.ReportRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make([]bizga4.ReportRequest, len(f.requests))
	copy(copied, f.requests)
	return copied
}

func adminHandler(reporter bizga4.Reporter, config Config) http.Handler {
	return NewHandler(fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: "admin"}}, config, reporter)
}

func configured() Config {
	return Config{PropertyID: "123456789", CredentialPresent: true}
}

func TestStatus掩码属性并且未配置报告明确失败(t *testing.T) {
	handler := adminHandler(nil, configured())

	status := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/status")
	if status.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d: %s", status.Code, status.Body.String())
	}
	data := responseData(t, status)
	if data["configured"] != true || data["propertyId"] != "***6789" {
		t.Fatalf("status data = %#v", data)
	}

	// 配置齐但客户端缺位：这是一个独立于「未配置」的终态。
	report := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/overview")
	if report.Code != http.StatusServiceUnavailable {
		t.Fatalf("configured-but-no-client report = %d: %s", report.Code, report.Body.String())
	}
	if responseCode(t, report) != "GA4_CLIENT_UNAVAILABLE" {
		t.Fatalf("report response = %s", report.Body.String())
	}

	missing := adminHandler(nil, Config{})
	report = request(t, missing, http.MethodGet, "/api/admin/analytics/ga4/overview")
	if report.Code != http.StatusServiceUnavailable || responseCode(t, report) != "GA4_NOT_CONFIGURED" {
		t.Fatalf("unconfigured report = %d: %s", report.Code, report.Body.String())
	}
}

func TestStatus在未配置时依然可读(t *testing.T) {
	// /status 必须先于门禁：它存在的意义就是回答「配好了没」。
	handler := adminHandler(nil, Config{})
	status := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/status")
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", status.Code, status.Body.String())
	}
	data := responseData(t, status)
	if data["configured"] != false || data["propertyId"] != nil {
		t.Fatalf("status data = %#v", data)
	}
}

func TestGA4先鉴权且拒绝普通用户(t *testing.T) {
	handler := NewHandler(
		fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1", Role: "user"}},
		Config{},
		nil,
	)
	if got := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/status"); got.Code != http.StatusForbidden {
		t.Fatalf("regular user = %d: %s", got.Code, got.Body.String())
	}
	if got := NewHandler(fakeAuthenticator{}, Config{}, nil); got == nil {
		t.Fatalf("handler must remain constructible")
	}
}

func Test未迁移路径返回501(t *testing.T) {
	handler := adminHandler(&fakeReporter{}, configured())
	got := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/unknown-report")
	if got.Code != http.StatusNotImplemented {
		t.Fatalf("unknown path = %d: %s", got.Code, got.Body.String())
	}
	if responseCode(t, got) != "ADMIN_API_NOT_MIGRATED" {
		t.Fatalf("unknown path code = %s", got.Body.String())
	}
}

func Test方法与路径不匹配时返回405(t *testing.T) {
	handler := adminHandler(&fakeReporter{}, configured())
	if got := request(t, handler, http.MethodPost, "/api/admin/analytics/ga4/overview"); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST overview = %d", got.Code)
	}
	if got := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/report"); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET report = %d", got.Code)
	}
}

func Test全部已接管路径都分发到业务层(t *testing.T) {
	// knownPath 收录 22 条：/status 由本层直答，/report 是 POST，
	// 其余 20 条 GET 必须全部落到业务层，一条都不能落进 501 兜底。
	paths := []string{
		"/realtime", "/overview", "/traffic", "/pages", "/demographics", "/devices",
		"/events", "/events/daily", "/funnel", "/retention", "/agents", "/search",
		"/insights", "/summary", "/conversion-compare",
		"/pwa/installs", "/pwa/active", "/pwa/platforms", "/pwa/summary", "/pwa/paywall-sources",
	}
	for _, path := range paths {
		reporter := &fakeReporter{}
		handler := adminHandler(reporter, configured())
		got := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4"+path)
		if got.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, got.Code, got.Body.String())
		}
		if len(reporter.snapshot()) == 0 {
			// /summary 与 /insights 也是打业务层，只是内部再扇出；这里只要求至少一次出站。
			t.Fatalf("GET %s 未到达业务层", path)
		}
	}

	reporter := &fakeReporter{}
	handler := adminHandler(reporter, configured())
	body := strings.NewReader(`{"dimensions":["date"],"metrics":["activeUsers"]}`)
	got := requestWithBody(t, handler, http.MethodPost, "/api/admin/analytics/ga4/report", body)
	if got.Code != http.StatusOK {
		t.Fatalf("POST /report = %d: %s", got.Code, got.Body.String())
	}
	if len(reporter.snapshot()) != 1 {
		t.Fatalf("POST /report 应产生一次出站请求，实际 %d", len(reporter.snapshot()))
	}
	// 维度度量必须由调用方决定，过滤器留空表示不传。
	sent := reporter.snapshot()[0]
	if len(sent.Dimensions) != 1 || sent.Dimensions[0] != "date" {
		t.Fatalf("dimensions = %#v", sent.Dimensions)
	}
	if sent.DimensionFilter != nil || sent.MetricFilter != nil {
		t.Fatalf("未给过滤器时不应产生过滤器：%#v / %#v", sent.DimensionFilter, sent.MetricFilter)
	}
	// 日期缺省由业务层补全，适配器只要求非空。
	if sent.StartDate == "" || sent.EndDate == "" {
		t.Fatalf("日期区间未被补全：%#v", sent)
	}
}

func Test查询参数解析对齐旧路由(t *testing.T) {
	reporter := &fakeReporter{}
	handler := adminHandler(reporter, configured())

	request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/traffic?startDate=2026-01-01&endDate=2026-01-31&limit=7")
	if len(reporter.snapshot()) != 1 {
		t.Fatalf("requests = %d", len(reporter.snapshot()))
	}
	sent := reporter.snapshot()[0]
	if sent.StartDate != "2026-01-01" || sent.EndDate != "2026-01-31" {
		t.Fatalf("日期区间 = %s..%s", sent.StartDate, sent.EndDate)
	}
	if sent.Limit != 7 {
		t.Fatalf("limit = %d, want 7", sent.Limit)
	}

	// 缺失与非法 limit 都交给业务层套默认值：/pages 的默认是 50，
	// 所以适配器看到的是 50 而不是 0，也不是把 abc 传给上游。
	reporter = &fakeReporter{}
	handler = adminHandler(reporter, configured())
	request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/pages?limit=abc")
	if got := reporter.snapshot()[0].Limit; got != 50 {
		t.Fatalf("非法 limit 应由业务层套用 /pages 的默认值 50，得到 %d", got)
	}
}

func Test事件名列表解析对齐旧路由(t *testing.T) {
	reporter := &fakeReporter{}
	handler := adminHandler(reporter, configured())

	request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/events/daily?events=page_view%2C%20purchase%20%2C%2Csign_up")
	if got := reporter.snapshot()[0].DimensionFilter; got == nil {
		t.Fatal("events 参数应被解析成 inList 过滤器")
	}
	filter := string(reporter.snapshot()[0].DimensionFilter)
	for _, want := range []string{"page_view", "purchase", "sign_up"} {
		if !strings.Contains(filter, want) {
			t.Fatalf("过滤器中缺 %s：%s", want, filter)
		}
	}
	if strings.Contains(filter, `""`) {
		t.Fatalf("空事件名应被丢弃：%s", filter)
	}
}

func Test转化对比参数默认值(t *testing.T) {
	reporter := &fakeReporter{}
	handler := adminHandler(reporter, configured())

	request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/conversion-compare")
	// 未给 days → 业务层用 7；includeBySource 缺省为「开」，因此会有来源查询。
	if count := len(reporter.snapshot()); count == 0 {
		t.Fatal("conversion-compare 未产生出站请求")
	}
	sent := reporter.snapshot()[0]
	if sent.StartDate == "" || sent.EndDate == "" {
		t.Fatalf("日期区间未被补全：%#v", sent)
	}

	reporter = &fakeReporter{}
	handler = adminHandler(reporter, configured())
	request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/conversion-compare?includeBySource=false&days=3")
	// 关掉来源后只剩「当期 + 上一期」两次基线查询。
	if count := len(reporter.snapshot()); count != 2 {
		t.Fatalf("includeBySource=false 应只有 2 次出站请求，实际 %d", count)
	}
}

func Test失败原因映射到不同状态码(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"未配置", bizga4.ErrNotConfigured, http.StatusServiceUnavailable, "GA4_NOT_CONFIGURED"},
		{"凭据被拒", fmt.Errorf("%w: invalid_grant", bizga4.ErrCredentialRejected), http.StatusServiceUnavailable, "GA4_CREDENTIAL_REJECTED"},
		{"上游故障", &bizga4.UpstreamError{Status: http.StatusInternalServerError, Body: "boom"}, http.StatusServiceUnavailable, "GA4_UPSTREAM_ERROR"},
		{"上游拒绝入参", &bizga4.UpstreamError{Status: http.StatusBadRequest, Body: "bad dimension"}, http.StatusBadRequest, "GA4_REPORT_INVALID"},
		{"本地入参非法", fmt.Errorf("%w: invalid endDate", bizga4.ErrInvalidQuery), http.StatusBadRequest, "GA4_INVALID_QUERY"},
	}
	for _, testCase := range cases {
		reporter := &fakeReporter{errors: []error{testCase.err}}
		handler := adminHandler(reporter, configured())
		got := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/overview")
		if got.Code != testCase.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", testCase.name, got.Code, testCase.wantStatus, got.Body.String())
		}
		if code := responseCode(t, got); code != testCase.wantCode {
			t.Fatalf("%s: code = %s, want %s", testCase.name, code, testCase.wantCode)
		}
	}
}

func TestGA4失败永不返回502(t *testing.T) {
	// 502 在本项目唯一含义是「回退给了 Node 而 Node 不在」；
	// /api/admin/ 是 fail-closed、永不回退，所以任何失败都不该是 502。
	errorsUnderTest := []error{
		bizga4.ErrNotConfigured,
		bizga4.ErrCredentialRejected,
		fmt.Errorf("%w: wrapper", bizga4.ErrUpstream),
		&bizga4.UpstreamError{Status: http.StatusBadGateway, Body: "upstream said 502"},
		&bizga4.UpstreamError{Status: http.StatusUnauthorized, Body: "nope"},
		bizga4.ErrInvalidQuery,
		&bizga4.UpstreamError{Status: http.StatusTooManyRequests, Body: "quota"},
	}
	for _, failure := range errorsUnderTest {
		reporter := &fakeReporter{errors: []error{failure}}
		handler := adminHandler(reporter, configured())
		got := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/overview")
		if got.Code == http.StatusBadGateway {
			t.Fatalf("%v 被渲染成 502，会被读成「回退了 Node」", failure)
		}
	}
}

func Test上游错误消息被裁剪(t *testing.T) {
	reporter := &fakeReporter{errors: []error{&bizga4.UpstreamError{Status: http.StatusInternalServerError, Body: strings.Repeat("x", 5000)}}}
	handler := adminHandler(reporter, configured())
	got := request(t, handler, http.MethodGet, "/api/admin/analytics/ga4/overview")
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len([]rune(payload.Message)) > maxErrorMessageRunes+1 {
		t.Fatalf("错误消息未被裁剪：%d 字符", len([]rune(payload.Message)))
	}
}

func Test自定义报表请求体畸形时报400(t *testing.T) {
	handler := adminHandler(&fakeReporter{}, configured())
	got := requestWithBody(t, handler, http.MethodPost, "/api/admin/analytics/ga4/report", strings.NewReader("{not json"))
	if got.Code != http.StatusBadRequest || responseCode(t, got) != "GA4_INVALID_QUERY" {
		t.Fatalf("畸形请求体 = %d: %s", got.Code, got.Body.String())
	}
}

func Test自定义报表缺维度时报400(t *testing.T) {
	reporter := &fakeReporter{}
	handler := adminHandler(reporter, configured())
	got := requestWithBody(t, handler, http.MethodPost, "/api/admin/analytics/ga4/report", strings.NewReader(`{"metrics":["activeUsers"]}`))
	if got.Code != http.StatusBadRequest || responseCode(t, got) != "GA4_INVALID_QUERY" {
		t.Fatalf("缺 dimensions = %d: %s", got.Code, got.Body.String())
	}
	if len(reporter.snapshot()) != 0 {
		t.Fatalf("入参非法时不应发出出站请求，实际 %d", len(reporter.snapshot()))
	}
}

func Test掩码规则(t *testing.T) {
	cases := map[string]any{"": nil, "123": "***123", "1234": "***1234", "123456789": "***6789"}
	for input, want := range cases {
		if got := maskedPropertyID(input); got != want {
			t.Fatalf("maskedPropertyID(%q) = %#v, want %#v", input, got, want)
		}
	}
}

func request(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func requestWithBody(t *testing.T, handler http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, body))
	return recorder
}

func responseData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil || !payload.Success {
		t.Fatalf("response = %s, error = %v", recorder.Body.String(), err)
	}
	return payload.Data
}

func responseCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Code
}
