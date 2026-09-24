package adminanalyticsga4

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件锁定「派生响应形状」这一层：旧实现是 ga4-reports.js / ga4-pwa.js /
// ga4-conversion-reports.js 里的 JS，前端契约是 ai-admin/src/api/analyticsGa4.ts
// 与 analyticsPwa.ts 的 interface。任何形状变化都必须让这里先红。

type stubReporter struct {
	mu       sync.Mutex
	requests []ReportRequest
	respond  func(ReportRequest) (Report, error)
}

func (s *stubReporter) RunReport(_ context.Context, request ReportRequest) (Report, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	respond := s.respond
	s.mu.Unlock()
	if respond == nil {
		return emptyReport(request), nil
	}
	return respond(request)
}

func (s *stubReporter) RunRealtimeReport(ctx context.Context, request ReportRequest) (Report, error) {
	return s.RunReport(ctx, request)
}

func (s *stubReporter) snapshot() []ReportRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make([]ReportRequest, len(s.requests))
	copy(copied, s.requests)
	return copied
}

func (s *stubReporter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// find 按维度名查找请求。并发扇出的端点（Devices、conversion-compare、PWA 系列）
// 记录顺序不确定，只能按内容找，不能按下标。
func (s *stubReporter) find(t *testing.T, dimension string) ReportRequest {
	t.Helper()
	for _, request := range s.snapshot() {
		if len(request.Dimensions) > 0 && request.Dimensions[0] == dimension {
			return request
		}
	}
	t.Fatalf("没有找到以 %q 为首维度的请求", dimension)
	return ReportRequest{}
}

func emptyReport(request ReportRequest) Report {
	return Report{Rows: []map[string]any{}, Metadata: ReportMetadata{
		Dimensions: append([]string{}, request.Dimensions...),
		Metrics:    []MetricMetadata{},
	}}
}

func reportOf(rows ...map[string]any) Report {
	if rows == nil {
		rows = []map[string]any{}
	}
	return Report{Rows: rows, RowCount: len(rows), Metadata: ReportMetadata{Dimensions: []string{}, Metrics: []MetricMetadata{}}}
}

func operationsWith(reporter Reporter) (*Operations, *stubReporter) {
	stub, ok := reporter.(*stubReporter)
	if !ok {
		panic("operationsWith requires a stubReporter")
	}
	fixed := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	return NewOperations(stub).WithClock(func() time.Time { return fixed }), stub
}

func Test上海日历日与GA4日期格式(t *testing.T) {
	// UTC 2026-09-20T20:00Z 已是上海 2026-09-21。
	moment := time.Date(2026, 9, 20, 20, 0, 0, 0, time.UTC)
	if got := shanghaiDateString(0, moment); got != "2026-09-21" {
		t.Fatalf("今天的上海日历日 = %q", got)
	}
	if got := shanghaiDateString(30, moment); got != "2026-08-22" {
		t.Fatalf("30 天前 = %q", got)
	}
	// 只有恰好 8 位数字才改写，其余原样返回。
	cases := map[string]string{
		"20260920":   "2026-09-20",
		"2026-09-20": "2026-09-20",
		"2026092":    "2026092",
		"2026092a":   "2026092a",
		"":           "",
	}
	for input, want := range cases {
		if got := formatGA4Date(input); got != want {
			t.Fatalf("formatGA4Date(%q) = %q, want %q", input, got, want)
		}
	}
}

func Test默认窗口是三十天且按上海日历日(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{})
	if _, err := operations.Overview(context.Background(), DateRangeQuery{}); err != nil {
		t.Fatalf("Overview: %v", err)
	}
	sent := stub.snapshot()[0]
	// 上海 2026-09-20 的 30 天前是 2026-08-21。
	if sent.StartDate != "2026-08-21" || sent.EndDate != "2026-09-20" {
		t.Fatalf("默认窗口 = %s..%s", sent.StartDate, sent.EndDate)
	}
}

