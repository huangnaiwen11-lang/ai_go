package adminimage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	biz "ai-business-service/internal/biz/adminimage"
)

func TestHandlerRejectsUnauthorizedRequest(t *testing.T) {
	handler := NewHandler(biz.NewUsecase(&fakeRepository{}, nil, fixedNow), func(*http.Request) error {
		return errors.New("not an admin")
	})

	response := request(handler, http.MethodGet, "/api/admin/images", "")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	assertFailureCode(t, response, "FORBIDDEN")
}

func TestHandlerListsImagesWithNormalizedQuery(t *testing.T) {
	repository := &fakeRepository{list: biz.ListResult{Images: []biz.Image{{ID: "image-1", Prompt: "hello", ImageURL: "https://example.test/image.png", Status: "completed", CreatedAt: fixedNow()}}, Total: 1}}
	handler := NewHandler(biz.NewUsecase(repository, nil, fixedNow), allow)

	response := request(handler, http.MethodGet, "/api/admin/images?status=hidden&page=2&limit=7&multiImage=true", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if repository.query.Status != "hidden" || repository.query.Page != 2 || repository.query.Limit != 7 || repository.query.MultiImage != "true" {
		t.Fatalf("query = %#v", repository.query)
	}

	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Images []struct {
				ID       string `json:"id"`
				ImageURL string `json:"imageUrl"`
			} `json:"images"`
			Pagination struct {
				Page  int `json:"page"`
				Limit int `json:"limit"`
				Total int `json:"total"`
			} `json:"pagination"`
		} `json:"data"`
	}
	decode(t, response, &payload)
	if !payload.Success || len(payload.Data.Images) != 1 || payload.Data.Images[0].ID != "image-1" || payload.Data.Images[0].ImageURL != "https://example.test/image.png" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Data.Pagination.Page != 2 || payload.Data.Pagination.Limit != 7 || payload.Data.Pagination.Total != 1 {
		t.Fatalf("pagination = %#v", payload.Data.Pagination)
	}
}

func TestHandlerTemplateOptionsEnvelope(t *testing.T) {
	repository := &fakeRepository{templates: biz.TemplateOptions{Options: []biz.TemplateOption{{Value: "portrait", Count: 4}}, WithTemplate: 4, WithoutTemplate: 9}}
	handler := NewHandler(biz.NewUsecase(repository, nil, fixedNow), allow)

	response := request(handler, http.MethodGet, "/api/admin/images/template-options", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Options []struct {
				Value string `json:"value"`
				Count int64  `json:"count"`
			} `json:"options"`
			Totals struct {
				WithTemplate    int64 `json:"withTemplate"`
				WithoutTemplate int64 `json:"withoutTemplate"`
			} `json:"totals"`
		} `json:"data"`
	}
	decode(t, response, &payload)
	if !payload.Success || len(payload.Data.Options) != 1 || payload.Data.Options[0].Value != "portrait" || payload.Data.Totals.WithTemplate != 4 || payload.Data.Totals.WithoutTemplate != 9 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestHandlerReturnsUnavailableWhenFleetIsMissing(t *testing.T) {
	handler := NewHandler(biz.NewUsecase(&fakeRepository{}, nil, fixedNow), allow)

	for _, path := range []string{"/api/admin/images/provider-status", "/api/admin/images/today-by-gpu"} {
		response := request(handler, http.MethodGet, path, "")
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, body = %s", path, response.Code, response.Body.String())
		}
		assertFailureCode(t, response, "SERVICE_UNAVAILABLE")
	}
}

func TestHandlerProviderStatusUsesFrontendGPUFieldNames(t *testing.T) {
	queue, vram := int64(3), int64(12)
	fleet := fakeFleet{status: biz.GPUStatus{Nodes: []biz.GPUNode{{URL: "https://gpu.example.test", Role: "qwen-edit", ImagePool: "qwen-edit-default", VRAMFree: &vram, QueueDepth: &queue}}}}
	handler := NewHandler(biz.NewUsecase(&fakeRepository{}, fleet, fixedNow), allow)

	response := request(handler, http.MethodGet, "/api/admin/images/provider-status", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			GPU struct {
				Nodes []map[string]any `json:"nodes"`
			} `json:"gpu"`
		} `json:"data"`
	}
	decode(t, response, &payload)
	if len(payload.Data.GPU.Nodes) != 1 || payload.Data.GPU.Nodes[0]["vram_free"] != float64(12) || payload.Data.GPU.Nodes[0]["queue_depth"] != float64(3) {
		t.Fatalf("nodes = %#v", payload.Data.GPU.Nodes)
	}
	if _, exists := payload.Data.GPU.Nodes[0]["vramFree"]; exists {
		t.Fatalf("nodes leaked non-contract key: %#v", payload.Data.GPU.Nodes[0])
	}
}

