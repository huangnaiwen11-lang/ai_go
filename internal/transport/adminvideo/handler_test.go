package adminvideo

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	biz "ai-business-service/internal/biz/adminreview"
	"ai-business-service/internal/biz/shared"
)

type fakeRepository struct {
	listResult     biz.ListResult
	listQuery      biz.ListQuery
	listErr        error
	stats          biz.StatusCounts
	statsQuery     biz.StatsQuery
	statsErr       error
	templates      biz.VideoTemplateOptions
	templatesErr   error
	reviewCommands []biz.ReviewCommand
	batchCommands  []biz.BatchReviewCommand
	reviewErr      error
	batchErr       error
}

func (r *fakeRepository) ListReviewItems(_ context.Context, query biz.ListQuery) (biz.ListResult, error) {
	r.listQuery = query
	return r.listResult, r.listErr
}

func (r *fakeRepository) CountReviewStatuses(_ context.Context, query biz.StatsQuery) (biz.StatusCounts, error) {
	r.statsQuery = query
	return r.stats, r.statsErr
}

func (r *fakeRepository) VideoTemplateOptions(context.Context) (biz.VideoTemplateOptions, error) {
	return r.templates, r.templatesErr
}

func (r *fakeRepository) VideoRuntime(context.Context, biz.VideoRuntimeQuery) (biz.VideoRuntime, error) {
	return biz.VideoRuntime{}, nil
}

func (r *fakeRepository) ApplyReview(_ context.Context, command biz.ReviewCommand) (biz.ReviewResult, error) {
	r.reviewCommands = append(r.reviewCommands, command)
	if r.reviewErr != nil {
		return biz.ReviewResult{}, r.reviewErr
	}
	return biz.ReviewResult{Message: "updated", Item: validVideo(command.ItemID, "generated", biz.GenerationStatusSucceeded)}, nil
}

func (r *fakeRepository) ApplyBatchReview(_ context.Context, command biz.BatchReviewCommand) (biz.BatchReviewResult, error) {
	r.batchCommands = append(r.batchCommands, command)
	if r.batchErr != nil {
		return biz.BatchReviewResult{}, r.batchErr
	}
	return biz.BatchReviewResult{Message: "updated", ModifiedCount: int64(len(command.ItemIDs))}, nil
}

func newHandler(repository any) http.Handler {
	return newHandlerAs(repository, "session-admin")
}

func newHandlerAs(repository any, actorID string) http.Handler {
	return NewHandler(biz.NewUsecase(repository), func(*http.Request) (string, error) { return actorID, nil })
}

func validVideo(id, source, generationStatus string) biz.ReviewItem {
	createdAt := time.Date(2026, time.September, 24, 9, 0, 0, 0, time.UTC)
	return biz.ReviewItem{
		ID: id, MediaType: biz.MediaTypeVideo, Source: source, OutputRef: "https://media.example/" + id + ".mp4",
		ReviewStatus: biz.ReviewStatusPending, GenerationStatus: generationStatus, Prompt: "prompt", CreatedAt: createdAt,
	}
}

