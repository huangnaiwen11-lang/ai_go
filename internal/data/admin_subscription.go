package data

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"ai-business-service/internal/biz/adminsubscription"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// mongoAdminSubscriptionRepository only projects facts already owned by the
// Go service. Subscription price/provider/tier and credit grants are not in
// this schema, so those fields remain explicit unknown/zero values instead of
// being inferred from one-time payment orders.
type mongoAdminSubscriptionRepository struct{ data *Data }

func NewAdminSubscriptionRepository(data *Data) adminsubscription.Repository {
	return &mongoAdminSubscriptionRepository{data: data}
}

type subscriptionRow struct{ document model.SubscriptionDocument }

func (r *mongoAdminSubscriptionRepository) rows(ctx context.Context) ([]subscriptionRow, error) {
	if r == nil || r.data == nil || r.data.database == nil {
		return nil, fmt.Errorf("admin subscription repository unavailable")
	}
	cursor, err := r.data.database.Collection(schema.CollectionSubscriptions).Find(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	defer cursor.Close(ctx)
	var docs []model.SubscriptionDocument
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("decode subscriptions: %w", err)
	}
	rows := make([]subscriptionRow, 0, len(docs))
	for _, doc := range docs {
		rows = append(rows, subscriptionRow{document: doc})
	}
	return rows, nil
}

func activeSubscription(doc model.SubscriptionDocument, now time.Time) bool {
	return strings.EqualFold(doc.Status, "active") && doc.ExpiresAt.After(now)
}
func expiredSubscription(doc model.SubscriptionDocument, now time.Time) bool {
	return strings.EqualFold(doc.Status, "expired") || (strings.EqualFold(doc.Status, "active") && !doc.ExpiresAt.IsZero() && !doc.ExpiresAt.After(now))
}
func daysBetween(start, end time.Time) int64 {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return int64(end.Sub(start) / (24 * time.Hour))
}
func windowStart(days int) time.Time { return time.Now().UTC().AddDate(0, 0, -days) }

func (r *mongoAdminSubscriptionRepository) SubscriptionOverview(ctx context.Context, days int) (adminsubscription.Overview, error) {
	rows, err := r.rows(ctx)
	if err != nil {
		return adminsubscription.Overview{}, err
	}
	now, from := time.Now().UTC(), windowStart(days)
	var out adminsubscription.Overview
	var totalDays int64
	for _, row := range rows {
		doc := row.document
		if activeSubscription(doc, now) {
			out.ActiveSubscribers++
		}
		if !doc.CreatedAt.Before(from) {
			out.NewSubscribers++
		}
		if strings.EqualFold(doc.Status, "cancelled") && !doc.UpdatedAt.Before(from) {
			out.CancelledSubscribers++
		}
		if expiredSubscription(doc, now) && !doc.UpdatedAt.Before(from) {
			out.ExpiredSubscribers++
		}
		if activeSubscription(doc, now) {
			for i, limit := range []int{7, 30, 90} {
				until := now.AddDate(0, 0, limit)
				if !doc.ExpiresAt.After(now) || doc.ExpiresAt.After(until) {
					continue
				}
				switch i {
				case 0:
					out.Expiring.D7++
				case 1:
					out.Expiring.D30++
				case 2:
					out.Expiring.D90++
				}
			}
		}
		if n := daysBetween(doc.StartsAt, doc.ExpiresAt); n > 0 {
			totalDays += n
		}
	}
	if len(rows) > 0 {
		out.AvgSubscriptionDays = float64(totalDays) / float64(len(rows))
	}
	losses := out.CancelledSubscribers + out.ExpiredSubscribers
	if out.ActiveSubscribers+losses > 0 {
		out.ChurnRate = float64(losses) * 100 / float64(out.ActiveSubscribers+losses)
	}
	// No price or provider exists in the Go subscription projection. MRR/ARR
	// stay zero until a trusted subscription price snapshot is migrated.
	return out, nil
}

func (r *mongoAdminSubscriptionRepository) SubscriptionTrends(ctx context.Context, days int) ([]adminsubscription.Trend, error) {
	rows, err := r.rows(ctx)
	if err != nil {
		return nil, err
	}
	now, from := time.Now().UTC(), windowStart(days)
	buckets := make(map[string]adminsubscription.Trend, days)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	for day := from.In(loc).Truncate(24 * time.Hour); !day.After(now.In(loc)); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		buckets[key] = adminsubscription.Trend{Date: key}
	}
	for _, row := range rows {
		doc := row.document
		if !doc.CreatedAt.Before(from) {
			key := doc.CreatedAt.In(loc).Format("2006-01-02")
			v := buckets[key]
			v.NewSubs++
			buckets[key] = v
		}
		if strings.EqualFold(doc.Status, "cancelled") && !doc.UpdatedAt.Before(from) {
			key := doc.UpdatedAt.In(loc).Format("2006-01-02")
			v := buckets[key]
			v.Cancelled++
			buckets[key] = v
		}
		if expiredSubscription(doc, now) && !doc.UpdatedAt.Before(from) {
			key := doc.UpdatedAt.In(loc).Format("2006-01-02")
			v := buckets[key]
			v.Expired++
			buckets[key] = v
		}
	}
	result := make([]adminsubscription.Trend, 0, len(buckets))
	active := int64(0)
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := buckets[key]
		v.NetChange = v.NewSubs - v.Cancelled - v.Expired
		active += v.NetChange
		v.ActiveCumulative = active
		result = append(result, v)
	}
	return result, nil
}

