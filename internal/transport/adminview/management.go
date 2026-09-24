package adminview

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	a "ai-business-service/internal/biz/adminview"
)

func parseQuery(r *http.Request) (a.Query, error) {
	values := r.URL.Query()
	for _, v := range values {
		if len(v) != 1 || len(v[0]) > 512 {
			return a.Query{}, a.ErrInvalid
		}
	}
	q := a.Query{Page: 1, Limit: 20, Status: values.Get("status"), Role: values.Get("role"), Search: values.Get("search"), ExactEmail: strings.ToLower(strings.TrimSpace(values.Get("exactEmail"))), LoginType: values.Get("loginType"), Platform: values.Get("platform"), Client: values.Get("client"), Source: values.Get("sourceChannel"), UserID: values.Get("userId"), Email: values.Get("email"), Type: values.Get("type"), Provider: values.Get("provider"), Country: values.Get("country")}
	if q.Source == "" {
		q.Source = values.Get("channel")
	}
	for key, target := range map[string]*int{"page": &q.Page, "limit": &q.Limit} {
		if raw := values.Get(key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return q, a.ErrInvalid
			}
			*target = n
		}
	}
	if q.Page < 1 || q.Page > 10000 || q.Limit < 1 || q.Limit > 100 || (q.Page-1)*q.Limit > 100000 {
		return q, a.ErrInvalid
	}
	window := func(from, to string) (a.Window, error) {
		w := a.Window{}
		for key, target := range map[string]*time.Time{from: &w.From, to: &w.Until} {
			if raw := values.Get(key); raw != "" {
				t, err := time.Parse(time.RFC3339Nano, raw)
				if err != nil {
					return w, a.ErrInvalid
				}
				*target = t
			}
		}
		if !w.Until.IsZero() {
			w.Until = w.Until.Add(time.Millisecond)
		}
		if !w.From.IsZero() && !w.Until.IsZero() && !w.From.Before(w.Until) {
			return w, a.ErrInvalid
		}
		return w, nil
	}
	var err error
	from, to := "createdFrom", "createdTo"
	if strings.HasSuffix(r.URL.Path, "/kpis") || strings.HasSuffix(r.URL.Path, "/active-count") {
		from, to = "from", "to"
	}
	q.Created, err = window(from, to)
	if err != nil {
		return q, err
	}
	q.Seen, err = window("lastSeenFrom", "lastSeenTo")
	return q, err
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return a.ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return a.ErrInvalid
	}
	return nil
}
func pagination(q a.Query, total int64) map[string]any {
	return map[string]any{"page": q.Page, "limit": q.Limit, "total": total, "totalPages": (total + int64(q.Limit) - 1) / int64(q.Limit)}
}
func userView(u a.User) map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "displayName": u.Name, "status": u.Status, "role": u.Role, "platform": u.Platform, "isGuest": u.Binding == "guest", "createdAt": u.CreatedAt, "updatedAt": u.UpdatedAt}
}
func ledgerView(entries []a.LedgerEntry) []map[string]any {
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		kind := "credit"
		if e.Delta < 0 {
			kind = "debit"
		}
		rows = append(rows, map[string]any{"id": e.ID, "userId": e.UserID, "userName": e.UserName, "userEmail": e.UserEmail, "type": kind, "delta": e.Delta, "balanceAfter": e.BalanceAfter, "reason": e.Reason, "refId": e.RefID, "createdAt": e.CreatedAt})
	}
	return rows
}
func (h *handler) management(w http.ResponseWriter, r *http.Request, actor a.Actor) bool {
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	ctx := r.Context()
	ops := a.NewOperations(h.reader)
	respond := func(v any, err error) {
		if err != nil {
			writeError(w, err)
		} else {
			writeSuccess(w, v)
		}
	}
	if r.Method == http.MethodGet {
		if path == "wallet/purchase-countries" {
			countries, err := h.reader.PurchaseCountries(ctx)
			rows := make([]map[string]any, 0, len(countries))
			for _, country := range countries {
				rows = append(rows, map[string]any{"country": country})
			}
			respond(map[string]any{"countries": rows}, err)
			return true
		}
		switch path {
		case "users", "users/kpis", "users/active-count", "wallet/ledger", "wallet/purchases", "wallet/stats", "reports/feedback", "reports/feedback/stats":
			q, err := parseQuery(r)
			if err != nil {
				writeError(w, err)
				return true
			}
			switch path {
			case "users":
				page, err := h.reader.Users(ctx, q)
				rows := make([]map[string]any, 0, len(page.Users))
				for _, u := range page.Users {
					rows = append(rows, userView(u))
				}
				respond(map[string]any{"users": rows, "pagination": pagination(q, page.Total)}, err)
			case "users/kpis", "users/active-count":
				k, err := h.reader.UserKPIs(ctx, q)
				v := map[string]any{"totalUsers": k.Total, "registeredUsers": k.Registered, "activeUsers": k.Active, "pwaActiveUsers": k.PWAActive}
				if path == "users/active-count" {
					v = map[string]any{"total": k.Active, "pwa": k.PWAActive}
				}
				respond(v, err)
			case "wallet/ledger":
				p, err := h.reader.Ledger(ctx, q)
				respond(map[string]any{"entries": ledgerView(p.Entries), "pagination": pagination(q, p.Total)}, err)
			case "wallet/purchases":
				p, err := h.reader.Purchases(ctx, q)
				rows := make([]map[string]any, 0, len(p.Purchases))
				for _, v := range p.Purchases {
					rows = append(rows, map[string]any{"id": v.ID, "userId": v.UserID, "userName": v.UserName, "userEmail": v.UserEmail, "provider": v.Provider, "providerTxnId": v.ProviderTxnID, "productId": v.ProductID, "coins": v.Coins, "priceInCents": v.Cents, "currency": v.Currency, "status": v.Status, "createdAt": v.CreatedAt})
				}
				respond(map[string]any{"purchases": rows, "pagination": pagination(q, p.Total)}, err)
			case "wallet/stats":
				v, err := h.reader.WalletStats(ctx, q)
				providers := map[string]any{}
				for key, p := range v.ByProvider {
					providers[key] = map[string]any{"count": p.Count, "total": p.Cents}
				}
				respond(map[string]any{"totalRevenue": v.TotalRevenue, "todayRevenue": v.TodayRevenue, "totalCoins": v.TotalCoins, "todayCoins": v.TodayCoins, "totalSpent": v.TotalSpent, "todaySpent": v.TodaySpent, "totalGranted": v.TotalGranted, "byProvider": providers, "currency": "USD"}, err)
			case "reports/feedback":
				p, err := h.reader.Feedbacks(ctx, q)
				rows := make([]map[string]any, 0, len(p.Feedbacks))
				for _, f := range p.Feedbacks {
					var u any
					if f.User != nil {
						u = userView(*f.User)
					}
					attachments := make([]map[string]any, 0, len(f.Attachments))
					for _, att := range f.Attachments {
						attachments = append(attachments, map[string]any{"url": att.URL, "filename": att.ID})
					}
					rows = append(rows, map[string]any{"id": f.ID, "type": f.Type, "message": f.Message, "email": f.Email, "status": f.Status, "user": u, "attachments": attachments, "createdAt": f.CreatedAt, "reviewedAt": f.ReviewedAt, "reviewNote": f.Note, "response": f.Response})
				}
				respond(map[string]any{"feedbacks": rows, "pagination": pagination(q, p.Total)}, err)
			case "reports/feedback/stats":
				s, err := h.reader.FeedbackStats(ctx)
				respond(map[string]any{"total": s.Total, "pending": s.Pending, "resolved": s.Resolved, "byType": s.ByType}, err)
			}
			return true
		}
		if strings.HasPrefix(path, "wallet/user/") {
			id := strings.TrimPrefix(path, "wallet/user/")
			v, err := h.reader.Wallet(ctx, id)
			respond(map[string]any{"user": userView(v.User), "wallet": map[string]any{"balance": v.Balance, "totalEarned": v.TotalEarned, "totalSpent": v.TotalSpent}, "recentTransactions": ledgerView(v.Entries)}, err)
			return true
		}
	}
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && parts[0] == "users" && !reservedUserSubresource(parts[1]) {
		id := parts[1]
		if len(parts) == 2 && r.Method == http.MethodGet {
			u, err := h.reader.User(ctx, id)
			respond(map[string]any{"user": userView(u)}, err)
			return true
		}
		in := a.UserChange{UserID: id}
		if len(parts) == 2 && r.Method == http.MethodDelete {
			in.Status = "deleted"
		} else if len(parts) == 3 && r.Method == http.MethodPatch && (parts[2] == "status" || parts[2] == "role") {
			if parts[2] == "status" {
				var body struct {
					Status string `json:"status"`
				}
				if err := decodeBody(w, r, &body); err != nil {
					writeError(w, err)
					return true
				}
				in.Status = body.Status
			} else {
				var body struct {
					Role string `json:"role"`
				}
				if err := decodeBody(w, r, &body); err != nil {
					writeError(w, err)
					return true
				}
				in.Role = body.Role
			}
		} else {
			return false
		}
		u, err := ops.ChangeUser(ctx, actor, in)
		respond(map[string]any{"user": userView(u)}, err)
		return true
	}
	if path == "wallet/adjust" && r.Method == http.MethodPost {
		var body struct {
			UserID string `json:"userId"`
			Delta  int64  `json:"delta"`
			Reason string `json:"reason"`
		}
		if err := decodeBody(w, r, &body); err != nil {
			writeError(w, err)
			return true
		}
		v, err := ops.Adjust(ctx, actor, a.Adjustment{UserID: body.UserID, Delta: body.Delta, Reason: body.Reason, Key: r.Header.Get("Idempotency-Key")})
		respond(map[string]any{"success": true, "newBalance": v.Balance, "userName": v.UserName}, err)
		return true
	}
	if path == "wallet/grant-subscription" && r.Method == http.MethodPost {
		var body struct {
			UserID string `json:"userId"`
			Tier   string `json:"tier"`
			Days   int    `json:"days"`
			Reason string `json:"reason"`
		}
		if err := decodeBody(w, r, &body); err != nil {
			writeError(w, err)
			return true
		}
		v, err := ops.GrantSubscription(ctx, actor, a.SubscriptionGrant{UserID: body.UserID, Tier: body.Tier, Days: body.Days, Reason: body.Reason})
		if err != nil {
			writeError(w, err)
			return true
		}
		writeSuccess(w, map[string]any{"success": true, "action": v.Action, "tier": v.Tier, "endDate": v.EndDate, "userName": v.UserName})
		return true
	}
	if len(parts) == 4 && parts[0] == "reports" && parts[1] == "feedback" && parts[3] == "respond" && r.Method == http.MethodPost {
		var body struct {
			Response string `json:"response"`
			Note     string `json:"note"`
		}
		if err := decodeBody(w, r, &body); err != nil {
			writeError(w, err)
			return true
		}
		err := ops.Respond(ctx, actor, a.Reply{ID: parts[2], Response: body.Response, Note: body.Note})
		respond(map[string]any{"success": true}, err)
		return true
	}
	return false
}

// reservedUserSubresources 标记 `/admin/users/<name>` 里属于「子资源」而不是用户 ID 的段。
// 这些端点尚未迁移，必须继续走 501；若被当成用户 ID 解析，会变成 404「用户不存在」，
// 把「没实现」伪装成「资源缺失」，前端与排障都会得出错误结论。
var reservedUserSubresources = map[string]struct{}{
	"duplicates": {},
	"overview":   {},
}

func reservedUserSubresource(segment string) bool {
	_, reserved := reservedUserSubresources[segment]
	return reserved
}
