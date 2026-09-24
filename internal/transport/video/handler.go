// Package video 提供 Gateway 精确分流后的模板视频 HTTP 适配器。
package video

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/shared"
	bizvideo "ai-business-service/internal/biz/video"
	"ai-business-service/internal/transport/sessionauth"

	"github.com/google/uuid"
)

const (
	createPath       = "/api/chat/video"
	statusesPath     = "/api/chat/videos/status"
	detailPathPrefix = "/api/chat/video/"
	maxRequestBytes  = 1 << 20
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type creator interface {
	Create(context.Context, bizvideo.CreateCommand) (*creations.CreateReservedResult, error)
}

type statusReader interface {
	List(context.Context, string, []string) ([]bizvideo.VideoView, error)
	Get(context.Context, string, string) (*bizvideo.VideoView, error)
}

type handler struct {
	authenticator authenticator
	creator       creator
	statuses      statusReader
}

// NewHandler 创建公开模板视频处理器；它不注册监听路由，仅由 Gateway 在三重开关后注入。
func NewHandler(authenticator authenticator, creator creator, statuses statusReader) http.Handler {
	return &handler{authenticator: authenticator, creator: creator, statuses: statuses}
}

// ServeHTTP 仅处理三条冻结视频路由。任何已接管请求的本地错误都直接返回，绝不回退 Node。
func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if request == nil || request.URL == nil {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == createPath:
		handler.create(writer, request, identity)
	case request.Method == http.MethodPost && request.URL.Path == statusesPath:
		handler.listStatuses(writer, request, identity.UserID)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, detailPathPrefix):
		handler.getStatus(writer, request, identity.UserID)
	default:
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
	}
}

func (handler *handler) authenticate(request *http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if handler == nil || handler.authenticator == nil {
		return nil, shared.ErrServiceUnavailable
	}
	identity, err := handler.authenticator.Authenticate(request)
	if err != nil {
		return nil, err
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		return nil, shared.ErrUnauthenticated
	}
	return identity, nil
}

type createRequest struct {
	TemplateID      string `json:"templateId"`
	ImageURL        string `json:"imageUrl,omitempty"`
	Prompt          string `json:"prompt,omitempty"`
	NegativePrompt  string `json:"negativePrompt,omitempty"`
	AspectRatio     string `json:"aspectRatio,omitempty"`
	DurationSeconds int32  `json:"durationSeconds,omitempty"`
	EnableAudio     bool   `json:"enableAudio,omitempty"`
}

func (handler *handler) create(writer http.ResponseWriter, request *http.Request, identity *sessionauth.AuthenticatedIdentity) {
	if handler.creator == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	var body createRequest
	if err := decodeStrictJSON(request, &body); err != nil || !validCreateRequest(body) {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	duration := body.DurationSeconds
	if duration == 0 {
		duration = 5
	}
	requestID := strings.TrimSpace(request.Header.Get("X-Request-Id"))
	if requestID == "" {
		requestID = uuid.NewString()
		request.Header.Set("X-Request-Id", requestID)
	}
	path := "text"
	if body.ImageURL != "" {
		path = "image"
	}
	result, err := handler.creator.Create(request.Context(), bizvideo.CreateCommand{
		UserID: identity.UserID, ContentAccess: identity.ContentAccess,
		IdempotencyKey: "gateway:video:" + path + ":" + requestID,
		TemplateID:     body.TemplateID,
		Input: bizvideo.TemplateVideoInput{
			UserImageURL: body.ImageURL, UserPrompt: body.Prompt, Duration: duration,
		},
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	if result == nil || result.Creation == nil || result.Reservation == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{
		"taskId": result.Creation.ID, "status": "generating", "coinsUsed": result.Reservation.PriceDiamonds,
		"coinsRemaining": result.DiamondBalanceAfter, "durationSeconds": duration,
		"message": "Video generation started. Use /api/chat/video/:taskId to check status.",
	})
}

func validCreateRequest(body createRequest) bool {
	if strings.TrimSpace(body.TemplateID) == "" || strings.TrimSpace(body.TemplateID) != body.TemplateID || body.EnableAudio || body.NegativePrompt != "" || body.AspectRatio != "" {
		return false
	}
	if body.DurationSeconds != 0 && body.DurationSeconds != 5 && body.DurationSeconds != 10 && body.DurationSeconds != 15 {
		return false
	}
	hasImage := body.ImageURL != ""
	hasPrompt := strings.TrimSpace(body.Prompt) != ""
	if hasImage == hasPrompt {
		return false
	}
	if !hasImage || strings.TrimSpace(body.ImageURL) != body.ImageURL {
		return !hasImage
	}
	parsed, err := url.Parse(body.ImageURL)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func (handler *handler) listStatuses(writer http.ResponseWriter, request *http.Request, userID string) {
	if handler.statuses == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	var body struct {
		TaskIDs []string `json:"taskIds"`
	}
	if err := decodeStrictJSON(request, &body); err != nil {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	views, err := handler.statuses.List(request.Context(), userID, body.TaskIDs)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"statuses": videoViews(views)})
}

func (handler *handler) getStatus(writer http.ResponseWriter, request *http.Request, userID string) {
	if handler.statuses == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	id := strings.TrimPrefix(request.URL.Path, detailPathPrefix)
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	view, err := handler.statuses.Get(request.Context(), userID, id)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, videoView(*view))
}

func videoViews(views []bizvideo.VideoView) []map[string]any {
	result := make([]map[string]any, 0, len(views))
	for _, view := range views {
		result = append(result, videoView(view))
	}
	return result
}

func videoView(view bizvideo.VideoView) map[string]any {
	result := map[string]any{"taskId": view.TaskID, "status": view.Status}
	if view.DurationSeconds != 0 {
		result["durationSeconds"] = view.DurationSeconds
	}
	if view.VideoURL != "" {
		result["videoUrl"] = view.VideoURL
	}
	if view.ErrorCode != "" {
		result["generationErrorCode"] = view.ErrorCode
		result["failedMessage"] = view.ErrorMessage
	}
	return result
}

func decodeStrictJSON(request *http.Request, target any) error {
	if request == nil || request.Body == nil {
		return errors.New("request body is required")
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxRequestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

func writeSuccess(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
}

func writeError(writer http.ResponseWriter, err error) {
	var contract interface {
		error
		StatusCode() int
		Code() string
		Message() string
		Details() any
	}
	if errors.As(err, &contract) {
		writeClientError(writer, contract.StatusCode(), contract.Code(), contract.Message())
		return
	}
	var apiError *shared.APIError
	if errors.As(err, &apiError) {
		writeClientError(writer, apiError.StatusCode(), apiError.Code(), apiError.Message())
		return
	}
	if errors.Is(err, bizvideo.ErrVideoNotFound) {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	// 与图片创建保持相同的幂等合同：同键不同请求没有产生二次预扣，
	// 调用方必须收到可识别的冲突，而不是被引导为参数修正后重试。
	if errors.Is(err, creations.ErrCreationCommandConflict) {
		writeClientError(writer, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency conflict")
		return
	}
	if errors.Is(err, bizvideo.ErrInvalidVideoStatusRequest) || errors.Is(err, bizvideo.ErrInvalidTemplateVideoRequest) {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	writeClientError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
}

func writeClientError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

var _ http.Handler = (*handler)(nil)