func (r *mongoAdminSubscriptionRepository) SubscriptionBreakdown(ctx context.Context) (adminsubscription.Breakdown, error) {
	rows, err := r.rows(ctx)
	if err != nil {
		return adminsubscription.Breakdown{}, err
	}
	periods := map[string]int64{}
	cancellations := make([]adminsubscription.Cancellation, 0)
	for _, row := range rows {
		doc := row.document
		period := strings.TrimSpace(doc.BillingPeriod)
		if period == "" {
			period = "unknown"
		}
		periods[period]++
		if strings.EqualFold(doc.Status, "cancelled") {
			n := daysBetween(doc.StartsAt, doc.UpdatedAt)
			cancellations = append(cancellations, adminsubscription.Cancellation{UserID: doc.UserID, Tier: "unknown", BillingPeriod: period, PaymentProvider: "unknown", CancelledAt: doc.UpdatedAt.UTC().Format(time.RFC3339Nano), SubscribedDays: &n})
		}
	}
	periodRows := make([]adminsubscription.BreakdownRow, 0, len(periods))
	for key, count := range periods {
		periodRows = append(periodRows, adminsubscription.BreakdownRow{Key: key, Count: count})
	}
	sort.Slice(periodRows, func(i, j int) bool { return periodRows[i].Key < periodRows[j].Key })
	sort.Slice(cancellations, func(i, j int) bool { return cancellations[i].CancelledAt > cancellations[j].CancelledAt })
	if len(cancellations) > 20 {
		cancellations = cancellations[:20]
	}
	return adminsubscription.Breakdown{ByPeriod: periodRows, RecentCancellations: cancellations}, nil
}

func (r *mongoAdminSubscriptionRepository) SubscriptionSubscribers(ctx context.Context, query adminsubscription.Query) (adminsubscription.SubscribersPage, error) {
	rows, err := r.rows(ctx)
	if err != nil {
		return adminsubscription.SubscribersPage{}, err
	}
	now := time.Now().UTC()
	filtered := make([]subscriptionRow, 0, len(rows))
	for _, row := range rows {
		if query.Status == "" || query.Status == "all" || (query.Status == "active" && activeSubscription(row.document, now)) || (query.Status == "expired" && expiredSubscription(row.document, now)) || (query.Status == "cancelled" && strings.EqualFold(row.document.Status, "cancelled")) {
			filtered = append(filtered, row)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].document.CreatedAt.After(filtered[j].document.CreatedAt) })
	total := int64(len(filtered))
	start := (query.Page - 1) * query.PageSize
	if start >= len(filtered) {
		return adminsubscription.SubscribersPage{Total: total, Subscribers: []adminsubscription.Subscriber{}}, nil
	}
	end := start + query.PageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	result := make([]adminsubscription.Subscriber, 0, end-start)
	for _, row := range filtered[start:end] {
		result = append(result, projectSubscriber(row.document, now))
	}
	return adminsubscription.SubscribersPage{Subscribers: result, Total: total}, nil
}

func projectSubscriber(doc model.SubscriptionDocument, now time.Time) adminsubscription.Subscriber {
	status := doc.Status
	if strings.EqualFold(status, "active") && !doc.ExpiresAt.After(now) {
		status = "expired"
	}
	return adminsubscription.Subscriber{SubscriptionID: doc.UserID, UserID: doc.UserID, Channel: "unknown", Tier: "unknown", BillingPeriod: valueOrUnknown(doc.BillingPeriod), Status: status, PaymentProvider: "unknown", AutoRenew: false, StartDate: timePtrString(doc.StartsAt), EndDate: timePtrString(doc.ExpiresAt), CreatedAt: doc.CreatedAt.UTC().Format(time.RFC3339Nano), SubscribedDays: daysBetween(doc.StartsAt, now)}
}
func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}
func timePtrString(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	s := value.UTC().Format(time.RFC3339Nano)
	return &s
}

var _ adminsubscription.Repository = (*mongoAdminSubscriptionRepository)(nil)