func TestOverview汇总口径(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return reportOf(
			map[string]any{"date": "2026-09-19", "activeUsers": float64(10), "sessions": float64(20), "screenPageViews": float64(30), "bounceRate": 0.4, "averageSessionDuration": 10},
			map[string]any{"date": "2026-09-20", "activeUsers": float64(5), "sessions": float64(8), "screenPageViews": float64(12), "bounceRate": 0.6, "averageSessionDuration": 20},
		), nil
	}})

	overview, err := operations.Overview(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	// 计数型累加，比率型取均值 —— 旧实现就是这种混合口径。
	if overview.Totals.ActiveUsers != 15 || overview.Totals.Sessions != 28 || overview.Totals.ScreenPageViews != 42 {
		t.Fatalf("累加项 = %#v", overview.Totals)
	}
	if overview.Totals.BounceRate != 0.5 {
		t.Fatalf("bounceRate 应为均值 0.5，得到 %v", overview.Totals.BounceRate)
	}
	if overview.Totals.AverageSessionDuration != 15 {
		t.Fatalf("averageSessionDuration 应为均值 15，得到 %v", overview.Totals.AverageSessionDuration)
	}
	if len(overview.Trends) != 2 {
		t.Fatalf("trends 长度 = %d", len(overview.Trends))
	}
}

func Test单请求端点的维度度量与默认行数(t *testing.T) {
	// limit 为 0 表示「业务层不指定，交给适配器套 DefaultReportRowLimit」——
	// 旧实现的 runReport 就是这么兜底的，这里如实锁定。
	cases := []struct {
		name       string
		invoke     func(context.Context, *Operations) error
		dimensions []string
		metrics    []string
		limit      int
	}{
		{"traffic", func(ctx context.Context, o *Operations) error {
			_, err := o.Traffic(ctx, LimitQuery{})
			return err
		}, []string{"sessionSource", "sessionMedium"}, []string{"sessions", "activeUsers", "bounceRate", "engagementRate"}, 20},
		{"pages", func(ctx context.Context, o *Operations) error {
			_, err := o.Pages(ctx, LimitQuery{})
			return err
		}, []string{"pagePath", "pageTitle"}, []string{"screenPageViews", "activeUsers", "averageSessionDuration", "bounceRate"}, 50},
		{"demographics", func(ctx context.Context, o *Operations) error {
			_, err := o.Demographics(ctx, LimitQuery{})
			return err
		}, []string{"country"}, []string{"activeUsers", "sessions", "engagementRate"}, 30},
		{"events", func(ctx context.Context, o *Operations) error {
			_, err := o.Events(ctx, LimitQuery{})
			return err
		}, []string{"eventName"}, []string{"eventCount", "activeUsers"}, 50},
		{"agents", func(ctx context.Context, o *Operations) error {
			_, err := o.AgentPages(ctx, LimitQuery{})
			return err
		}, []string{"pagePath", "pageTitle"}, []string{"screenPageViews", "activeUsers", "averageSessionDuration", "bounceRate", "engagementRate"}, 50},
		{"search", func(ctx context.Context, o *Operations) error {
			_, err := o.SearchTerms(ctx, LimitQuery{})
			return err
		}, []string{"customEvent:search_term"}, []string{"eventCount"}, 100},
		{"funnel", func(ctx context.Context, o *Operations) error {
			_, err := o.Funnel(ctx, DateRangeQuery{})
			return err
		}, []string{"eventName"}, []string{"eventCount", "activeUsers"}, 0},
		{"retention", func(ctx context.Context, o *Operations) error {
			_, err := o.Retention(ctx, DateRangeQuery{})
			return err
		}, []string{"newVsReturning", "date"}, []string{"activeUsers", "sessions", "engagementRate"}, 0},
		{"events-daily", func(ctx context.Context, o *Operations) error {
			_, err := o.EventsDaily(ctx, EventsDailyQuery{})
			return err
		}, []string{"date", "eventName"}, []string{"eventCount", "activeUsers"}, DefaultReportRowLimit},
	}
	for _, testCase := range cases {
		operations, stub := operationsWith(&stubReporter{})
		if err := testCase.invoke(context.Background(), operations); err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if stub.count() != 1 {
			t.Fatalf("%s: 出站请求 = %d, want 1", testCase.name, stub.count())
		}
		sent := stub.snapshot()[0]
		if strings.Join(sent.Dimensions, ",") != strings.Join(testCase.dimensions, ",") {
			t.Fatalf("%s: dimensions = %v, want %v", testCase.name, sent.Dimensions, testCase.dimensions)
		}
		if strings.Join(sent.Metrics, ",") != strings.Join(testCase.metrics, ",") {
			t.Fatalf("%s: metrics = %v, want %v", testCase.name, sent.Metrics, testCase.metrics)
		}
		if sent.Limit != testCase.limit {
			t.Fatalf("%s: limit = %d, want %d", testCase.name, sent.Limit, testCase.limit)
		}
	}
}

