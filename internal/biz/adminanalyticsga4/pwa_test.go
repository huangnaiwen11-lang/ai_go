package adminanalyticsga4

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 本文件锁定 PWA 与转化对比两族端点的派生形状。

func TestPwaInstalls选择landing漏斗并算转化率(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		if request.Dimensions[0] == "date" {
			return reportOf(
				map[string]any{"date": "20260919", "eventCount": float64(3), "activeUsers": float64(3)},
				map[string]any{"date": "20260920", "eventCount": float64(4), "activeUsers": float64(4)},
			), nil
		}
		// landing 代有量 → 选 landing 漏斗。
		return reportOf(
			map[string]any{"eventName": "pwa_landing_view", "eventCount": float64(100), "activeUsers": float64(90)},
			map[string]any{"eventName": "pwa_landing_install_clicked", "eventCount": float64(20), "activeUsers": float64(18)},
			map[string]any{"eventName": "pwa_install_intent", "eventCount": float64(12), "activeUsers": float64(10)},
			map[string]any{"eventName": "pwa_install_verified", "eventCount": float64(7), "activeUsers": float64(6)},
			map[string]any{"eventName": "pwa_install_banner_shown", "eventCount": float64(5), "activeUsers": float64(5)},
			map[string]any{"eventName": "pwa_install_clicked", "eventCount": float64(2), "activeUsers": float64(2)},
		), nil
	}})

	installs, err := operations.PwaInstalls(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("PwaInstalls: %v", err)
	}
	if installs.Totals.Installs != 7 {
		t.Fatalf("installs = %v, want 7（日曲线之和）", installs.Totals.Installs)
	}
	if installs.Totals.FunnelType != "landing" {
		t.Fatalf("funnelType = %q", installs.Totals.FunnelType)
	}
	if installs.Totals.FunnelBase != 100 {
		t.Fatalf("funnelBase = %v, want 100", installs.Totals.FunnelBase)
	}
	// 首步固定为 1，其余按「本步 / 上一步」。
	if len(installs.Funnel) != 4 {
		t.Fatalf("漏斗步数 = %d", len(installs.Funnel))
	}
	if installs.Funnel[0].ConversionRate != 1 {
		t.Fatalf("首步转化率 = %v, want 1", installs.Funnel[0].ConversionRate)
	}
	if installs.Funnel[1].ConversionRate != 0.2 {
		t.Fatalf("第二步转化率 = %v, want 0.2", installs.Funnel[1].ConversionRate)
	}
	// 两代字段全部平铺输出，即便当前选的是 landing。
	if installs.Totals.BannerShown != 5 || installs.Totals.Clicked != 2 {
		t.Fatalf("legacy totals 未平铺：%#v", installs.Totals)
	}
	if installs.Totals.LandingView != 100 || installs.Totals.LandingClicked != 20 {
		t.Fatalf("landing totals 未平铺：%#v", installs.Totals)
	}
	if stub.find(t, "date").DimensionFilter == nil {
		t.Fatal("日曲线应带 pwa_installed 事件过滤")
	}
}

func TestPwaInstalls无landing数据时回退legacy漏斗(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		if request.Dimensions[0] == "date" {
			return reportOf(), nil
		}
		return reportOf(
			map[string]any{"eventName": "pwa_install_banner_shown", "eventCount": float64(50)},
			map[string]any{"eventName": "pwa_install_clicked", "eventCount": float64(10)},
		), nil
	}})

	installs, err := operations.PwaInstalls(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("PwaInstalls: %v", err)
	}
	if installs.Totals.FunnelType != "legacy" {
		t.Fatalf("funnelType = %q", installs.Totals.FunnelType)
	}
	if len(installs.Funnel) != 4 || installs.Funnel[0].EventName != "pwa_install_banner_shown" {
		t.Fatalf("回退漏斗 = %#v", installs.Funnel)
	}
	// 首步 50、次步 10 → 0.2；缺数据的两步计数为 0，转化率为 0。
	if installs.Funnel[1].ConversionRate != 0.2 || installs.Funnel[2].ConversionRate != 0 {
		t.Fatalf("回退漏斗转化率 = %#v", installs.Funnel)
	}
	if installs.Totals.FunnelBase != 50 {
		t.Fatalf("funnelBase = %v, want 50", installs.Totals.FunnelBase)
	}
}

