package adminanalyticsga4

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ConversionQuery 对应 GET /conversion-compare 的查询参数。
type ConversionQuery struct {
	Days                int
	EndDate             string
	ConversionEventName string
	IncludeBySource     bool
	Limit               int
}

const (
	defaultComparisonDays  = 7
	defaultComparisonLimit = 20
	maxComparisonDays      = 365
)

type ConversionComparison struct {
	Input    ConversionInput    `json:"input"`
	Ranges   ConversionRanges   `json:"ranges"`
	Totals   ConversionTotals   `json:"totals"`
	Delta    ConversionDelta    `json:"delta"`
	Verdict  ConversionVerdict  `json:"verdict"`
	Trends   ConversionTrends   `json:"trends"`
	BySource []ConversionSource `json:"bySource"`
}

type ConversionInput struct {
	Days                int     `json:"days"`
	EndDate             string  `json:"endDate"`
	ConversionEventName *string `json:"conversionEventName"`
	Metric              string  `json:"metric"`
}

type ConversionRanges struct {
	Current  ConversionRange `json:"current"`
	Previous ConversionRange `json:"previous"`
}

type ConversionRange struct {
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
}

type ConversionTotals struct {
	Current  ConversionTotal `json:"current"`
	Previous ConversionTotal `json:"previous"`
}

type ConversionTotal struct {
	Sessions       float64 `json:"sessions"`
	Conversions    float64 `json:"conversions"`
	Revenue        float64 `json:"revenue"`
	ConversionRate float64 `json:"conversionRate"`
}

type ConversionDelta struct {
	Sessions       float64            `json:"sessions"`
	Conversions    float64            `json:"conversions"`
	Revenue        float64            `json:"revenue"`
	ConversionRate float64            `json:"conversionRate"`
	DeltaPct       ConversionDeltaPct `json:"deltaPct"`
}

// ConversionDeltaPct 的四个字段都可空：旧实现的 `pct()` 在基数为 0 时返回 null，
// 前端据此显示「—」而不是 0%。
type ConversionDeltaPct struct {
	Sessions       *float64 `json:"sessions"`
	Conversions    *float64 `json:"conversions"`
	Revenue        *float64 `json:"revenue"`
	ConversionRate *float64 `json:"conversionRate"`
}

type ConversionVerdict struct {
	Improved  bool `json:"improved"`
	Worsened  bool `json:"worsened"`
	Unchanged bool `json:"unchanged"`
}

type ConversionTrends struct {
	Current  []map[string]any `json:"current"`
	Previous []map[string]any `json:"previous"`
}

type ConversionSource struct {
	Source   string                 `json:"source"`
	Current  ConversionSourceCounts `json:"current"`
	Previous ConversionSourceCounts `json:"previous"`
	Delta    ConversionSourceDelta  `json:"delta"`
}

type ConversionSourceCounts struct {
	Sessions    float64 `json:"sessions"`
	Conversions float64 `json:"conversions"`
}

type ConversionSourceDelta struct {
	Sessions       float64 `json:"sessions"`
	Conversions    float64 `json:"conversions"`
	ConversionRate float64 `json:"conversionRate"`
}

