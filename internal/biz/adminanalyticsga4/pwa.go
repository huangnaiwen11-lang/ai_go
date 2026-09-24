package adminanalyticsga4

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// ---- 事件名清单：顺序直接决定 /pwa/installs 的漏斗与 totals，不要重排 ----

var (
	pwaLandingFunnelEvents = []string{
		"pwa_landing_view",
		"pwa_landing_install_clicked",
		"pwa_install_intent",
		"pwa_install_verified",
	}
	pwaLegacyFunnelEvents = []string{
		"pwa_install_banner_shown",
		"pwa_install_clicked",
		"pwa_install_intent",
		"pwa_install_verified",
	}
	pwaIOSFunnelEvents = []string{
		"pwa_ios_install_guide_opened",
		"pwa_standalone_entered",
	}
	// pwaPaywallEvents 对应旧实现的默认事件序列。
	pwaPaywallEvents = []string{"subscription_modal_open", "begin_checkout", "purchase"}
	// pwaEngagementV2Events 对应 PWA Engagement v2 漏斗。
	pwaEngagementV2Events = []string{
		"pwa_engagement_shown",
		"pwa_engagement_dismissed",
		"pwa_engagement_skipped",
		"pwa_install_clicked",
		"pwa_installed",
	}
)

const (
	pwaSourceDimension       = "customEvent:source"
	pwaCohortDimension       = "customEvent:engagement_score_bucket"
	pwaInstallCompletedEvent = "pwa_installed"
	pwaTotalLaunchEvent      = "app_launch"
	pwaStandaloneLaunchEvent = "pwa_app_launch"
)

// pwaFunnelEvents 复刻 `[...new Set([...landing, ...legacy, ...ios])]`：
// 去重且保留首次出现顺序。
func pwaFunnelEvents() []string {
	ordered := make([]string, 0, 8)
	seen := map[string]struct{}{}
	for _, group := range [][]string{pwaLandingFunnelEvents, pwaLegacyFunnelEvents, pwaIOSFunnelEvents} {
		for _, event := range group {
			if _, ok := seen[event]; ok {
				continue
			}
			seen[event] = struct{}{}
			ordered = append(ordered, event)
		}
	}
	return ordered
}

// ---- 响应形状 ----

type PwaInstalls struct {
	Daily  []map[string]any  `json:"daily"`
	Funnel []PwaFunnelStep   `json:"funnel"`
	Totals PwaInstallsTotals `json:"totals"`
}

type PwaFunnelStep struct {
	EventName      string  `json:"eventName"`
	EventCount     float64 `json:"eventCount"`
	ActiveUsers    float64 `json:"activeUsers"`
	ConversionRate float64 `json:"conversionRate"`
}

type PwaInstallsTotals struct {
	Installs          float64 `json:"installs"`
	BannerShown       float64 `json:"bannerShown"`
	Clicked           float64 `json:"clicked"`
	InstallIntent     float64 `json:"installIntent"`
	InstallVerified   float64 `json:"installVerified"`
	IOSGuideOpened    float64 `json:"iosGuideOpened"`
	StandaloneEntered float64 `json:"standaloneEntered"`
	LandingView       float64 `json:"landingView"`
	LandingClicked    float64 `json:"landingClicked"`
	FunnelType        string  `json:"funnelType"`
	FunnelBase        float64 `json:"funnelBase"`
}

type PwaActive struct {
	Daily    []PwaActiveDay  `json:"daily"`
	Totals   PwaActiveTotals `json:"totals"`
	PwaRatio float64         `json:"pwaRatio"`
}

type PwaActiveDay struct {
	Date    string  `json:"date"`
	Pwa     float64 `json:"pwa"`
	Browser float64 `json:"browser"`
	Total   float64 `json:"total"`
}

type PwaActiveTotals struct {
	Pwa     float64 `json:"pwa"`
	Browser float64 `json:"browser"`
	Total   float64 `json:"total"`
}

