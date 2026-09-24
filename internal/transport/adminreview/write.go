package adminreview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	biz "ai-business-service/internal/biz/adminreview"
)

const maxReviewWriteBody = 2 << 20

// serveWrite preserves the legacy admin paths while routing every mutation
// through the Go projection usecase.  The caller has already authenticated the
// request in Handler.ServeHTTP.
func serveWrite(w http.ResponseWriter, r *http.Request, usecase *biz.Usecase) {
	if usecase == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Admin review handler unavailable")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	mediaType := ""
	switch {
	case strings.HasPrefix(path, "image-review"):
		mediaType = biz.MediaTypeImage
		path = strings.TrimPrefix(path, "image-review")
	case strings.HasPrefix(path, "video-review"):
		mediaType = biz.MediaTypeVideo
		path = strings.TrimPrefix(path, "video-review")
	default:
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This review API has not been migrated to the Go Gateway")
		return
	}
	path = strings.TrimPrefix(path, "/")
	if mediaType == biz.MediaTypeVideo && path == "translate" && r.Method == http.MethodPost {
		videoDataUnavailable(w, "Prompt translation is not configured in the Go Gateway")
		return
	}
	// adminauth.Authorizer intentionally exposes only an error function.  Never
	// trust a caller-provided actor header: a successful authorization is the
	// only fact available at this boundary, so use a fixed audit principal.
	actor := "authenticated-admin"
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	body, err := decodeWriteBody(r)
	if err != nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review request body")
		return
	}
	if idempotencyKey == "" {
		idempotencyKey = derivedIdempotencyKey(actor, r.Method, mediaType, path, body)
	}

	switch {
	case path == "batch-review" && r.Method == http.MethodPost:
		handleBatchReview(w, r, usecase, mediaType, actor, idempotencyKey, body)
	case strings.HasSuffix(path, "/review") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(path, "/review")
		handleSingleReview(w, r, usecase, mediaType, id, actor, idempotencyKey, body)
	case strings.HasSuffix(path, "/prompt") && r.Method == http.MethodPatch:
		id := strings.TrimSuffix(path, "/prompt")
		handlePrompt(w, r, usecase, mediaType, id, actor, idempotencyKey, body)
	case r.Method == http.MethodDelete && path != "":
		handleDelete(w, r, usecase, mediaType, path, actor, idempotencyKey)
	default:
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This review API has not been migrated to the Go Gateway")
	}
}

type writeBody struct {
	Action          string   `json:"action"`
	Reason          string   `json:"reason"`
	Prompt          *string  `json:"prompt"`
	NegativePrompt  *string  `json:"negativePrompt"`
	ExpectedVersion *int64   `json:"expectedVersion"`
	Version         *int64   `json:"version"`
	ImageIDs        []string `json:"imageIds"`
	VideoIDs        []string `json:"videoIds"`
}

func decodeWriteBody(r *http.Request) (writeBody, error) {
	if r.Body == nil || r.Method == http.MethodDelete {
		return writeBody{}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxReviewWriteBody+1))
	if err != nil || len(raw) > maxReviewWriteBody {
		return writeBody{}, errors.New("request body too large")
	}
	var body writeBody
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return writeBody{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return writeBody{}, errors.New("request body must contain one JSON object")
	}
	return body, nil
}

func handleSingleReview(w http.ResponseWriter, r *http.Request, usecase *biz.Usecase, mediaType, id, actor, key string, body writeBody) {
	if id == "" || strings.Contains(id, "/") {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review item id")
		return
	}
	action := biz.ReviewAction(body.Action)
	if action != biz.ReviewActionApprove && action != biz.ReviewActionReject {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Action must be approve or reject")
		return
	}
	expected := int64(-1)
	if body.ExpectedVersion != nil {
		expected = *body.ExpectedVersion
	} else if body.Version != nil {
		expected = *body.Version
	}
	result, err := usecase.Review(r.Context(), biz.ReviewCommand{ItemID: id, MediaType: mediaType, Action: action, ActorID: actor, ExpectedVersion: expected, IdempotencyKey: key, Reason: body.Reason})
	if err != nil {
		writeWriteError(w, err)
		return
	}
	success(w, map[string]any{"message": result.Message, "item": reviewItemView(result.Item)})
}

func handleBatchReview(w http.ResponseWriter, r *http.Request, usecase *biz.Usecase, mediaType, actor, key string, body writeBody) {
	ids := body.ImageIDs
	if mediaType == biz.MediaTypeVideo {
		ids = body.VideoIDs
	}
	result, err := usecase.BatchReview(r.Context(), biz.BatchReviewCommand{ItemIDs: ids, MediaType: mediaType, Action: biz.ReviewAction(body.Action), ActorID: actor, IdempotencyKey: key, Reason: body.Reason})
	if err != nil {
		writeWriteError(w, err)
		return
	}
	success(w, map[string]any{"message": result.Message, "modifiedCount": result.ModifiedCount, "items": result.Items})
}

func handlePrompt(w http.ResponseWriter, r *http.Request, usecase *biz.Usecase, mediaType, id, actor, key string, body writeBody) {
	if id == "" || strings.Contains(id, "/") || (body.Prompt == nil && body.NegativePrompt == nil) {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Prompt is required")
		return
	}
	result, err := usecase.Review(r.Context(), biz.ReviewCommand{ItemID: id, MediaType: mediaType, Action: biz.ReviewActionPrompt, ActorID: actor, ExpectedVersion: versionOrAuto(body), IdempotencyKey: key, Prompt: body.Prompt, NegativePrompt: body.NegativePrompt})
	if err != nil {
		writeWriteError(w, err)
		return
	}
	success(w, map[string]any{"message": result.Message, "item": reviewItemView(result.Item)})
}

func handleDelete(w http.ResponseWriter, r *http.Request, usecase *biz.Usecase, mediaType, id, actor, key string) {
	if id == "" || strings.Contains(id, "/") {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid review item id")
		return
	}
	result, err := usecase.Review(r.Context(), biz.ReviewCommand{ItemID: id, MediaType: mediaType, Action: biz.ReviewActionDelete, ActorID: actor, ExpectedVersion: -1, IdempotencyKey: key})
	if err != nil {
		writeWriteError(w, err)
		return
	}
	success(w, map[string]any{"message": result.Message, "item": reviewItemView(result.Item)})
}

func versionOrAuto(body writeBody) int64 {
	if body.ExpectedVersion != nil {
		return *body.ExpectedVersion
	}
	if body.Version != nil {
		return *body.Version
	}
	return -1
}

func derivedIdempotencyKey(actor, method, mediaType, path string, body writeBody) string {
	payload, _ := json.Marshal(body)
	sum := sha256.Sum256([]byte(actor + "\x00" + method + "\x00" + mediaType + "\x00" + path + "\x00" + string(payload)))
	return "derived:" + hex.EncodeToString(sum[:])
}

func writeWriteError(w http.ResponseWriter, err error) {
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