func TestPwaActive按日期对齐并相减(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		filter := string(request.DimensionFilter)
		if strings.Contains(filter, "pwa_app_launch") {
			return reportOf(
				map[string]any{"date": "20260919", "activeUsers": float64(3)},
				map[string]any{"date": "20260920", "activeUsers": float64(5)},
			), nil
		}
		if strings.Contains(filter, "app_launch") {
			return reportOf(
				map[string]any{"date": "20260920", "activeUsers": float64(20)},
				map[string]any{"date": "20260921", "activeUsers": float64(9)},
			), nil
		}
		return emptyReport(request), nil
	}})

	active, err := operations.PwaActive(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("PwaActive: %v", err)
	}
	if stub.count() != 2 {
		t.Fatalf("出站请求 = %d, want 2", stub.count())
	}
	if len(active.Daily) != 3 {
		t.Fatalf("daily = %#v", active.Daily)
	}
	// 按日期升序排序，浏览器用户为总用户减 PWA 用户且不为负。
	// 注意日期保持 GA4 原始的 YYYYMMDD：旧实现在 /pwa/active 里没有调用
	// formatGa4Date（只有 /events/daily 调了），本次迁移保持等价，
	// 该不一致登记在 docs/待确认事项与配置参数清单.md（K7）。
	want := []PwaActiveDay{
		{Date: "20260919", Pwa: 3, Total: 0, Browser: 0},
		{Date: "20260920", Pwa: 5, Total: 20, Browser: 15},
		{Date: "20260921", Pwa: 0, Total: 9, Browser: 9},
	}
	for index, day := range want {
		if active.Daily[index] != day {
			t.Fatalf("daily[%d] = %#v, want %#v", index, active.Daily[index], day)
		}
	}
	if active.Totals != (PwaActiveTotals{Pwa: 8, Browser: 24, Total: 29}) {
		t.Fatalf("totals = %#v", active.Totals)
	}
	if active.PwaRatio != 8.0/29.0 {
		t.Fatalf("pwaRatio = %v", active.PwaRatio)
	}
}

func TestPwaPlatforms归并操作系统(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		filter := string(request.DimensionFilter)
		if strings.Contains(filter, "pwa_app_launch") {
			return reportOf(
				map[string]any{"operatingSystem": "iOS", "activeUsers": float64(4)},
				map[string]any{"operatingSystem": "Android", "activeUsers": float64(6)},
			), nil
		}
		if strings.Contains(filter, "app_launch") {
			return reportOf(
				map[string]any{"operatingSystem": "iOS", "activeUsers": float64(10)},
				map[string]any{"operatingSystem": "Macintosh", "activeUsers": float64(2)},
				map[string]any{"operatingSystem": "Android", "activeUsers": float64(9)},
				map[string]any{"operatingSystem": "Windows", "activeUsers": float64(5)},
			), nil
		}
		return emptyReport(request), nil
	}})

	platforms, err := operations.PwaPlatforms(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("PwaPlatforms: %v", err)
	}
	got := map[string]PwaPlatform{}
	order := make([]string, 0, len(platforms.Platforms))
	for _, item := range platforms.Platforms {
		got[item.Platform] = item
		order = append(order, item.Platform)
	}
	// Macintosh 被归入 ios —— 旧实现的既有归类，本次迁移保持等价。
	if got["ios"] != (PwaPlatform{Platform: "ios", PwaUsers: 4, BrowserUsers: 8}) {
		t.Fatalf("ios = %#v", got["ios"])
	}
	if got["android"] != (PwaPlatform{Platform: "android", PwaUsers: 6, BrowserUsers: 3}) {
		t.Fatalf("android = %#v", got["android"])
	}
	if got["desktop"] != (PwaPlatform{Platform: "desktop", PwaUsers: 0, BrowserUsers: 5}) {
		t.Fatalf("desktop = %#v", got["desktop"])
	}
	if strings.Join(order, ",") != "ios,android,desktop" {
		t.Fatalf("平台顺序 = %v", order)
	}
}