type PwaPlatforms struct {
	Platforms []PwaPlatform `json:"platforms"`
}

type PwaPlatform struct {
	Platform     string  `json:"platform"`
	PwaUsers     float64 `json:"pwaUsers"`
	BrowserUsers float64 `json:"browserUsers"`
}

type PwaPaywallSources struct {
	SourceDimensionAvailable bool               `json:"sourceDimensionAvailable"`
	Events                   []string           `json:"events"`
	Sources                  []PwaPaywallSource `json:"sources"`
	Totals                   PwaPaywallTotals   `json:"totals"`
	Notice                   string             `json:"notice,omitempty"`
	GeneratedAt              string             `json:"generatedAt"`
}

type PwaPaywallSource struct {
	Source                 string  `json:"source"`
	SubscriptionModalOpen  float64 `json:"subscription_modal_open"`
	BeginCheckout          float64 `json:"begin_checkout"`
	Purchase               float64 `json:"purchase"`
	CheckoutRate           float64 `json:"checkoutRate"`
	PurchaseRate           float64 `json:"purchaseRate"`
	CheckoutToPurchaseRate float64 `json:"checkoutToPurchaseRate"`
}

type PwaPaywallTotals struct {
	SubscriptionModalOpen  float64 `json:"subscription_modal_open"`
	BeginCheckout          float64 `json:"begin_checkout"`
	Purchase               float64 `json:"purchase"`
	CheckoutRate           float64 `json:"checkoutRate"`
	PurchaseRate           float64 `json:"purchaseRate"`
	CheckoutToPurchaseRate float64 `json:"checkoutToPurchaseRate"`
}

type PwaEngagementCohort struct {
	Cohort      string  `json:"cohort"`
	Shown       float64 `json:"pwa_engagement_shown"`
	Dismissed   float64 `json:"pwa_engagement_dismissed"`
	Skipped     float64 `json:"pwa_engagement_skipped"`
	Clicked     float64 `json:"pwa_install_clicked"`
	Installed   float64 `json:"pwa_installed"`
	CTR         float64 `json:"ctr"`
	InstallRate float64 `json:"installRate"`
}

type PwaEngagementV2 struct {
	CohortDimensionAvailable bool                  `json:"cohortDimensionAvailable"`
	Events                   []string              `json:"events"`
	Cohorts                  []PwaEngagementCohort `json:"cohorts"`
	GeneratedAt              string                `json:"generatedAt"`
}

// PwaSummary 与旧实现一样多返回一个 engagementV2：前端类型只声明了三个字段，
// 多出的键不会破坏契约，反而是旧行为的一部分。
type PwaSummary struct {
	Installs     *PwaInstalls     `json:"installs"`
	ActiveUsers  *PwaActive       `json:"activeUsers"`
	Platforms    *PwaPlatforms    `json:"platforms"`
	EngagementV2 *PwaEngagementV2 `json:"engagementV2"`
	GeneratedAt  string           `json:"generatedAt"`
}

// ---- 端点 ----