func TestListForwardsSupportedFiltersAndOnlyReturnsKnownFacts(t *testing.T) {
	repository := &fakeRepository{listResult: biz.ListResult{
		Items: []biz.ReviewItem{
			validVideo("generated", "generated", biz.GenerationStatusSucceeded),
			validVideo("animate", "legacy.animates", biz.GenerationStatusGenerating),
		},
		Total: 2, Page: 2, Limit: 3, TotalPages: 1,
	}}
	w := serve(newHandler(repository), http.MethodGet, "/api/admin/video-review?status=pending&source=all&template=__none__&dateRange=today&sortBy=outputAt&videoPool=all&page=2&limit=3", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got, want := repository.listQuery, (biz.ListQuery{MediaType: biz.MediaTypeVideo, Status: "pending", Source: "all", Template: "__none__", DateRange: "today", SortBy: "outputAt", VideoPool: "all", Page: 2, Limit: 3}); got != want {
		t.Fatalf("ListQuery = %#v, want %#v", got, want)
	}
	data := successData(t, w)
	videos := data["videos"].([]any)
	generated := videos[0].(map[string]any)
	if generated["sourceImageUrl"] != nil || generated["apiProvider"] != nil || generated["videoPool"] != nil || generated["enableAudio"] != nil || generated["generationMs"] != nil {
		t.Fatalf("generated response fabricated unavailable metadata: %#v", generated)
	}
	external := videos[1].(map[string]any)
	if external["apiProvider"] != "a2e" || external["videoPool"] != "external" {
		t.Fatalf("external response = %#v", external)
	}
}

func TestStatsAndTemplateOptionsUseProjection(t *testing.T) {
	repository := &fakeRepository{
		stats:     biz.StatusCounts{Pending: 1, Approved: 2, Rejected: 3, Deleted: 4, Total: 10, Today: 5},
		templates: biz.VideoTemplateOptions{Options: []biz.VideoTemplateOption{{Value: "template", Count: 2}}, WithTemplate: 2, WithoutTemplate: 8},
	}
	handler := newHandler(repository)
	stats := serve(handler, http.MethodGet, "/api/admin/video-review/stats", "")
	if stats.Code != http.StatusOK || repository.statsQuery.MediaType != biz.MediaTypeVideo {
		t.Fatalf("stats status/query = %d/%#v", stats.Code, repository.statsQuery)
	}
	if got := successData(t, stats)["todayCount"]; got != float64(5) {
		t.Fatalf("todayCount = %#v", got)
	}
	templates := serve(handler, http.MethodGet, "/api/admin/video-review/template-options", "")
	if templates.Code != http.StatusOK {
		t.Fatalf("template options status = %d, body = %s", templates.Code, templates.Body.String())
	}
	if got := successData(t, templates)["totals"].(map[string]any)["withTemplate"]; got != float64(2) {
		t.Fatalf("withTemplate = %#v", got)
	}
}

func TestUnavailableGenerationFactsFailClosed(t *testing.T) {
	handler := newHandler(&fakeRepository{})
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/admin/video-review?multiImage=true", ""},
		{http.MethodGet, "/api/admin/video-review?clientSource=web", ""},
		{http.MethodGet, "/api/admin/video-review?videoPool=wan22-original", ""},
		{http.MethodGet, "/api/admin/video-review/gpu-status", ""},
		{http.MethodGet, "/api/admin/video-review/today-by-gpu", ""},
		{http.MethodGet, "/api/admin/video-review/item/log", ""},
		{http.MethodPost, "/api/admin/video-review/translate", "{\"text\":\"hello\"}"},
		{http.MethodGet, "/api/admin/homepage/review-template/video/item", ""},
	} {
		w := serve(handler, test.method, test.path, test.body)
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s status = %d, body = %s", test.method, test.path, w.Code, w.Body.String())
		}
		if got := failureCode(t, w); got != "ADMIN_API_DATA_UNAVAILABLE" {
			t.Fatalf("%s %s code = %s", test.method, test.path, got)
		}
	}
}

func TestTranslateEmptyStringIsDeterministic(t *testing.T) {
	w := serve(newHandler(&fakeRepository{}), http.MethodPost, "/api/admin/video-review/translate", "{\"text\":\"\"}")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := successData(t, w)["translation"]; got != "" {
		t.Fatalf("translation = %#v", got)
	}
}

func TestInvalidProjectionItemsFailClosed(t *testing.T) {
	for _, item := range []biz.ReviewItem{
		validVideo("bad-source", "unknown", biz.GenerationStatusSucceeded),
		validVideo("bad-status", "generated", "unknown"),
		{ID: "missing-output", Source: "generated", ReviewStatus: biz.ReviewStatusPending, GenerationStatus: biz.GenerationStatusSucceeded, CreatedAt: time.Now()},
	} {
		repository := &fakeRepository{listResult: biz.ListResult{Items: []biz.ReviewItem{item}, Page: 1, Limit: 20}}
		w := serve(newHandler(repository), http.MethodGet, "/api/admin/video-review", "")
		if w.Code != http.StatusNotImplemented || failureCode(t, w) != "ADMIN_API_DATA_UNAVAILABLE" {
			t.Fatalf("item %#v response = %d %s", item, w.Code, w.Body.String())
		}
	}
}

