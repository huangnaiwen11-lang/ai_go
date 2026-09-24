package adminanalyticsga4

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// 这些常量逐条对应 gaConfig 的回退值。
// ga4-client.js 用的是 `config.ga || {}`，而整个旧仓库没有任何地方定义过 `ga` 键
// （已用 `grep '"ga"'` 全仓确认），因此 `gaConfig.reports`、`gaConfig.thresholds`、
// `gaConfig.defaultDateRange` 全都取回退分支 —— 下面的常量就是实际生效的口径。
const (
	defaultDateRangeDays            = 30
	defaultTrafficLimit             = 20
	defaultPagesLimit               = 50
	defaultDemographicsLimit        = 30
	defaultEventsLimit              = 50
	defaultAgentPagesLimit          = 50
	defaultSearchTermsLimit         = 100
	realtimeMinuteRangeStart        = 29
	realtimeMinuteRangeEnd          = 0
	realtimeLimit                   = 100
	thresholdHighBounceRate         = 0.7
	thresholdLowSessionDuration     = 30
	thresholdMinSessionsForAnalysis = 100
)

// DefaultReportRowLimit 是 runReport 的缺省行数上限。
// 适配器也引用它：Devices / EventsDaily 等调用刻意不传 limit，
// 缺省值必须只有一个事实源。
const DefaultReportRowLimit = 10000

// defaultFunnelEvents 对应 gaConfig.reports.userJourney.eventNames 的回退值。
// 顺序直接影响 /funnel 的响应，不要重排。
var defaultFunnelEvents = []string{
	"page_view",
	"sign_up",
	"login",
	"chat_started",
	"message_sent",
	"begin_checkout",
	"purchase",
}

// Operations 把 Reporter 的原始报表翻译成后台前端已冻结的响应形状。
type Operations struct {
	reporter Reporter
	now      func() time.Time
}

func NewOperations(reporter Reporter) *Operations {
	return &Operations{reporter: reporter, now: time.Now}
}

// WithClock 覆盖时间源，仅用于让「今天」在测试中可确定。
func (o *Operations) WithClock(now func() time.Time) *Operations {
	if o != nil && now != nil {
		o.now = now
	}
	return o
}

func (o *Operations) clock() time.Time {
	if o == nil || o.now == nil {
		return time.Now()
	}
	return o.now()
}

func (o *Operations) ready() error {
	if o == nil || o.reporter == nil {
		return ErrNotConfigured
	}
	return nil
}

// ---- 请求参数 ----

type DateRangeQuery struct {
	StartDate string
	EndDate   string
}

type LimitQuery struct {
	StartDate string
	EndDate   string
	Limit     int
}

type EventsDailyQuery struct {
	StartDate  string
	EndDate    string
	EventNames []string
}

type CustomReportQuery struct {
	Dimensions      []string
	Metrics         []string
	StartDate       string
	EndDate         string
	Limit           int
	DimensionFilter json.RawMessage
	MetricFilter    json.RawMessage
}

// ---- 响应形状 ----

type Overview struct {
	Trends []map[string]any `json:"trends"`
	Totals OverviewTotals   `json:"totals"`
}

type OverviewTotals struct {
	ActiveUsers            float64 `json:"activeUsers"`
	Sessions               float64 `json:"sessions"`
	ScreenPageViews        float64 `json:"screenPageViews"`
	BounceRate             float64 `json:"bounceRate"`
	AverageSessionDuration float64 `json:"averageSessionDuration"`
}

type Devices struct {
	ByDevice  []map[string]any `json:"byDevice"`
	ByBrowser []map[string]any `json:"byBrowser"`
	ByOS      []map[string]any `json:"byOS"`
}

type EventsDaily struct {
	Rows       []map[string]any `json:"rows"`
	RowCount   int              `json:"rowCount"`
	Metadata   ReportMetadata   `json:"metadata"`
	EventNames []string         `json:"eventNames"`
}

type Funnel struct {
	Funnel      []map[string]any `json:"funnel"`
	FunnelSteps []string         `json:"funnelSteps"`
}

type Retention struct {
	Trends  []RetentionPoint `json:"trends"`
	Summary RetentionSummary `json:"summary"`
}

type RetentionPoint struct {
	Date      string  `json:"date"`
	New       float64 `json:"new"`
	Returning float64 `json:"returning"`
}

type RetentionSummary struct {
	New       float64 `json:"new"`
	Returning float64 `json:"returning"`
}