func TestPwaPaywallSources按来源聚合(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return reportOf(
			map[string]any{"eventName": "subscription_modal_open", "eventCount": float64(40), pwaSourceDimension: "hero"},
			map[string]any{"eventName": "begin_checkout", "eventCount": float64(20), pwaSourceDimension: "hero"},
			map[string]any{"eventName": "purchase", "eventCount": float64(10), pwaSourceDimension: "hero"},
			map[string]any{"eventName": "subscription_modal_open", "eventCount": float64(30), pwaSourceDimension: "settings"},
			map[string]any{"eventName": "begin_checkout", "eventCount": float64(6), pwaSourceDimension: "settings"},
			map[string]any{"eventName": "purchase", "eventCount": float64(3), pwaSourceDimension: "settings"},
		), nil
	}})

	funnel, err := operations.PwaPaywallSources(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("PwaPaywallSources: %v", err)
	}
	if !funnel.SourceDimensionAvailable || funnel.Notice != "" {
		t.Fatalf("正常路径不应带 notice：%#v", funnel)
	}
	if len(funnel.Sources) != 2 || funnel.Sources[0].Source != "hero" {
		t.Fatalf("sources = %#v", funnel.Sources)
	}
	if funnel.Sources[0].CheckoutRate != 0.5 || funnel.Sources[0].PurchaseRate != 0.25 || funnel.Sources[0].CheckoutToPurchaseRate != 0.5 {
		t.Fatalf("hero 转化率 = %#v", funnel.Sources[0])
	}
	if funnel.Totals.SubscriptionModalOpen != 70 || funnel.Totals.BeginCheckout != 26 || funnel.Totals.Purchase != 13 {
		t.Fatalf("totals = %#v", funnel.Totals)
	}
	if funnel.Totals.CheckoutToPurchaseRate != 0.5 {
		t.Fatalf("整体结账到购买转化率 = %v", funnel.Totals.CheckoutToPurchaseRate)
	}
}

func TestPwaPaywallSources维度缺失时降级(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		if len(request.Dimensions) == 2 {
			return Report{}, &UpstreamError{Status: 400, Body: `{"error":{"message":"Field customEvent:source is not a valid dimension"}}`}
		}
		return reportOf(
			map[string]any{"eventName": "subscription_modal_open", "eventCount": float64(12)},
			map[string]any{"eventName": "begin_checkout", "eventCount": float64(6)},
			map[string]any{"eventName": "purchase", "eventCount": float64(3)},
		), nil
	}})

	funnel, err := operations.PwaPaywallSources(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("降级失败：%v", err)
	}
	if stub.count() != 2 {
		t.Fatalf("降级应恰好两次出站请求，实际 %d", stub.count())
	}
	if funnel.SourceDimensionAvailable {
		t.Fatal("降级后 sourceDimensionAvailable 应为 false")
	}
	if funnel.Notice == "" {
		t.Fatal("降级必须带 notice，否则口径变化对使用者不可见")
	}
	if len(funnel.Sources) != 1 || funnel.Sources[0].Source != "all" {
		t.Fatalf("降级来源 = %#v", funnel.Sources)
	}
	if funnel.Sources[0].SubscriptionModalOpen != 12 || funnel.Totals.Purchase != 3 {
		t.Fatalf("降级计数 = %#v / %#v", funnel.Sources[0], funnel.Totals)
	}
}

func TestPwaPaywallSources基础设施故障不降级(t *testing.T) {
	// 只有「自定义维度没建」才允许降级。凭据、网络、配额问题伪装成
	// 「来源维度不可用」会把配置事故说成产品设计，必须原样上报。
	operations, stub := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return Report{}, &UpstreamError{Status: 500, Body: "backend error"}
	}})
	if _, err := operations.PwaPaywallSources(context.Background(), DateRangeQuery{}); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if stub.count() != 1 {
		t.Fatalf("不应尝试降级查询，出站请求 = %d", stub.count())
	}

	operations, _ = operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return Report{}, ErrCredentialRejected
	}})
	if _, err := operations.PwaPaywallSources(context.Background(), DateRangeQuery{}); !errors.Is(err, ErrCredentialRejected) {
		t.Fatalf("err = %v, want ErrCredentialRejected", err)
	}
}

func TestPwaEngagementV2队列维度缺失时回退(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		if len(request.Dimensions) == 2 {
			return Report{}, &UpstreamError{Status: 400, Body: "custom dimension customEvent:engagement_score_bucket unavailable"}
		}
		return reportOf(
			map[string]any{"eventName": "pwa_engagement_shown", "eventCount": float64(100)},
			map[string]any{"eventName": "pwa_install_clicked", "eventCount": float64(25)},
			map[string]any{"eventName": "pwa_installed", "eventCount": float64(10)},
			// 不在清单里的事件必须被丢弃。
			map[string]any{"eventName": "unrelated", "eventCount": float64(999)},
		), nil
	}})

	engagement, err := operations.PwaEngagementV2(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("PwaEngagementV2: %v", err)
	}
	if stub.count() != 2 {
		t.Fatalf("回退应恰好两次出站请求，实际 %d", stub.count())
	}
	if engagement.CohortDimensionAvailable {
		t.Fatal("回退后 cohortDimensionAvailable 应为 false")
	}
	if len(engagement.Cohorts) != 1 || engagement.Cohorts[0].Cohort != "all" {
		t.Fatalf("回退队列 = %#v", engagement.Cohorts)
	}
	if engagement.Cohorts[0].CTR != 0.25 || engagement.Cohorts[0].InstallRate != 0.1 {
		t.Fatalf("队列转化率 = %#v", engagement.Cohorts[0])
	}
}

