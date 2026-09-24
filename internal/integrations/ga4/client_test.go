package ga4

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bizga4 "ai-business-service/internal/biz/adminanalyticsga4"
)

// 合同测试全程只打 httptest 伪上游，不接触 Google，也不需要真实凭据。
// 密钥在测试内即时生成，仓库里不落任何私钥。

const (
	testPropertyID   = "123456789"
	testClientEmail  = "cling-ga4@example.iam.gserviceaccount.com"
	testPrivateKeyID = "key-id-1"
)

type fakeUpstream struct {
	server *httptest.Server

	tokenRequests  int
	reportRequests int
	redirectHits   int
	lastReportBody map[string]any
	lastAssertion  string
	lastAuthHeader string
	lastReportPath string
	tokenStatus    int
	tokenBody      string
	reportStatus   int
	reportBody     string
	redirectToken  bool
	publicKey      *rsa.PublicKey

	mu sync.Mutex
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	upstream := &fakeUpstream{
		tokenStatus:  http.StatusOK,
		reportStatus: http.StatusOK,
		tokenBody:    `{"access_token":"token-1","expires_in":3600}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		upstream.tokenRequests++
		upstream.lastAssertion = r.FormValue("assertion")
		grantType := r.FormValue("grant_type")
		redirect := upstream.redirectToken
		status := upstream.tokenStatus
		body := upstream.tokenBody
		upstream.mu.Unlock()

		if contentType := r.Header.Get("Content-Type"); contentType != "application/x-www-form-urlencoded" {
			t.Errorf("token Content-Type = %q", contentType)
		}
		if grantType != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", grantType)
		}
		if redirect {
			w.Header().Set("Location", "/token-elsewhere")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/token-elsewhere", func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		upstream.redirectHits++
		upstream.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1beta/", func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		upstream.reportRequests++
		upstream.lastAuthHeader = r.Header.Get("Authorization")
		upstream.lastReportPath = r.URL.Path
		status := upstream.reportStatus
		body := upstream.reportBody
		upstream.mu.Unlock()

		decoded, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read report body: %v", err)
		}
		var parsed map[string]any
		if len(decoded) > 0 {
			if err := json.Unmarshal(decoded, &parsed); err != nil {
				t.Errorf("report body is not JSON: %v (%s)", err, decoded)
			}
		}
		upstream.mu.Lock()
		upstream.lastReportBody = parsed
		upstream.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	})
	upstream.server = httptest.NewServer(mux)
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (f *fakeUpstream) options(t *testing.T, credentials Credentials, now func() time.Time) Options {
	t.Helper()
	return Options{
		PropertyID:  testPropertyID,
		Credentials: credentials,
		APIBase:     f.server.URL + "/v1beta",
		TokenURL:    f.server.URL + "/token",
		Timeout:     5 * time.Second,
		Now:         now,
	}
}

func (f *fakeUpstream) counters() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenRequests, f.reportRequests
}

func (f *fakeUpstream) body() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReportBody
}

// newCredentials 生成一份自签的服务账号凭据，并把公钥留给断言校验用。
func newCredentials(t *testing.T) Credentials {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	return Credentials{
		ClientEmail:  testClientEmail,
		PrivateKey:   string(block),
		PrivateKeyID: testPrivateKeyID,
	}
}

func newClient(t *testing.T, upstream *fakeUpstream, now func() time.Time) *Client {
	t.Helper()
	credentials := newCredentials(t)
	client, err := New(upstream.options(t, credentials, now))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key, err := parsePrivateKey(credentials.PrivateKey)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	upstream.mu.Lock()
	upstream.publicKey = &key.PublicKey
	upstream.mu.Unlock()
	return client
}

func TestRunReport签出可验证的RS256断言并复用令牌缓存(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.reportBody = `{"rows":[{"dimensionValues":[{"value":"20260920"}],"metricValues":[{"value":"12"}]}],"rowCount":"1"}`
	fixed := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	client := newClient(t, upstream, func() time.Time { return fixed })

	request := bizga4.ReportRequest{
		Dimensions: []string{"date"},
		Metrics:    []string{"activeUsers"},
		StartDate:  "2026-08-22",
		EndDate:    "2026-09-20",
		Limit:      20,
		OrderBys:   []bizga4.OrderBy{{Dimension: &bizga4.OrderByDimension{DimensionName: "date"}}},
	}
	for attempt := 0; attempt < 2; attempt++ {
		report, err := client.RunReport(context.Background(), request)
		if err != nil {
			t.Fatalf("RunReport #%d: %v", attempt, err)
		}
		if report.RowCount != 1 {
			t.Fatalf("rowCount = %d", report.RowCount)
		}
		if got := report.Rows[0]["activeUsers"]; got != float64(12) {
			t.Fatalf("activeUsers = %#v", got)
		}
	}

	tokenRequests, reportRequests := upstream.counters()
	if tokenRequests != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 (令牌缓存失效)", tokenRequests)
	}
	if reportRequests != 2 {
		t.Fatalf("report endpoint hit %d times, want 2", reportRequests)
	}

	upstream.mu.Lock()
	assertion := upstream.lastAssertion
	authHeader := upstream.lastAuthHeader
	path := upstream.lastReportPath
	upstream.mu.Unlock()

	if authHeader != "Bearer token-1" {
		t.Fatalf("Authorization = %q", authHeader)
	}
	if want := "/v1beta/properties/" + testPropertyID + ":runReport"; path != want {
		t.Fatalf("report path = %q, want %q", path, want)
	}
	verifyAssertion(t, assertion, upstream)
}

func verifyAssertion(t *testing.T, assertion string, upstream *fakeUpstream) {
	t.Helper()
	segments := strings.Split(assertion, ".")
	if len(segments) != 3 {
		t.Fatalf("assertion has %d segments, want 3", len(segments))
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Algorithm != "RS256" || header.Type != "JWT" || header.KeyID != testPrivateKeyID {
		t.Fatalf("header = %#v", header)
	}

	claimsRaw, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims struct {
		Issuer   string `json:"iss"`
		Subject  string `json:"sub"`
		Audience string `json:"aud"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
		Scope    string `json:"scope"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Issuer != testClientEmail || claims.Subject != testClientEmail {
		t.Fatalf("claims iss/sub = %q/%q", claims.Issuer, claims.Subject)
	}
	if want := upstream.server.URL + "/token"; claims.Audience != want {
		t.Fatalf("aud = %q, want %q", claims.Audience, want)
	}
	if claims.Scope != analyticsScope {
		t.Fatalf("scope = %q", claims.Scope)
	}
	if claims.Expires-claims.IssuedAt != 3600 {
		t.Fatalf("assertion lifetime = %d, want 3600", claims.Expires-claims.IssuedAt)
	}

	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	upstream.mu.Lock()
	publicKey := upstream.publicKey
	upstream.mu.Unlock()
	if publicKey == nil {
		t.Fatal("public key was not captured")
	}
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("assertion signature does not verify: %v", err)
	}
}

func TestRunReport请求体逐字段对齐旧实现(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.reportBody = `{"rows":[]}`
	client := newClient(t, upstream, time.Now)

	if _, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions:      []string{"eventName", "customEvent:source"},
		Metrics:         []string{"eventCount"},
		StartDate:       "2026-01-01",
		EndDate:         "2026-01-31",
		Limit:           30,
		DimensionFilter: json.RawMessage(`{"filter":{"fieldName":"eventName","inListFilter":{"values":["purchase"]}}}`),
		OrderBys: []bizga4.OrderBy{
			{Dimension: &bizga4.OrderByDimension{DimensionName: "eventName"}},
			{Metric: &bizga4.OrderByMetric{MetricName: "eventCount"}, Desc: true},
		},
	}); err != nil {
		t.Fatalf("RunReport: %v", err)
	}

	body := upstream.body()
	if body == nil {
		t.Fatal("upstream did not receive a body")
	}
	// offset 必须显式出现（旧实现总是带上它），limit 必须透传。
	if offset, ok := body["offset"]; !ok || offset != float64(0) {
		t.Fatalf("offset = %#v (present=%v)", offset, ok)
	}
	if body["limit"] != float64(30) {
		t.Fatalf("limit = %#v", body["limit"])
	}
	dateRanges, ok := body["dateRanges"].([]any)
	if !ok || len(dateRanges) != 1 {
		t.Fatalf("dateRanges = %#v", body["dateRanges"])
	}
	first := dateRanges[0].(map[string]any)
	if first["startDate"] != "2026-01-01" || first["endDate"] != "2026-01-31" {
		t.Fatalf("dateRanges[0] = %#v", first)
	}
	dimensions, ok := body["dimensions"].([]any)
	if !ok || len(dimensions) != 2 {
		t.Fatalf("dimensions = %#v", body["dimensions"])
	}
	if dimensions[0].(map[string]any)["name"] != "eventName" {
		t.Fatalf("dimensions[0] = %#v", dimensions[0])
	}
	orderBys, ok := body["orderBys"].([]any)
	if !ok || len(orderBys) != 2 {
		t.Fatalf("orderBys = %#v", body["orderBys"])
	}
	firstOrder := orderBys[0].(map[string]any)
	if _, hasDesc := firstOrder["desc"]; hasDesc {
		t.Fatalf("升序排序不应带 desc 键：%#v", firstOrder)
	}
	secondOrder := orderBys[1].(map[string]any)
	if secondOrder["desc"] != true {
		t.Fatalf("降序排序应带 desc=true：%#v", secondOrder)
	}
	if !strings.Contains(string(mustMarshal(t, body["dimensionFilter"])), "inListFilter") {
		t.Fatalf("dimensionFilter 未透传：%#v", body["dimensionFilter"])
	}
}

func TestRealtimeReport不带日期区间但带分钟窗口(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.reportBody = `{"rows":[{"dimensionValues":[{"value":"China"}],"metricValues":[{"value":"7"}]}]}`
	client := newClient(t, upstream, time.Now)

	report, err := client.RunRealtimeReport(context.Background(), bizga4.ReportRequest{
		Dimensions:   []string{"country"},
		Metrics:      []string{"activeUsers"},
		MinuteRanges: []bizga4.MinuteRange{{StartMinutesAgo: 29, EndMinutesAgo: 0}},
	})
	if err != nil {
		t.Fatalf("RunRealtimeReport: %v", err)
	}
	if got := report.Rows[0]["activeUsers"]; got != float64(7) {
		t.Fatalf("activeUsers = %#v", got)
	}
	upstream.mu.Lock()
	path := upstream.lastReportPath
	upstream.mu.Unlock()
	if want := "/v1beta/properties/" + testPropertyID + ":runRealtimeReport"; path != want {
		t.Fatalf("realtime path = %q, want %q", path, want)
	}

	body := upstream.body()
	if _, hasDateRanges := body["dateRanges"]; hasDateRanges {
		t.Fatalf("实时报表不应带 dateRanges：%#v", body)
	}
	minuteRanges, ok := body["minuteRanges"].([]any)
	if !ok || len(minuteRanges) != 1 {
		t.Fatalf("minuteRanges = %#v", body["minuteRanges"])
	}
	window := minuteRanges[0].(map[string]any)
	if window["startMinutesAgo"] != float64(29) || window["endMinutesAgo"] != float64(0) {
		t.Fatalf("minuteRanges[0] = %#v", window)
	}
}

func TestRunReport缺省行数上限为旧实现口径(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.reportBody = `{"rows":[]}`
	client := newClient(t, upstream, time.Now)

	if _, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"date"},
		Metrics:    []string{"activeUsers"},
		StartDate:  "2026-09-01",
		EndDate:    "2026-09-20",
	}); err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if limit := upstream.body()["limit"]; limit != float64(bizga4.DefaultReportRowLimit) {
		t.Fatalf("limit = %#v, want %d", limit, bizga4.DefaultReportRowLimit)
	}
}

func TestRunReport令牌过期后重新签发(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.tokenBody = `{"access_token":"token-1","expires_in":120}`
	upstream.reportBody = `{"rows":[]}`
	clock := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	client := newClient(t, upstream, func() time.Time { return clock })

	request := bizga4.ReportRequest{Dimensions: []string{"date"}, Metrics: []string{"activeUsers"}, StartDate: "2026-09-01", EndDate: "2026-09-20"}
	if _, err := client.RunReport(context.Background(), request); err != nil {
		t.Fatalf("RunReport #1: %v", err)
	}
	// expires_in 120 秒、余量 60 秒 → 有效窗口 60 秒。
	clock = clock.Add(30 * time.Second)
	if _, err := client.RunReport(context.Background(), request); err != nil {
		t.Fatalf("RunReport #2: %v", err)
	}
	tokenRequests, _ := upstream.counters()
	if tokenRequests != 1 {
		t.Fatalf("窗口内不应重取令牌，token 请求 = %d", tokenRequests)
	}

	clock = clock.Add(40 * time.Second)
	if _, err := client.RunReport(context.Background(), request); err != nil {
		t.Fatalf("RunReport #3: %v", err)
	}
	tokenRequests, _ = upstream.counters()
	if tokenRequests != 2 {
		t.Fatalf("过期后应重取令牌，token 请求 = %d", tokenRequests)
	}
}

func TestRunReport上游故障映射为UpstreamError(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.reportStatus = http.StatusInternalServerError
	upstream.reportBody = `{"error":{"message":"backend error"}}`
	client := newClient(t, upstream, time.Now)

	_, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"date"}, Metrics: []string{"activeUsers"}, StartDate: "2026-09-01", EndDate: "2026-09-20",
	})
	if !errors.Is(err, bizga4.ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	var upstreamError *bizga4.UpstreamError
	if !errors.As(err, &upstreamError) || upstreamError.Status != http.StatusInternalServerError {
		t.Fatalf("upstream error = %#v", upstreamError)
	}
	if !strings.Contains(upstreamError.Body, "backend error") {
		t.Fatalf("上游错误体未保留：%q", upstreamError.Body)
	}
}

func TestRunReport区分维度不可用与凭据被拒(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.reportStatus = http.StatusBadRequest
	upstream.reportBody = `{"error":{"message":"Field customEvent:source is not a valid dimension"}}`
	client := newClient(t, upstream, time.Now)

	_, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"customEvent:source"}, Metrics: []string{"eventCount"}, StartDate: "2026-09-01", EndDate: "2026-09-20",
	})
	if !errors.Is(err, bizga4.ErrUpstream) {
		t.Fatalf("400 应归为上游错误，err = %v", err)
	}
	if errors.Is(err, bizga4.ErrCredentialRejected) {
		t.Fatalf("400 不应归为凭据被拒：%v", err)
	}
}

func Test数据端点401归为凭据被拒且只重试一次(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.reportStatus = http.StatusUnauthorized
	upstream.reportBody = `{"error":{"message":"invalid credentials"}}`
	client := newClient(t, upstream, time.Now)

	_, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"date"}, Metrics: []string{"activeUsers"}, StartDate: "2026-09-01", EndDate: "2026-09-20",
	})
	if !errors.Is(err, bizga4.ErrCredentialRejected) {
		t.Fatalf("err = %v, want ErrCredentialRejected", err)
	}
	tokenRequests, reportRequests := upstream.counters()
	if reportRequests != 2 {
		t.Fatalf("401 后应恰好重试一次，report 请求 = %d", reportRequests)
	}
	if tokenRequests != 2 {
		t.Fatalf("重试前应强制重取令牌，token 请求 = %d", tokenRequests)
	}
}

func Test令牌端点拒绝凭据(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.tokenStatus = http.StatusForbidden
	upstream.tokenBody = `{"error":"invalid_grant"}`
	client := newClient(t, upstream, time.Now)

	_, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"date"}, Metrics: []string{"activeUsers"}, StartDate: "2026-09-01", EndDate: "2026-09-20",
	})
	if !errors.Is(err, bizga4.ErrCredentialRejected) {
		t.Fatalf("err = %v, want ErrCredentialRejected", err)
	}
	if _, reportRequests := upstream.counters(); reportRequests != 0 {
		t.Fatalf("凭据被拒时不应打数据端点，report 请求 = %d", reportRequests)
	}
}

func Test出站请求不跟随重定向(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.redirectToken = true
	client := newClient(t, upstream, time.Now)

	_, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"date"}, Metrics: []string{"activeUsers"}, StartDate: "2026-09-01", EndDate: "2026-09-20",
	})
	if err == nil {
		t.Fatal("302 应被判为失败")
	}
	if !errors.Is(err, bizga4.ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	upstream.mu.Lock()
	redirectHits := upstream.redirectHits
	upstream.mu.Unlock()
	if redirectHits != 0 {
		t.Fatalf("重定向目标被请求了 %d 次，Bearer 令牌可能已泄漏", redirectHits)
	}
}

func TestRunReport缺日期区间时明确报错(t *testing.T) {
	upstream := newFakeUpstream(t)
	client := newClient(t, upstream, time.Now)

	_, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"date"}, Metrics: []string{"activeUsers"},
	})
	if !errors.Is(err, bizga4.ErrInvalidQuery) {
		t.Fatalf("err = %v, want ErrInvalidQuery", err)
	}
	if _, reportRequests := upstream.counters(); reportRequests != 0 {
		t.Fatalf("入参非法时不应发出请求，report 请求 = %d", reportRequests)
	}
}

func TestNew拒绝不完整配置(t *testing.T) {
	credentials := newCredentials(t)

	cases := map[string]Options{
		"缺属性ID":  {Credentials: credentials},
		"缺凭据":    {PropertyID: testPropertyID},
		"凭据不完整":  {PropertyID: testPropertyID, Credentials: Credentials{ClientEmail: testClientEmail, PrivateKeyID: testPrivateKeyID}},
		"私钥非PEM": {PropertyID: testPropertyID, Credentials: Credentials{ClientEmail: testClientEmail, PrivateKeyID: testPrivateKeyID, PrivateKey: "not-a-pem"}},
		"端点非法":   {PropertyID: testPropertyID, Credentials: credentials, APIBase: "ftp://example.com"},
		"端点带查询串": {PropertyID: testPropertyID, Credentials: credentials, APIBase: "https://example.com/?x=1"},
	}
	for name, options := range cases {
		if _, err := New(options); !errors.Is(err, bizga4.ErrNotConfigured) {
			t.Fatalf("%s: err = %v, want ErrNotConfigured", name, err)
		}
	}
}

func TestLoadCredentials支持内联与文件两种来源(t *testing.T) {
	credentials := newCredentials(t)
	payload, err := json.Marshal(credentials)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	inline, err := LoadCredentials(string(payload))
	if err != nil {
		t.Fatalf("LoadCredentials(inline): %v", err)
	}
	if inline.ClientEmail != testClientEmail || inline.PrivateKey != credentials.PrivateKey {
		t.Fatalf("inline credentials = %#v", inline)
	}

	directory := t.TempDir()
	path := filepath.Join(directory, "service-account.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write credentials file: %v", err)
	}
	fromFile, err := LoadCredentials(path)
	if err != nil {
		t.Fatalf("LoadCredentials(file): %v", err)
	}
	if fromFile.ClientEmail != testClientEmail {
		t.Fatalf("file credentials = %#v", fromFile)
	}

	if _, err := LoadCredentials(""); !errors.Is(err, bizga4.ErrNotConfigured) {
		t.Fatalf("空来源应报 ErrNotConfigured，err = %v", err)
	}
	if _, err := LoadCredentials("{not json"); !errors.Is(err, bizga4.ErrNotConfigured) {
		t.Fatalf("畸形 JSON 应报 ErrNotConfigured，err = %v", err)
	}
	if _, err := LoadCredentials(filepath.Join(directory, "missing.json")); !errors.Is(err, bizga4.ErrNotConfigured) {
		t.Fatalf("缺失文件应报 ErrNotConfigured，err = %v", err)
	}
}

func TestFromEnvironment优先顺序与旧实现一致(t *testing.T) {
	// t.Setenv 会在用例结束后自动还原，不会污染其它用例。
	t.Setenv("GA4_PROPERTY_ID", "")
	t.Setenv("GOOGLE_ANALYTICS_PROPERTY_ID", "")
	t.Setenv("GA_PROPERTY_ID", "")
	t.Setenv("GA4_PROPERTY", "")
	t.Setenv("GOOGLE_SERVICE_ACCOUNT_KEY", "")
	t.Setenv("GOOGLE_SERVICE_ACCOUNT_JSON", "")
	t.Setenv("GOOGLE_SERVICE_ACCOUNT", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	if FromEnvironment().Configured() {
		t.Fatal("空环境下不应判定为已配置")
	}

	t.Setenv("GA4_PROPERTY", "999")
	t.Setenv("GOOGLE_SERVICE_ACCOUNT_JSON", `{"client_email":"json@example.com"}`)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/tmp/credentials.json")

	environment := FromEnvironment()
	if environment.PropertyID != "999" {
		t.Fatalf("PropertyID = %q", environment.PropertyID)
	}
	// GOOGLE_APPLICATION_CREDENTIALS 的优先级高于 GOOGLE_SERVICE_ACCOUNT_JSON。
	if environment.CredentialSource != "/tmp/credentials.json" {
		t.Fatalf("CredentialSource = %q, want 文件路径优先", environment.CredentialSource)
	}

	t.Setenv("GOOGLE_SERVICE_ACCOUNT_KEY", `{"client_email":"key@example.com"}`)
	if got := FromEnvironment().CredentialSource; got != `{"client_email":"key@example.com"}` {
		t.Fatalf("CredentialSource = %q, want GOOGLE_SERVICE_ACCOUNT_KEY 最高优先", got)
	}
}

func Test归一化度量与行数(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.reportBody = `{
      "rowCount": "3",
      "dimensionHeaders": [{"name":"country"},{"name":"date"}],
      "metricHeaders": [{"name":"activeUsers","type":"TYPE_INTEGER"},{"name":"bounceRate","type":"TYPE_FLOAT"}],
      "rows": [
        {"dimensionValues":[{"value":"China"},{"value":"20260920"}],"metricValues":[{"value":"12"},{"value":"0.5"}]},
        {"dimensionValues":[{"value":"(not set)"},{"value":"20260920"}],"metricValues":[{"value":"NaN"},{"value":""}]},
        {"dimensionValues":[{"value":"Japan"},{"value":"20260919"}],"metricValues":[{"value":"7"},{"value":"0"}]}
      ]
    }`
	client := newClient(t, upstream, time.Now)

	report, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"country", "date"},
		Metrics:    []string{"activeUsers", "bounceRate"},
		StartDate:  "2026-09-01",
		EndDate:    "2026-09-20",
	})
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}

	if report.RowCount != 3 {
		t.Fatalf("rowCount = %d, want 3（上游给的是字符串 \"3\"）", report.RowCount)
	}
	if got := report.Rows[0]["activeUsers"]; got != float64(12) {
		t.Fatalf("activeUsers = %#v, want 12", got)
	}
	if got := report.Rows[0]["bounceRate"]; got != 0.5 {
		t.Fatalf("bounceRate = %#v, want 0.5", got)
	}
	// 解析不出数值时必须保留原始字符串，否则会变成 0 并污染汇总。
	if got := report.Rows[1]["activeUsers"]; got != "NaN" {
		t.Fatalf("NaN 度量应保留字符串，得到 %#v", got)
	}
	if got := report.Rows[1]["country"]; got != "(not set)" {
		t.Fatalf("维度取值应原样保留，得到 %#v", got)
	}
	if got := report.Rows[1]["bounceRate"]; got != "" {
		t.Fatalf("空度量应保留空字符串，得到 %#v", got)
	}
	if report.Metadata.Dimensions[0] != "country" || report.Metadata.Dimensions[1] != "date" {
		t.Fatalf("metadata.dimensions = %#v", report.Metadata.Dimensions)
	}
	if report.Metadata.Metrics[0].Name != "activeUsers" || report.Metadata.Metrics[0].Type != "TYPE_INTEGER" {
		t.Fatalf("metadata.metrics[0] = %#v", report.Metadata.Metrics[0])
	}

	// 序列化必须成功：任何非有限浮点数漏进 rows 都会让整个响应写不出去。
	if _, err := json.Marshal(report); err != nil {
		t.Fatalf("report 无法序列化：%v", err)
	}
}

func Test无头表时回退到请求侧维度与度量名(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.reportBody = `{"rows":[]}`
	client := newClient(t, upstream, time.Now)

	report, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"date"}, Metrics: []string{"activeUsers"}, StartDate: "2026-09-01", EndDate: "2026-09-20",
	})
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if len(report.Metadata.Dimensions) != 1 || report.Metadata.Dimensions[0] != "date" {
		t.Fatalf("metadata.dimensions = %#v", report.Metadata.Dimensions)
	}
	if len(report.Metadata.Metrics) != 1 || report.Metadata.Metrics[0].Name != "activeUsers" {
		t.Fatalf("metadata.metrics = %#v", report.Metadata.Metrics)
	}
	if report.Rows == nil {
		t.Fatal("无数据时 rows 应为空切片而不是 nil，否则会序列化成 null")
	}
	if report.RowCount != 0 {
		t.Fatalf("rowCount = %d, want 0（回退到 len(rows)）", report.RowCount)
	}
}

func Test响应体不可解析时报上游错误(t *testing.T) {
	upstream := newFakeUpstream(t)
	// 这是被 io.LimitReader 截断后的形态：JSON 中途断开，解码必然失败。
	upstream.reportBody = `{"rows":[` + strings.Repeat(`{"dimensionValues":[{"value":"x"}],"metricValues":[{"value":"1"}]},`, 8)
	client := newClient(t, upstream, time.Now)

	_, err := client.RunReport(context.Background(), bizga4.ReportRequest{
		Dimensions: []string{"date"}, Metrics: []string{"activeUsers"}, StartDate: "2026-09-01", EndDate: "2026-09-20",
	})
	if !errors.Is(err, bizga4.ErrUpstream) {
		t.Fatalf("截断响应应报 ErrUpstream，err = %v", err)
	}
}

func TestNil客户端返回未配置(t *testing.T) {
	var client *Client
	if _, err := client.RunReport(context.Background(), bizga4.ReportRequest{}); !errors.Is(err, bizga4.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if _, err := client.RunRealtimeReport(context.Background(), bizga4.ReportRequest{}); !errors.Is(err, bizga4.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}
