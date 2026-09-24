// Package adminvideo serves the frozen admin video-review API from the
// Go-owned moderation projection.
package adminvideo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	biz "ai-business-service/internal/biz/adminreview"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/adminauth"
)

const maxWriteBody = 2 << 20

type Handler struct {
	usecase   *biz.Usecase
	authorize adminauth.ActorAuthorizer
}

// NewHandler reuses the projection usecase so list, stats, and every review
// mutation retain the existing projection and atomic-write guarantees. authorize
// must return the real server-verified admin ID used by review audits.
func NewHandler(usecase *biz.Usecase, authorize adminauth.ActorAuthorizer) http.Handler {
	return &Handler{usecase: usecase, authorize: authorize}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.usecase == nil || h.authorize == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Admin video handler unavailable")
		return
	}
	actorID, err := h.authorize(r)
	if err != nil {
		adminauth.WriteDenial(w, err)
		return
	}
	if strings.TrimSpace(actorID) == "" {
		adminauth.WriteDenial(w, shared.ErrUnauthenticated)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/admin/homepage/review-template/video/") {
		dataUnavailable(w, "Review template drafts require generation metadata not retained by the review projection")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	switch {
	case r.Method == http.MethodGet && path == "video-review":
		h.serveList(w, r)
	case r.Method == http.MethodGet && path == "video-review/stats":
		h.serveStats(w, r)
	case r.Method == http.MethodGet && path == "video-review/template-options":
		h.serveTemplateOptions(w, r)
	case r.Method == http.MethodGet && (path == "video-review/gpu-status" || path == "video-review/today-by-gpu"):
		dataUnavailable(w, "GPU node attribution is not retained by the review projection")
	case r.Method == http.MethodGet && isGenerationLogPath(path):
		dataUnavailable(w, "Generation logs are not retained by the review projection")
	case r.Method == http.MethodPost && path == "video-review/translate":
		h.serveTranslate(w, r)
	case r.Method == http.MethodPost && path == "video-review/batch-review":
		h.serveBatchReview(w, r, actorID)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "video-review/") && strings.HasSuffix(path, "/review"):
		h.serveSingleReview(w, r, actorID, strings.TrimSuffix(strings.TrimPrefix(path, "video-review/"), "/review"))
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "video-review/") && strings.HasSuffix(path, "/prompt"):
		h.servePrompt(w, r, actorID, strings.TrimSuffix(strings.TrimPrefix(path, "video-review/"), "/prompt"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "video-review/"):
		h.serveDelete(w, r, actorID, strings.TrimPrefix(path, "video-review/"))
	default:
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This video review API has not been migrated to the Go Gateway")
	}
}

func (h *Handler) serveList(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	if unsupportedVideoFilters(values.Get("multiImage"), values.Get("clientSource"), values.Get("videoPool")) {
		dataUnavailable(w, "This video filter requires generation metadata not retained by the review projection")
		return
	}
	page, limit, ok := parsePagination(w, r)
	if !ok {
		return
	}
	result, err := h.usecase.List(r.Context(), biz.ListQuery{
		MediaType: biz.MediaTypeVideo,
		Status:    valueOr(values.Get("status"), "all"),
		Source:    valueOr(values.Get("source"), "all"),
		Template:  valueOr(values.Get("template"), "all"),
		DateRange: valueOr(values.Get("dateRange"), "all"),
		SortBy:    valueOr(values.Get("sortBy"), "outputAt"),
		VideoPool: valueOr(values.Get("videoPool"), "all"),
		Page:      page,
		Limit:     limit,
	})
	if err != nil {
		writeReadError(w, err)
		return
	}
	videos := make([]map[string]any, 0, len(result.Items))
	for _, item := range result.Items {
		video, err := videoView(item)
		if err != nil {
			dataUnavailable(w, "Video review projection contains an item without required video facts")
			return
		}
		videos = append(videos, video)
	}
	success(w, map[string]any{
		"videos": videos,
		"pagination": map[string]any{
			"page": result.Page, "limit": result.Limit, "total": result.Total, "totalPages": result.TotalPages,
		},
	})
}

func (h *Handler) serveStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.usecase.Stats(r.Context(), biz.StatsQuery{MediaType: biz.MediaTypeVideo})
	if err != nil {
		writeReadError(w, err)
		return
	}
	success(w, map[string]any{
		"pending": stats.Pending, "approved": stats.Approved, "rejected": stats.Rejected,
		"deleted": stats.Deleted, "total": stats.Total, "todayCount": stats.Today,
	})
}

