package adminreview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	biz "ai-business-service/internal/biz/adminreview"
)

type stubRepository struct {
	list            biz.ListResult
	stats           biz.StatusCounts
	templateOptions biz.ImageTemplateOptions
	runtime         biz.ImageRuntime
	videoOptions    biz.VideoTemplateOptions
	videoRuntime    biz.VideoRuntime
	listQuery       biz.ListQuery
	err             error
}

func (s *stubRepository) ListReviewItems(_ context.Context, query biz.ListQuery) (biz.ListResult, error) {
	s.listQuery = query
	return s.list, s.err
}
func (s *stubRepository) CountReviewStatuses(context.Context, biz.StatsQuery) (biz.StatusCounts, error) {
	return s.stats, s.err
}
func (s *stubRepository) ImageTemplateOptions(context.Context) (biz.ImageTemplateOptions, error) {
	return s.templateOptions, s.err
}
func (s *stubRepository) ImageRuntime(context.Context, biz.ImageRuntimeQuery) (biz.ImageRuntime, error) {
	return s.runtime, s.err
}
func (s *stubRepository) VideoTemplateOptions(context.Context) (biz.VideoTemplateOptions, error) {
	return s.videoOptions, s.err
}
func (s *stubRepository) VideoRuntime(context.Context, biz.VideoRuntimeQuery) (biz.VideoRuntime, error) {
	return s.videoRuntime, s.err
}

func allow(*http.Request) error { return nil }
func deny(*http.Request) error  { return errors.New("forbidden") }

func TestReviewProjectionUnavailableFailsClosed(t *testing.T) {
	handler := NewHandler(&stubRepository{err: biz.ErrProjectionUnavailable}, allow)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/image-review", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	var envelope map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope["success"] != false || envelope["code"] != "REVIEW_PROJECTION_NOT_READY" {
		t.Fatalf("response = %#v, want explicit projection readiness error", envelope)
	}
}

func TestReviewListUsesIndependentReviewStatusProjection(t *testing.T) {
	now := time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC)
	handler := NewHandler(&stubRepository{list: biz.ListResult{
		Items: []biz.ReviewItem{{ID: "img-1", MediaType: biz.MediaTypeImage, ReviewStatus: biz.ReviewStatusPending, GenerationStatus: biz.GenerationStatusSucceeded, CreatedAt: now, Version: 3}},
		Total: 1, Page: 1, Limit: 20, TotalPages: 1,
	}}, allow)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/image-review/pending", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var envelope struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !envelope.Success {
		t.Fatalf("response not successful: %s", recorder.Body.String())
	}
	rows := envelope.Data["images"].([]interface{})
	if len(rows) != 1 || rows[0].(map[string]interface{})["reviewStatus"] != biz.ReviewStatusPending {
		t.Fatalf("images = %#v", envelope.Data["images"])
	}
}

func TestImagesListCompatibilityUsesImageReviewProjection(t *testing.T) {
	now := time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC)
	repository := &stubRepository{list: biz.ListResult{
		Items: []biz.ReviewItem{{ID: "img-1", MediaType: biz.MediaTypeImage, Source: "legacy.generatedimages", OutputRef: "https://media.example.test/img-1.png", ReviewStatus: biz.ReviewStatusApproved, VisibilityStatus: "public", GenerationStatus: biz.GenerationStatusSucceeded, Prompt: "portrait", CreatedAt: now, OutputAt: now.Add(3 * time.Second), Version: 3}},
		Total: 1, Page: 1, Limit: 20, TotalPages: 1,
	}}
	handler := NewHandler(repository, allow)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/images", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !envelope.Success {
		t.Fatalf("response not successful: %s", recorder.Body.String())
	}
	rows, ok := envelope.Data["images"].([]interface{})
	if !ok || len(rows) != 1 || rows[0].(map[string]interface{})["id"] != "img-1" {
		t.Fatalf("images = %#v", envelope.Data["images"])
	}
	image := rows[0].(map[string]interface{})
	if image["imageUrl"] != "https://media.example.test/img-1.png" || image["status"] != biz.ReviewStatusApproved || image["isPublic"] != true || image["source"] != "generated" || image["apiProvider"] != "unknown" || image["imagePool"] != "unknown" || image["generationMs"] != float64(3000) || image["likes"] != float64(0) || image["views"] != float64(0) {
		t.Fatalf("incompatible admin image row = %#v", image)
	}
	if pagination, ok := envelope.Data["pagination"].(map[string]interface{}); !ok || pagination["total"] != float64(1) {
		t.Fatalf("pagination = %#v", envelope.Data["pagination"])
	}
	if repository.listQuery.Status != "all" {
		t.Fatalf("default list status = %q, want all", repository.listQuery.Status)
	}
}

