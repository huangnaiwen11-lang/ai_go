package adminreview

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMapLegacyGeneratedImageKeepsRealOutputAndStableID(t *testing.T) {
	id := bson.NewObjectID()
	created := time.Date(2026, 9, 22, 1, 2, 3, 0, time.UTC)
	doc := bson.M{
		"_id": id, "userId": bson.NewObjectID(), "imageUrl": "https://pub.example.test/a.png",
		"prompt": "portrait", "status": "approved", "generationStatus": "completed",
		"createdAt": created,
	}
	first := MapLegacyDocument(LegacyGeneratedImages, doc)
	second := MapLegacyDocument(LegacyGeneratedImages, doc)
	if first.Classification != ImportReady || second.Classification != ImportReady {
		t.Fatalf("classification = %s/%s, want ready", first.Classification, second.Classification)
	}
	if first.Record.ID == "" || first.Record.ID != second.Record.ID {
		t.Fatalf("stable ID = %q/%q", first.Record.ID, second.Record.ID)
	}
	if first.Record.OutputRef != "https://pub.example.test/a.png" || first.Record.ReviewStatus != ReviewStatusApproved {
		t.Fatalf("record = %#v", first.Record)
	}
	if first.Record.GenerationStatus != GenerationStatusSucceeded || !first.Record.CreatedAt.Equal(created) {
		t.Fatalf("generation/timestamp = %q/%s", first.Record.GenerationStatus, first.Record.CreatedAt)
	}
}

func TestMapLegacyAnimateNeverAutoApproves(t *testing.T) {
	outcome := MapLegacyDocument(LegacyAnimates, bson.M{
		"_id": "animate-1", "userId": "user-1", "resultUrl": "https://pub.example.test/a.mp4",
		"status": "completed", "createdAt": time.Now().UTC(),
	})
	if outcome.Classification != ImportReady || outcome.Record.ReviewStatus != ReviewStatusPending {
		t.Fatalf("outcome = %#v, want ready/pending", outcome)
	}
}

func TestMapLegacyAnimateR2KeyAloneIsOrphan(t *testing.T) {
	outcome := MapLegacyDocument(LegacyAnimates, bson.M{
		"_id": "animate-r2-only", "userId": "user-1", "resultR2Key": "videos/animate/output.mp4",
		"status": "completed", "createdAt": time.Now().UTC(),
	})
	if outcome.Classification != ImportOrphan || outcome.Record.OutputRef != "" {
		t.Fatalf("outcome = %#v, want orphan without fabricated URL", outcome)
	}
}

func TestMapLegacyMissingOutputIsOrphanAndDoesNotInventURL(t *testing.T) {
	outcome := MapLegacyDocument(LegacyGeneratedVideos, bson.M{
		"_id": "video-1", "userId": "user-1", "generationStatus": "completed",
		"createdAt": time.Now().UTC(),
	})
	if outcome.Classification != ImportOrphan || outcome.Record.OutputRef != "" {
		t.Fatalf("outcome = %#v, want orphan with empty output", outcome)
	}
}

func TestMapLegacyVideoInfersSucceededForHistoricalRowsWithoutGenerationStatus(t *testing.T) {
	outcome := MapLegacyDocument(LegacyGeneratedVideos, bson.M{
		"_id": "video-old", "userId": "user-1", "videoUrl": "https://pub.example.test/a.mp4",
		"status": "pending", "createdAt": time.Now().UTC(),
	})
	if outcome.Classification != ImportReady || outcome.Record.GenerationStatus != GenerationStatusSucceeded {
		t.Fatalf("outcome = %#v, want ready/succeeded", outcome)
	}
}

func TestMapLegacyFaceSwapUnknownTypeIsConflict(t *testing.T) {
	outcome := MapLegacyDocument(LegacyFaceSwapTasks, bson.M{
		"_id": "swap-1", "userId": "user-1", "type": "audio", "resultVideoUrl": "https://pub.example.test/a.mp4",
		"generationStatus": "completed", "createdAt": time.Now().UTC(),
	})
	if outcome.Classification != ImportConflict {
		t.Fatalf("classification = %s, want conflict", outcome.Classification)
	}
}