func (h *Handler) serveTemplateOptions(w http.ResponseWriter, r *http.Request) {
	result, err := h.usecase.VideoTemplateOptions(r.Context())
	if err != nil {
		writeReadError(w, err)
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

type translateBody struct{ Text *string }

func (h *Handler) serveTranslate(w http.ResponseWriter, r *http.Request) {
	var body translateBody
	if err := decodeJSONBody(r, &body); err != nil || body.Text == nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid translation request body")
		return
	}
	if *body.Text != "" {
		dataUnavailable(w, "Prompt translation is not configured in the Go Gateway")
		return
	}
	success(w, map[string]any{"translation": ""})
}

type reviewBody struct {
	Action          string
	Reason          string
	VideoIDs        []string
	Prompt          *string
	NegativePrompt  *string
	ExpectedVersion *int64
	Version         *int64
}

func (h *Handler) serveSingleReview(w http.ResponseWriter, r *http.Request, actorID, id string) {
	if !validItemID(id) {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review item id")
		return
	}
	body, key, ok := decodeReviewRequest(w, r, actorID)
	if !ok {
		return
	}
	action := biz.ReviewAction(body.Action)
	if action != biz.ReviewActionApprove && action != biz.ReviewActionReject {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Action must be approve or reject")
		return
	}
	result, err := h.usecase.Review(r.Context(), biz.ReviewCommand{
		ItemID: id, MediaType: biz.MediaTypeVideo, Action: action, ActorID: actorID,
		ExpectedVersion: versionOrAuto(body), IdempotencyKey: key, Reason: body.Reason,
	})
	if err != nil {
		writeMutationError(w, err)
		return
	}
	success(w, map[string]any{"message": result.Message})
}

func (h *Handler) serveBatchReview(w http.ResponseWriter, r *http.Request, actorID string) {
	body, key, ok := decodeReviewRequest(w, r, actorID)
	if !ok {
		return
	}
	result, err := h.usecase.BatchReview(r.Context(), biz.BatchReviewCommand{
		ItemIDs: body.VideoIDs, MediaType: biz.MediaTypeVideo, Action: biz.ReviewAction(body.Action),
		ActorID: actorID, IdempotencyKey: key, Reason: body.Reason,
	})
	if err != nil {
		writeMutationError(w, err)
		return
	}
	success(w, map[string]any{"modifiedCount": result.ModifiedCount})
}

func (h *Handler) servePrompt(w http.ResponseWriter, r *http.Request, actorID, id string) {
	if !validItemID(id) {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review item id")
		return
	}
	body, key, ok := decodeReviewRequest(w, r, actorID)
	if !ok {
		return
	}
	if body.Prompt == nil && body.NegativePrompt == nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Prompt is required")
		return
	}
	result, err := h.usecase.Review(r.Context(), biz.ReviewCommand{
		ItemID: id, MediaType: biz.MediaTypeVideo, Action: biz.ReviewActionPrompt, ActorID: actorID,
		ExpectedVersion: versionOrAuto(body), IdempotencyKey: key, Prompt: body.Prompt, NegativePrompt: body.NegativePrompt,
	})
	if err != nil {
		writeMutationError(w, err)
		return
	}
	success(w, map[string]any{"message": result.Message})
}

func (h *Handler) serveDelete(w http.ResponseWriter, r *http.Request, actorID, id string) {
	if !validItemID(id) {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review item id")
		return
	}
	key := idempotencyKey(r, actorID, reviewBody{})
	result, err := h.usecase.Review(r.Context(), biz.ReviewCommand{
		ItemID: id, MediaType: biz.MediaTypeVideo, Action: biz.ReviewActionDelete, ActorID: actorID,
		ExpectedVersion: -1, IdempotencyKey: key,
	})
	if err != nil {
		writeMutationError(w, err)
		return
	}
	success(w, map[string]any{"message": result.Message})
}

func decodeReviewRequest(w http.ResponseWriter, r *http.Request, actorID string) (reviewBody, string, bool) {
	var body reviewBody
	if err := decodeJSONBody(r, &body); err != nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review request body")
		return reviewBody{}, "", false
	}
	return body, idempotencyKey(r, actorID, body), true
}

func decodeJSONBody(r *http.Request, target any) error {
	if r.Body == nil {
		return errors.New("missing request body")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBody+1))
	if err != nil || len(raw) > maxWriteBody {
		return errors.New("request body too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func idempotencyKey(r *http.Request, actorID string, body reviewBody) string {
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
		return key
	}
	payload, _ := json.Marshal(body)
	sum := sha256.Sum256([]byte(actorID + "\x00" + r.Method + "\x00" + r.URL.Path + "\x00" + string(payload)))
	return "derived:" + hex.EncodeToString(sum[:])
}

