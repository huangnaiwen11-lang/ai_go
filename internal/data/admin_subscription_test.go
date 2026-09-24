package data

import (
	"context"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/adminsubscription"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
)

func TestAdminSubscriptionRepositoryProjectsLifecycleFactsOnly(t *testing.T) {
	client := newLocalMongoClient(t)
	db := client.Database("admin_subscription_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	ctx := context.Background()
	// newLocalMongoClient owns client cleanup; only drop this test database here.
	t.Cleanup(func() { _ = db.Drop(ctx) })
	now := time.Now().UTC()
	for _, doc := range []model.SubscriptionDocument{
		{UserID: "active", Status: "active", BillingPeriod: "monthly", StartsAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(48 * time.Hour), CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour)},
		{UserID: "cancelled", Status: "cancelled", BillingPeriod: "yearly", StartsAt: now.Add(-72 * time.Hour), ExpiresAt: now.Add(300 * time.Hour), CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-time.Hour)},
	} {
		if _, err := db.Collection(schema.CollectionSubscriptions).InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	repository := NewAdminSubscriptionRepository(&Data{client: client, database: db})
	overview, err := repository.SubscriptionOverview(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if overview.ActiveSubscribers != 1 || overview.NewSubscribers != 2 || overview.CancelledSubscribers != 1 {
		t.Fatalf("overview = %#v", overview)
	}
	breakdown, err := repository.SubscriptionBreakdown(ctx)
	if err != nil || len(breakdown.ByPeriod) != 2 || len(breakdown.RecentCancellations) != 1 {
		t.Fatalf("breakdown = %#v, err=%v", breakdown, err)
	}
	page, err := repository.SubscriptionSubscribers(ctx, adminsubscription.Query{Page: 1, PageSize: 10, Status: "active"})
	if err != nil || page.Total != 1 || len(page.Subscribers) != 1 {
		t.Fatalf("subscribers = %#v, err=%v", page, err)
	}
	if page.Subscribers[0].Tier != "unknown" || page.Subscribers[0].PaymentProvider != "unknown" {
		t.Fatalf("subscriber leaked unsupported facts: %#v", page.Subscribers[0])
	}
}
