// Package t2i 提供 Gateway 已精确分流后的公开文生图 HTTP 适配器。
package t2i

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/shared"
	bizt2i "ai-business-service/internal/biz/t2i"
	"ai-business-service/internal/transport/sessionauth"

	"github.com/google/uuid"
)

const (
	createPath       = "/api/chat/image/async"
	statusesPath     = "/api/images/statuses"
	detailPathPrefix = "/api/images/"
	maxRequestBytes  = 1 << 20
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type creator interface {
	Create(context.Context, bizt2i.CreateCommand) (*creations.CreateReservedResult, error)
}

type imageEditor interface {
	Create(context.Context, bizt2i.ImageEditCommand) (*creations.CreateReservedResult, error)
}

type statusReader interface {
	List(context.Context, string, []string) ([]bizt2i.ImageView, error)
	Get(context.Context, string, string) (*bizt2i.ImageView, error)
}

type handler struct {
	authenticator authenticator
	creator       creator
	imageEditor   imageEditor
	statuses      statusReader
}

// NewHandler 创建受 Gateway 双开关控制的 T2I 处理器；它本身不注册监听路由。
func NewHandler(authenticator authenticator, creator creator, statuses statusReader, editors ...imageEditor) http.Handler {
	var editor imageEditor
	if len(editors) == 1 {
		editor = editors[0]
	}
	return &handler{authenticator: authenticator, creator: creator, imageEditor: editor, statuses: statuses}
}

// ServeHTTP 处理三条冻结的公开 T2I 路由。任何本地错误均直接返回，绝不回退 Node。
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
	Prompt         string   `json:"prompt"`
	NegativePrompt string   `json:"negativePrompt,omitempty"`
	AspectRatio    string   `json:"aspectRatio,omitempty"`
	TemplateID     string   `json:"templateId,omitempty"`
	InputImages    []string `json:"inputImages,omitempty"`
	// 下列字段是前端既有图片请求的产品兼容字段。Go 只接受其冻结安全值，
	// 不将其传入配方、预扣或生成中台，避免客户端影响技术与结算事实。
	Operation      string   `json:"operation,omitempty"`
	PresetTags     []string `json:"presetTags,omitempty"`
	OptimizePrompt bool     `json:"optimizePrompt,omitempty"`
}

func (handler *handler) create(writer http.ResponseWriter, request *http.Request, identity *sessionauth.AuthenticatedIdentity) {
	if handler.creator == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	if identity == nil || identity.UserID == "" {
		writeError(writer, shared.ErrUnauthenticated)
		return
	}
	var body createRequest
	if err := decodeStrictJSON(request, &body); err != nil {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	if body.TemplateID == "" && strings.TrimSpace(body.Prompt) == "" {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	// Gateway 候选分类同样执行该白名单校验；Handler 再校验一次，确保装配测试或
	// 后续调用方不能绕过 HTTP 边界，把旧 operation、标签或提示词优化语义带入 Go。
	if (body.Operation != "" && body.Operation != "generate") || body.OptimizePrompt ||
		(body.AspectRatio != "" && !imageEditAspectRatioPattern.MatchString(body.AspectRatio)) ||
		(body.TemplateID != "" && len(body.PresetTags) != 0 && (len(body.PresetTags) != 1 || body.PresetTags[0] != body.TemplateID)) {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	requestID := strings.TrimSpace(request.Header.Get("X-Request-Id"))
	if requestID == "" {
		requestID = uuid.NewString()
		request.Header.Set("X-Request-Id", requestID)
	}
	var result *creations.CreateReservedResult
	var err error
	if body.TemplateID != "" || body.InputImages != nil {
		if handler.imageEditor == nil || body.TemplateID == "" || len(body.InputImages) != 1 {
			writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
			return
		}
		result, err = handler.imageEditor.Create(request.Context(), bizt2i.ImageEditCommand{UserID: identity.UserID, ContentAccess: identity.ContentAccess, IdempotencyKey: "gateway:i2i:" + requestID, TemplateID: body.TemplateID, UserImageURL: body.InputImages[0], UserPrompt: body.Prompt, AspectRatio: body.AspectRatio})
	} else {
		result, err = handler.creator.Create(request.Context(), bizt2i.CreateCommand{UserID: identity.UserID, IdempotencyKey: "gateway:t2i:" + requestID, Prompt: body.Prompt, NegativePrompt: body.NegativePrompt, AspectRatio: body.AspectRatio})
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	if result == nil || result.Creation == nil || len(result.Steps) != 1 || result.Steps[0].ID == "" || result.Reservation == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{
		"imageId": result.Creation.ID, "jobId": result.Steps[0].ID, "status": "generating",
		"cost": result.Reservation.PriceDiamonds, "coinsCharged": result.Reservation.ChargedDiamonds,
		"coinsRemaining": result.DiamondBalanceAfter,
		"creditsUsed":    map[string]int64{"imageCreditsUsed": 0, "videoCreditsUsed": 0},
		"billing":        map[string]any{"source": result.Reservation.Source, "coinsCharged": result.Reservation.ChargedDiamonds},
	})
}

var imageEditAspectRatioPattern = regexp.MustCompile(`^\d+:\d+$`)

func (handler *handler) listStatuses(writer http.ResponseWriter, request *http.Request, userID string) {
	if handler.statuses == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	var body struct {
		ImageIDs []string `json:"imageIds"`
	}
	if err := decodeStrictJSON(request, &body); err != nil {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	views, err := handler.statuses.List(request.Context(), userID, body.ImageIDs)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"statuses": imageViews(views)})
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
	writeSuccess(writer, http.StatusOK, map[string]any{"image": imageView(*view)})
}

func imageViews(views []bizt2i.ImageView) []map[string]any {
	result := make([]map[string]any, 0, len(views))
	for _, view := range views {
		result = append(result, imageView(view))
	}
	return result
}
func imageView(view bizt2i.ImageView) map[string]any {
	return map[string]any{"id": view.ID, "imageUrl": view.ImageURL, "generationStatus": view.GenerationStatus, "generationErrorCode": view.GenerationErrorCode, "generationErrorMessage": view.GenerationErrorMessage, "generationErrorDetail": view.GenerationErrorDetail}
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
	var apiError *shared.APIError
	if errors.As(err, &apiError) {
		writeClientError(writer, apiError.StatusCode(), apiError.Code(), apiError.Message())
		return
	}
	if errors.Is(err, bizt2i.ErrImageNotFound) {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	// 同一幂等键对应不同请求指纹时，创作没有被再次预扣；必须让调用方
	// 得到可重试策略明确的冲突，而不是把它误判为可修正的参数错误。
	if errors.Is(err, creations.ErrCreationCommandConflict) {
		writeClientError(writer, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency conflict")
		return
	}
	if errors.Is(err, bizt2i.ErrInvalidImageStatusRequest) || errors.Is(err, bizt2i.ErrInvalidImageEditRequest) {
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
