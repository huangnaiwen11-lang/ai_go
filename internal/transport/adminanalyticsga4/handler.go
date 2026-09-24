// Package adminanalyticsga4 owns the authenticated GA4 administration boundary.
//
// The legacy implementation used a service-account JSON credential and the
// Google Analytics Data API.  The Gateway must never turn a missing credential
// into a successful empty dashboard: status is inspectable, while reports
// fail explicitly with a precise reason instead of a fabricated empty payload
// or a generic upstream failure.
package adminanalyticsga4

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	bizga4 "ai-business-service/internal/biz/adminanalyticsga4"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/adminauth"
	"ai-business-service/internal/transport/sessionauth"
)

const prefix = "/api/admin/analytics/ga4"

// 请求体上限：POST /report 只接受维度、度量、日期与过滤器，
// 任何超出这个体积的载荷都是异常输入。
const maxCustomReportRequestBytes = 64 << 10

// 错误消息回显上限。上游错误体已经限长，这里再裁一次是为了让响应体保持可读。
const maxErrorMessageRunes = 300

// Config 只描述配置存在性。它从不返回或记录服务账号凭据 ——
// 凭据的读取与校验全部发生在 integrations/ga4，本层只看到「在不在」。
type Config struct {
	PropertyID        string
	CredentialPresent bool
}

func (c Config) configured() bool {
	return strings.TrimSpace(c.PropertyID) != "" && c.CredentialPresent
}

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

// Handler 覆盖全部已迁移的 GA4/PWA 报表路径。
//
// reporter 为 nil 时不再假装可用：配置齐但客户端缺位会得到 GA4_CLIENT_UNAVAILABLE，
// 而不是一个编造的报表。
type Handler struct {
	authenticator authenticator
	config        Config
	reporter      bizga4.Reporter
	operations    *bizga4.Operations
}

// NewHandler 装配 GA4 管理端点。reporter 由组合根注入，可以是 nil。
func NewHandler(authenticator authenticator, config Config, reporter bizga4.Reporter) http.Handler {
	return &Handler{
		authenticator: authenticator,
		config:        config,
		reporter:      reporter,
		operations:    bizga4.NewOperations(reporter),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.authenticator == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "GA4 admin handler unavailable")
		return
	}
	if !h.authorize(w, r) {
		return
	}

	switch r.URL.Path {
	case prefix + "/status":
		// 配置状态必须排在门禁之前：它存在的意义就是回答「配好了没」。
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		success(w, map[string]any{"configured": h.config.configured(), "propertyId": maskedPropertyID(h.config.PropertyID)})
		return
	case prefix + "/report":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if !h.available(w) {
			return
		}
		h.customReport(w, r)
		return
	}

	if !knownPath(r.URL.Path) {
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This GA4 API has not been migrated to the Go Gateway")
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !h.available(w) {
		return
	}
	h.report(w, r)
}

// available 判定「能不能真的去取数」，并给出精确的拒绝原因。
//
// 这是本包的失败语义核心：未配置与「配了但客户端缺位」是两件事，
// 运维动作不同，所以码也必须不同。
func (h *Handler) available(w http.ResponseWriter) bool {
	if !h.config.configured() {
		failure(w, http.StatusServiceUnavailable, "GA4_NOT_CONFIGURED", "GA4 is not configured. Set a service account credential and GA4_PROPERTY_ID")
		return false
	}
	if h.reporter == nil {
		failure(w, http.StatusServiceUnavailable, "GA4_CLIENT_UNAVAILABLE", "GA4 is configured but the Analytics Data API client is unavailable")
		return false
	}
	return true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) bool {
	identity, err := h.authenticator.Authenticate(r)
	if err != nil {
		adminauth.WriteDenial(w, err)
		return false
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		adminauth.WriteDenial(w, shared.ErrUnauthenticated)
		return false
	}
	if identity.Role != "admin" && identity.Role != "super_admin" {
		adminauth.WriteDenial(w, adminauth.ErrForbidden)
		return false
	}
	return true
}

