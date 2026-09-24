package adminsubscription

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	biz "ai-business-service/internal/biz/adminsubscription"
)

type stubRepository struct {
	overview    biz.Overview
	trends      []biz.Trend
	breakdown   biz.Breakdown
	subscribers biz.SubscribersPage
	err         error
	lastDays    int
	lastQuery   biz.Query
}

func (stub *stubRepository) SubscriptionOverview(_ context.Context, days int) (biz.Overview, error) {
	stub.lastDays = days
	return stub.overview, stub.err
}
func (stub *stubRepository) SubscriptionTrends(_ context.Context, days int) ([]biz.Trend, error) {
	stub.lastDays = days
	return stub.trends, stub.err
}
func (stub *stubRepository) SubscriptionBreakdown(context.Context) (biz.Breakdown, error) {
	return stub.breakdown, stub.err
}
func (stub *stubRepository) SubscriptionSubscribers(_ context.Context, query biz.Query) (biz.SubscribersPage, error) {
	stub.lastQuery = query
	return stub.subscribers, stub.err
}

func allow(*http.Request) error { return nil }
func deny(*http.Request) error  { return errors.New("forbidden") }

func call(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func dataOf(t *testing.T, recorder *httptest.ResponseRecorder) any {
	t.Helper()
	var envelope struct {
		Success bool `json:"success"`
		Data    any  `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, recorder.Body.String())
	}
	if !envelope.Success {
		t.Fatalf("响应未成功: %s", recorder.Body.String())
	}
	return envelope.Data
}

func Test订阅概览默认30天并映射字段(t *testing.T) {
	repository := &stubRepository{overview: biz.Overview{
		ActiveSubscribers: 3,
		ChurnRate:         12.5,
		Expiring:          biz.Expiring{D7: 1, D30: 2, D90: 3},
		RenewalForecast:   []biz.RenewalForecast{{Month: "2026-10", Count: 2, Revenue: 9.9}},
	}}
	handler := NewHandler(repository, allow)
	data := dataOf(t, call(t, handler, "/api/admin/analytics/subscription/overview")).(map[string]any)
	if repository.lastDays != 30 {
		t.Fatalf("days = %d，期望默认 30", repository.lastDays)
	}
	if data["activeSubscribers"].(float64) != 3 || data["churnRate"].(float64) != 12.5 {
		t.Fatalf("概览映射异常: %#v", data)
	}
	expiring := data["expiring"].(map[string]any)
	if expiring["d7"].(float64) != 1 || expiring["d90"].(float64) != 3 {
		t.Fatalf("到期分布异常: %#v", expiring)
	}
	if len(data["renewalForecast"].([]any)) != 1 {
		t.Fatalf("续订预测异常: %#v", data["renewalForecast"])
	}
}

func Test订阅趋势直接返回数组(t *testing.T) {
	repository := &stubRepository{trends: []biz.Trend{{Date: "2026-09-18", NewSubs: 2, NetChange: 2, ActiveCumulative: 5}}}
	handler := NewHandler(repository, allow)
	rows := dataOf(t, call(t, handler, "/api/admin/analytics/subscription/trends?days=7")).([]any)
	if len(rows) != 1 {
		t.Fatalf("趋势行数 = %d，期望 1", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["date"] != "2026-09-18" || row["activeCumulative"].(float64) != 5 {
		t.Fatalf("趋势映射异常: %#v", row)
	}
	if repository.lastDays != 7 {
		t.Fatalf("days = %d，期望 7", repository.lastDays)
	}
}

func Test订阅分解对缺失维度返回空数组而不是伪造(t *testing.T) {
	repository := &stubRepository{breakdown: biz.Breakdown{
		ByPeriod: []biz.BreakdownRow{{Key: "monthly", Count: 2}},
		RecentCancellations: []biz.Cancellation{{
			UserID: "u1", Tier: "unknown", BillingPeriod: "monthly", PaymentProvider: "unknown", CancelledAt: "2026-09-18T00:00:00Z",
		}},
	}}
	handler := NewHandler(repository, allow)
	data := dataOf(t, call(t, handler, "/api/admin/analytics/subscription/breakdown")).(map[string]any)
	for _, key := range []string{"byTier", "byProvider", "byCountry", "byChannel"} {
		rows, ok := data[key].([]any)
		if !ok || len(rows) != 0 {
			t.Fatalf("%s 必须是空数组，实际 %#v", key, data[key])
		}
	}
	period := data["byPeriod"].([]any)[0].(map[string]any)
	if period["period"] != "monthly" || period["count"].(float64) != 2 {
		t.Fatalf("周期分解异常: %#v", period)
	}
	if len(data["recentCancellations"].([]any)) != 1 {
		t.Fatalf("近期取消异常: %#v", data["recentCancellations"])
	}
}

func Test订阅订阅者分页与查询参数(t *testing.T) {
	repository := &stubRepository{subscribers: biz.SubscribersPage{Total: 42, Subscribers: []biz.Subscriber{{SubscriptionID: "s1", UserID: "u1", Tier: "unknown", Status: "active", Channel: "unknown", PaymentProvider: "unknown", CreatedAt: "2026-09-18T00:00:00Z"}}}}
	handler := NewHandler(repository, allow)
	data := dataOf(t, call(t, handler, "/api/admin/analytics/subscription/subscribers?page=2&pageSize=10&status=active")).(map[string]any)
	if repository.lastQuery.Page != 2 || repository.lastQuery.PageSize != 10 || repository.lastQuery.Status != "active" {
		t.Fatalf("查询参数异常: %#v", repository.lastQuery)
	}
	if data["total"].(float64) != 42 || data["page"].(float64) != 2 || data["pageSize"].(float64) != 10 {
		t.Fatalf("分页回显异常: %#v", data)
	}
	subscriber := data["subscribers"].([]any)[0].(map[string]any)
	if subscriber["subscriptionId"] != "s1" || subscriber["status"] != "active" {
		t.Fatalf("订阅者映射异常: %#v", subscriber)
	}
	if subscriber["username"] != nil || subscriber["pricePaid"] != nil {
		t.Fatalf("未知字段必须为 null: %#v", subscriber)
	}
}

func Test订阅拒绝越界参数与未迁移路径(t *testing.T) {
	handler := NewHandler(&stubRepository{}, allow)
	if recorder := call(t, handler, "/api/admin/analytics/subscription/overview?days=0"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("days=0 状态码 = %d，期望 400", recorder.Code)
	}
	if recorder := call(t, handler, "/api/admin/analytics/subscription/subscribers?pageSize=1000"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("pageSize=1000 状态码 = %d，期望 400", recorder.Code)
	}
	if recorder := call(t, handler, "/api/admin/analytics/subscription/unknown"); recorder.Code != http.StatusNotImplemented {
		t.Fatalf("未知路径状态码 = %d，期望 501", recorder.Code)
	}
	write := httptest.NewRecorder()
	handler.ServeHTTP(write, httptest.NewRequest(http.MethodPost, "/api/admin/analytics/subscription/overview", nil))
	if write.Code != http.StatusNotImplemented {
		t.Fatalf("写方法状态码 = %d，期望 501", write.Code)
	}
}

func Test订阅授权失败返回403(t *testing.T) {
	handler := NewHandler(&stubRepository{}, deny)
	recorder := call(t, handler, "/api/admin/analytics/subscription/overview")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403", recorder.Code)
	}
}
