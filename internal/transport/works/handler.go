// Package works 提供由 Gateway 精确接管的用户本人作品历史 HTTP 接口。
//
// 它只使用 Go 会话身份和 Go 自有 Mongo 投影，绝不从客户端接受 userId，也不回退旧 Node。
package works

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-business-service/internal/biz/shared"
	bizworks "ai-business-service/internal/biz/works"
	"ai-business-service/internal/transport/sessionauth"
)

const (
	worksPath         = "/api/works"
	defaultWorksLimit = 50
	maxWorksLimit     = 100
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type worksReader interface {
	List(context.Context, bizworks.ListQuery) (*bizworks.Page, error)
	Get(context.Context, string, string) (*bizworks.Work, error)
}

type handler struct {
	authenticator authenticator
	reader        worksReader
}

// NewHandler 创建作品历史处理器；是否接管仍由 Gateway 的独立开关和精确路由开关决定。
func NewHandler(authenticator authenticator, reader worksReader) http.Handler {
	return &handler{authenticator: authenticator, reader: reader}
}

// ServeHTTP 只服务 GET /api/works 和 GET /api/works/:id。
func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if request == nil || request.URL == nil || request.URL.EscapedPath() != request.URL.Path {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}

	switch {
	case request.Method == http.MethodGet && request.URL.Path == worksPath:
		handler.list(writer, request, identity.UserID)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, worksPath+"/"):
		handler.detail(writer, request, identity.UserID)
	default:
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
	}
}

func (handler *handler) authenticate(request *http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if handler == nil || handler.authenticator == nil || handler.reader == nil {
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

func (handler *handler) list(writer http.ResponseWriter, request *http.Request, userID string) {
	query, err := parseListQuery(request, userID)
	if err != nil {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	page, err := handler.reader.List(request.Context(), query)
	if err != nil {
		writeError(writer, err)
		return
	}
	if page == nil {
		page = &bizworks.Page{Items: []bizworks.Work{}}
	}
	writeSuccess(writer, http.StatusOK, pageView(*page))
}

func (handler *handler) detail(writer http.ResponseWriter, request *http.Request, userID string) {
	if request.URL.RawQuery != "" || request.URL.ForceQuery {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	id := strings.TrimPrefix(request.URL.Path, worksPath+"/")
	if id == "" || strings.Contains(id, "/") {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	work, err := handler.reader.Get(request.Context(), userID, id)
	if err != nil {
		writeError(writer, err)
		return
	}
	if work == nil {
		writeError(writer, bizworks.ErrWorkNotFound)
		return
	}
	writeSuccess(writer, http.StatusOK, workView(*work))
}

func parseListQuery(request *http.Request, userID string) (bizworks.ListQuery, error) {
	if request == nil || request.URL == nil {
		return bizworks.ListQuery{}, bizworks.ErrInvalidQuery
	}
	values := request.URL.Query()
	// kind 缺失代表前端“全部作品”筛选；只有显式提供时才校验其白名单值。
	kindRaw, err := parseOptionalSingle(values["kind"])
	if err != nil {
		return bizworks.ListQuery{}, err
	}
	kind := bizworks.Kind(kindRaw)
	if kind != "" && kind != bizworks.KindImage && kind != bizworks.KindVideo {
		return bizworks.ListQuery{}, bizworks.ErrInvalidQuery
	}
	limit, err := parseBoundedInteger(values["limit"], defaultWorksLimit, 1, maxWorksLimit)
	if err != nil {
		return bizworks.ListQuery{}, err
	}
	cursor, err := parseOptionalSingle(values["cursor"])
	if err != nil {
		return bizworks.ListQuery{}, err
	}
	if cursor != "" {
		if _, err := bizworks.ParseCursor(cursor); err != nil {
			return bizworks.ListQuery{}, bizworks.ErrInvalidQuery
		}
	}
	return bizworks.ListQuery{UserID: userID, Kind: kind, Limit: limit, Cursor: cursor}, nil
}

func parseRequiredSingle(values []string) (string, error) {
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", bizworks.ErrInvalidQuery
	}
	return values[0], nil
}

func parseOptionalSingle(values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 {
		return "", bizworks.ErrInvalidQuery
	}
	return values[0], nil
}

func parseBoundedInteger(values []string, fallback, minimum, maximum int) (int, error) {
	if len(values) == 0 {
		return fallback, nil
	}
	if len(values) != 1 || values[0] == "" {
		return 0, bizworks.ErrInvalidQuery
	}
	value, err := strconv.Atoi(values[0])
	if err != nil || value < minimum || value > maximum {
		return 0, bizworks.ErrInvalidQuery
	}
	return value, nil
}

func pageView(page bizworks.Page) map[string]any {
	items := make([]map[string]any, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, workView(item))
	}
	var nextCursor any
	if page.NextCursor != "" {
		nextCursor = page.NextCursor
	}
	return map[string]any{"items": items, "nextCursor": nextCursor}
}

// workView 是唯一对外投影点，刻意不从 CreationDocument 直接编码，防止新增内部字段意外泄露。
func workView(work bizworks.Work) map[string]any {
	view := map[string]any{
		"id":              work.ID,
		"kind":            string(work.Kind),
		"status":          work.Status,
		"templateId":      optionalString(work.TemplateID),
		"templateVersion": work.TemplateVersion,
		"createdAt":       work.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updatedAt":       work.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"resultUrl":       nil,
		"error":           nil,
	}
	if work.Kind == bizworks.KindVideo {
		view["durationSeconds"] = work.DurationSeconds
	}
	if work.Status == "succeeded" && work.ResultURL != "" {
		view["resultUrl"] = work.ResultURL
	}
	if work.Status != "succeeded" && work.Error != "" {
		view["error"] = work.Error
	}
	return view
}

func optionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func writeError(writer http.ResponseWriter, err error) {
	var apiError *shared.APIError
	if errors.As(err, &apiError) {
		writeClientError(writer, apiError.StatusCode(), apiError.Code(), apiError.Message())
		return
	}
	switch {
	case errors.Is(err, bizworks.ErrInvalidQuery):
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
	case errors.Is(err, bizworks.ErrWorkNotFound):
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
	case errors.Is(err, bizworks.ErrResultUnavailable):
		writeClientError(writer, http.StatusServiceUnavailable, "WORK_RESULT_UNAVAILABLE", "Work result unavailable")
	default:
		writeClientError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	}
}

func writeSuccess(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
}

func writeClientError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

var _ http.Handler = (*handler)(nil)