func TestHandlerMutationsPreserveLegacyNestedResponse(t *testing.T) {
	repository := &fakeRepository{}
	handler := NewHandler(biz.NewUsecase(repository, nil, fixedNow), allow)

	for _, test := range []struct {
		method string
		path   string
		body   string
		want   biz.Mutation
	}{
		{http.MethodPost, "/api/admin/images/image-1/hide", "{}", biz.MutationHide},
		{http.MethodPost, "/api/admin/images/image-1/restore", "{}", biz.MutationRestore},
		{http.MethodDelete, "/api/admin/images/image-1", "", biz.MutationDelete},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := request(handler, test.method, test.path, test.body)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if repository.mutation != test.want || repository.mutationID != "image-1" {
				t.Fatalf("mutation = %q, id = %q", repository.mutation, repository.mutationID)
			}
			var payload struct {
				Success bool `json:"success"`
				Data    struct {
					Success bool `json:"success"`
				} `json:"data"`
			}
			decode(t, response, &payload)
			if !payload.Success || !payload.Data.Success {
				t.Fatalf("payload = %#v", payload)
			}
		})
	}
}

func TestHandlerBatchHideRequiresNonEmptyIDs(t *testing.T) {
	repository := &fakeRepository{}
	handler := NewHandler(biz.NewUsecase(repository, nil, fixedNow), allow)

	for _, body := range []string{"", `{}`, `{"ids":[]}`, `{"ids":["image-1"]} {}`} {
		response := request(handler, http.MethodPost, "/api/admin/images/batch-hide", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, response = %s", body, response.Code, response.Body.String())
		}
		assertFailureCode(t, response, "INVALID_REQUEST")
	}

	response := request(handler, http.MethodPost, "/api/admin/images/batch-hide", `{"ids":["image-1","image-2"]}`)
	if response.Code != http.StatusOK || len(repository.batchIDs) != 2 {
		t.Fatalf("status = %d, ids = %#v, response = %s", response.Code, repository.batchIDs, response.Body.String())
	}
}

func TestHandlerReturnsNotMigratedForUnknownImagePath(t *testing.T) {
	handler := NewHandler(biz.NewUsecase(&fakeRepository{}, nil, fixedNow), allow)

	response := request(handler, http.MethodGet, "/api/admin/images/not-yet-migrated", "")
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	assertFailureCode(t, response, "ADMIN_API_NOT_MIGRATED")
}

func allow(*http.Request) error { return nil }

func fixedNow() time.Time { return time.Date(2026, time.September, 24, 8, 30, 0, 0, time.UTC) }

func request(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decode(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
}

func assertFailureCode(t *testing.T, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	var payload struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
	}
	decode(t, response, &payload)
	if payload.Success || payload.Code != want {
		t.Fatalf("payload = %#v, want code %q", payload, want)
	}
}

type fakeRepository struct {
	list       biz.ListResult
	query      biz.ListQuery
	templates  biz.TemplateOptions
	mutation   biz.Mutation
	mutationID string
	batchIDs   []string
}

func (f *fakeRepository) List(_ context.Context, query biz.ListQuery) (biz.ListResult, error) {
	f.query = query
	return f.list, nil
}

func (f *fakeRepository) Stats(context.Context, time.Time) (biz.Stats, error) {
	return biz.Stats{}, nil
}

func (f *fakeRepository) TemplateOptions(context.Context) (biz.TemplateOptions, error) {
	return f.templates, nil
}

func (f *fakeRepository) ProviderRollup(context.Context, time.Time) (map[string]biz.ProviderRollup, int64, error) {
	return map[string]biz.ProviderRollup{}, 0, nil
}

func (f *fakeRepository) ExternalToday(context.Context, time.Time) (int64, error) { return 0, nil }

func (f *fakeRepository) Mutate(_ context.Context, id string, mutation biz.Mutation) error {
	f.mutationID, f.mutation = id, mutation
	return nil
}

func (f *fakeRepository) BatchHide(_ context.Context, ids []string) error {
	f.batchIDs = append([]string(nil), ids...)
	return nil
}

type fakeFleet struct {
	status biz.GPUStatus
}

func (f fakeFleet) ImageProviderStatus(context.Context) (biz.GPUStatus, *biz.RecentThroughput, error) {
	return f.status, nil, nil
}

func (f fakeFleet) ImageTodayByGPU(context.Context, time.Time) ([]biz.NodeOutput, error) {
	return nil, nil
}