// PwaInstalls 对应 GET /pwa/installs：安装日曲线 + 安装漏斗。
//
// 漏斗事件有 landing / legacy 两代命名，旧实现按「landing 事件是否有量」二选一，
// 并把 totals 里两代字段全部平铺输出。
func (o *Operations) PwaInstalls(ctx context.Context, query DateRangeQuery) (PwaInstalls, error) {
	if err := o.ready(); err != nil {
		return PwaInstalls{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)

	installs, err := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      []string{"date"},
		Metrics:         []string{"eventCount", "activeUsers"},
		StartDate:       start,
		EndDate:         end,
		DimensionFilter: filterEventNameEquals(pwaInstallCompletedEvent),
		OrderBys:        []OrderBy{{Dimension: &OrderByDimension{DimensionName: "date"}}},
	})
	if err != nil {
		return PwaInstalls{}, err
	}

	allFunnelEvents := pwaFunnelEvents()
	funnelReport, err := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      []string{"eventName"},
		Metrics:         []string{"eventCount", "activeUsers"},
		StartDate:       start,
		EndDate:         end,
		DimensionFilter: filterEventNameInList(allFunnelEvents),
	})
	if err != nil {
		return PwaInstalls{}, err
	}

	eventCounts := make(map[string]float64, len(allFunnelEvents))
	// 事件级去重：同一事件名只保留首次出现行的计数，与旧实现的 find() 一致。
	byName := make(map[string]map[string]any, len(funnelReport.Rows))
	for _, row := range funnelReport.Rows {
		name := stringValue(row, "eventName")
		if _, ok := byName[name]; !ok {
			byName[name] = row
		}
	}
	for _, name := range allFunnelEvents {
		if row, ok := byName[name]; ok {
			eventCounts[name] = metricNumber(row, "eventCount")
		}
	}

	hasLandingData := false
	for _, name := range pwaLandingFunnelEvents {
		if eventCounts[name] > 0 {
			hasLandingData = true
			break
		}
	}
	selected := pwaLegacyFunnelEvents
	funnelType := "legacy"
	if hasLandingData {
		selected = pwaLandingFunnelEvents
		funnelType = "landing"
	}

	steps := make([]PwaFunnelStep, 0, len(selected))
	for _, name := range selected {
		step := PwaFunnelStep{EventName: name}
		if row, ok := byName[name]; ok {
			step.EventCount = metricNumber(row, "eventCount")
			step.ActiveUsers = metricNumber(row, "activeUsers")
		}
		steps = append(steps, step)
	}
	for index := range steps {
		if index == 0 {
			steps[index].ConversionRate = 1
			continue
		}
		previous := steps[index-1].EventCount
		if previous > 0 {
			steps[index].ConversionRate = steps[index].EventCount / previous
		}
	}

	totalInstalls := 0.0
	for _, row := range installs.Rows {
		totalInstalls += metricNumber(row, "eventCount")
	}
	// 复刻 `selectedRows[0].eventCount || (hasLanding ? landing_view : banner_shown)`：
	// 漏斗首步为 0 时回退到该代的入口事件计数。
	funnelBase := 0.0
	if len(steps) > 0 {
		funnelBase = steps[0].EventCount
	}
	if funnelBase == 0 {
		if hasLandingData {
			funnelBase = eventCounts["pwa_landing_view"]
		} else {
			funnelBase = eventCounts["pwa_install_banner_shown"]
		}
	}

	return PwaInstalls{
		Daily:  installs.Rows,
		Funnel: steps,
		Totals: PwaInstallsTotals{
			Installs:          totalInstalls,
			BannerShown:       eventCounts["pwa_install_banner_shown"],
			Clicked:           eventCounts["pwa_install_clicked"],
			InstallIntent:     eventCounts["pwa_install_intent"],
			InstallVerified:   eventCounts["pwa_install_verified"],
			IOSGuideOpened:    eventCounts["pwa_ios_install_guide_opened"],
			StandaloneEntered: eventCounts["pwa_standalone_entered"],
			LandingView:       eventCounts["pwa_landing_view"],
			LandingClicked:    eventCounts["pwa_landing_install_clicked"],
			FunnelType:        funnelType,
			FunnelBase:        funnelBase,
		},
	}, nil
}