type Insight struct {
	Type           string           `json:"type"`
	Category       string           `json:"category"`
	Title          string           `json:"title"`
	Description    string           `json:"description"`
	Data           []map[string]any `json:"data,omitempty"`
	Recommendation string           `json:"recommendation,omitempty"`
}

// Summary 的十个子报表都是可空的：旧实现对每一项做了 `.catch(() => null)`，
// 前端类型也如实声明为 `T | null`。这是被建模的契约，不是「把失败演成空数据」。
type Summary struct {
	Overview       *Overview  `json:"overview"`
	TrafficSources *Report    `json:"trafficSources"`
	TopPages       *Report    `json:"topPages"`
	Demographics   *Report    `json:"demographics"`
	Devices        *Devices   `json:"devices"`
	Events         *Report    `json:"events"`
	UserJourney    *Funnel    `json:"userJourney"`
	Retention      *Retention `json:"retention"`
	Realtime       *Report    `json:"realtime"`
	AgentPages     *Report    `json:"agentPages"`
	Insights       []Insight  `json:"insights"`
	GeneratedAt    string     `json:"generatedAt"`
}

// ---- 端点 ----

// Realtime 对应 GET /realtime。维度取自旧路由而不是 getRealtimeData 的函数默认值：
// 路由显式传了 ['country','deviceCategory','unifiedScreenName']，覆盖了函数默认的两维。
func (o *Operations) Realtime(ctx context.Context) (Report, error) {
	if err := o.ready(); err != nil {
		return Report{}, err
	}
	return o.reporter.RunRealtimeReport(ctx, ReportRequest{
		Dimensions:   []string{"country", "deviceCategory", "unifiedScreenName"},
		Metrics:      []string{"activeUsers"},
		Limit:        realtimeLimit,
		MinuteRanges: []MinuteRange{{StartMinutesAgo: realtimeMinuteRangeStart, EndMinutesAgo: realtimeMinuteRangeEnd}},
	})
}

func (o *Operations) Overview(ctx context.Context, query DateRangeQuery) (Overview, error) {
	if err := o.ready(); err != nil {
		return Overview{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)
	report, err := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions: []string{"date"},
		Metrics:    []string{"activeUsers", "sessions", "screenPageViews", "bounceRate", "averageSessionDuration"},
		StartDate:  start,
		EndDate:    end,
		OrderBys:   []OrderBy{{Dimension: &OrderByDimension{DimensionName: "date"}}},
	})
	if err != nil {
		return Overview{}, err
	}

	totals := OverviewTotals{}
	for _, row := range report.Rows {
		totals.ActiveUsers += metricNumber(row, "activeUsers")
		totals.Sessions += metricNumber(row, "sessions")
		totals.ScreenPageViews += metricNumber(row, "screenPageViews")
		totals.BounceRate += metricNumber(row, "bounceRate")
		totals.AverageSessionDuration += metricNumber(row, "averageSessionDuration")
	}
	// 只有比率型度量取均值，计数型保持累加 —— 与旧实现一致。
	if count := len(report.Rows); count > 0 {
		totals.BounceRate /= float64(count)
		totals.AverageSessionDuration /= float64(count)
	}
	return Overview{Trends: report.Rows, Totals: totals}, nil
}

func (o *Operations) Traffic(ctx context.Context, query LimitQuery) (Report, error) {
	return o.runLimited(ctx, query, reportSpec{
		dimensions:   []string{"sessionSource", "sessionMedium"},
		metrics:      []string{"sessions", "activeUsers", "bounceRate", "engagementRate"},
		defaultLimit: defaultTrafficLimit,
		orderBys:     []OrderBy{{Metric: &OrderByMetric{MetricName: "sessions"}, Desc: true}},
	})
}

func (o *Operations) Pages(ctx context.Context, query LimitQuery) (Report, error) {
	return o.runLimited(ctx, query, reportSpec{
		dimensions:   []string{"pagePath", "pageTitle"},
		metrics:      []string{"screenPageViews", "activeUsers", "averageSessionDuration", "bounceRate"},
		defaultLimit: defaultPagesLimit,
		orderBys:     []OrderBy{{Metric: &OrderByMetric{MetricName: "screenPageViews"}, Desc: true}},
	})
}

func (o *Operations) Demographics(ctx context.Context, query LimitQuery) (Report, error) {
	return o.runLimited(ctx, query, reportSpec{
		dimensions:   []string{"country"},
		metrics:      []string{"activeUsers", "sessions", "engagementRate"},
		defaultLimit: defaultDemographicsLimit,
		orderBys:     []OrderBy{{Metric: &OrderByMetric{MetricName: "activeUsers"}, Desc: true}},
	})
}

