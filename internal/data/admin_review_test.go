package data

import (
	"context"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/adminreview"
	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoAdminReviewImageOverviewUsesOnlyProjectionFacts(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("admin_review_overview_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	t.Cleanup(func() { _ = database.Drop(context.Background()) })
	ctx := context.Background()
	now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
	todayStart := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	if _, err := database.Collection(schema.CollectionAdminReviewProjectionState).InsertOne(ctx, bson.M{"_id": adminReviewProjectionStateID, "ready": true}); err != nil {
		t.Fatal(err)
	}
	rows := []bson.M{
		{"_id": "portrait-1", "media_type": "image", "source": "legacy.generatedimages", "generation_status": "succeeded", "template_title": "Portrait", "created_at": now.Add(-30 * time.Minute)},
		{"_id": "portrait-2", "media_type": "image", "source": "legacy.generatedimages", "generation_status": "succeeded", "template_title": "Portrait", "created_at": now.Add(-40 * time.Minute)},
		{"_id": "freeform", "media_type": "image", "source": "legacy.generatedimages", "generation_status": "succeeded", "template_title": "", "created_at": now.Add(-50 * time.Minute)},
		{"_id": "untitled", "media_type": "image", "source": "legacy.generatedimages", "generation_status": "succeeded", "created_at": now.Add(-55 * time.Minute)},
		{"_id": "faceswap", "media_type": "image", "source": "legacy.faceswaptasks", "generation_status": "succeeded", "template_title": "Swap", "created_at": now.Add(-time.Hour)},
		{"_id": "generating", "media_type": "image", "source": "legacy.generatedimages", "generation_status": "generating", "created_at": now.Add(-time.Hour)},
		{"_id": "old-generating", "media_type": "image", "source": "legacy.generatedimages", "generation_status": "generating", "created_at": now.Add(-3 * time.Hour)},
	}
	if _, err := database.Collection(schema.CollectionAdminReviewItems).InsertMany(ctx, rows); err != nil {
		t.Fatal(err)
	}
	repository, ok := NewAdminReviewRepository(&Data{client: client, database: database}).(adminreview.ImageOverviewRepository)
	if !ok {
		t.Fatal("admin review repository must expose the image overview projection")
	}
	templates, err := repository.ImageTemplateOptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if templates.WithTemplate != 3 || templates.WithoutTemplate != 2 || len(templates.Options) != 2 || templates.Options[0] != (adminreview.ImageTemplateOption{Value: "Portrait", Count: 2}) || templates.Options[1] != (adminreview.ImageTemplateOption{Value: "Swap", Count: 1}) {
		t.Fatalf("template options = %#v", templates)
	}
	runtime, err := repository.ImageRuntime(ctx, adminreview.ImageRuntimeQuery{GeneratingSince: now.Add(-2 * time.Hour), TodayStart: todayStart})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Generating != 1 || runtime.ExternalToday != 1 {
		t.Fatalf("runtime = %#v", runtime)
	}
}

func TestMongoAdminReviewVideoOverviewUsesOnlyProjectionFacts(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("admin_review_video_overview_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	t.Cleanup(func() { _ = database.Drop(context.Background()) })
	ctx := context.Background()
	now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
	todayStart := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	if _, err := database.Collection(schema.CollectionAdminReviewProjectionState).InsertOne(ctx, bson.M{"_id": adminReviewProjectionStateID, "ready": true}); err != nil {
		t.Fatal(err)
	}
	rows := []bson.M{
		{"_id": "motion-1", "media_type": "video", "source": "legacy.generatedvideos", "generation_status": "succeeded", "template_title": "Motion", "created_at": now.Add(-30 * time.Minute)},
		{"_id": "motion-2", "media_type": "video", "source": "legacy.generatedvideos", "generation_status": "succeeded", "template_title": "Motion", "created_at": now.Add(-40 * time.Minute)},
		{"_id": "freeform", "media_type": "video", "source": "legacy.generatedvideos", "generation_status": "succeeded", "template_title": "", "created_at": now.Add(-50 * time.Minute)},
		{"_id": "animate", "media_type": "video", "source": "legacy.animates", "generation_status": "succeeded", "template_title": "Animate", "created_at": now.Add(-time.Hour)},
		{"_id": "faceswap", "media_type": "video", "source": "legacy.faceswaptasks", "generation_status": "succeeded", "template_title": "User supplied name", "created_at": now.Add(-time.Hour)},
		{"_id": "generating", "media_type": "video", "source": "legacy.generatedvideos", "generation_status": "generating", "created_at": now.Add(-time.Hour)},
		{"_id": "old-generating", "media_type": "video", "source": "legacy.generatedvideos", "generation_status": "generating", "created_at": now.Add(-3 * time.Hour)},
	}
	if _, err := database.Collection(schema.CollectionAdminReviewItems).InsertMany(ctx, rows); err != nil {
		t.Fatal(err)
	}
	repository, ok := NewAdminReviewRepository(&Data{client: client, database: database}).(adminreview.VideoOverviewRepository)
	if !ok {
		t.Fatal("admin review repository must expose the video overview projection")
	}
	templates, err := repository.VideoTemplateOptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if templates.WithTemplate != 3 || templates.WithoutTemplate != 1 || len(templates.Options) != 2 || templates.Options[0] != (adminreview.VideoTemplateOption{Value: "Motion", Count: 2}) || templates.Options[1] != (adminreview.VideoTemplateOption{Value: "Animate", Count: 1}) {
		t.Fatalf("template options = %#v", templates)
	}
	runtime, err := repository.VideoRuntime(ctx, adminreview.VideoRuntimeQuery{GeneratingSince: now.Add(-2 * time.Hour), TodayStart: todayStart})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Generating != 1 || runtime.ExternalToday != 2 {
		t.Fatalf("runtime = %#v", runtime)
	}
}
