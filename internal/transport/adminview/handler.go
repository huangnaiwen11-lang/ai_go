// Package adminview exposes management projections owned by the Go service.
package adminview

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	bizadmin "ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}
type reader = bizadmin.Repository

type handler struct {
	authenticator authenticator
	reader        reader
}

func NewHandler(authenticator authenticator, reader reader) http.Handler {
	return &handler{authenticator: authenticator, reader: reader}
}

func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if identity.Role != "admin" && identity.Role != "super_admin" {
		writeFailure(writer, http.StatusForbidden, "FORBIDDEN", "Admin access required")
		return
	}
	if handler.management(writer, request, bizadmin.Actor{ID: identity.UserID, Role: identity.Role}) {
		return
	}
	if request.Method == http.MethodGet && handler.analytics(writer, request) {
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/api/admin/analytics/overview" {
		overview, err := handler.reader.Overview(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeSuccess(writer, overviewView(overview))
		return
	}
	writeFailure(writer, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This admin API has not been migrated to the Go Gateway")
}

func (h *handler) analytics(w http.ResponseWriter, r *http.Request) bool {
	metric := strings.TrimPrefix(r.URL.Path, "/api/admin/analytics/")
	switch metric {
	case "users", "images", "videos", "revenue-trends":
		days := 30
		if raw, ok := r.URL.Query()["days"]; ok {
			var err error
			if len(raw) != 1 {
				writeFailure(w, 400, "INVALID_REQUEST", "Invalid days")
				return true
			}
			days, err = strconv.Atoi(raw[0])
			if err != nil || days < 1 || days > 365 {
				writeFailure(w, 400, "INVALID_REQUEST", "days must be between 1 and 365")
				return true
			}
		}
		until := bizadmin.DayStart(time.Now()).AddDate(0, 0, 1)
		points, err := h.reader.Trends(r.Context(), metric, bizadmin.Window{From: until.AddDate(0, 0, -days), Until: until})
		if err != nil {
			writeError(w, err)
			return true
		}
		rows := make([]map[string]any, 0, len(points))
		var total int64
		for _, p := range points {
			row := map[string]any{"date": p.Date, "count": p.Count}
			if metric == "revenue-trends" {
				row["amount"] = float64(p.Cents) / 100
				total += p.Cents
			} else {
				total += p.Count
			}
			rows = append(rows, row)
		}
		var sum any = total
		if metric == "revenue-trends" {
			sum = float64(total) / 100
		}
		writeSuccess(w, map[string]any{"trends": rows, "total": sum, "timezone": "Asia/Shanghai", "currency": "USD"})
		return true
	case "cohort/revenue-breakdowns":
		from, e1 := time.ParseInLocation("2006-01-02", r.URL.Query().Get("from"), bizadmin.Location)
		to, e2 := time.ParseInLocation("2006-01-02", r.URL.Query().Get("to"), bizadmin.Location)
		if e1 != nil || e2 != nil || to.Before(from) || to.Sub(from) > 30*24*time.Hour {
			writeFailure(w, 400, "INVALID_REQUEST", "Expected date range of 1 to 31 days")
			return true
		}
		result, err := h.reader.RevenueBreakdowns(r.Context(), bizadmin.Window{From: from, Until: to.AddDate(0, 0, 1)})
		if err != nil {
			writeError(w, err)
			return true
		}
		views := func(buckets []bizadmin.RevenueBucket) map[string]any {
			rows := make([]map[string]any, 0, len(buckets))
			for _, b := range buckets {
				rows = append(rows, map[string]any{"key": b.Key, "label": b.Label, "totalRevenue": float64(b.Cents) / 100, "d0Revenue": float64(b.D0Cents) / 100})
			}
			return map[string]any{"rows": rows}
		}
		writeSuccess(w, map[string]any{"from": from.Format("2006-01-02"), "to": to.Format("2006-01-02"), "country": views(result.Country), "source": views(result.Source), "client": views(result.Client)})
		return true
	}
	return false
}

func (handler *handler) authenticate(request *http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if handler == nil || handler.authenticator == nil || handler.reader == nil {
		return nil, shared.ErrServiceUnavailable
	}
	identity, err := handler.authenticator.Authenticate(request)
	if err != nil {
		return nil, err
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		return nil, shared.ErrUnauthenticated
	}
	return identity, nil
}

func overviewView(value bizadmin.Overview) map[string]any {
	return map[string]any{
		"users":   map[string]any{"total": value.TotalUsers, "today": value.TodayUsers, "weekly": value.WeeklyUsers, "monthly": value.MonthlyUsers},
		"images":  map[string]any{"total": value.TotalImages, "today": value.TodayImages, "pending": value.PendingImages},
		"videos":  map[string]any{"total": value.TotalVideos, "today": value.TodayVideos, "pending": value.PendingVideos},
		"revenue": map[string]any{"total": float64(value.TotalRevenueCents) / 100, "today": float64(value.TodayRevenueCents) / 100},
	}
}
func writeSuccess(writer http.ResponseWriter, data any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
}
func writeFailure(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}
func writeError(writer http.ResponseWriter, err error) {
	for _, entry := range []struct {
		err           error
		status        int
		code, message string
	}{
		{bizadmin.ErrInvalid, 400, "INVALID_REQUEST", "Invalid request"},
		{bizadmin.ErrForbidden, 403, "FORBIDDEN", "Operation not permitted"},
		{bizadmin.ErrNotFound, 404, "NOT_FOUND", "Resource not found"},
		{bizadmin.ErrConflict, 409, "CONFLICT", "Conflicting operation"},
		{bizadmin.ErrInsufficientBalance, 409, "INSUFFICIENT_BALANCE", "Insufficient balance or account unavailable"},
	} {
		if errors.Is(err, entry.err) {
			writeFailure(writer, entry.status, entry.code, entry.message)
			return
		}
	}
	if errors.Is(err, shared.ErrUnauthenticated) {
		writeFailure(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
		return
	}
	writeFailure(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
}
