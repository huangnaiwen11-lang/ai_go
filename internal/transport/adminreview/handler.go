// Package adminreview exposes the Go-owned moderation projection.
//
// Reads and moderation writes are served from the explicit Go projection.
package adminreview

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	biz "ai-business-service/internal/biz/adminreview"
	"ai-business-service/internal/transport/adminauth"
)

type authorizer func(*http.Request) error

type Handler struct {
	usecase   *biz.Usecase
	authorize authorizer
}

func NewHandler(repository biz.Repository, authorize func(*http.Request) error) http.Handler {
	return &Handler{usecase: biz.NewUsecase(repository), authorize: authorize}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.usecase == nil || h.authorize == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Admin review handler unavailable")
		return
	}
	if err := h.authorize(r); err != nil {
		adminauth.WriteDenial(w, err)
		return
	}
	if isVideoReviewTemplatePath(r.URL.Path) {
		videoDataUnavailable(w, "Review template drafts require generation metadata not retained by the review projection")
		return
	}
	if r.Method != http.MethodGet {
		serveWrite(w, r, h.usecase)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	if isVideoGenerationLogPath(path) {
		videoDataUnavailable(w, "Generation logs are not retained by the review projection")
		return
	}
	if isImageGenerationLogPath(path) {
		imageDataUnavailable(w, "Generation logs are not retained by the review projection")
		return
	}
	switch path {
	case "images":
		h.serveImagesList(w, r)
		return
	case "images/template-options":
		h.serveImageTemplateOptions(w, r)
		return
	case "images/provider-status":
		h.serveImageProviderStatus(w, r)
		return
	case "images/today-by-gpu":
		h.serveImageTodayByGPU(w, r)
		return
	case "video-review":
		h.serveVideosList(w, r)
		return
	case "video-review/template-options":
		h.serveVideoTemplateOptions(w, r)
		return
	case "video-review/gpu-status":
		h.serveVideoGPUStatus(w, r)
		return
	case "video-review/today-by-gpu":
		h.serveVideoTodayByGPU(w, r)
		return
	}
	var mediaType string
	switch {
	case path == "image-review" || path == "image-review/pending" || path == "image-review/stats":
		mediaType = biz.MediaTypeImage
	case path == "video-review" || path == "video-review/pending" || path == "video-review/stats":
		mediaType = biz.MediaTypeVideo
	default:
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This review API has not been migrated to the Go Gateway")
		return
	}
	if strings.HasSuffix(path, "/stats") {
		stats, err := h.usecase.Stats(r.Context(), biz.StatsQuery{MediaType: mediaType})
		if err != nil {
			writeError(w, err)
			return
		}
		success(w, map[string]any{"pending": stats.Pending, "approved": stats.Approved, "rejected": stats.Rejected, "deleted": stats.Deleted, "total": stats.Total, "todayCount": stats.Today})
		return
	}
	status := r.URL.Query().Get("status")
	if path == "image-review/pending" || path == "video-review/pending" {
		status = biz.ReviewStatusPending
	}
	page, limit, ok := parsePagination(w, r)
	if !ok {
		return
	}
	result, err := h.usecase.List(r.Context(), biz.ListQuery{MediaType: mediaType, Status: status, Page: page, Limit: limit})
	if err != nil {
		writeError(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(result.Items))
	for _, item := range result.Items {
		rows = append(rows, reviewItemView(item))
	}
	payload := map[string]any{"pagination": map[string]any{"page": result.Page, "limit": result.Limit, "total": result.Total, "totalPages": result.TotalPages}}
	if mediaType == biz.MediaTypeImage {
		payload["images"] = rows
	} else {
		videos := make([]map[string]any, 0, len(result.Items))
		for _, item := range result.Items {
			videos = append(videos, adminVideoView(item))
		}
		payload["videos"] = videos
	}
	success(w, payload)
}

func (h *Handler) serveImagesList(w http.ResponseWriter, r *http.Request) {
	page, limit, ok := parsePagination(w, r)
	if !ok {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "all"
	}
	status = imageListStatus(status)
	result, err := h.usecase.List(r.Context(), biz.ListQuery{MediaType: biz.MediaTypeImage, Status: status, Page: page, Limit: limit})
	if err != nil {
		writeError(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(result.Items))
	for _, item := range result.Items {
		rows = append(rows, adminImageView(item))
	}
	success(w, map[string]any{
		"images": rows,
		"pagination": map[string]any{
			"page": result.Page, "limit": result.Limit, "total": result.Total, "totalPages": result.TotalPages,
		},
	})
}

func (h *Handler) serveVideosList(w http.ResponseWriter, r *http.Request) {
	queryValues := r.URL.Query()
	if unsupportedVideoFilter(queryValues.Get("multiImage"), queryValues.Get("clientSource"), queryValues.Get("videoPool")) {
		failure(w, http.StatusNotImplemented, "ADMIN_API_DATA_UNAVAILABLE", "This video filter requires generation metadata not retained by the review projection")
		return
	}
	page, limit, ok := parsePagination(w, r)
	if !ok {
		return
	}
	result, err := h.usecase.List(r.Context(), biz.ListQuery{
		MediaType: biz.MediaTypeVideo, Status: valueOr(queryValues.Get("status"), "all"),
		Source: valueOr(queryValues.Get("source"), "all"), Template: valueOr(queryValues.Get("template"), "all"),
		DateRange: valueOr(queryValues.Get("dateRange"), "all"), SortBy: valueOr(queryValues.Get("sortBy"), "outputAt"),
		VideoPool: valueOr(queryValues.Get("videoPool"), "all"), Page: page, Limit: limit,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	videos := make([]map[string]any, 0, len(result.Items))
	for _, item := range result.Items {
		videos = append(videos, adminVideoView(item))
	}
	success(w, map[string]any{
		"videos": videos,
		"pagination": map[string]any{
			"page": result.Page, "limit": result.Limit, "total": result.Total, "totalPages": result.TotalPages,
		},
	})
}

func unsupportedVideoFilter(multiImage, clientSource, videoPool string) bool {
	if multiImage != "" && multiImage != "all" {
		return true
	}
	if clientSource != "" && clientSource != "all" {
		return true
	}
	switch videoPool {
	case "", "all", "external", "unknown":
		return false
	default:
		return true
	}
}

func isVideoGenerationLogPath(path string) bool {
	return strings.HasPrefix(path, "video-review/") && strings.HasSuffix(path, "/log")
}

func isImageGenerationLogPath(path string) bool {
	return strings.HasPrefix(path, "image-review/") && strings.HasSuffix(path, "/log")
}

func isVideoReviewTemplatePath(path string) bool {
	return strings.HasPrefix(path, "/api/admin/homepage/review-template/video/")
}

func videoDataUnavailable(w http.ResponseWriter, message string) {
	failure(w, http.StatusNotImplemented, "ADMIN_API_DATA_UNAVAILABLE", message)
}

func imageDataUnavailable(w http.ResponseWriter, message string) {
	failure(w, http.StatusServiceUnavailable, "ADMIN_API_DATA_UNAVAILABLE", message)
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (h *Handler) serveImageTemplateOptions(w http.ResponseWriter, r *http.Request) {
	result, err := h.usecase.ImageTemplateOptions(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	options := make([]map[string]any, 0, len(result.Options))
	for _, option := range result.Options {
		options = append(options, map[string]any{"value": option.Value, "count": option.Count})
	}
	success(w, map[string]any{
		"options": options,
		"totals":  map[string]any{"withTemplate": result.WithTemplate, "withoutTemplate": result.WithoutTemplate},
	})
}

func (h *Handler) serveImageProviderStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	runtime, err := h.usecase.ImageRuntime(r.Context(), biz.ImageRuntimeQuery{GeneratingSince: now.Add(-2 * time.Hour), TodayStart: shanghaiStartOfDay(now)})
	if err != nil {
		writeError(w, err)
		return
	}
	// The moderation projection records lifecycle state but not provider or GPU
	// attribution. Keep those facts explicitly unavailable instead of reporting
	// fabricated zeroes.
	success(w, map[string]any{"providers": map[string]any{}, "generating": runtime.Generating, "gpu": nil, "recentThroughput": nil})
}

func (h *Handler) serveImageTodayByGPU(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	runtime, err := h.usecase.ImageRuntime(r.Context(), biz.ImageRuntimeQuery{GeneratingSince: now.Add(-2 * time.Hour), TodayStart: shanghaiStartOfDay(now)})
	if err != nil {
		writeError(w, err)
		return
	}
	// Node attribution and quality evidence live in the generation platform, not
	// in this projection. FaceSwap is the only retained external image fact.
	success(w, map[string]any{
		"todayStart":         shanghaiStartOfDay(now),
		"byNode":             []any{},
		"external":           map[string]any{"provider": "a2e", "count": runtime.ExternalToday},
		"total":              runtime.ExternalToday,
		"genUnreachable":     true,
		"qualityUnreachable": true,
	})
}

func (h *Handler) serveVideoTemplateOptions(w http.ResponseWriter, r *http.Request) {
	result, err := h.usecase.VideoTemplateOptions(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	options := make([]map[string]any, 0, len(result.Options))
	for _, option := range result.Options {
		options = append(options, map[string]any{"value": option.Value, "count": option.Count})
	}
	success(w, map[string]any{
		"options": options,
		"totals":  map[string]any{"withTemplate": result.WithTemplate, "withoutTemplate": result.WithoutTemplate},
	})
}

func (h *Handler) serveVideoGPUStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	runtime, err := h.usecase.VideoRuntime(r.Context(), biz.VideoRuntimeQuery{GeneratingSince: now.Add(-2 * time.Hour), TodayStart: shanghaiStartOfDay(now)})
	if err != nil {
		writeError(w, err)
		return
	}
	// The review projection can count lifecycle state, but never records the
	// physical GPU registry, queues, or provider telemetry. fleetSnapshotOk
	// makes that absence visible to the frozen page without inventing a node.
	success(w, map[string]any{
		"comfyui": map[string]any{
			"nodes": []any{}, "reportedVideoNodes": 0, "fleetSnapshotOk": false,
			"totalQueueDepth": nil, "onGpuProcessing": nil, "canAcceptJob": nil, "maxQueue": nil,
		},
		"generating": runtime.Generating, "providerStats": map[string]any{}, "recentThroughput": nil,
	})
}

func (h *Handler) serveVideoTodayByGPU(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	todayStart := shanghaiStartOfDay(now)
	runtime, err := h.usecase.VideoRuntime(r.Context(), biz.VideoRuntimeQuery{GeneratingSince: now.Add(-2 * time.Hour), TodayStart: todayStart})
	if err != nil {
		writeError(w, err)
		return
	}
	// External A2E output is attributable from the retained source. GPU-node
	// output is intentionally left unavailable rather than collapsed into an
	// indistinguishable zero-count node.
	success(w, map[string]any{
		"todayStart": todayStart, "byNode": []any{},
		"external": map[string]any{"provider": "a2e", "count": runtime.ExternalToday},
		"total":    runtime.ExternalToday, "genUnreachable": true,
	})
}

func imageListStatus(value string) string {
	switch strings.TrimSpace(value) {
	case "active":
		return biz.ReviewStatusApproved
	case "hidden":
		return biz.ReviewStatusRejected
	default:
		return value
	}
}

func adminImageView(item biz.ReviewItem) map[string]any {
	source := adminImageSource(item.Source)
	provider, pool := "unknown", "unknown"
	if source == "faceswap" {
		provider, pool = "a2e", "external"
	}
	return map[string]any{
		"id": item.ID, "prompt": item.Prompt, "imageUrl": item.OutputRef,
		"status": item.ReviewStatus, "isPublic": item.VisibilityStatus == "public",
		"likes": 0, "views": 0, "source": source, "apiProvider": provider,
		"imagePool": pool, "imageProfile": nil, "comfyNode": nil,
		"templateTitle": nullableString(item.TemplateTitle),
		"generationMs":  imageGenerationMillis(item), "additionalImageCount": 0,
		"clientSource": nil, "createdAt": item.CreatedAt,
	}
}

func adminImageSource(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "legacy.faceswaptasks", "faceswap":
		return "faceswap"
	case "legacy.generatedimages", "generated":
		return "generated"
	case "t2i", "i2i", "undress", "doubleaction", "tryon", "sexpose":
		return value
	default:
		return "generated"
	}
}

func adminVideoView(item biz.ReviewItem) map[string]any {
	source := adminVideoSource(item.Source)
	provider, pool := "unknown", "unknown"
	if source == "animate" || source == "faceswap" {
		provider, pool = "a2e", "external"
	}
	return map[string]any{
		"id": item.ID, "videoUrl": item.OutputRef, "sourceImageUrl": "",
		"userPrompt": "", "prompt": item.Prompt,
		"templateTitle": nullableString(item.TemplateTitle),
		"status":        item.ReviewStatus, "generationStatus": adminVideoGenerationStatus(item.GenerationStatus),
		"source": source, "videoInputMode": nil, "apiProvider": provider,
		"comfyNode": nil, "videoPool": pool, "videoProfile": nil,
		"generationSupplyLane": nil, "durationSeconds": nil, "enableAudio": false,
		"additionalImageCount": 0, "referenceImageCount": 0, "videoWorkflowMode": nil,
		"clientSource": nil, "generationMs": imageGenerationMillis(item), "user": nil,
		"reviewedAt": nullableTime(item.ReviewedAt), "rejectReason": nullableString(item.RejectReason),
		"createdAt": item.CreatedAt, "outputAt": videoOutputAt(item),
	}
}

func adminVideoSource(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "legacy.animates", "animate":
		return "animate"
	case "legacy.faceswaptasks", "faceswap":
		return "faceswap"
	case "legacy.generatedvideos", "generated":
		return "generated"
	default:
		return "generated"
	}
}

func adminVideoGenerationStatus(value string) string {
	switch value {
	case biz.GenerationStatusGenerating:
		return "generating"
	case biz.GenerationStatusSucceeded:
		return "completed"
	default:
		return "failed"
	}
}

func videoOutputAt(item biz.ReviewItem) time.Time {
	if item.OutputAt.IsZero() {
		return item.CreatedAt
	}
	return item.OutputAt
}

func imageGenerationMillis(item biz.ReviewItem) any {
	if item.OutputAt.IsZero() || item.CreatedAt.IsZero() || item.OutputAt.Before(item.CreatedAt) {
		return nil
	}
	return item.OutputAt.Sub(item.CreatedAt).Milliseconds()
}

func shanghaiStartOfDay(now time.Time) time.Time {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		location = time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).UTC()
}

func parsePagination(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	page, limit := 1, 20
	if raw := r.URL.Query().Get("page"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid page")
			return 0, 0, false
		}
		page = value
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid limit")
			return 0, 0, false
		}
		limit = value
	}
	return page, limit, true
}