func knownPath(path string) bool {
	switch path {
	case prefix + "/status", prefix + "/realtime", prefix + "/overview", prefix + "/traffic", prefix + "/pages", prefix + "/demographics", prefix + "/devices", prefix + "/events", prefix + "/events/daily", prefix + "/funnel", prefix + "/retention", prefix + "/agents", prefix + "/search", prefix + "/insights", prefix + "/summary", prefix + "/conversion-compare", prefix + "/report", prefix + "/pwa/installs", prefix + "/pwa/active", prefix + "/pwa/platforms", prefix + "/pwa/summary", prefix + "/pwa/paywall-sources":
		return true
	default:
		return false
	}
}

// ---- 端点分派 ----

func (h *Handler) report(w http.ResponseWriter, r *http.Request) {
	parameters := r.URL.Query()
	dateQuery := bizga4.DateRangeQuery{StartDate: parameters.Get("startDate"), EndDate: parameters.Get("endDate")}
	limitQuery := bizga4.LimitQuery{StartDate: dateQuery.StartDate, EndDate: dateQuery.EndDate, Limit: limitParameter(parameters)}

	switch r.URL.Path {
	case prefix + "/realtime":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Realtime(ctx) })
	case prefix + "/overview":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Overview(ctx, dateQuery) })
	case prefix + "/traffic":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Traffic(ctx, limitQuery) })
	case prefix + "/pages":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Pages(ctx, limitQuery) })
	case prefix + "/demographics":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Demographics(ctx, limitQuery) })
	case prefix + "/devices":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Devices(ctx, dateQuery) })
	case prefix + "/events":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Events(ctx, limitQuery) })
	case prefix + "/events/daily":
		h.respond(w, r, func(ctx context.Context) (any, error) {
			return h.operations.EventsDaily(ctx, bizga4.EventsDailyQuery{
				StartDate:  dateQuery.StartDate,
				EndDate:    dateQuery.EndDate,
				EventNames: eventNamesParameter(parameters.Get("events")),
			})
		})
	case prefix + "/funnel":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Funnel(ctx, dateQuery) })
	case prefix + "/retention":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Retention(ctx, dateQuery) })
	case prefix + "/agents":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.AgentPages(ctx, limitQuery) })
	case prefix + "/search":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.SearchTerms(ctx, limitQuery) })
	case prefix + "/insights":
		h.respond(w, r, func(ctx context.Context) (any, error) {
			insights, err := h.operations.Insights(ctx, dateQuery)
			if err != nil {
				return nil, err
			}
			return map[string]any{"insights": insights}, nil
		})
	case prefix + "/summary":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.Summary(ctx, dateQuery) })
	case prefix + "/conversion-compare":
		h.respond(w, r, func(ctx context.Context) (any, error) {
			return h.operations.ConversionCompare(ctx, bizga4.ConversionQuery{
				Days:                intParameter(parameters.Get("days")),
				EndDate:             parameters.Get("endDate"),
				ConversionEventName: strings.TrimSpace(parameters.Get("conversionEventName")),
				IncludeBySource:     parameters.Get("includeBySource") != "false",
				Limit:               intParameter(parameters.Get("limit")),
			})
		})
	case prefix + "/pwa/installs":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.PwaInstalls(ctx, dateQuery) })
	case prefix + "/pwa/active":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.PwaActive(ctx, dateQuery) })
	case prefix + "/pwa/platforms":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.PwaPlatforms(ctx, dateQuery) })
	case prefix + "/pwa/summary":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.PwaSummary(ctx, dateQuery) })
	case prefix + "/pwa/paywall-sources":
		h.respond(w, r, func(ctx context.Context) (any, error) { return h.operations.PwaPaywallSources(ctx, dateQuery) })
	default:
		// ServeHTTP 已经把已知路径收窄到这里，兜底只可能是分派表漏了一项。
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This GA4 API has not been migrated to the Go Gateway")
	}
}