func (o *Operations) Events(ctx context.Context, query LimitQuery) (Report, error) {
	return o.runLimited(ctx, query, reportSpec{
		dimensions:   []string{"eventName"},
		metrics:      []string{"eventCount", "activeUsers"},
		defaultLimit: defaultEventsLimit,
		orderBys:     []OrderBy{{Metric: &OrderByMetric{MetricName: "eventCount"}, Desc: true}},
	})
}

func (o *Operations) AgentPages(ctx context.Context, query LimitQuery) (Report, error) {
	return o.runLimited(ctx, query, reportSpec{
		dimensions:      []string{"pagePath", "pageTitle"},
		metrics:         []string{"screenPageViews", "activeUsers", "averageSessionDuration", "bounceRate", "engagementRate"},
		defaultLimit:    defaultAgentPagesLimit,
		orderBys:        []OrderBy{{Metric: &OrderByMetric{MetricName: "screenPageViews"}, Desc: true}},
		dimensionFilter: filterFieldContains("pagePath", "/character/"),
	})
}

func (o *Operations) SearchTerms(ctx context.Context, query LimitQuery) (Report, error) {
	return o.runLimited(ctx, query, reportSpec{
		dimensions:      []string{"customEvent:search_term"},
		metrics:         []string{"eventCount"},
		defaultLimit:    defaultSearchTermsLimit,
		orderBys:        []OrderBy{{Metric: &OrderByMetric{MetricName: "eventCount"}, Desc: true}},
		dimensionFilter: filterEventNameEquals("search"),
	})
}

func (o *Operations) Devices(ctx context.Context, query DateRangeQuery) (Devices, error) {
	if err := o.ready(); err != nil {
		return Devices{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)
	var byDevice, byBrowser, byOS Report
	failure := parallel(
		func() error {
			report, err := o.reporter.RunReport(ctx, ReportRequest{
				Dimensions: []string{"deviceCategory"},
				Metrics:    []string{"activeUsers", "sessions", "bounceRate"},
				StartDate:  start,
				EndDate:    end,
			})
			byDevice = report
			return err
		},
		func() error {
			report, err := o.reporter.RunReport(ctx, ReportRequest{
				Dimensions: []string{"browser"},
				Metrics:    []string{"activeUsers", "sessions"},
				StartDate:  start,
				EndDate:    end,
				Limit:      10,
				OrderBys:   []OrderBy{{Metric: &OrderByMetric{MetricName: "activeUsers"}, Desc: true}},
			})
			byBrowser = report
			return err
		},
		func() error {
			report, err := o.reporter.RunReport(ctx, ReportRequest{
				Dimensions: []string{"operatingSystem"},
				Metrics:    []string{"activeUsers", "sessions"},
				StartDate:  start,
				EndDate:    end,
				Limit:      10,
				OrderBys:   []OrderBy{{Metric: &OrderByMetric{MetricName: "activeUsers"}, Desc: true}},
			})
			byOS = report
			return err
		},
	)
	if failure != nil {
		return Devices{}, failure
	}
	return Devices{ByDevice: byDevice.Rows, ByBrowser: byBrowser.Rows, ByOS: byOS.Rows}, nil
}

func (o *Operations) EventsDaily(ctx context.Context, query EventsDailyQuery) (EventsDaily, error) {
	if err := o.ready(); err != nil {
		return EventsDaily{}, err
	}
	names := query.EventNames
	if len(names) == 0 {
		names = defaultFunnelEvents
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)
	report, err := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      []string{"date", "eventName"},
		Metrics:         []string{"eventCount", "activeUsers"},
		StartDate:       start,
		EndDate:         end,
		Limit:           DefaultReportRowLimit,
		DimensionFilter: filterEventNameInList(names),
		OrderBys: []OrderBy{
			{Dimension: &OrderByDimension{DimensionName: "date"}},
			{Dimension: &OrderByDimension{DimensionName: "eventName"}},
		},
	})
	if err != nil {
		return EventsDaily{}, err
	}

	rows := make([]map[string]any, 0, len(report.Rows))
	for _, row := range report.Rows {
		normalized := make(map[string]any, len(row))
		for key, value := range row {
			normalized[key] = value
		}
		normalized["date"] = formatGA4Date(stringValue(row, "date"))
		rows = append(rows, normalized)
	}
	return EventsDaily{Rows: rows, RowCount: report.RowCount, Metadata: report.Metadata, EventNames: names}, nil
}

