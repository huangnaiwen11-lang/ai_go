// Package adminanalyticsga4 承载 GA4 后台报表的业务投影。
//
// 它把 Analytics Data API 的原始响应翻译成后台前端已经冻结的响应形状
// （见 ai-admin/src/api/analyticsGa4.ts 与 analyticsPwa.ts）。
// 本包不读取环境变量、不持有凭据、不发起 HTTP —— 出站适配器由
// integrations/ga4 实现 Reporter，装配点在 cmd/api-gateway。
package adminanalyticsga4

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 哨兵故意拆成四个而不是一个笼统的「不可用」：
// transport 需要按原因给出不同的 HTTP 语义（未配置 / 凭据被拒 / 上游失败 / 入参非法），
// 单一哨兵会迫使调用方解析错误文本，这正是本项目在 B2B 链路上踩过的坑。
var (
	// ErrNotConfigured 表示没有可用的 Reporter：既没有凭据，也没有装配出站适配器。
	ErrNotConfigured = errors.New("ga4: reporter not configured")
	// ErrCredentialRejected 表示 token 端点明确拒绝了服务账号凭据（401/403）。
	// 它与 ErrUpstream 分开，因为「我们的凭据是错的」和「Google 挂了」需要不同的运维动作。
	ErrCredentialRejected = errors.New("ga4: token endpoint rejected the service account credential")
	// ErrUpstream 表示 Analytics Data API 请求本身失败。
	ErrUpstream = errors.New("ga4: analytics data api request failed")
	// ErrInvalidQuery 表示入参不合法（例如 conversion-compare 的 endDate 无法解析）。
	ErrInvalidQuery = errors.New("ga4: invalid report query")
)

// UpstreamError 保留上游状态码与响应片段。
//
// 状态码必须能被 transport 读到：POST /report 的维度与度量由调用方给出，
// 上游的 400 属于调用方错误，不该被渲染成「网关故障」。
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	status := strconv.Itoa(e.Status)
	if e.Body == "" {
		return "ga4: upstream returned " + status
	}
	return "ga4: upstream returned " + status + ": " + e.Body
}

func (e *UpstreamError) Unwrap() error { return ErrUpstream }

// Reporter 是有界的 GA4 Data API 适配器契约。实现必须自行处理凭据与令牌缓存，
// 且不得把上游响应无上限地读入内存。
type Reporter interface {
	RunReport(ctx context.Context, request ReportRequest) (Report, error)
	RunRealtimeReport(ctx context.Context, request ReportRequest) (Report, error)
}

// ReportRequest 是有界的 Analytics Data API 报表请求。
// 过滤器按原样透传原始 JSON：上游报文结构稳定，重新建模只会引入偏差，
// 而 POST /report 本来就要把调用方的过滤器原封不动交出去。
type ReportRequest struct {
	Dimensions      []string
	Metrics         []string
	StartDate       string
	EndDate         string
	Limit           int
	Offset          int
	OrderBys        []OrderBy
	DimensionFilter json.RawMessage
	MetricFilter    json.RawMessage
	MinuteRanges    []MinuteRange
}

// OrderBy 复刻上游排序子句。Desc 为 false 时整个 desc 键必须缺席（omitempty），
// 与旧实现发出的报文逐字节一致。
type OrderBy struct {
	Dimension *OrderByDimension `json:"dimension,omitempty"`
	Metric    *OrderByMetric    `json:"metric,omitempty"`
	Desc      bool              `json:"desc,omitempty"`
}

type OrderByDimension struct {
	DimensionName string `json:"dimensionName"`
}

type OrderByMetric struct {
	MetricName string `json:"metricName"`
}

// MinuteRange 只用于实时报表。
type MinuteRange struct {
	StartMinutesAgo int `json:"startMinutesAgo"`
	EndMinutesAgo   int `json:"endMinutesAgo"`
}

// Report 是归一化后的报表。行内字段既有维度取值（字符串）也有度量取值（数字或原始字符串）。
type Report struct {
	Rows     []map[string]any `json:"rows"`
	RowCount int              `json:"rowCount"`
	Metadata ReportMetadata   `json:"metadata"`
}

type ReportMetadata struct {
	Dimensions []string         `json:"dimensions"`
	Metrics    []MetricMetadata `json:"metrics"`
}

type MetricMetadata struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// ---- 归一化助手：以下四个函数都是旧实现的逐条复刻，改动等同改契约 ----

// shanghaiDateString 复刻 shanghaiDateString(daysAgo)：按 UTC+8 取日历日。
// 上海没有夏令时，因此固定偏移即可，与旧实现的时间换算逐日一致。
func shanghaiDateString(daysAgo int, now time.Time) string {
	return now.UTC().Add(8*time.Hour).AddDate(0, 0, -daysAgo).Format("2006-01-02")
}