func Test过滤器逐字节对齐旧实现(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{})
	if _, err := operations.AgentPages(context.Background(), LimitQuery{}); err != nil {
		t.Fatalf("AgentPages: %v", err)
	}
	if _, err := operations.SearchTerms(context.Background(), LimitQuery{}); err != nil {
		t.Fatalf("SearchTerms: %v", err)
	}
	if _, err := operations.Funnel(context.Background(), DateRangeQuery{}); err != nil {
		t.Fatalf("Funnel: %v", err)
	}

	requests := stub.snapshot()
	wantContains := map[string][]string{
		"pagePath":                {`"matchType":"CONTAINS"`, `"/character/"`},
		"customEvent:search_term": {`"fieldName":"eventName"`, `"stringFilter":{"value":"search"}`},
		"eventName":               {`"inListFilter"`, `"page_view"`, `"purchase"`},
	}
	for _, request := range requests {
		key := request.Dimensions[0]
		filter := string(request.DimensionFilter)
		for _, fragment := range wantContains[key] {
			if !strings.Contains(filter, fragment) {
				t.Fatalf("%s 过滤器缺 %q：%s", key, fragment, filter)
			}
		}
	}
}

func TestRealtime维度取自旧路由(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{})
	if _, err := operations.Realtime(context.Background()); err != nil {
		t.Fatalf("Realtime: %v", err)
	}
	sent := stub.snapshot()[0]
	want := "country,deviceCategory,unifiedScreenName"
	if strings.Join(sent.Dimensions, ",") != want {
		t.Fatalf("dimensions = %v, want %s", sent.Dimensions, want)
	}
	if len(sent.MinuteRanges) != 1 || sent.MinuteRanges[0].StartMinutesAgo != 29 {
		t.Fatalf("minuteRanges = %#v", sent.MinuteRanges)
	}
	if sent.Limit != 100 {
		t.Fatalf("limit = %d, want 100", sent.Limit)
	}
}

func TestDevices并发取三份报表(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{})
	devices, err := operations.Devices(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if stub.count() != 3 {
		t.Fatalf("出站请求 = %d, want 3", stub.count())
	}
	device := stub.find(t, "deviceCategory")
	// 旧实现此处没传 limit，交给 runReport 兜底 DefaultReportRowLimit；
	// 业务层保持 0，由适配器解析成缺省值。
	if device.Limit != 0 {
		t.Fatalf("deviceCategory limit = %d, want 0（由适配器兜底）", device.Limit)
	}
	if device.DimensionFilter != nil || len(device.OrderBys) != 0 {
		t.Fatalf("deviceCategory 不应带过滤或排序：%#v", device)
	}
	browser := stub.find(t, "browser")
	if browser.Limit != 10 {
		t.Fatalf("browser limit = %d", browser.Limit)
	}
	os := stub.find(t, "operatingSystem")
	if os.Limit != 10 {
		t.Fatalf("operatingSystem limit = %d", os.Limit)
	}
	// 三个子报表都要有非 nil 的切片，否则会序列化成 null。
	if devices.ByDevice == nil || devices.ByBrowser == nil || devices.ByOS == nil {
		t.Fatalf("devices 子报表为 nil：%#v", devices)
	}
}

func TestFunnel排序复刻旧实现的零值怪癖(t *testing.T) {
	// 旧实现是 `(eventOrder[name] || 99)`：JS 里 0 是 falsy，
	// 所以下标为 0 的 page_view 被算成 99，与未知事件同档，
	// 稳定排序保留它们的相对次序。
	operations, _ := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return reportOf(
			map[string]any{"eventName": "purchase", "eventCount": float64(4)},
			map[string]any{"eventName": "page_view", "eventCount": float64(100)},
			map[string]any{"eventName": "sign_up", "eventCount": float64(9)},
			map[string]any{"eventName": "unknown_event", "eventCount": float64(1)},
		), nil
	}})

	funnel, err := operations.Funnel(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("Funnel: %v", err)
	}
	got := make([]string, 0, len(funnel.Funnel))
	for _, row := range funnel.Funnel {
		got = append(got, stringValue(row, "eventName"))
	}
	want := "sign_up,purchase,page_view,unknown_event"
	if strings.Join(got, ",") != want {
		t.Fatalf("漏斗顺序 = %v, want %s", got, want)
	}
	if strings.Join(funnel.FunnelSteps, ",") != strings.Join(defaultFunnelEvents, ",") {
		t.Fatalf("funnelSteps = %v", funnel.FunnelSteps)
	}
}