func (o *Operations) Funnel(ctx context.Context, query DateRangeQuery) (Funnel, error) {
	if err := o.ready(); err != nil {
		return Funnel{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)
	report, err := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      []string{"eventName"},
		Metrics:         []string{"eventCount", "activeUsers"},
		StartDate:       start,
		EndDate:         end,
		DimensionFilter: filterEventNameInList(defaultFunnelEvents),
	})
	if err != nil {
		return Funnel{}, err
	}

	rows := make([]map[string]any, len(report.Rows))
	copy(rows, report.Rows)
	sort.SliceStable(rows, func(first, second int) bool {
		return funnelRank(stringValue(rows[first], "eventName")) < funnelRank(stringValue(rows[second], "eventName"))
	})
	return Funnel{Funnel: rows, FunnelSteps: defaultFunnelEvents}, nil
}

// funnelRank 复刻旧实现的 `(eventOrder[name] || 99)`。
//
// 注意：JS 里 0 是 falsy，所以下标为 0 的 page_view 被算成 99 而不是 0；
// 未知事件名同样是 99。稳定排序把两者放在同一档并保留上游返回的相对次序。
// 这是旧实现既有行为，本次迁移保持等价，并把该怪癖登记在
// docs/待确认事项与配置参数清单.md（K7），不在代码里顺手「修正」。
func funnelRank(name string) int {
	for index, candidate := range defaultFunnelEvents {
		if candidate != name {
			continue
		}
		if index == 0 {
			return 99
		}
		return index
	}
	return 99
}

func (o *Operations) Retention(ctx context.Context, query DateRangeQuery) (Retention, error) {
	if err := o.ready(); err != nil {
		return Retention{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)
	report, err := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions: []string{"newVsReturning", "date"},
		Metrics:    []string{"activeUsers", "sessions", "engagementRate"},
		StartDate:  start,
		EndDate:    end,
		OrderBys:   []OrderBy{{Dimension: &OrderByDimension{DimensionName: "date"}}},
	})
	if err != nil {
		return Retention{}, err
	}

	byDate := newOrderedAccumulator[RetentionPoint]()
	summary := RetentionSummary{}
	for _, row := range report.Rows {
		date := stringValue(row, "date")
		point := byDate.ensure(date, func() *RetentionPoint { return &RetentionPoint{Date: date} })
		activeUsers := metricNumber(row, "activeUsers")
		// 旧实现只判 'new'，其余（含 'returning' 与 '(not set)'）都归到 returning。
		if stringValue(row, "newVsReturning") == "new" {
			point.New = activeUsers
			summary.New += activeUsers
		} else {
			point.Returning = activeUsers
			summary.Returning += activeUsers
		}
	}

	points := byDate.list()
	trends := make([]RetentionPoint, 0, len(points))
	for _, point := range points {
		trends = append(trends, *point)
	}
	return Retention{Trends: trends, Summary: summary}, nil
}

