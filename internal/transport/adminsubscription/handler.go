// Package adminsubscription 把 Go 自有订阅事实的只读投影适配成 HTTP。
// 授权由调用方注入，必须与 /api/admin/ 其余投影使用同一 admin/super_admin 门禁。
// 该投影不提供任何订阅、权益、支付、退款或发放的写入能力。
package adminsubscription

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	biz "ai-business-service/internal/biz/adminsubscription"
	"ai-business-service/internal/transport/adminauth"
)

type authorizer func(*http.Request) error

// Handler 暴露 /api/admin/analytics/subscription/ 下的四个只读端点。
type Handler struct {
	repository biz.Repository
	authorize  authorizer
}

func NewHandler(repository biz.Repository, authorize func(*http.Request) error) http.Handler {
	return &Handler{repository: repository, authorize: authorize}
}

const prefix = "/api/admin/analytics/subscription/"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.repository == nil || h.authorize == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Subscription admin handler unavailable")
		return
	}
	if err := h.authorize(r); err != nil {
		adminauth.WriteDenial(w, err)
		return
	}
	if r.Method != http.MethodGet {
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This subscription API is read-only in the Go Gateway")
		return
	}
	operations := biz.NewOperations(h.repository)
	switch strings.TrimPrefix(r.URL.Path, prefix) {
	case "overview":
		days, ok := parseDays(w, r)
		if !ok {
			return
		}
		value, err := operations.Overview(r.Context(), days)
		if err != nil {
			writeError(w, err)
			return
		}
		success(w, overviewView(value))
	case "trends":
		days, ok := parseDays(w, r)
		if !ok {
			return
		}
		rows, err := operations.Trends(r.Context(), days)
		if err != nil {
			writeError(w, err)
			return
		}
		views := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			views = append(views, map[string]any{
				"date":             row.Date,
				"newSubs":          row.NewSubs,
				"cancelled":        row.Cancelled,
				"expired":          row.Expired,
				"netChange":        row.NetChange,
				"activeCumulative": row.ActiveCumulative,
			})
		}
		success(w, views)
	case "breakdown":
		value, err := operations.Breakdown(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		success(w, breakdownView(value))
	case "subscribers":
		query, ok := parseSubscribers(w, r)
		if !ok {
			return
		}
		page, err := operations.Subscribers(r.Context(), query)
		if err != nil {
			writeError(w, err)
			return
		}
		rows := make([]map[string]any, 0, len(page.Subscribers))
		for _, subscriber := range page.Subscribers {
			rows = append(rows, subscriberView(subscriber))
		}
		success(w, map[string]any{"subscribers": rows, "total": page.Total, "page": query.Page, "pageSize": query.PageSize})
	default:
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This subscription API has not been migrated to the Go Gateway")
	}
}

func parseDays(w http.ResponseWriter, r *http.Request) (int, bool) {
	days := 30
	raw := r.URL.Query().Get("days")
	if raw == "" {
		return days, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 365 {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "days must be between 1 and 365")
		return 0, false
	}
	return value, true
}

func parseSubscribers(w http.ResponseWriter, r *http.Request) (biz.Query, bool) {
	values := r.URL.Query()
	query := biz.Query{Page: 1, PageSize: 20, Status: values.Get("status")}
	if raw := values.Get("page"); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid page")
			return query, false
		}
		query.Page = page
	}
	if raw := values.Get("pageSize"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < 1 || size > 100 {
			failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid pageSize")
			return query, false
		}
		query.PageSize = size
	}
	return query, true
}

func overviewView(value biz.Overview) map[string]any {
	forecast := make([]map[string]any, 0, len(value.RenewalForecast))
	for _, row := range value.RenewalForecast {
		forecast = append(forecast, map[string]any{"month": row.Month, "count": row.Count, "revenue": row.Revenue, "revenueNet": row.RevenueNet})
	}
	return map[string]any{
		"activeSubscribers":    value.ActiveSubscribers,
		"newSubscribers":       value.NewSubscribers,
		"cancelledSubscribers": value.CancelledSubscribers,
		"expiredSubscribers":   value.ExpiredSubscribers,
		"churnRate":            value.ChurnRate,
		"mrr":                  value.MRR,
		"arr":                  value.ARR,
		"avgSubscriptionDays":  value.AvgSubscriptionDays,
		"expiring":             map[string]any{"d7": value.Expiring.D7, "d30": value.Expiring.D30, "d90": value.Expiring.D90},
		"renewalForecast":      forecast,
	}
}