func TestRetention分组与非new归入returning(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return reportOf(
			map[string]any{"newVsReturning": "new", "date": "2026-09-19", "activeUsers": float64(10)},
			map[string]any{"newVsReturning": "returning", "date": "2026-09-19", "activeUsers": float64(3)},
			map[string]any{"newVsReturning": "(not set)", "date": "2026-09-20", "activeUsers": float64(2)},
			map[string]any{"newVsReturning": "new", "date": "2026-09-20", "activeUsers": float64(7)},
		), nil
	}})

	retention, err := operations.Retention(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if len(retention.Trends) != 2 {
		t.Fatalf("trends = %#v", retention.Trends)
	}
	// 首日：new=10 returning=3。次日 '(not set)' 也归入 returning。
	if retention.Trends[0] != (RetentionPoint{Date: "2026-09-19", New: 10, Returning: 3}) {
		t.Fatalf("trends[0] = %#v", retention.Trends[0])
	}
	if retention.Trends[1] != (RetentionPoint{Date: "2026-09-20", New: 7, Returning: 2}) {
		t.Fatalf("trends[1] = %#v", retention.Trends[1])
	}
	if retention.Summary != (RetentionSummary{New: 17, Returning: 5}) {
		t.Fatalf("summary = %#v", retention.Summary)
	}
}

func TestEventsDaily格式化日期并回填事件名(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return Report{
			Rows: []map[string]any{
				{"date": "20260920", "eventName": "page_view", "eventCount": float64(5), "activeUsers": float64(4)},
			},
			RowCount: 1,
			Metadata: ReportMetadata{Dimensions: []string{"date", "eventName"}, Metrics: []MetricMetadata{{Name: "eventCount", Type: "TYPE_INTEGER"}}},
		}, nil
	}})

	daily, err := operations.EventsDaily(context.Background(), EventsDailyQuery{})
	if err != nil {
		t.Fatalf("EventsDaily: %v", err)
	}
	if daily.Rows[0]["date"] != "2026-09-20" {
		t.Fatalf("日期未被格式化：%#v", daily.Rows[0]["date"])
	}
	// 原始行不能被就地改写：其它地方的 rows 仍是共享引用。
	if len(daily.EventNames) != len(defaultFunnelEvents) {
		t.Fatalf("缺省事件名 = %v", daily.EventNames)
	}
	if daily.RowCount != 1 || len(daily.Metadata.Metrics) != 1 {
		t.Fatalf("rowCount/metadata 未被透传：%#v", daily)
	}

	operations, _ = operationsWith(&stubReporter{})
	custom, err := operations.EventsDaily(context.Background(), EventsDailyQuery{EventNames: []string{"purchase"}})
	if err != nil {
		t.Fatalf("EventsDaily(custom): %v", err)
	}
	if strings.Join(custom.EventNames, ",") != "purchase" {
		t.Fatalf("自定义事件名 = %v", custom.EventNames)
	}
}

func Test洞察按阈值生成(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		switch {
		case request.Dimensions[0] == "pagePath":
			return reportOf(
				// 高跳出率 + 低时长，且浏览量达到分析门槛。
				map[string]any{"pagePath": "/a", "screenPageViews": float64(200), "bounceRate": 0.9, "averageSessionDuration": 5},
				// 浏览量不足门槛，不应产生洞察。
				map[string]any{"pagePath": "/b", "screenPageViews": float64(10), "bounceRate": 0.95, "averageSessionDuration": 1},
			), nil
		case request.Dimensions[0] == "sessionSource":
			return reportOf(map[string]any{"sessionSource": "google", "sessionMedium": "organic", "sessions": float64(1200)}), nil
		case request.Dimensions[0] == "newVsReturning":
			return reportOf(
				map[string]any{"newVsReturning": "new", "date": "2026-09-20", "activeUsers": float64(95)},
				map[string]any{"newVsReturning": "returning", "date": "2026-09-20", "activeUsers": float64(5)},
			), nil
		default:
			return emptyReport(request), nil
		}
	}})

	insights, err := operations.Insights(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("Insights: %v", err)
	}
	byTitle := map[string]Insight{}
	for _, insight := range insights {
		byTitle[insight.Title] = insight
	}
	if len(byTitle) != 4 {
		t.Fatalf("洞察条数 = %d，标题 = %#v", len(byTitle), insights)
	}
	if got := byTitle["High Bounce Rate Pages"]; got.Type != "warning" || got.Description != "1 pages have bounce rates above 70%" {
		t.Fatalf("高跳出率洞察 = %#v", got)
	}
	if got := byTitle["Low Engagement Pages"]; got.Description != "1 pages have average session duration under 30 seconds" {
		t.Fatalf("低时长洞察 = %#v", got)
	}
	if got := byTitle["Top Traffic Source"]; got.Description != "google/organic drives 1200 sessions" || got.Type != "info" {
		t.Fatalf("头部来源洞察 = %#v", got)
	}
	// returning/total = 5/100 = 5% < 20% → 低回访率告警。
	if got := byTitle["Low Returning User Rate"]; got.Type != "warning" || got.Description != "Only 5.0% of users are returning visitors" {
		t.Fatalf("回访率洞察 = %#v", got)
	}
	// data 字段来自 slice(0,5) / slice(0,3)，且必须非 nil。
	if len(byTitle["High Bounce Rate Pages"].Data) != 1 || len(byTitle["Top Traffic Source"].Data) != 1 {
		t.Fatalf("洞察 data 长度异常：%#v", insights)
	}
}

