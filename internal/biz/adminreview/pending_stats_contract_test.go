//go:build adminreview_contract

// These tests intentionally describe the first red contract for the review
// projection. They are gated until the adminreview package and repository are
// implemented; running `go test -tags adminreview_contract ./internal/biz/adminreview`
// is expected to fail at the moment because the production package does not
// exist yet. Keep the cases green before wiring HTTP handlers.
package adminreview_test

import (
	"context"
	"testing"
	"time"

	"ai-business-service/internal/biz/adminreview"
)

type projectionRepo struct {
	items []adminreview.ReviewItem
}

func (r *projectionRepo) ListReviewItems(context.Context, adminreview.ListQuery) ([]adminreview.ReviewItem, int64, error) {
	return append([]adminreview.ReviewItem(nil), r.items...), int64(len(r.items)), nil
}

func (r *projectionRepo) CountReviewStatuses(context.Context, adminreview.StatsQuery) (adminreview.StatusCounts, error) {
	var counts adminreview.StatusCounts
	for _, item := range r.items {
		switch item.ReviewStatus {
		case adminreview.ReviewStatusPending:
			counts.Pending++
		case adminreview.ReviewStatusApproved:
			counts.Approved++
		case adminreview.ReviewStatusRejected:
			counts.Rejected++
		case adminreview.ReviewStatusDeleted:
			counts.Deleted++
		}
	}
	counts.Total = counts.Pending + counts.Approved + counts.Rejected
	return counts, nil
}

func TestListPendingUsesReviewStatusAndStableCreatedAtOrder(t *testing.T) {
	repo := &projectionRepo{items: []adminreview.ReviewItem{
		{ID: "older", MediaType: adminreview.MediaTypeImage, ReviewStatus: adminreview.ReviewStatusPending, CreatedAt: time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)},
		{ID: "newer", MediaType: adminreview.MediaTypeImage, ReviewStatus: adminreview.ReviewStatusPending, CreatedAt: time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC)},
		{ID: "approved", MediaType: adminreview.MediaTypeImage, ReviewStatus: adminreview.ReviewStatusApproved, CreatedAt: time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)},
	}}
	uc := adminreview.NewUsecase(repo)
	result, err := uc.List(context.Background(), adminreview.ListQuery{Status: adminreview.ReviewStatusPending, Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if result.Total != 2 || len(result.Items) != 2 {
		t.Fatalf("List() = %#v, want two pending items", result)
	}
	if result.Items[0].ID != "newer" || result.Items[1].ID != "older" {
		t.Fatalf("List() order = %#v, want newest first", result.Items)
	}
}

func TestStatsDoesNotTreatGenerationStatusAsReviewStatus(t *testing.T) {
	repo := &projectionRepo{items: []adminreview.ReviewItem{
		{ID: "generated", GenerationStatus: adminreview.GenerationStatusSucceeded, ReviewStatus: adminreview.ReviewStatusPending},
		{ID: "failed", GenerationStatus: adminreview.GenerationStatusFailed, ReviewStatus: adminreview.ReviewStatusRejected},
	}}
	uc := adminreview.NewUsecase(repo)
	result, err := uc.Stats(context.Background(), adminreview.StatsQuery{})
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if result.Pending != 1 || result.Rejected != 1 || result.Total != 2 {
		t.Fatalf("Stats() = %#v, want pending=1 rejected=1 total=2", result)
	}
}