// PwaActive 对应 GET /pwa/active：全部启动与 PWA 启动两个口径按日对齐。
//
// 与旧实现的一处刻意偏离：旧实现对两侧都做了 `.catch(() => ({rows: []}))`，
// 于是凭据写错也会得到 200 + 全零，读起来就是「没人用 PWA」。
// 这里改为照 Devices 的做法上报错误 —— 成功但零行仍然是正常结果，
// 被吞掉的只有真故障。该决定登记在 docs/待确认事项与配置参数清单.md（K7）。
func (o *Operations) PwaActive(ctx context.Context, query DateRangeQuery) (PwaActive, error) {
	if err := o.ready(); err != nil {
		return PwaActive{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)

	var totalRows, pwaRows []map[string]any
	if err := parallel(
		func() error {
			rows, err := o.rowsForEvent(ctx, start, end, pwaTotalLaunchEvent, []string{"date"}, []string{"activeUsers"}, []OrderBy{{Dimension: &OrderByDimension{DimensionName: "date"}}})
			totalRows = rows
			return err
		},
		func() error {
			rows, err := o.rowsForEvent(ctx, start, end, pwaStandaloneLaunchEvent, []string{"date"}, []string{"activeUsers"}, []OrderBy{{Dimension: &OrderByDimension{DimensionName: "date"}}})
			pwaRows = rows
			return err
		},
	); err != nil {
		return PwaActive{}, err
	}

	byDate := newOrderedAccumulator[PwaActiveDay]()
	for _, row := range totalRows {
		date := stringValue(row, "date")
		byDate.ensure(date, func() *PwaActiveDay { return &PwaActiveDay{Date: date} }).Total = metricNumber(row, "activeUsers")
	}
	for _, row := range pwaRows {
		date := stringValue(row, "date")
		byDate.ensure(date, func() *PwaActiveDay { return &PwaActiveDay{Date: date} }).Pwa = metricNumber(row, "activeUsers")
	}

	daily := make([]PwaActiveDay, 0, len(byDate.keys))
	for _, item := range byDate.list() {
		item.Browser = item.Total - item.Pwa
		if item.Browser < 0 {
			item.Browser = 0
		}
		daily = append(daily, *item)
	}
	sortByDate(daily)

	totals := PwaActiveTotals{}
	for _, item := range daily {
		totals.Pwa += item.Pwa
		totals.Browser += item.Browser
		totals.Total += item.Total
	}
	ratio := 0.0
	if totals.Total > 0 {
		ratio = totals.Pwa / totals.Total
	}
	return PwaActive{Daily: daily, Totals: totals, PwaRatio: ratio}, nil
}

// PwaPlatforms 对应 GET /pwa/platforms：把 operatingSystem 归并成 ios / android / desktop 三档。
// 与 PwaActive 同样的偏离：不再把上游故障吞成空平台列表。
func (o *Operations) PwaPlatforms(ctx context.Context, query DateRangeQuery) (PwaPlatforms, error) {
	if err := o.ready(); err != nil {
		return PwaPlatforms{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)

	var totalRows, pwaRows []map[string]any
	if err := parallel(
		func() error {
			rows, err := o.rowsForEvent(ctx, start, end, pwaTotalLaunchEvent, []string{"operatingSystem"}, []string{"activeUsers"}, nil)
			totalRows = rows
			return err
		},
		func() error {
			rows, err := o.rowsForEvent(ctx, start, end, pwaStandaloneLaunchEvent, []string{"operatingSystem"}, []string{"activeUsers"}, nil)
			pwaRows = rows
			return err
		},
	); err != nil {
		return PwaPlatforms{}, err
	}

	byPlatform := newOrderedAccumulator[platformAccumulator]()
	for _, row := range totalRows {
		key := platformKey(stringValue(row, "operatingSystem"))
		byPlatform.ensure(key, func() *platformAccumulator { return &platformAccumulator{platform: key} }).total += metricNumber(row, "activeUsers")
	}
	for _, row := range pwaRows {
		key := platformKey(stringValue(row, "operatingSystem"))
		byPlatform.ensure(key, func() *platformAccumulator { return &platformAccumulator{platform: key} }).pwa += metricNumber(row, "activeUsers")
	}

	platforms := make([]PwaPlatform, 0, len(byPlatform.keys))
	for _, item := range byPlatform.list() {
		browserUsers := item.total - item.pwa
		if browserUsers < 0 {
			browserUsers = 0
		}
		platforms = append(platforms, PwaPlatform{
			Platform:     item.platform,
			PwaUsers:     item.pwa,
			BrowserUsers: browserUsers,
		})
	}
	return PwaPlatforms{Platforms: platforms}, nil
}

// platformAccumulator 是 /pwa/platforms 的中间状态：
// 先分别累计「全部启动」与「PWA 启动」，相减才是浏览器用户。
type platformAccumulator struct {
	platform string
	total    float64
	pwa      float64
}

// platformKey 复刻 keyFromOs：mac 被算进 ios（旧实现的既有归类）。
func platformKey(operatingSystem string) string {
	normalized := strings.ToLower(operatingSystem)
	switch {
	case strings.Contains(normalized, "ios"),
		strings.Contains(normalized, "ipad"),
		strings.Contains(normalized, "iphone"),
		strings.Contains(normalized, "mac"):
		return "ios"
	case strings.Contains(normalized, "android"):
		return "android"
	default:
		return "desktop"
	}
}

// PwaPaywallSources 对应 GET /pwa/paywall-sources。
//
// 自定义维度 customEvent:source 未在 GA4 属性里建好时，第一次查询会失败；
// 旧实现据此降级成「不分来源的整体漏斗」并附 notice。这是被建模的降级，
// 与「把故障演成空数据」不同：降级结果依然是有意义的真实数据，且 notice 明确标注了口径变化。
func (o *Operations) PwaPaywallSources(ctx context.Context, query DateRangeQuery) (PwaPaywallSources, error) {
	if err := o.ready(); err != nil {
		return PwaPaywallSources{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)

	report, err := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      []string{"eventName", pwaSourceDimension},
		Metrics:         []string{"eventCount"},
		StartDate:       start,
		EndDate:         end,
		Limit:           DefaultReportRowLimit,
		DimensionFilter: filterEventNameInList(pwaPaywallEvents),
		OrderBys: []OrderBy{
			{Dimension: &OrderByDimension{DimensionName: pwaSourceDimension}},
			{Dimension: &OrderByDimension{DimensionName: "eventName"}},
		},
	})
	if err == nil {
		sources := paywallSourcesFromRows(report.Rows, pwaSourceDimension)
		return PwaPaywallSources{
			SourceDimensionAvailable: true,
			Events:                   pwaPaywallEvents,
			Sources:                  sources,
			Totals:                   paywallTotals(sources),
			GeneratedAt:              jsISOString(o.clock()),
		}, nil
	}
	// 只有「维度不可用」这一种失败该降级；凭据被拒或网络故障必须原样上报，
	// 否则会把配置问题伪装成「来源维度没建」。
	if !isUnavailableDimension(err) {
		return PwaPaywallSources{}, err
	}

	fallback, fallbackErr := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      []string{"eventName"},
		Metrics:         []string{"eventCount"},
		StartDate:       start,
		EndDate:         end,
		DimensionFilter: filterEventNameInList(pwaPaywallEvents),
		OrderBys:        []OrderBy{{Dimension: &OrderByDimension{DimensionName: "eventName"}}},
	})
	if fallbackErr != nil {
		return PwaPaywallSources{}, fallbackErr
	}

	counts := map[string]float64{}
	for _, row := range fallback.Rows {
		counts[stringValue(row, "eventName")] = metricNumber(row, "eventCount")
	}
	fallbackRows := make([]map[string]any, 0, len(pwaPaywallEvents))
	for _, event := range pwaPaywallEvents {
		fallbackRows = append(fallbackRows, map[string]any{
			"eventName":        event,
			"eventCount":       counts[event],
			pwaSourceDimension: "all",
		})
	}
	sources := paywallSourcesFromRows(fallbackRows, pwaSourceDimension)
	return PwaPaywallSources{
		SourceDimensionAvailable: false,
		Events:                   pwaPaywallEvents,
		Sources:                  sources,
		Totals:                   paywallTotals(sources),
		Notice:                   "GA4 custom dimension customEvent:source not available; showing overall funnel only.",
		GeneratedAt:              jsISOString(o.clock()),
	}, nil
}