func Test洞察不吞上游错误(t *testing.T) {
	// 刻意偏离旧实现：旧实现把整个生成过程包在 try/catch 里，
	// 上游失败会静默变成空数组，让「取不到数据」与「没有问题」不可区分。
	operations, _ := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return Report{}, &UpstreamError{Status: 500, Body: "boom"}
	}})
	if _, err := operations.Insights(context.Background(), DateRangeQuery{}); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
}

func TestSummary全部子报表失败时上报错误(t *testing.T) {
	// 全部为 null 的仪表盘会让「凭据写错」看起来像「没有数据」，必须报错。
	operations, _ := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return Report{}, ErrCredentialRejected
	}})
	if _, err := operations.Summary(context.Background(), DateRangeQuery{}); !errors.Is(err, ErrCredentialRejected) {
		t.Fatalf("err = %v, want ErrCredentialRejected", err)
	}
}

func TestSummary部分失败时留空位(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		// 只让 pagePath 维度的报表失败，其余成功。
		if len(request.Dimensions) > 0 && request.Dimensions[0] == "pagePath" {
			return Report{}, ErrUpstream
		}
		return emptyReport(request), nil
	}})

	summary, err := operations.Summary(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.Overview == nil || summary.Realtime == nil {
		t.Fatalf("成功的子报表不应为 nil：%#v", summary)
	}
	if summary.TopPages != nil || summary.AgentPages != nil {
		t.Fatalf("失败的子报表应为 nil：%#v / %#v", summary.TopPages, summary.AgentPages)
	}
	if summary.Insights == nil {
		t.Fatal("insights 应是非 nil 空数组，与前端非空类型一致")
	}
	if summary.GeneratedAt == "" || !strings.HasSuffix(summary.GeneratedAt, "Z") {
		t.Fatalf("generatedAt = %q", summary.GeneratedAt)
	}
}

func TestCustomReport判空口径(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{})

	// 旧实现 `if (!dimensions || !metrics)` 只挡 undefined/null，
	// 空数组是 truthy 会被放行 —— 判 nil 才是等价移植。
	if _, err := operations.CustomReport(context.Background(), CustomReportQuery{Metrics: []string{"activeUsers"}}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("缺 dimensions 应报 ErrInvalidQuery，得到 %v", err)
	}
	if _, err := operations.CustomReport(context.Background(), CustomReportQuery{Dimensions: []string{"date"}}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("缺 metrics 应报 ErrInvalidQuery，得到 %v", err)
	}
	if _, err := operations.CustomReport(context.Background(), CustomReportQuery{Dimensions: []string{}, Metrics: []string{"activeUsers"}}); err != nil {
		t.Fatalf("空数组应被放行（等价于旧实现的 truthy 空数组），得到 %v", err)
	}
}

func Test未配置报告时所有端点都返回未配置(t *testing.T) {
	operations := NewOperations(nil)
	if _, err := operations.Realtime(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Realtime: %v", err)
	}
	if _, err := operations.Summary(context.Background(), DateRangeQuery{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Summary: %v", err)
	}
	if _, err := operations.PwaSummary(context.Background(), DateRangeQuery{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("PwaSummary: %v", err)
	}
}