func reviewItemView(item biz.ReviewItem) map[string]any {
	return map[string]any{
		"id": item.ID, "assetId": item.AssetID, "creationId": item.CreationID,
		"userId": item.UserID, "source": item.Source, "mediaType": item.MediaType,
		"status": item.ReviewStatus, "reviewStatus": item.ReviewStatus,
		"visibilityStatus": item.VisibilityStatus, "generationStatus": item.GenerationStatus,
		"outputRef": item.OutputRef, "prompt": item.Prompt, "negativePrompt": item.NegativePrompt,
		"templateId": item.TemplateID, "templateTitle": item.TemplateTitle,
		"reviewedBy": nullableString(item.ReviewedBy), "reviewedAt": nullableTime(item.ReviewedAt),
		"rejectReason": nullableString(item.RejectReason), "version": item.Version,
		"createdAt": item.CreatedAt, "outputAt": nullableTime(item.OutputAt), "updatedAt": item.UpdatedAt,
	}
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableTime(value interface{ IsZero() bool }) any {
	if value.IsZero() {
		return nil
	}
	return value
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

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrInvalidQuery):
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review query")
	case errors.Is(err, biz.ErrProjectionUnavailable):
		failure(w, http.StatusServiceUnavailable, "REVIEW_PROJECTION_NOT_READY", "Review projection is not ready")
	default:
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	}
}