// PwaSummary 对应 GET /pwa/summary。与 /summary 同一条规则：
// 允许部分子报表为 null，但不允许全部失败后仍返回一个全空信封。
func (o *Operations) PwaSummary(ctx context.Context, query DateRangeQuery) (PwaSummary, error) {
	if err := o.ready(); err != nil {
		return PwaSummary{}, err
	}
	var installs PwaInstalls
	var active PwaActive
	var platforms PwaPlatforms
	var engagement PwaEngagementV2

	failures := parallelErrors([]func() error{
		func() error { value, err := o.PwaInstalls(ctx, query); installs = value; return err },
		func() error { value, err := o.PwaActive(ctx, query); active = value; return err },
		func() error { value, err := o.PwaPlatforms(ctx, query); platforms = value; return err },
		func() error { value, err := o.PwaEngagementV2(ctx, query); engagement = value; return err },
	})
	allFailed := true
	for _, failure := range failures {
		if failure == nil {
			allFailed = false
			break
		}
	}
	if allFailed {
		return PwaSummary{}, failures[0]
	}

	summary := PwaSummary{GeneratedAt: jsISOString(o.clock())}
	if failures[0] == nil {
		summary.Installs = &installs
	}
	if failures[1] == nil {
		summary.ActiveUsers = &active
	}
	if failures[2] == nil {
		summary.Platforms = &platforms
	}
	if failures[3] == nil {
		summary.EngagementV2 = &engagement
	}
	return summary, nil
}