// ConversionCompare 对应 GET /conversion-compare：最近 N 天与上一个 N 天的对比。
func (o *Operations) ConversionCompare(ctx context.Context, query ConversionQuery) (ConversionComparison, error) {
	if err := o.ready(); err != nil {
		return ConversionComparison{}, err
	}

	// 复刻 `Math.max(1, Math.min(365, Number(days) || 7))`：
	// 0 与 NaN 落到 7，负数被夹到 1，而不是被当成「未给」。
	days := query.Days
	if days == 0 {
		days = defaultComparisonDays
	}
	if days < 1 {
		days = 1
	}
	if days > maxComparisonDays {
		days = maxComparisonDays
	}

	endDate := query.EndDate
	if endDate == "" {
		endDate = shanghaiDateString(0, o.clock())
	}
	// 旧实现把 endDate 拼上固定的时间后缀再交给 Date 解析，无法解析即 400。
	// 这里保持同一形状：只接受 YYYY-MM-DD。
	endMoment, err := time.Parse("2006-01-02T15:04:05.000Z", endDate+"T00:00:00.000Z")
	if err != nil {
		return ConversionComparison{}, fmt.Errorf("%w: invalid endDate, expected YYYY-MM-DD", ErrInvalidQuery)
	}

	format := func(moment time.Time) string { return moment.UTC().Format("2006-01-02") }
	currentStart := format(endMoment.AddDate(0, 0, -(days - 1)))
	previousEnd := format(endMoment.AddDate(0, 0, -days))
	previousStart := format(endMoment.AddDate(0, 0, -(days*2 - 1)))

	// 指定了转化事件时，用带事件名过滤的 eventCount；否则用 GA4 的原生 conversions 度量。
	metricName := "conversions"
	if query.ConversionEventName != "" {
		metricName = "eventCount"
	}
	var eventFilter []byte
	if query.ConversionEventName != "" {
		eventFilter = filterEventNameEquals(query.ConversionEventName)
	}

	baseReport := func(startDate, endDate string) (Report, error) {
		return o.reporter.RunReport(ctx, ReportRequest{
			Dimensions:      []string{"date"},
			Metrics:         []string{"sessions", metricName, "totalRevenue"},
			StartDate:       startDate,
			EndDate:         endDate,
			OrderBys:        []OrderBy{{Dimension: &OrderByDimension{DimensionName: "date"}}},
			DimensionFilter: eventFilter,
		})
	}

	var current, previous Report
	if err := parallel(
		func() error { value, err := baseReport(currentStart, endDate); current = value; return err },
		func() error { value, err := baseReport(previousStart, previousEnd); previous = value; return err },
	); err != nil {
		return ConversionComparison{}, err
	}

	sum := func(report Report, field string) float64 {
		total := 0.0
		for _, row := range report.Rows {
			total += metricNumber(row, field)
		}
		return total
	}
	totalsCurrent := ConversionTotal{
		Sessions:    sum(current, "sessions"),
		Conversions: sum(current, metricName),
		Revenue:     sum(current, "totalRevenue"),
	}
	totalsPrevious := ConversionTotal{
		Sessions:    sum(previous, "sessions"),
		Conversions: sum(previous, metricName),
		Revenue:     sum(previous, "totalRevenue"),
	}
	rate := func(conversions, sessions float64) float64 {
		if sessions == 0 {
			return 0
		}
		return conversions / sessions
	}
	currentRate := rate(totalsCurrent.Conversions, totalsCurrent.Sessions)
	previousRate := rate(totalsPrevious.Conversions, totalsPrevious.Sessions)
	totalsCurrent.ConversionRate = currentRate
	totalsPrevious.ConversionRate = previousRate

	delta := ConversionDelta{
		Sessions:       totalsCurrent.Sessions - totalsPrevious.Sessions,
		Conversions:    totalsCurrent.Conversions - totalsPrevious.Conversions,
		Revenue:        totalsCurrent.Revenue - totalsPrevious.Revenue,
		ConversionRate: currentRate - previousRate,
	}
	percentage := func(value, base float64) *float64 {
		if base == 0 {
			return nil
		}
		result := value / base
		return &result
	}
	delta.DeltaPct = ConversionDeltaPct{
		Sessions:       percentage(delta.Sessions, totalsPrevious.Sessions),
		Conversions:    percentage(delta.Conversions, totalsPrevious.Conversions),
		Revenue:        percentage(delta.Revenue, totalsPrevious.Revenue),
		ConversionRate: percentage(delta.ConversionRate, previousRate),
	}

	// includeBySource 为 false 时保持 nil，序列化成 null —— 旧实现同样是 null 而不是 []。
	var bySource []ConversionSource
	if query.IncludeBySource {
		limit := query.Limit
		if limit <= 0 {
			limit = defaultComparisonLimit
		}
		sourceReport := func(startDate, endDate string) (Report, error) {
			return o.reporter.RunReport(ctx, ReportRequest{
				Dimensions:      []string{"sessionSource", "sessionMedium"},
				Metrics:         []string{"sessions", metricName},
				StartDate:       startDate,
				EndDate:         endDate,
				Limit:           limit,
				OrderBys:        []OrderBy{{Metric: &OrderByMetric{MetricName: "sessions"}, Desc: true}},
				DimensionFilter: eventFilter,
			})
		}
		var currentSources, previousSources Report
		if err := parallel(
			func() error { value, err := sourceReport(currentStart, endDate); currentSources = value; return err },
			func() error {
				value, err := sourceReport(previousStart, previousEnd)
				previousSources = value
				return err
			},
		); err != nil {
			return ConversionComparison{}, err
		}

		keyOf := func(row map[string]any) string {
			return stringValue(row, "sessionSource") + "/" + stringValue(row, "sessionMedium")
		}
		mapRows := func(report Report) map[string]ConversionSourceCounts {
			mapped := make(map[string]ConversionSourceCounts, len(report.Rows))
			for _, row := range report.Rows {
				mapped[keyOf(row)] = ConversionSourceCounts{
					Sessions:    metricNumber(row, "sessions"),
					Conversions: metricNumber(row, metricName),
				}
			}
			return mapped
		}
		currentMap := mapRows(currentSources)
		previousMap := mapRows(previousSources)

		// 保持首次出现的顺序：旧实现用 Set 合并键，顺序为「当前期全部键」后接「仅上一期出现的键」。
		keys := make([]string, 0, len(currentMap)+len(previousMap))
		seen := make(map[string]struct{}, len(currentMap)+len(previousMap))
		for _, report := range []Report{currentSources, previousSources} {
			for _, row := range report.Rows {
				key := keyOf(row)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				keys = append(keys, key)
			}
		}

		bySource = make([]ConversionSource, 0, len(keys))
		for _, key := range keys {
			item := ConversionSource{
				Source:   key,
				Current:  currentMap[key],
				Previous: previousMap[key],
			}
			item.Delta = ConversionSourceDelta{
				Sessions:       item.Current.Sessions - item.Previous.Sessions,
				Conversions:    item.Current.Conversions - item.Previous.Conversions,
				ConversionRate: rate(item.Current.Conversions, item.Current.Sessions) - rate(item.Previous.Conversions, item.Previous.Sessions),
			}
			bySource = append(bySource, item)
		}
		sort.SliceStable(bySource, func(first, second int) bool {
			return absFloat(bySource[first].Delta.Conversions) > absFloat(bySource[second].Delta.Conversions)
		})
		if len(bySource) > limit {
			bySource = bySource[:limit]
		}
	}

	var conversionEventName *string
	if query.ConversionEventName != "" {
		value := query.ConversionEventName
		conversionEventName = &value
	}
	metric := "conversions"
	if query.ConversionEventName != "" {
		metric = "eventCount(eventName)"
	}

	return ConversionComparison{
		Input: ConversionInput{
			Days:                days,
			EndDate:             endDate,
			ConversionEventName: conversionEventName,
			Metric:              metric,
		},
		Ranges: ConversionRanges{
			Current:  ConversionRange{StartDate: currentStart, EndDate: endDate},
			Previous: ConversionRange{StartDate: previousStart, EndDate: previousEnd},
		},
		Totals: ConversionTotals{Current: totalsCurrent, Previous: totalsPrevious},
		Delta:  delta,
		Verdict: ConversionVerdict{
			Improved:  delta.ConversionRate > 0,
			Worsened:  delta.ConversionRate < 0,
			Unchanged: delta.ConversionRate == 0,
		},
		Trends:   ConversionTrends{Current: current.Rows, Previous: previous.Rows},
		BySource: bySource,
	}, nil
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