func videoView(item biz.ReviewItem) (map[string]any, error) {
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.OutputRef) == "" || item.CreatedAt.IsZero() || !validReviewStatus(item.ReviewStatus) {
		return nil, errors.New("missing required video review fact")
	}
	source, provider, pool, err := videoSource(item.Source)
	if err != nil {
		return nil, err
	}
	generationStatus, err := videoGenerationStatus(item.GenerationStatus)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": item.ID, "videoUrl": item.OutputRef, "sourceImageUrl": nil,
		"userPrompt": nil, "prompt": nullableString(item.Prompt), "templateTitle": nullableString(item.TemplateTitle),
		"status": item.ReviewStatus, "generationStatus": generationStatus, "source": source,
		"videoInputMode": nil, "apiProvider": provider, "comfyNode": nil, "videoPool": pool,
		"videoProfile": nil, "generationSupplyLane": nil, "durationSeconds": nil, "enableAudio": nil,
		"additionalImageCount": nil, "referenceImageCount": nil, "videoWorkflowMode": nil, "clientSource": nil,
		"generationMs": nil, "user": nil, "reviewedAt": nullableTime(item.ReviewedAt),
		"rejectReason": nullableString(item.RejectReason), "createdAt": item.CreatedAt, "outputAt": nullableTime(item.OutputAt),
	}, nil
}

func videoSource(value string) (source string, provider any, pool any, err error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "legacy.animates", "animate":
		return "animate", "a2e", "external", nil
	case "legacy.faceswaptasks", "faceswap":
		return "faceswap", "a2e", "external", nil
	case "legacy.generatedvideos", "generated":
		return "generated", nil, nil, nil
	default:
		return "", nil, nil, errors.New("unsupported video source")
	}
}

func videoGenerationStatus(value string) (string, error) {
	switch value {
	case biz.GenerationStatusGenerating:
		return "generating", nil
	case biz.GenerationStatusSucceeded:
		return "completed", nil
	case biz.GenerationStatusFailed, biz.GenerationStatusCancelled:
		return "failed", nil
	default:
		return "", errors.New("unsupported generation status")
	}
}

func unsupportedVideoFilters(multiImage, clientSource, videoPool string) bool {
	if multiImage != "" && multiImage != "all" {
		return true
	}
	if clientSource != "" && clientSource != "all" {
		return true
	}
	switch videoPool {
	case "", "all", "external":
		return false
	default:
		return true
	}
}

func isGenerationLogPath(path string) bool {
	id := strings.TrimSuffix(strings.TrimPrefix(path, "video-review/"), "/log")
	return strings.HasPrefix(path, "video-review/") && strings.HasSuffix(path, "/log") && validItemID(id)
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

func validItemID(id string) bool { return strings.TrimSpace(id) != "" && !strings.Contains(id, "/") }

func validReviewStatus(value string) bool {
	switch value {
	case biz.ReviewStatusPending, biz.ReviewStatusApproved, biz.ReviewStatusRejected, biz.ReviewStatusDeleted:
		return true
	default:
		return false
	}
}

func versionOrAuto(body reviewBody) int64 {
	if body.ExpectedVersion != nil {
		return *body.ExpectedVersion
	}
	if body.Version != nil {
		return *body.Version
	}
	return -1
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func writeReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrInvalidQuery):
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review query")
	case errors.Is(err, biz.ErrProjectionUnavailable):
		failure(w, http.StatusServiceUnavailable, "REVIEW_PROJECTION_NOT_READY", "Review projection is not ready")
	default:
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	}
}

func writeMutationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrProjectionUnavailable):
		failure(w, http.StatusServiceUnavailable, "REVIEW_PROJECTION_NOT_READY", "Review projection is not ready")
	case errors.Is(err, biz.ErrReviewConflict):
		failure(w, http.StatusConflict, "REVIEW_CONFLICT", "Review item was changed; reload before retrying")
	case errors.Is(err, biz.ErrReviewNotFound):
		failure(w, http.StatusNotFound, "NOT_FOUND", "Review item not found")
	case errors.Is(err, biz.ErrReviewIdempotency):
		failure(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency key was already used for another review operation")
	case errors.Is(err, biz.ErrReviewInvalidCommand):
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review request")
	default:
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	}
}

func dataUnavailable(w http.ResponseWriter, message string) {
	failure(w, http.StatusNotImplemented, "ADMIN_API_DATA_UNAVAILABLE", message)
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