// PwaEngagementV2 只被 /pwa/summary 使用，旧实现没有为它开路由。
func (o *Operations) PwaEngagementV2(ctx context.Context, query DateRangeQuery) (PwaEngagementV2, error) {
	if err := o.ready(); err != nil {
		return PwaEngagementV2{}, err
	}
	start, end := o.rangeOf(query.StartDate, query.EndDate)

	withCohort, err := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      []string{"eventName", pwaCohortDimension},
		Metrics:         []string{"eventCount", "activeUsers"},
		StartDate:       start,
		EndDate:         end,
		Limit:           DefaultReportRowLimit,
		DimensionFilter: filterEventNameInList(pwaEngagementV2Events),
	})
	cohortAvailable := true
	if err != nil {
		if !isUnavailableDimension(err) {
			return PwaEngagementV2{}, err
		}
		cohortAvailable = false
		withCohort, err = o.reporter.RunReport(ctx, ReportRequest{
			Dimensions:      []string{"eventName"},
			Metrics:         []string{"eventCount", "activeUsers"},
			StartDate:       start,
			EndDate:         end,
			DimensionFilter: filterEventNameInList(pwaEngagementV2Events),
		})
		if err != nil {
			return PwaEngagementV2{}, err
		}
	}

	byCohort := newOrderedAccumulator[PwaEngagementCohort]()
	for _, row := range withCohort.Rows {
		cohort := "all"
		if cohortAvailable {
			cohort = strings.TrimSpace(stringValue(row, pwaCohortDimension))
			if cohort == "" {
				cohort = "unknown"
			}
		}
		eventName := stringValue(row, "eventName")
		if !containsString(pwaEngagementV2Events, eventName) {
			continue
		}
		item := byCohort.ensure(cohort, func() *PwaEngagementCohort { return &PwaEngagementCohort{Cohort: cohort} })
		count := metricNumber(row, "eventCount")
		switch eventName {
		case "pwa_engagement_shown":
			item.Shown += count
		case "pwa_engagement_dismissed":
			item.Dismissed += count
		case "pwa_engagement_skipped":
			item.Skipped += count
		case "pwa_install_clicked":
			item.Clicked += count
		case pwaInstallCompletedEvent:
			item.Installed += count
		}
	}

	cohorts := make([]PwaEngagementCohort, 0, len(byCohort.keys))
	for _, item := range byCohort.list() {
		if item.Shown > 0 {
			item.CTR = item.Clicked / item.Shown
			item.InstallRate = item.Installed / item.Shown
		}
		cohorts = append(cohorts, *item)
	}
	return PwaEngagementV2{
		CohortDimensionAvailable: cohortAvailable,
		Events:                   pwaEngagementV2Events,
		Cohorts:                  cohorts,
		GeneratedAt:              jsISOString(o.clock()),
	}, nil
}

