// Package notification 提供用户通知的查询、已读和删除接口。
//
// 通知只属于 Go 自有用户域。用户 ID 始终来自已验证会话，客户端不能通过
// query、请求体或路径参数读取、修改其他用户的通知。
package notification

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	biznotification "ai-business-service/internal/biz/notification"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

const notificationsPath = "/api/notifications"

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type handler struct {
	authenticator authenticator
	usecase       *biznotification.Usecase
}

// NewHandler 创建通知处理器。usecase 使用 Go 自有 Mongo 仓储，不连接旧 Node 通知表。
func NewHandler(authenticator authenticator, usecase *biznotification.Usecase) http.Handler {
	return &handler{authenticator: authenticator, usecase: usecase}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity, err := h.authenticate(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if r == nil || r.URL == nil || r.URL.EscapedPath() != r.URL.Path {
		writeClientError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == notificationsPath:
		h.list(w, r, identity.UserID)
	case r.Method == http.MethodGet && r.URL.Path == notificationsPath+"/unread-count":
		h.unreadCount(w, r, identity.UserID)
	case r.Method == http.MethodPost && r.URL.Path == notificationsPath+"/read-all":
		h.markAllRead(w, r, identity.UserID)
	case r.Method == http.MethodDelete && r.URL.Path == notificationsPath:
		h.clearRead(w, r, identity.UserID)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, notificationsPath+"/") && strings.HasSuffix(r.URL.Path, "/read"):
		h.markRead(w, r, identity.UserID)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, notificationsPath+"/"):
		h.delete(w, r, identity.UserID)
	default:
		writeClientError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
	}
}

func (h *handler) authenticate(r *http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if h == nil || h.authenticator == nil || h.usecase == nil {
		return nil, shared.ErrServiceUnavailable
	}
	identity, err := h.authenticator.Authenticate(r)
	if err != nil {
		return nil, err
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		return nil, shared.ErrUnauthenticated
	}
	return identity, nil
}

func (h *handler) list(w http.ResponseWriter, r *http.Request, userID string) {
	values := r.URL.Query()
	limit := 50
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeClientError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
			return
		}
		limit = parsed
	}
	unreadOnly := values.Get("unreadOnly") == "true"
	items, unread, err := h.usecase.List(r.Context(), biznotification.ListQuery{UserID: userID, Limit: limit, UnreadOnly: unreadOnly})
	if err != nil {
		writeError(w, err)
		return
	}
	writeSuccess(w, http.StatusOK, map[string]any{"items": itemViews(items), "unreadCount": unread})
}

func (h *handler) unreadCount(w http.ResponseWriter, r *http.Request, userID string) {
	items, unread, err := h.usecase.List(r.Context(), biznotification.ListQuery{UserID: userID, Limit: 1, UnreadOnly: true})
	_ = items
	if err != nil {
		writeError(w, err)
		return
	}
	writeSuccess(w, http.StatusOK, map[string]any{"count": unread})
}

func (h *handler) markAllRead(w http.ResponseWriter, r *http.Request, userID string) {
	count, err := h.usecase.MarkAllRead(r.Context(), userID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeSuccess(w, http.StatusOK, map[string]any{"updated": count})
}

// clearRead 仅清除已读通知；不接受 query 或 body 来改变删除范围。
func (h *handler) clearRead(w http.ResponseWriter, r *http.Request, userID string) {
	if r.URL.RawQuery != "" || r.ContentLength > 0 {
		writeClientError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	deleted, err := h.usecase.DeleteRead(r.Context(), userID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeSuccess(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (h *handler) markRead(w http.ResponseWriter, r *http.Request, userID string) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, notificationsPath+"/"), "/read")
	if id == "" || strings.Contains(id, "/") || r.URL.RawQuery != "" {
		writeClientError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	if err := h.usecase.MarkRead(r.Context(), userID, id); err != nil {
		writeError(w, err)
		return
	}
	writeSuccess(w, http.StatusOK, map[string]any{"id": id, "read": true})
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request, userID string) {
	id := strings.TrimPrefix(r.URL.Path, notificationsPath+"/")
	if id == "" || strings.Contains(id, "/") || r.URL.RawQuery != "" {
		writeClientError(w, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	if err := h.usecase.Delete(r.Context(), userID, id); err != nil {
		writeError(w, err)
		return
	}
	writeSuccess(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

func itemViews(items []biznotification.Item) []map[string]any {
	views := make([]map[string]any, 0, len(items))
	for _, item := range items {
		views = append(views, map[string]any{"id": item.ID, "userId": item.UserID, "type": item.Type, "title": item.Title, "body": item.Body, "data": item.Data, "read": item.Read, "readAt": item.ReadAt, "createdAt": item.CreatedAt})
	}
	return views
}

func writeSuccess(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func writeError(w http.ResponseWriter, err error) {
	var apiError *shared.APIError
	if errors.As(err, &apiError) {
		writeClientError(w, apiError.StatusCode(), apiError.Code(), apiError.Message())
		return
	}
	if errors.Is(err, biznotification.ErrInvalidInput) {
		writeClientError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	writeClientError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
}

func writeClientError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

var _ http.Handler = (*handler)(nil)
