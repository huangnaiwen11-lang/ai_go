package adminreview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	biz "ai-business-service/internal/biz/adminreview"
)

type writeRepositoryStub struct {
	lastSingle biz.ReviewCommand
	lastBatch  biz.BatchReviewCommand
	err        error
}

func (s *writeRepositoryStub) ListReviewItems(context.Context, biz.ListQuery) (biz.ListResult, error) {
	return biz.ListResult{}, nil
}
func (s *writeRepositoryStub) CountReviewStatuses(context.Context, biz.StatsQuery) (biz.StatusCounts, error) {
	return biz.StatusCounts{}, nil
}
func (s *writeRepositoryStub) ApplyReview(_ context.Context, command biz.ReviewCommand) (biz.ReviewResult, error) {
	s.lastSingle = command
	if s.err != nil {
		return biz.ReviewResult{}, s.err
	}
	return biz.ReviewResult{Message: "Review approved", Item: biz.ReviewItem{ID: command.ItemID, MediaType: command.MediaType, ReviewStatus: string(command.Action), Version: 2}}, nil
}
func (s *writeRepositoryStub) ApplyBatchReview(_ context.Context, command biz.BatchReviewCommand) (biz.BatchReviewResult, error) {
	s.lastBatch = command
	if s.err != nil {
		return biz.BatchReviewResult{}, s.err
	}
	return biz.BatchReviewResult{Message: "Review approved", ModifiedCount: int64(len(command.ItemIDs))}, nil
}

func TestReviewWriteApproveUsesCASAndIdempotency(t *testing.T) {
	repo := &writeRepositoryStub{}
	handler := NewHandler(repo, allow)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/image-review/img-1/review", mustJSON(map[string]any{"action": "approve", "expectedVersion": 7}))
	req.Header.Set("Idempotency-Key", "review-key-1")
	req.Header.Set("X-Admin-Actor", "admin-1")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if repo.lastSingle.ItemID != "img-1" || repo.lastSingle.MediaType != biz.MediaTypeImage || repo.lastSingle.ExpectedVersion != 7 || repo.lastSingle.IdempotencyKey != "review-key-1" || repo.lastSingle.ActorID != "authenticated-admin" {
		t.Fatalf("command = %#v", repo.lastSingle)
	}
}

func TestReviewWriteBatchProjectionUnavailableFailsClosed(t *testing.T) {
	repo := &writeRepositoryStub{err: biz.ErrProjectionUnavailable}
	handler := NewHandler(repo, allow)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/video-review/batch-review", mustJSON(map[string]any{"videoIds": []string{"v-1"}, "action": "reject"}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	var envelope map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["code"] != "REVIEW_PROJECTION_NOT_READY" {
		t.Fatalf("response = %#v", envelope)
	}
}

func TestReviewWritePromptAndDeleteKeepLegacyPaths(t *testing.T) {
	repo := &writeRepositoryStub{}
	handler := NewHandler(repo, allow)
	promptReq := httptest.NewRequest(http.MethodPatch, "/api/admin/video-review/v-1/prompt", mustJSON(map[string]any{"prompt": "new prompt", "negativePrompt": "bad"}))
	promptRec := httptest.NewRecorder()
	handler.ServeHTTP(promptRec, promptReq)
	if promptRec.Code != http.StatusOK || repo.lastSingle.Action != biz.ReviewActionPrompt {
		t.Fatalf("prompt status=%d command=%#v", promptRec.Code, repo.lastSingle)
	}
	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/admin/image-review/img-1", nil)
	deleteRec := httptest.NewRecorder()
	handler.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK || repo.lastSingle.Action != biz.ReviewActionDelete {
		t.Fatalf("delete status=%d command=%#v", deleteRec.Code, repo.lastSingle)
	}
}

func mustJSON(value any) *strings.Reader {
	bytes, _ := json.Marshal(value)
	return strings.NewReader(string(bytes))
}