func TestPwaEngagementV2带队列维度(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return reportOf(
			map[string]any{"eventName": "pwa_engagement_shown", "eventCount": float64(40), pwaCohortDimension: "engaged"},
			map[string]any{"eventName": "pwa_install_clicked", "eventCount": float64(8), pwaCohortDimension: "engaged"},
			map[string]any{"eventName": "pwa_engagement_shown", "eventCount": float64(10), pwaCohortDimension: "  "},
		), nil
	}})

	engagement, err := operations.PwaEngagementV2(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("PwaEngagementV2: %v", err)
	}
	if !engagement.CohortDimensionAvailable {
		t.Fatal("队列维度可用时不应标记为回退")
	}
	if len(engagement.Cohorts) != 2 {
		t.Fatalf("cohorts = %#v", engagement.Cohorts)
	}
	// 空白队列名归为 unknown。
	if engagement.Cohorts[0].Cohort != "engaged" || engagement.Cohorts[1].Cohort != "unknown" {
		t.Fatalf("队列名 = %#v", engagement.Cohorts)
	}
	if engagement.Cohorts[0].CTR != 0.2 {
		t.Fatalf("engaged CTR = %v", engagement.Cohorts[0].CTR)
	}
	// 未被点击的队列 installRate 为 0（分母非零但分子为零）。
	if engagement.Cohorts[1].InstallRate != 0 {
		t.Fatalf("unknown installRate = %v", engagement.Cohorts[1].InstallRate)
	}
}

func TestPwaSummary全部失败时上报错误(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(ReportRequest) (Report, error) {
		return Report{}, ErrNotConfigured
	}})
	if _, err := operations.PwaSummary(context.Background(), DateRangeQuery{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestPwaSummary部分失败时留空位(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		// operatingSystem 维度只出现在 platforms 分支。
		if len(request.Dimensions) > 0 && request.Dimensions[0] == "operatingSystem" {
			return Report{}, ErrUpstream
		}
		return emptyReport(request), nil
	}})

	summary, err := operations.PwaSummary(context.Background(), DateRangeQuery{})
	if err != nil {
		t.Fatalf("PwaSummary: %v", err)
	}
	if summary.Installs == nil || summary.ActiveUsers == nil || summary.EngagementV2 == nil {
		t.Fatalf("成功的子报表不应为 nil：%#v", summary)
	}
	if summary.Platforms != nil {
		t.Fatal("失败的 platforms 应为 nil")
	}
	if summary.GeneratedAt == "" {
		t.Fatal("generatedAt 缺失")
	}
}

func Test转化对比区间计算与差值(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		if request.Dimensions[0] == "sessionSource" {
			return reportOf(map[string]any{"sessionSource": "google", "sessionMedium": "organic", "sessions": float64(60), "conversions": float64(6)}), nil
		}
		// 当期区间以 09-14 开头，上一期以 09-07 开头。
		if request.StartDate == "2026-09-14" {
			return reportOf(map[string]any{"date": "20260914", "sessions": float64(100), "conversions": float64(10), "totalRevenue": float64(500)}), nil
		}
		return reportOf(map[string]any{"date": "20260907", "sessions": float64(80), "conversions": float64(4), "totalRevenue": float64(200)}), nil
	}})

	comparison, err := operations.ConversionCompare(context.Background(), ConversionQuery{
		Days:            7,
		EndDate:         "2026-09-20",
		IncludeBySource: true,
	})
	if err != nil {
		t.Fatalf("ConversionCompare: %v", err)
	}

	if comparison.Ranges.Current != (ConversionRange{StartDate: "2026-09-14", EndDate: "2026-09-20"}) {
		t.Fatalf("当期区间 = %#v", comparison.Ranges.Current)
	}
	if comparison.Ranges.Previous != (ConversionRange{StartDate: "2026-09-07", EndDate: "2026-09-13"}) {
		t.Fatalf("上期区间 = %#v", comparison.Ranges.Previous)
	}
	if comparison.Input.Days != 7 || comparison.Input.Metric != "conversions" || comparison.Input.ConversionEventName != nil {
		t.Fatalf("input = %#v", comparison.Input)
	}
	if comparison.Totals.Current.Sessions != 100 || comparison.Totals.Current.Revenue != 500 || comparison.Totals.Current.ConversionRate != 0.1 {
		t.Fatalf("当期合计 = %#v", comparison.Totals.Current)
	}
	if comparison.Totals.Previous.ConversionRate != 0.05 {
		t.Fatalf("上期转化率 = %v", comparison.Totals.Previous.ConversionRate)
	}
	if comparison.Delta.Sessions != 20 || comparison.Delta.Conversions != 6 || comparison.Delta.Revenue != 300 {
		t.Fatalf("差值 = %#v", comparison.Delta)
	}
	if comparison.Delta.DeltaPct.Sessions == nil || *comparison.Delta.DeltaPct.Sessions != 0.25 {
		t.Fatalf("sessions 变化率 = %#v", comparison.Delta.DeltaPct.Sessions)
	}
	if !comparison.Verdict.Improved || comparison.Verdict.Worsened || comparison.Verdict.Unchanged {
		t.Fatalf("verdict = %#v", comparison.Verdict)
	}
	if len(comparison.BySource) != 1 || comparison.BySource[0].Source != "google/organic" {
		t.Fatalf("bySource = %#v", comparison.BySource)
	}
	if stub.count() != 4 {
		t.Fatalf("含来源时应有 4 次出站请求，实际 %d", stub.count())
	}
}