func TestVideosListCompatibilityUsesVideoReviewProjection(t *testing.T) {
	now := time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC)
	handler := NewHandler(&stubRepository{list: biz.ListResult{
		Items: []biz.ReviewItem{{
			ID: "video-1", MediaType: biz.MediaTypeVideo, Source: "legacy.generatedvideos",
			OutputRef: "https://media.example.test/video-1.mp4", ReviewStatus: biz.ReviewStatusApproved,
			GenerationStatus: biz.GenerationStatusSucceeded, Prompt: "camera drifts", TemplateTitle: "Motion",
			CreatedAt: now, OutputAt: now.Add(3 * time.Second), Version: 3,
		}},
		Total: 1, Page: 1, Limit: 20, TotalPages: 1,
	}}, allow)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/video-review", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !envelope.Success {
		t.Fatalf("response not successful: %s", recorder.Body.String())
	}
	rows, ok := envelope.Data["videos"].([]interface{})
	if !ok || len(rows) != 1 {
		t.Fatalf("videos = %#v", envelope.Data["videos"])
	}
	video := rows[0].(map[string]interface{})
	if video["videoUrl"] != "https://media.example.test/video-1.mp4" ||
		video["sourceImageUrl"] != "" || video["status"] != biz.ReviewStatusApproved ||
		video["generationStatus"] != "completed" || video["source"] != "generated" ||
		video["apiProvider"] != "unknown" || video["videoPool"] != "unknown" ||
		video["generationMs"] != float64(3000) || video["prompt"] != "camera drifts" ||
		video["templateTitle"] != "Motion" || video["user"] != nil {
		t.Fatalf("incompatible admin video row = %#v", video)
	}
	if pagination, ok := envelope.Data["pagination"].(map[string]interface{}); !ok || pagination["total"] != float64(1) {
		t.Fatalf("pagination = %#v", envelope.Data["pagination"])
	}
}

func TestVideosListForwardsProjectionBackedFilters(t *testing.T) {
	repository := &stubRepository{list: biz.ListResult{Page: 2, Limit: 20}}
	handler := NewHandler(repository, allow)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/video-review?status=approved&source=animate&template=Motion&dateRange=today&sortBy=outputAt&videoPool=external&page=2", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	query := repository.listQuery
	if query.MediaType != biz.MediaTypeVideo || query.Status != biz.ReviewStatusApproved || query.Source != "animate" || query.Template != "Motion" || query.DateRange != "today" || query.SortBy != "outputAt" || query.VideoPool != "external" || query.Page != 2 {
		t.Fatalf("list query = %#v", query)
	}
}

func TestVideosListRejectsFiltersWhoseFactsAreNotProjected(t *testing.T) {
	handler := NewHandler(&stubRepository{}, allow)
	for _, path := range []string{
		"/api/admin/video-review?multiImage=true",
		"/api/admin/video-review?clientSource=ios",
		"/api/admin/video-review?videoPool=wan22-original",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("path %s: status = %d, want 501; body=%s", path, recorder.Code, recorder.Body.String())
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Code != "ADMIN_API_DATA_UNAVAILABLE" {
			t.Fatalf("path %s: code = %q, want ADMIN_API_DATA_UNAVAILABLE", path, envelope.Code)
		}
	}
}

func TestVideoOverviewRoutesAreNotUnmigrated(t *testing.T) {
	handler := NewHandler(&stubRepository{
		videoOptions: biz.VideoTemplateOptions{
			Options: []biz.VideoTemplateOption{{Value: "Motion", Count: 2}}, WithTemplate: 2, WithoutTemplate: 1,
		},
		videoRuntime: biz.VideoRuntime{Generating: 3, ExternalToday: 4},
	}, allow)
	template := requestSuccess(t, handler, "/api/admin/video-review/template-options")
	if options, ok := template["options"].([]interface{}); !ok || len(options) != 1 || options[0].(map[string]interface{})["value"] != "Motion" || options[0].(map[string]interface{})["count"] != float64(2) {
		t.Fatalf("template options = %#v", template["options"])
	}
	if totals, ok := template["totals"].(map[string]interface{}); !ok || totals["withTemplate"] != float64(2) || totals["withoutTemplate"] != float64(1) {
		t.Fatalf("template totals = %#v", template["totals"])
	}

	status := requestSuccess(t, handler, "/api/admin/video-review/gpu-status")
	if status["generating"] != float64(3) || status["recentThroughput"] != nil {
		t.Fatalf("gpu status = %#v", status)
	}
	comfy, ok := status["comfyui"].(map[string]interface{})
	if !ok || comfy["fleetSnapshotOk"] != false || comfy["reportedVideoNodes"] != float64(0) {
		t.Fatalf("comfy status = %#v", status["comfyui"])
	}
	if nodes, ok := comfy["nodes"].([]interface{}); !ok || len(nodes) != 0 {
		t.Fatalf("comfy nodes = %#v", comfy["nodes"])
	}

	today := requestSuccess(t, handler, "/api/admin/video-review/today-by-gpu")
	if _, ok := today["todayStart"].(string); !ok {
		t.Fatalf("todayStart = %#v", today["todayStart"])
	}
	if rows, ok := today["byNode"].([]interface{}); !ok || len(rows) != 0 {
		t.Fatalf("today byNode = %#v", today["byNode"])
	}
	if external, ok := today["external"].(map[string]interface{}); !ok || external["provider"] != "a2e" || external["count"] != float64(4) || today["total"] != float64(4) || today["genUnreachable"] != true {
		t.Fatalf("today by gpu = %#v", today)
	}
}

