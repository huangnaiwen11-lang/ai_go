package data

import (
	"context"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/adminapps"
	"ai-business-service/internal/biz/adminutm"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestAdminAppsRepositoryListsAndReadsLegacyAppDocuments(t *testing.T) {
	client := newLocalMongoClient(t)
	db := client.Database("admin_apps_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	t.Cleanup(func() { _ = db.Drop(context.Background()) })
	now := time.Now().UTC()
	_, err := db.Collection("apps").InsertOne(context.Background(), bson.M{
		"_id": "legacy-app", "name": "Legacy App", "platform": "android", "status": "active",
		"clientId": "com.example.legacy", "domain": "legacy.example.test", "createdAt": now,
		"updatedAt": now, "stats": bson.M{"totalUsers": 3, "totalMessages": 4, "totalRevenue": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := NewAdminAppsRepository(&Data{client: client, database: db})
	page, err := repository.List(context.Background(), adminapps.Query{Page: 1, Limit: 20, Search: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Apps) != 1 || page.Apps[0].ID != "legacy-app" {
		t.Fatalf("page=%+v", page)
	}
	app, err := repository.Get(context.Background(), "legacy-app")
	if err != nil || app.Name != "Legacy App" || app.ResolvedAPIURL != "https://legacy.example.test" {
		t.Fatalf("app=%+v err=%v", app, err)
	}
	overview, err := repository.Overview(context.Background())
	if err != nil || overview.Total != 1 || overview.Active != 1 || overview.ByPlatform["android"] != 1 {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}
}

func TestAdminUTMRepositoryListsLinksAndAggregatesSources(t *testing.T) {
	client := newLocalMongoClient(t)
	db := client.Database("admin_utm_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	t.Cleanup(func() { _ = db.Drop(context.Background()) })
	for _, row := range []bson.M{
		{"_id": "utm-a", "slug": "campaign-a", "label": "A", "targetPath": "/create", "utmSource": "facebook", "utmMedium": "paid", "enabled": true, "clicks": 7, "createdAt": time.Now().UTC()},
		{"_id": "utm-b", "slug": "campaign-b", "label": "B", "targetPath": "/create", "utmSource": "facebook", "utmMedium": "paid", "enabled": false, "clicks": 2, "createdAt": time.Now().UTC().Add(-time.Hour)},
	} {
		if _, err := db.Collection("utmlinks").InsertOne(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	repository := NewAdminUTMRepository(&Data{client: client, database: db})
	page, err := repository.List(context.Background(), adminutm.Query{Keyword: "campaign", Limit: 100})
	if err != nil || page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	sources, err := repository.Sources(context.Background(), true)
	if err != nil || len(sources) != 1 || sources[0].UtmSource != "facebook" || sources[0].TotalClicks != 7 || len(sources[0].Links) != 1 {
		t.Fatalf("sources=%+v err=%v", sources, err)
	}
}
