//go:build adminreview_contract

// Red contract for write-side safety. This is intentionally gated until the
// review projection exists; do not replace these assertions with an
// updateMany/modifiedCount-only implementation.
package adminreview_test

import (
	"context"
	"errors"
	"testing"

	"ai-business-service/internal/biz/adminreview"
)

type writeRepo struct {
	item    adminreview.ReviewItem
	updates int
	audits  int
	byKey   map[string]adminreview.ReviewAudit
}

func (r *writeRepo) FindReviewItem(context.Context, string) (adminreview.ReviewItem, error) {
	return r.item, nil
}

func (r *writeRepo) CompareAndSwapReview(_ context.Context, mutation adminreview.ReviewMutation) (adminreview.ReviewItem, bool, error) {
	if mutation.ExpectedVersion != r.item.Version {
		return adminreview.ReviewItem{}, false, nil
	}
	r.updates++
	r.item.ReviewStatus = adminreview.ReviewStatusApproved
	r.item.Version++
	return r.item, true, nil
}

func (r *writeRepo) WriteReviewAudit(_ context.Context, audit adminreview.ReviewAudit) error {
	r.audits++
	if r.byKey != nil {
		r.byKey[audit.IdempotencyKey] = audit
	}
	return nil
}

func (r *writeRepo) FindReviewAudit(_ context.Context, key string) (adminreview.ReviewAudit, bool, error) {
	audit, ok := r.byKey[key]
	return audit, ok, nil
}

func TestReviewRequiresExpectedVersionAndAtomicAudit(t *testing.T) {
	repo := &writeRepo{item: adminreview.ReviewItem{ID: "image-1", ReviewStatus: adminreview.ReviewStatusPending, Version: 7}, byKey: make(map[string]adminreview.ReviewAudit)}
	uc := adminreview.NewUsecase(repo)
	_, err := uc.Review(context.Background(), adminreview.ReviewCommand{
		ItemID: "image-1", Action: adminreview.ReviewActionApprove, ActorID: "admin-1", ExpectedVersion: 6,
		IdempotencyKey: "review:image-1:approve:1",
	})
	if !errors.Is(err, adminreview.ErrReviewConflict) {
		t.Fatalf("Review() error = %v, want ErrReviewConflict", err)
	}
	if repo.updates != 0 || repo.audits != 0 {
		t.Fatalf("CAS conflict changed state: updates=%d audits=%d", repo.updates, repo.audits)
	}
}

func TestReviewReplayUsesIdempotencyKeyWithoutSecondMutation(t *testing.T) {
	repo := &writeRepo{item: adminreview.ReviewItem{ID: "image-1", ReviewStatus: adminreview.ReviewStatusPending, Version: 7}, byKey: make(map[string]adminreview.ReviewAudit)}
	uc := adminreview.NewUsecase(repo)
	command := adminreview.ReviewCommand{
		ItemID: "image-1", Action: adminreview.ReviewActionApprove, ActorID: "admin-1", ExpectedVersion: 7,
		IdempotencyKey: "review:image-1:approve:1",
	}
	if _, err := uc.Review(context.Background(), command); err != nil {
		t.Fatalf("first Review() error = %v", err)
	}
	if _, err := uc.Review(context.Background(), command); err != nil {
		t.Fatalf("replayed Review() error = %v", err)
	}
	if repo.updates != 1 || repo.audits != 1 {
		t.Fatalf("replay duplicated side effects: updates=%d audits=%d", repo.updates, repo.audits)
	}
}