func TestVideoDataGapsAreExplicit(t *testing.T) {
	handler := NewHandler(&stubRepository{}, allow)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/admin/video-review/video-1/log", nil),
		httptest.NewRequest(http.MethodPost, "/api/admin/video-review/translate", mustJSON(map[string]any{"text": "hello"})),
		httptest.NewRequest(http.MethodGet, "/api/admin/homepage/review-template/video/video-1", nil),
		httptest.NewRequest(http.MethodPost, "/api/admin/homepage/review-template/video/video-1", mustJSON(map[string]any{"title": "Motion"})),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s: status = %d, want 501; body=%s", request.Method, request.URL.Path, recorder.Code, recorder.Body.String())
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Code != "ADMIN_API_DATA_UNAVAILABLE" {
			t.Fatalf("%s %s: code = %q, want explicit data gap", request.Method, request.URL.Path, envelope.Code)
		}
	}
}

func TestImageGenerationLogDataGapIsExplicit(t *testing.T) {
	handler := NewHandler(&stubRepository{}, allow)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/image-review/image-1/log", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != "ADMIN_API_DATA_UNAVAILABLE" {
		t.Fatalf("code = %q, want explicit data gap", envelope.Code)
	}
}

func TestImagesOverviewDependenciesAreHandled(t *testing.T) {
	handler := NewHandler(&stubRepository{
		templateOptions: biz.ImageTemplateOptions{
			Options: []biz.ImageTemplateOption{{Value: "Portrait", Count: 2}}, WithTemplate: 2, WithoutTemplate: 1,
		},
		runtime: biz.ImageRuntime{Generating: 3, ExternalToday: 4},
	}, allow)
	template := requestSuccess(t, handler, "/api/admin/images/template-options")
	if options, ok := template["options"].([]interface{}); !ok || len(options) != 1 || options[0].(map[string]interface{})["value"] != "Portrait" || options[0].(map[string]interface{})["count"] != float64(2) {
		t.Fatalf("template options = %#v", template["options"])
	}
	if totals, ok := template["totals"].(map[string]interface{}); !ok || totals["withTemplate"] != float64(2) || totals["withoutTemplate"] != float64(1) {
		t.Fatalf("template totals = %#v", template["totals"])
	}

	provider := requestSuccess(t, handler, "/api/admin/images/provider-status")
	if provider["generating"] != float64(3) || provider["gpu"] != nil || provider["recentThroughput"] != nil {
		t.Fatalf("provider status = %#v", provider)
	}
	if providers, ok := provider["providers"].(map[string]interface{}); !ok || len(providers) != 0 {
		t.Fatalf("providers = %#v", provider["providers"])
	}

	today := requestSuccess(t, handler, "/api/admin/images/today-by-gpu")
	if _, ok := today["todayStart"].(string); !ok {
		t.Fatalf("todayStart = %#v", today["todayStart"])
	}
	if rows, ok := today["byNode"].([]interface{}); !ok || len(rows) != 0 {
		t.Fatalf("today byNode = %#v", today["byNode"])
	}
	if external, ok := today["external"].(map[string]interface{}); !ok || external["provider"] != "a2e" || external["count"] != float64(4) || today["total"] != float64(4) || today["genUnreachable"] != true || today["qualityUnreachable"] != true {
		t.Fatalf("today by gpu = %#v", today)
	}
}

func requestSuccess(t *testing.T, handler http.Handler, path string) map[string]interface{} {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("path %s: status = %d, want 200; body=%s", path, recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || !envelope.Success {
		t.Fatalf("path %s: response = %s, err=%v", path, recorder.Body.String(), err)
	}
	return envelope.Data
}

func TestReviewStatsAndAuthorization(t *testing.T) {
	handler := NewHandler(&stubRepository{stats: biz.StatusCounts{Pending: 2, Approved: 3, Rejected: 1, Deleted: 4, Total: 6, Today: 5}}, allow)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/video-review/stats", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", recorder.Code)
	}
	denied := NewHandler(&stubRepository{}, deny)
	deniedRecorder := httptest.NewRecorder()
	denied.ServeHTTP(deniedRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/video-review/stats", nil))
	if deniedRecorder.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d, want 403", deniedRecorder.Code)
	}
}