// formatGA4Date 复刻 formatGa4Date：仅当取值恰好是 8 位数字时改写为 YYYY-MM-DD。
func formatGA4Date(value string) string {
	if len(value) != 8 {
		return value
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return value
		}
	}
	return value[0:4] + "-" + value[4:6] + "-" + value[6:8]
}

// coerceMetric 复刻 transformReportResponse 的 parseFloat 语义：
// 能解析成数字就用数字，否则保留原始字符串（例如 "(not set)"）。
// NaN / ±Inf 必须回退成字符串——json.Marshal 遇到非有限浮点数会直接报错。
func coerceMetric(value string) any {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return value
	}
	return parsed
}

// dateRange 复刻 runReport 的日期缺省：未显式给出时，
// 起点是 N 天前、终点是今天，均按上海日历日计算。
func dateRange(startDate, endDate string, defaultStartDaysAgo int, now time.Time) (string, string) {
	if startDate == "" {
		startDate = shanghaiDateString(defaultStartDaysAgo, now)
	}
	if endDate == "" {
		endDate = shanghaiDateString(0, now)
	}
	return startDate, endDate
}

// ---- 过滤器构造：与旧实现发出的 JSON 结构逐字段一致 ----

func filterEventNameEquals(value string) json.RawMessage {
	return mustJSON(map[string]any{
		"filter": map[string]any{
			"fieldName":    "eventName",
			"stringFilter": map[string]any{"value": value},
		},
	})
}

func filterEventNameInList(values []string) json.RawMessage {
	return mustJSON(map[string]any{
		"filter": map[string]any{
			"fieldName":    "eventName",
			"inListFilter": map[string]any{"values": values},
		},
	})
}

func filterFieldContains(fieldName, value string) json.RawMessage {
	return mustJSON(map[string]any{
		"filter": map[string]any{
			"fieldName": fieldName,
			"stringFilter": map[string]any{
				"matchType": "CONTAINS",
				"value":     value,
			},
		},
	})
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		// 上述结构全部是字面量，编码失败只可能来自编程错误。
		panic("adminanalyticsga4: unencodable filter: " + err.Error())
	}
	return encoded
}

// metricNumber 把行内取值归一成 float64。
//
// 生产路径上的取值只会是 float64 或 string（来自 JSON 反序列化），
// 但整数与 json.Number 也一并接受：静默返回 0 会把「类型意外」伪装成「指标为零」，
// 这类 bug 在汇总数字上极难发现。
func metricNumber(row map[string]any, field string) float64 {
	switch value := row[field].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return 0
		}
		return parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

func stringValue(row map[string]any, field string) string {
	switch value := row[field].(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case json.Number:
		return value.String()
	default:
		return ""
	}
}

// parallelErrors 并发执行任务，并把每个任务的错误按下标返回。
//
// 下标有序而不是按完成顺序 —— 并发下的确定性优于时序。
// 供 /summary 这类「允许部分失败」的聚合使用；不允许部分失败时请用 parallel。
func parallelErrors(tasks []func() error) []error {
	var wait sync.WaitGroup
	failures := make([]error, len(tasks))
	for index, task := range tasks {
		if task == nil {
			continue
		}
		wait.Add(1)
		go func(index int, task func() error) {
			defer wait.Done()
			failures[index] = task()
		}(index, task)
	}
	wait.Wait()
	return failures
}

// parallel 复刻 Promise.all 的「任一失败即失败」语义，
// 返回下标最小的错误。
func parallel(tasks ...func() error) error {
	for _, failure := range parallelErrors(tasks) {
		if failure != nil {
			return failure
		}
	}
	return nil
}

// jsNumber 复刻 JS 模板字符串里的 Number -> String 行为，
// 用于需要把数字拼进描述文本的洞察生成。
func jsNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// jsISOString 复刻 new Date().toISOString()：UTC、毫秒固定三位、以 Z 结尾。
func jsISOString(moment time.Time) string {
	return moment.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// orderedAccumulator 保留首次出现顺序，复刻 Object.values() 的迭代次序。
// 元素类型固定为指针：调用方拿到后直接改写字段，不需要回写。
type orderedAccumulator[T any] struct {
	keys   []string
	values map[string]*T
}

func newOrderedAccumulator[T any]() *orderedAccumulator[T] {
	return &orderedAccumulator[T]{values: map[string]*T{}}
}

// ensure 返回已有元素，或创建一个并记录其首次出现位置。
func (a *orderedAccumulator[T]) ensure(key string, create func() *T) *T {
	if existing, ok := a.values[key]; ok {
		return existing
	}
	created := create()
	a.values[key] = created
	a.keys = append(a.keys, key)
	return created
}

func (a *orderedAccumulator[T]) list() []*T {
	result := make([]*T, 0, len(a.keys))
	for _, key := range a.keys {
		result = append(result, a.values[key])
	}
	return result
}