// Insights 对应 GET /insights。
//
// 与旧实现的一处刻意偏离：旧实现把整个生成过程包在 try/catch 里，
// 任何上游失败都会静默降级成空数组 —— 「取不到数据」与「没有问题」于是无法区分，
// 而这正是本次迁移要消灭的失败模式。这里改为向上返回错误，
// 由 transport 渲染成 503；该决定登记在 docs/待确认事项与配置参数清单.md（K7）。
func (o *Operations) Insights(ctx context.Context, query DateRangeQuery) ([]Insight, error) {
	if err := o.ready(); err != nil {
		return nil, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)

	pages, err := o.runRange(ctx, start, end, reportSpec{
		dimensions:   []string{"pagePath", "pageTitle"},
		metrics:      []string{"screenPageViews", "activeUsers", "averageSessionDuration", "bounceRate"},
		defaultLimit: 100,
		orderBys:     []OrderBy{{Metric: &OrderByMetric{MetricName: "screenPageViews"}, Desc: true}},
	})
	if err != nil {
		return nil, err
	}

	insights := make([]Insight, 0, 4)
	highBounce := selectRows(pages.Rows, func(row map[string]any) bool {
		return metricNumber(row, "bounceRate") > thresholdHighBounceRate &&
			metricNumber(row, "screenPageViews") >= thresholdMinSessionsForAnalysis
	})
	if len(highBounce) > 0 {
		insights = append(insights, Insight{
			Type:           "warning",
			Category:       "engagement",
			Title:          "High Bounce Rate Pages",
			Description:    fmt.Sprintf("%d pages have bounce rates above %.0f%%", len(highBounce), thresholdHighBounceRate*100),
			Data:           head(highBounce, 5),
			Recommendation: "Consider improving content, page load speed, or adding clearer CTAs on these pages.",
		})
	}

	lowDuration := selectRows(pages.Rows, func(row map[string]any) bool {
		return metricNumber(row, "averageSessionDuration") < thresholdLowSessionDuration &&
			metricNumber(row, "screenPageViews") >= thresholdMinSessionsForAnalysis
	})
	if len(lowDuration) > 0 {
		insights = append(insights, Insight{
			Type:           "warning",
			Category:       "engagement",
			Title:          "Low Engagement Pages",
			Description:    fmt.Sprintf("%d pages have average session duration under %d seconds", len(lowDuration), thresholdLowSessionDuration),
			Data:           head(lowDuration, 5),
			Recommendation: "These pages may need more engaging content or better user experience.",
		})
	}

	sources, err := o.runRange(ctx, start, end, reportSpec{
		dimensions:   []string{"sessionSource", "sessionMedium"},
		metrics:      []string{"sessions", "activeUsers", "bounceRate", "engagementRate"},
		defaultLimit: 10,
		orderBys:     []OrderBy{{Metric: &OrderByMetric{MetricName: "sessions"}, Desc: true}},
	})
	if err != nil {
		return nil, err
	}
	if len(sources.Rows) > 0 {
		topSource := sources.Rows[0]
		insights = append(insights, Insight{
			Type:     "info",
			Category: "traffic",
			Title:    "Top Traffic Source",
			Description: fmt.Sprintf("%s/%s drives %s sessions",
				stringValue(topSource, "sessionSource"),
				stringValue(topSource, "sessionMedium"),
				jsNumber(metricNumber(topSource, "sessions")),
			),
			Data: head(sources.Rows, 3),
		})
	}

	retention, err := o.Retention(ctx, DateRangeQuery{StartDate: start, EndDate: end})
	if err != nil {
		return nil, err
	}
	returningRatio := retention.Summary.Returning / (retention.Summary.New + retention.Summary.Returning)
	// 分母为 0 时得到 NaN，两个分支都不成立 —— 与旧实现一致，此处不额外兜底。
	switch {
	case returningRatio < 0.2:
		insights = append(insights, Insight{
			Type:           "warning",
			Category:       "retention",
			Title:          "Low Returning User Rate",
			Description:    fmt.Sprintf("Only %.1f%% of users are returning visitors", returningRatio*100),
			Recommendation: "Consider implementing push notifications, email campaigns, or loyalty features.",
		})
	case returningRatio > 0.5:
		insights = append(insights, Insight{
			Type:        "success",
			Category:    "retention",
			Title:       "Strong User Retention",
			Description: fmt.Sprintf("%.1f%% of users are returning visitors", returningRatio*100),
		})
	}
	return insights, nil
}

