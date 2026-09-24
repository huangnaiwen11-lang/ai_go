// Package adminimage exposes the legacy image-admin projection over HTTP.
package adminimage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	biz "ai-business-service/internal/biz/adminimage"
	"ai-business-service/internal/transport/adminauth"
)

const (
	imagesPath       = "/api/admin/images"
	maxBatchHideBody = 64 << 10
)

type authorizer func(*http.Request) error

type Handler struct {
	usecase   *biz.Usecase
	authorize authorizer
}

func NewHandler(usecase *biz.Usecase, authorize func(*http.Request) error) http.Handler {
	return &Handler{usecase: usecase, authorize: authorize}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.usecase == nil || h.authorize == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Admin image handler unavailable")
		return
	}
	if err := h.authorize(r); err != nil {
		adminauth.WriteDenial(w, err)
		return
	}
	if r == nil || r.URL == nil {
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This image API has not been migrated to the Go Gateway")
		return
	}

	switch {
	case r.URL.Path == imagesPath:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.list(w, r)
	case r.URL.Path == imagesPath+"/stats":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.stats(w, r)
	case r.URL.Path == imagesPath+"/template-options":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.templateOptions(w, r)
	case r.URL.Path == imagesPath+"/provider-status":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.providerStatus(w, r)
	case r.URL.Path == imagesPath+"/today-by-gpu":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.todayByGPU(w, r)
	case r.URL.Path == imagesPath+"/batch-hide":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.batchHide(w, r)
	default:
		h.singleMutationOrNotMigrated(w, r)
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	query, err := listQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := h.usecase.List(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	images := make([]map[string]any, 0, len(result.Images))
	for _, image := range result.Images {
		images = append(images, imageView(image))
	}
	success(w, map[string]any{
		"images": images,
		"pagination": map[string]any{
			"page": result.Page, "limit": result.Limit, "total": result.Total, "totalPages": result.TotalPages,
		},
	})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	result, err := h.usecase.Stats(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	success(w, map[string]any{"active": result.Active, "hidden": result.Hidden, "deleted": result.Deleted, "total": result.Total, "today": result.Today})
}

func (h *Handler) templateOptions(w http.ResponseWriter, r *http.Request) {
	result, err := h.usecase.TemplateOptions(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	options := make([]map[string]any, 0, len(result.Options))
	for _, option := range result.Options {
		options = append(options, map[string]any{"value": option.Value, "count": option.Count})
	}
	success(w, map[string]any{"options": options, "totals": map[string]any{"withTemplate": result.WithTemplate, "withoutTemplate": result.WithoutTemplate}})
}

func (h *Handler) providerStatus(w http.ResponseWriter, r *http.Request) {
	result, err := h.usecase.ProviderStatus(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	success(w, providerStatusView(result))
}

func (h *Handler) todayByGPU(w http.ResponseWriter, r *http.Request) {
	result, err := h.usecase.TodayByGPU(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	byNode := make([]map[string]any, 0, len(result.ByNode))
	for _, node := range result.ByNode {
		byNode = append(byNode, map[string]any{
			"nodeUrl": node.NodeURL, "origin": node.Origin, "imagePool": node.ImagePool, "count": node.Count,
			"avgMs": node.AvgMS, "imageProfile": node.ImageProfile, "quality": node.Quality,
		})
	}
	success(w, map[string]any{
		"todayStart": result.TodayStart.Format(time.RFC3339), "byNode": byNode,
		"external": map[string]any{"provider": "a2e", "count": result.External}, "total": result.Total,
	})
}

func (h *Handler) batchHide(w http.ResponseWriter, r *http.Request) {
	ids, err := decodeBatchHide(r)
	if err != nil {
		writeError(w, biz.ErrInvalidQuery)
		return
	}
	if err := h.usecase.BatchHide(r.Context(), ids); err != nil {
		writeError(w, err)
		return
	}
	success(w, map[string]any{"success": true, "count": len(ids)})
}

func (h *Handler) singleMutationOrNotMigrated(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, imagesPath+"/") {
		notMigrated(w)
		return
	}
	relative := strings.TrimPrefix(r.URL.Path, imagesPath+"/")
	parts := strings.Split(relative, "/")
	if len(parts) == 1 && r.Method == http.MethodDelete && validID(parts[0]) {
		h.mutate(w, r, parts[0], biz.MutationDelete)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost && validID(parts[0]) {
		switch parts[1] {
		case string(biz.MutationHide):
			h.mutate(w, r, parts[0], biz.MutationHide)
			return
		case string(biz.MutationRestore):
			h.mutate(w, r, parts[0], biz.MutationRestore)
			return
		}
	}
	notMigrated(w)
}

func (h *Handler) mutate(w http.ResponseWriter, r *http.Request, id string, mutation biz.Mutation) {
	if err := h.usecase.Mutate(r.Context(), id, mutation); err != nil {
		writeError(w, err)
		return
	}
	success(w, map[string]any{"success": true})
}

func listQuery(r *http.Request) (biz.ListQuery, error) {
	if r == nil || r.URL == nil {
		return biz.ListQuery{}, biz.ErrInvalidQuery
	}
	values := r.URL.Query()
	page, err := optionalPositiveInt(values, "page", 1, 1, 0)
	if err != nil {
		return biz.ListQuery{}, biz.ErrInvalidQuery
	}
	limit, err := optionalPositiveInt(values, "limit", 20, 1, 100)
	if err != nil {
		return biz.ListQuery{}, biz.ErrInvalidQuery
	}
	return biz.ListQuery{
		Status: valueOr(values, "status", "all"), Source: valueOr(values, "source", "all"),
		Template: valueOr(values, "template", "all"), ImagePool: valueOr(values, "imagePool", "all"),
		ClientSource: valueOr(values, "clientSource", "all"), MultiImage: valueOr(values, "multiImage", "all"),
		DateRange: valueOr(values, "dateRange", "all"), Page: page, Limit: limit,
	}, nil
}

func optionalPositiveInt(values map[string][]string, name string, fallback, minimum, maximum int) (int, error) {
	raw := values[name]
	if len(raw) == 0 || strings.TrimSpace(raw[0]) == "" {
		return fallback, nil
	}
	if len(raw) != 1 {
		return 0, errors.New("multiple values")
	}
	value, err := strconv.Atoi(raw[0])
	if err != nil || value < minimum || (maximum > 0 && value > maximum) {
		return 0, errors.New("invalid integer")
	}
	return value, nil
}

func valueOr(values map[string][]string, name, fallback string) string {
	if raw := values[name]; len(raw) > 0 && strings.TrimSpace(raw[0]) != "" {
		return raw[0]
	}
	return fallback
}

func decodeBatchHide(r *http.Request) ([]string, error) {
	if r == nil || r.Body == nil {
		return nil, errors.New("missing body")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBatchHideBody+1))
	if err != nil || len(raw) > maxBatchHideBody || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return nil, errors.New("invalid body")
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	if len(body.IDs) == 0 {
		return nil, errors.New("empty ids")
	}
	for _, id := range body.IDs {
		if !validID(id) {
			return nil, errors.New("invalid id")
		}
	}
	return body.IDs, nil
}

func validID(id string) bool { return strings.TrimSpace(id) != "" && !strings.Contains(id, "/") }

func imageView(image biz.Image) map[string]any {
	var creator any
	if image.Creator != nil {
		creator = map[string]any{"id": image.Creator.ID, "email": image.Creator.Email, "displayName": image.Creator.DisplayName, "avatarUrl": image.Creator.AvatarURL}
	}
	return map[string]any{
		"id": image.ID, "prompt": image.Prompt, "imageUrl": image.ImageURL, "style": image.Style,
		"status": image.Status, "isPublic": image.IsPublic, "likes": image.Likes, "views": image.Views,
		"source": image.Source, "apiProvider": image.APIProvider, "imagePool": image.ImagePool,
		"imageProfile": image.ImageProfile, "comfyNode": image.ComfyNode, "templateTitle": image.TemplateTitle,
		"generationMs": image.GenerationMS, "additionalImageCount": image.AdditionalImageCount,
		"clientSource": image.ClientSource, "creator": creator, "createdAt": image.CreatedAt.Format(time.RFC3339),
	}
}

func providerStatusView(status biz.ProviderStatus) map[string]any {
	providers := make(map[string]map[string]any, len(status.Providers))
	for name, rollup := range status.Providers {
		providers[name] = map[string]any{"total1h": rollup.Total1H, "completed1h": rollup.Completed1H, "failed1h": rollup.Failed1H, "avgDurationSec": rollup.AvgDurationSec}
	}
	nodes := make([]map[string]any, 0, len(status.GPU.Nodes))
	for _, node := range status.GPU.Nodes {
		nodes = append(nodes, map[string]any{
			"id": node.ID, "label": node.Label, "url": node.URL, "role": node.Role, "imagePool": node.ImagePool,
			"imageProfile": node.ImageProfile, "pools": node.Pools, "ok": node.OK, "gpu": node.GPU,
			"vram_free": node.VRAMFree, "vram_total": node.VRAMTotal, "queue_depth": node.QueueDepth, "maxQueue": node.MaxQueue,
			"circuitState": node.CircuitState, "circuitCanRequest": node.CircuitCanRequest, "circuitFailures": node.CircuitFailures,
			"circuitLastReason": node.CircuitLastReason, "schedulable": node.Schedulable,
		})
	}
	gpu := map[string]any{
		"nodes": nodes, "reportedImageNodes": status.GPU.ReportedImageNodes, "fleetSnapshotOk": status.GPU.FleetSnapshotOK,
		"totalQueueDepth": status.GPU.TotalQueueDepth, "onGpuProcessing": status.GPU.OnGPUProcessing, "canAcceptJob": status.GPU.CanAcceptJob,
	}
	var throughput any
	if status.RecentThroughput != nil {
		throughput = map[string]any{
			"window": status.RecentThroughput.Window, "imagePerHour": status.RecentThroughput.ImagePerHour, "videoPerHour": status.RecentThroughput.VideoPerHour,
			"completedInWindow": status.RecentThroughput.CompletedInWindow, "failedInWindow": status.RecentThroughput.FailedInWindow, "failureRate": status.RecentThroughput.FailureRate,
		}
	}
	return map[string]any{"providers": providers, "generating": status.Generating, "gpu": gpu, "recentThroughput": throughput}
}

func success(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func failure(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	failure(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
}

func notMigrated(w http.ResponseWriter) {
	failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This image API has not been migrated to the Go Gateway")
}

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrInvalidQuery):
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid image request")
	case errors.Is(err, biz.ErrNotFound):
		failure(w, http.StatusNotFound, "NOT_FOUND", "Image not found")
	case errors.Is(err, biz.ErrUnavailable):
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	default:
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	}
}

var _ http.Handler = (*Handler)(nil)