// breakdownView 只回填 Go 订阅事实确实存在的维度：计费周期与近期取消。
// 套餐层级、支付渠道、国家、来源在 Go 自有 schema 中不存在，保持空数组，
// 不用一次性支付订单反推订阅语义。
func breakdownView(value biz.Breakdown) map[string]any {
	periods := make([]map[string]any, 0, len(value.ByPeriod))
	for _, row := range value.ByPeriod {
		periods = append(periods, map[string]any{"period": row.Key, "count": row.Count, "revenue": row.Revenue, "revenueNet": row.RevenueNet})
	}
	cancellations := make([]map[string]any, 0, len(value.RecentCancellations))
	for _, row := range value.RecentCancellations {
		cancellations = append(cancellations, map[string]any{
			"userId":          row.UserID,
			"username":        row.Username,
			"country":         row.Country,
			"tier":            row.Tier,
			"billingPeriod":   row.BillingPeriod,
			"paymentProvider": row.PaymentProvider,
			"cancelledAt":     row.CancelledAt,
			"subscribedDays":  row.SubscribedDays,
		})
	}
	return map[string]any{
		"byTier":              breakdownRows(value.ByTier, "tier"),
		"byProvider":          breakdownRows(value.ByProvider, "provider"),
		"byPeriod":            periods,
		"byCountry":           countryRows(value.ByCountry),
		"byChannel":           breakdownRows(value.ByChannel, "channel"),
		"recentCancellations": cancellations,
	}
}

func breakdownRows(rows []biz.BreakdownRow, key string) []map[string]any {
	views := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		views = append(views, map[string]any{key: row.Key, "count": row.Count, "revenue": row.Revenue, "revenueNet": row.RevenueNet})
	}
	return views
}

func countryRows(rows []biz.CountryRow) []map[string]any {
	views := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		views = append(views, map[string]any{"country": row.Country, "countryName": row.CountryName, "count": row.Count, "revenue": row.Revenue, "revenueNet": row.RevenueNet})
	}
	return views
}

func subscriberView(value biz.Subscriber) map[string]any {
	return map[string]any{
		"subscriptionId":        value.SubscriptionID,
		"userId":                value.UserID,
		"username":              value.Username,
		"email":                 value.Email,
		"country":               value.Country,
		"countryName":           value.CountryName,
		"channel":               value.Channel,
		"authProvider":          value.AuthProvider,
		"userRegisteredAt":      value.UserRegisteredAt,
		"lastSeenAt":            value.LastSeenAt,
		"tier":                  value.Tier,
		"billingPeriod":         value.BillingPeriod,
		"status":                value.Status,
		"paymentProvider":       value.PaymentProvider,
		"pricePaid":             value.PricePaid,
		"autoRenew":             value.AutoRenew,
		"startDate":             value.StartDate,
		"endDate":               value.EndDate,
		"cancelledAt":           value.CancelledAt,
		"createdAt":             value.CreatedAt,
		"subscribedDays":        value.SubscribedDays,
		"diamondClaimDays":      value.DiamondClaimDays,
		"walletBalance":         value.WalletBalance,
		"imageCreditsRemaining": value.ImageCreditsRemaining,
		"videoCreditsRemaining": value.VideoCreditsRemaining,
		"imageCreditsGranted":   value.ImageCreditsGranted,
		"videoCreditsGranted":   value.VideoCreditsGranted,
	}
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

func writeError(w http.ResponseWriter, err error) {
	if errors.Is(err, biz.ErrDependenciesUnavailable) {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
		return
	}
	if err != nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid subscription request")
		return
	}
	failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
}