func TestWritesUseVideoReviewUsecase(t *testing.T) {
	repository := &fakeRepository{}
	handler := newHandler(repository)

	review := serve(handler, http.MethodPost, "/api/admin/video-review/one/review", "{\"action\":\"approve\",\"version\":3}")
	if review.Code != http.StatusOK || len(repository.reviewCommands) != 1 {
		t.Fatalf("review response = %d %s; commands = %#v", review.Code, review.Body.String(), repository.reviewCommands)
	}
	if got := repository.reviewCommands[0]; got.MediaType != biz.MediaTypeVideo || got.Action != biz.ReviewActionApprove || got.ActorID != "session-admin" || got.ExpectedVersion != 3 || got.IdempotencyKey == "" {
		t.Fatalf("review command = %#v", got)
	}

	batch := serve(handler, http.MethodPost, "/api/admin/video-review/batch-review", "{\"videoIds\":[\"one\",\"two\"],\"action\":\"reject\",\"reason\":\"policy\"}")
	if batch.Code != http.StatusOK || len(repository.batchCommands) != 1 {
		t.Fatalf("batch response = %d %s; commands = %#v", batch.Code, batch.Body.String(), repository.batchCommands)
	}
	if got := repository.batchCommands[0]; got.MediaType != biz.MediaTypeVideo || got.Action != biz.ReviewActionReject || len(got.ItemIDs) != 2 || got.ActorID != "session-admin" || got.IdempotencyKey == "" {
		t.Fatalf("batch command = %#v", got)
	}
	if got := successData(t, batch)["modifiedCount"]; got != float64(2) {
		t.Fatalf("modifiedCount = %#v", got)
	}

	prompt := serve(handler, http.MethodPatch, "/api/admin/video-review/one/prompt", "{\"prompt\":\"new\",\"version\":2,\"expectedVersion\":4}")
	if prompt.Code != http.StatusOK || len(repository.reviewCommands) != 2 {
		t.Fatalf("prompt response = %d %s; commands = %#v", prompt.Code, prompt.Body.String(), repository.reviewCommands)
	}
	if got := repository.reviewCommands[1]; got.Action != biz.ReviewActionPrompt || got.ExpectedVersion != 4 || got.Prompt == nil || *got.Prompt != "new" {
		t.Fatalf("prompt command = %#v", got)
	}

	deleted := serve(handler, http.MethodDelete, "/api/admin/video-review/one", "")
	if deleted.Code != http.StatusOK || len(repository.reviewCommands) != 3 {
		t.Fatalf("delete response = %d %s; commands = %#v", deleted.Code, deleted.Body.String(), repository.reviewCommands)
	}
	if got := repository.reviewCommands[2]; got.Action != biz.ReviewActionDelete || got.ExpectedVersion != -1 || got.IdempotencyKey == "" {
		t.Fatalf("delete command = %#v", got)
	}
}