func Test转化对比指定事件时换用eventCount(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{})

	comparison, err := operations.ConversionCompare(context.Background(), ConversionQuery{
		Days:                7,
		EndDate:             "2026-09-20",
		ConversionEventName: "purchase",
		IncludeBySource:     false,
	})
	if err != nil {
		t.Fatalf("ConversionCompare: %v", err)
	}
	if comparison.Input.Metric != "eventCount(eventName)" {
		t.Fatalf("metric = %q", comparison.Input.Metric)
	}
	if comparison.Input.ConversionEventName == nil || *comparison.Input.ConversionEventName != "purchase" {
		t.Fatalf("conversionEventName = %#v", comparison.Input.ConversionEventName)
	}
	for _, request := range stub.snapshot() {
		if len(request.Metrics) != 3 || request.Metrics[1] != "eventCount" {
			t.Fatalf("metrics = %#v，应把第二项换成 eventCount", request.Metrics)
		}
		if request.DimensionFilter == nil {
			t.Fatal("指定事件时每次查询都应带事件过滤")
		}
	}
	// 关掉来源后只有当期与上一期两次基线查询；bySource 保持 null。
	if stub.count() != 2 {
		t.Fatalf("出站请求 = %d, want 2", stub.count())
	}
	if comparison.BySource != nil {
		t.Fatalf("includeBySource=false 时 bySource 应为 null，得到 %#v", comparison.BySource)
	}
}

func Test转化对比零基数时变化率为null(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{respond: func(request ReportRequest) (Report, error) {
		if request.StartDate == "2026-09-14" {
			return reportOf(map[string]any{"date": "20260914", "sessions": float64(10), "conversions": float64(1), "totalRevenue": float64(5)}), nil
		}
		// 上期全零 → 变化率必须是 null，而不是 0% 或 +Inf。
		return reportOf(), nil
	}})

	comparison, err := operations.ConversionCompare(context.Background(), ConversionQuery{Days: 7, EndDate: "2026-09-20"})
	if err != nil {
		t.Fatalf("ConversionCompare: %v", err)
	}
	if comparison.Delta.DeltaPct.Sessions != nil || comparison.Delta.DeltaPct.Revenue != nil {
		t.Fatalf("零基数变化率应为 null：%#v", comparison.Delta.DeltaPct)
	}
}

func Test转化对比非法结束日期(t *testing.T) {
	operations, stub := operationsWith(&stubReporter{})
	if _, err := operations.ConversionCompare(context.Background(), ConversionQuery{EndDate: "2026/09/20"}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("err = %v, want ErrInvalidQuery", err)
	}
	if stub.count() != 0 {
		t.Fatalf("入参非法时不应发出请求，实际 %d", stub.count())
	}
}

func Test转化对比天数被裁剪(t *testing.T) {
	operations, _ := operationsWith(&stubReporter{})
	comparison, err := operations.ConversionCompare(context.Background(), ConversionQuery{Days: 900, EndDate: "2026-09-20", IncludeBySource: false})
	if err != nil {
		t.Fatalf("ConversionCompare: %v", err)
	}
	if comparison.Input.Days != 365 {
		t.Fatalf("days = %d, want 365", comparison.Input.Days)
	}

	comparison, err = operations.ConversionCompare(context.Background(), ConversionQuery{Days: -5, EndDate: "2026-09-20", IncludeBySource: false})
	if err != nil {
		t.Fatalf("ConversionCompare: %v", err)
	}
	if comparison.Input.Days != 1 {
		t.Fatalf("days = %d, want 1", comparison.Input.Days)
	}
}