// customReport 对应 POST /report：维度与度量由调用方给出，过滤器原样透传。
func (h *Handler) customReport(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxCustomReportRequestBytes))
	var payload struct {
		Dimensions      []string        `json:"dimensions"`
		Metrics         []string        `json:"metrics"`
		StartDate       string          `json:"startDate"`
		EndDate         string          `json:"endDate"`
		Limit           int             `json:"limit"`
		DimensionFilter json.RawMessage `json:"dimensionFilter"`
		MetricFilter    json.RawMessage `json:"metricFilter"`
	}
	if err := decoder.Decode(&payload); err != nil {
		failure(w, http.StatusBadRequest, "GA4_INVALID_QUERY", "Request body must be JSON with dimensions and metrics")
		return
	}
	h.respond(w, r, func(ctx context.Context) (any, error) {
		return h.operations.CustomReport(ctx, bizga4.CustomReportQuery{
			Dimensions:      payload.Dimensions,
			Metrics:         payload.Metrics,
			StartDate:       payload.StartDate,
			EndDate:         payload.EndDate,
			Limit:           payload.Limit,
			DimensionFilter: payload.DimensionFilter,
			MetricFilter:    payload.MetricFilter,
		})
	})
}

// respond 把业务结果或业务错误统一渲染成响应。
func (h *Handler) respond(w http.ResponseWriter, r *http.Request, produce func(context.Context) (any, error)) {
	value, err := produce(r.Context())
	if err != nil {
		writeFailure(w, err)
		return
	}
	success(w, value)
}

// writeFailure 是 GA4 的失败语义映射表。
//
// 注意：这里绝不返回 502。本项目的 502 有唯一含义 ——「请求回退给了 Node 而 Node 不在」，
// 而 /api/admin/ 是 fail-closed、永不回退。所有 GA4 失败都是 503 家族，
// 唯一的 4xx 是「你的请求本身不合法」。
func writeFailure(w http.ResponseWriter, err error) {
	var upstream *bizga4.UpstreamError
	switch {
	case errors.Is(err, bizga4.ErrNotConfigured):
		failure(w, http.StatusServiceUnavailable, "GA4_NOT_CONFIGURED", "GA4 is not configured. Set a service account credential and GA4_PROPERTY_ID")
	case errors.Is(err, bizga4.ErrCredentialRejected):
		failure(w, http.StatusServiceUnavailable, "GA4_CREDENTIAL_REJECTED", clipMessage(err))
	case errors.As(err, &upstream):
		// 自定义报表的维度度量由调用方给出，上游 400 属于调用方错误而不是网关故障。
		if upstream.Status == http.StatusBadRequest {
			failure(w, http.StatusBadRequest, "GA4_REPORT_INVALID", clipMessage(err))
			return
		}
		failure(w, http.StatusServiceUnavailable, "GA4_UPSTREAM_ERROR", clipMessage(err))
	case errors.Is(err, bizga4.ErrInvalidQuery):
		failure(w, http.StatusBadRequest, "GA4_INVALID_QUERY", clipMessage(err))
	default:
		failure(w, http.StatusServiceUnavailable, "GA4_UPSTREAM_ERROR", "GA4 report could not be produced")
	}
}

// ---- 参数解析 ----

// limitParameter 只在调用方显式给了正整数时才透传。
//
// 缺省值与非法值一律返回 0，由 biz 层套用它自己的默认值 ——
// 这样「每个端点的默认行数」只有一个事实源。
// 旧实现把负数原样透传给上游并换回一个 400，这里收敛成默认值。
func limitParameter(parameters url.Values) int {
	value := intParameter(parameters.Get("limit"))
	if value <= 0 {
		return 0
	}
	return value
}

func intParameter(raw string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return parsed
}

// eventNamesParameter 复刻旧路由的 `events.split(',').map(trim).filter(Boolean).slice(0,30)`。
func eventNamesParameter(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	names := make([]string, 0, 8)
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		names = append(names, trimmed)
		if len(names) == 30 {
			break
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

func clipMessage(err error) string {
	if err == nil {
		return ""
	}
	runes := []rune(err.Error())
	if len(runes) <= maxErrorMessageRunes {
		return string(runes)
	}
	return string(runes[:maxErrorMessageRunes]) + "…"
}

func maskedPropertyID(propertyID string) any {
	propertyID = strings.TrimSpace(propertyID)
	if propertyID == "" {
		return nil
	}
	if len(propertyID) <= 4 {
		return "***" + propertyID
	}
	return "***" + propertyID[len(propertyID)-4:]
}

func success(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func failure(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	failure(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
}