func TestAuthorizationAndProjectionFailuresUseStableErrors(t *testing.T) {
	unauthenticated := NewHandler(biz.NewUsecase(&fakeRepository{}), func(*http.Request) (string, error) { return "", shared.ErrUnauthenticated })
	w := serve(unauthenticated, http.MethodGet, "/api/admin/video-review", "")
	if w.Code != http.StatusUnauthorized || failureCode(t, w) != "UNAUTHENTICATED" {
		t.Fatalf("authorization response = %d %s", w.Code, w.Body.String())
	}

	projectionUnavailable := NewHandler(biz.NewUsecase(nil), func(*http.Request) (string, error) { return "session-admin", nil })
	w = serve(projectionUnavailable, http.MethodGet, "/api/admin/video-review", "")
	if w.Code != http.StatusServiceUnavailable || failureCode(t, w) != "REVIEW_PROJECTION_NOT_READY" {
		t.Fatalf("projection response = %d %s", w.Code, w.Body.String())
	}

	repository := &fakeRepository{reviewErr: biz.ErrReviewConflict}
	w = serve(newHandler(repository), http.MethodPost, "/api/admin/video-review/one/review", "{\"action\":\"approve\"}")
	if w.Code != http.StatusConflict || failureCode(t, w) != "REVIEW_CONFLICT" {
		t.Fatalf("write response = %d %s", w.Code, w.Body.String())
	}

}

func TestWritesUseVerifiedActorAndDerivedKeysAreActorScoped(t *testing.T) {
	firstRepository := &fakeRepository{}
	secondRepository := &fakeRepository{}
	firstHandler := newHandlerAs(firstRepository, "session-admin-one")
	secondHandler := newHandlerAs(secondRepository, "session-admin-two")

	request := httptest.NewRequest(http.MethodPost, "/api/admin/video-review/video-one/review", bytes.NewBufferString(`{"action":"approve"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-User-Id", "forged-admin")
	response := httptest.NewRecorder()
	firstHandler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(firstRepository.reviewCommands) != 1 {
		t.Fatalf("first write response = %d %s; commands = %#v", response.Code, response.Body.String(), firstRepository.reviewCommands)
	}

	second := serve(secondHandler, http.MethodPost, "/api/admin/video-review/video-one/review", `{"action":"approve"}`)
	if second.Code != http.StatusOK || len(secondRepository.reviewCommands) != 1 {
		t.Fatalf("second write response = %d %s; commands = %#v", second.Code, second.Body.String(), secondRepository.reviewCommands)
	}

	first := firstRepository.reviewCommands[0]
	if first.ActorID != "session-admin-one" {
		t.Fatalf("forged X-User-Id changed audit actor: %#v", first)
	}
	secondCommand := secondRepository.reviewCommands[0]
	if secondCommand.ActorID != "session-admin-two" {
		t.Fatalf("second audit actor = %#v", secondCommand)
	}
	if first.IdempotencyKey == secondCommand.IdempotencyKey {
		t.Fatalf("derived idempotency keys must be actor-scoped: %q", first.IdempotencyKey)
	}
}

func TestWritesRejectClientSuppliedOrMissingActor(t *testing.T) {
	repository := &fakeRepository{}
	handler := newHandler(repository)
	spoofedBody := serve(handler, http.MethodPost, "/api/admin/video-review/video-one/review", `{"action":"approve","actorId":"forged-admin"}`)
	if spoofedBody.Code != http.StatusBadRequest || failureCode(t, spoofedBody) != "INVALID_REQUEST" || len(repository.reviewCommands) != 0 {
		t.Fatalf("client actor body response = %d %s; commands = %#v", spoofedBody.Code, spoofedBody.Body.String(), repository.reviewCommands)
	}

	missingActor := NewHandler(biz.NewUsecase(repository), func(*http.Request) (string, error) { return "", nil })
	response := serve(missingActor, http.MethodPost, "/api/admin/video-review/video-one/review", `{"action":"approve"}`)
	if response.Code != http.StatusUnauthorized || failureCode(t, response) != "UNAUTHENTICATED" || len(repository.reviewCommands) != 0 {
		t.Fatalf("missing actor response = %d %s; commands = %#v", response.Code, response.Body.String(), repository.reviewCommands)
	}
}

func serve(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func successData(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload struct {
		Success bool
		Data    map[string]any
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, response.Body.String())
	}
	if !payload.Success {
		t.Fatalf("response is not successful: %s", response.Body.String())
	}
	return payload.Data
}

func failureCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct{ Code string }
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, response.Body.String())
	}
	return payload.Code
}