// Summary 对应 GET /summary。
//
// 旧实现对十个子报表逐个 `.catch(() => null)`，对洞察 `.catch(() => [])`，
// 所以部分失败是被建模的正常结果。
// 但「全部子报表都失败」必须报错：否则凭据写错会伪装成一个全空仪表盘，
// 那正是本次迁移要消灭的失败模式。
func (o *Operations) Summary(ctx context.Context, query DateRangeQuery) (Summary, error) {
	if err := o.ready(); err != nil {
		return Summary{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)

	var (
		overview        Overview
		trafficSources  Report
		topPages        Report
		demographics    Report
		devices         Devices
		events          Report
		userJourney     Funnel
		retentionReport Retention
		realtime        Report
		agentPages      Report
	)

	dateRangeValue := DateRangeQuery{StartDate: start, EndDate: end}
	limitRange := func(limit int) LimitQuery {
		return LimitQuery{StartDate: start, EndDate: end, Limit: limit}
	}

	// 下标即断言顺序，见下方按 failures[i] 组装响应的代码。
	failures := parallelErrors([]func() error{
		func() error { value, err := o.Overview(ctx, dateRangeValue); overview = value; return err },
		func() error {
			value, err := o.Traffic(ctx, limitRange(defaultTrafficLimit))
			trafficSources = value
			return err
		},
		func() error {
			value, err := o.Pages(ctx, limitRange(defaultTrafficLimit))
			topPages = value
			return err
		},
		func() error { value, err := o.Demographics(ctx, limitRange(0)); demographics = value; return err },
		func() error { value, err := o.Devices(ctx, dateRangeValue); devices = value; return err },
		func() error { value, err := o.Events(ctx, limitRange(0)); events = value; return err },
		func() error { value, err := o.Funnel(ctx, dateRangeValue); userJourney = value; return err },
		func() error { value, err := o.Retention(ctx, dateRangeValue); retentionReport = value; return err },
		func() error { value, err := o.Realtime(ctx); realtime = value; return err },
		func() error { value, err := o.AgentPages(ctx, limitRange(0)); agentPages = value; return err },
	})

	// 洞察在旧实现里被吞成 []，前端类型也声明为非空数组，这里保持 []。
	insights, insightsErr := o.Insights(ctx, dateRangeValue)
	if insightsErr != nil {
		insights = []Insight{}
	}

	allReportsFailed := true
	for _, failure := range failures {
		if failure == nil {
			allReportsFailed = false
			break
		}
	}
	if allReportsFailed {
		return Summary{}, failures[0]
	}

	summary := Summary{Insights: insights, GeneratedAt: jsISOString(o.clock())}
	if failures[0] == nil {
		summary.Overview = &overview
	}
	if failures[1] == nil {
		summary.TrafficSources = &trafficSources
	}
	if failures[2] == nil {
		summary.TopPages = &topPages
	}
	if failures[3] == nil {
		summary.Demographics = &demographics
	}
	if failures[4] == nil {
		summary.Devices = &devices
	}
	if failures[5] == nil {
		summary.Events = &events
	}
	if failures[6] == nil {
		summary.UserJourney = &userJourney
	}
	if failures[7] == nil {
		summary.Retention = &retentionReport
	}
	if failures[8] == nil {
		summary.Realtime = &realtime
	}
	if failures[9] == nil {
		summary.AgentPages = &agentPages
	}
	return summary, nil
}

// CustomReport 对应 POST /report。维度与度量由调用方给出，过滤器原样透传。
func (o *Operations) CustomReport(ctx context.Context, query CustomReportQuery) (Report, error) {
	if err := o.ready(); err != nil {
		return Report{}, err
	}
	// 旧实现的守卫是 `if (!dimensions || !metrics)`：只挡 undefined/null，
	// 空数组是 truthy 会被放行。Go 里 `[]` 反序列化成非 nil 空切片，
	// 因此判 nil 才是等价的移植。
	if query.Dimensions == nil || query.Metrics == nil {
		return Report{}, fmt.Errorf("%w: dimensions and metrics are required", ErrInvalidQuery)
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)
	return o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      query.Dimensions,
		Metrics:         query.Metrics,
		StartDate:       start,
		EndDate:         end,
		Limit:           query.Limit,
		DimensionFilter: query.DimensionFilter,
		MetricFilter:    query.MetricFilter,
	})
}

// ---- 内部 ----

type reportSpec struct {
	dimensions      []string
	metrics         []string
	defaultLimit    int
	orderBys        []OrderBy
	dimensionFilter json.RawMessage
}

func (o *Operations) rangeOf(startDate, endDate string) (string, string) {
	return dateRange(startDate, endDate, defaultDateRangeDays, o.clock())
}

func (o *Operations) runLimited(ctx context.Context, query LimitQuery, spec reportSpec) (Report, error) {
	if err := o.ready(); err != nil {
		return Report{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)
	limit := query.Limit
	if limit <= 0 {
		limit = spec.defaultLimit
	}
	return o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      spec.dimensions,
		Metrics:         spec.metrics,
		StartDate:       start,
		EndDate:         end,
		Limit:           limit,
		OrderBys:        spec.orderBys,
		DimensionFilter: spec.dimensionFilter,
	})
}

func (o *Operations) runRange(ctx context.Context, start, end string, spec reportSpec) (Report, error) {
	if err := o.ready(); err != nil {
		return Report{}, err
	}
	return o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      spec.dimensions,
		Metrics:         spec.metrics,
		StartDate:       start,
		EndDate:         end,
		Limit:           spec.defaultLimit,
		OrderBys:        spec.orderBys,
		DimensionFilter: spec.dimensionFilter,
	})
}

func selectRows(rows []map[string]any, keep func(map[string]any) bool) []map[string]any {
	selected := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if keep(row) {
			selected = append(selected, row)
		}
	}
	return selected
}

func head(rows []map[string]any, limit int) []map[string]any {
	if len(rows) <= limit {
		return rows
	}
	return rows[:limit]
}