// ---- 内部 ----

func paywallSourcesFromRows(rows []map[string]any, sourceDimension string) []PwaPaywallSource {
	bySource := newOrderedAccumulator[PwaPaywallSource]()
	for _, row := range rows {
		source := strings.TrimSpace(stringValue(row, sourceDimension))
		if source == "" {
			source = "unknown"
		}
		eventName := stringValue(row, "eventName")
		count := metricNumber(row, "eventCount")
		item := bySource.ensure(source, func() *PwaPaywallSource { return &PwaPaywallSource{Source: source} })
		if !containsString(pwaPaywallEvents, eventName) {
			continue
		}
		switch eventName {
		case "subscription_modal_open":
			item.SubscriptionModalOpen += count
		case "begin_checkout":
			item.BeginCheckout += count
		case "purchase":
			item.Purchase += count
		}
	}

	sources := make([]PwaPaywallSource, 0, len(bySource.keys))
	for _, item := range bySource.list() {
		if item.SubscriptionModalOpen > 0 {
			item.CheckoutRate = item.BeginCheckout / item.SubscriptionModalOpen
			item.PurchaseRate = item.Purchase / item.SubscriptionModalOpen
		}
		if item.BeginCheckout > 0 {
			item.CheckoutToPurchaseRate = item.Purchase / item.BeginCheckout
		}
		sources = append(sources, *item)
	}
	sortByModalOpen(sources)
	return sources
}

func paywallTotals(sources []PwaPaywallSource) PwaPaywallTotals {
	totals := PwaPaywallTotals{}
	for _, source := range sources {
		totals.SubscriptionModalOpen += source.SubscriptionModalOpen
		totals.BeginCheckout += source.BeginCheckout
		totals.Purchase += source.Purchase
	}
	if totals.SubscriptionModalOpen > 0 {
		totals.CheckoutRate = totals.BeginCheckout / totals.SubscriptionModalOpen
		totals.PurchaseRate = totals.Purchase / totals.SubscriptionModalOpen
	}
	if totals.BeginCheckout > 0 {
		totals.CheckoutToPurchaseRate = totals.Purchase / totals.BeginCheckout
	}
	return totals
}

// isUnavailableDimension 判断上游失败是否属于「自定义维度尚未在 GA4 属性里建立」。
// 只有这一种才允许降级：凭据、网络、配额问题都必须原样上报。
func isUnavailableDimension(err error) bool {
	var upstream *UpstreamError
	if !errors.As(err, &upstream) {
		return false
	}
	if upstream.Status != 400 && upstream.Status != 403 {
		return false
	}
	body := strings.ToLower(upstream.Body)
	return strings.Contains(body, "custom") && (strings.Contains(body, "dimension") || strings.Contains(body, "field"))
}

// rowsForEvent 是 PWA 系列共用的单事件查询：按事件名过滤后取行。
func (o *Operations) rowsForEvent(ctx context.Context, start, end, eventName string, dimensions, metrics []string, orderBys []OrderBy) ([]map[string]any, error) {
	report, err := o.reporter.RunReport(ctx, ReportRequest{
		Dimensions:      dimensions,
		Metrics:         metrics,
		StartDate:       start,
		EndDate:         end,
		DimensionFilter: filterEventNameEquals(eventName),
		OrderBys:        orderBys,
	})
	if err != nil {
		return nil, err
	}
	return report.Rows, nil
}

func sortByDate(days []PwaActiveDay) {
	sort.SliceStable(days, func(first, second int) bool { return days[first].Date < days[second].Date })
}

func sortByModalOpen(sources []PwaPaywallSource) {
	sort.SliceStable(sources, func(first, second int) bool {
		return sources[first].SubscriptionModalOpen > sources[second].SubscriptionModalOpen
	})
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
