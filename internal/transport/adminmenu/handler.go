// Package adminmenu 把管理后台的全局配置（菜单节点可见性）适配成 HTTP。
//
// 鉴权口径：GET 与 PUT 都要求 super_admin。
//
// 这是「改一次影响所有管理员」的全局配置 —— 普通 admin 即使拿到了任意一项
// permissions，也不该能改别人的侧栏。adminauth.Authorizer 放行 admin，
// 所以这里直接看 identity.Role，与 UTM 的处理同源。
//
// 语义上只读写**覆盖项**：后端不知道有哪些菜单节点，节点清单的真相源在
// ai-admin 的 routes/routeManifest.tsx。因此这里没有「节点列表」接口，
// 也不会替前端校验路径是否存在 —— 那会把清单复制到后端，一改就漂。
package adminmenu

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	biz "ai-business-service/internal/biz/adminmenu"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/adminauth"
	"ai-business-service/internal/transport/sessionauth"
)

const visibilityPath = "system/menu-visibility"

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type Handler struct {
	repository    biz.Repository
	authenticator authenticator
}

func NewHandler(repository biz.Repository, authenticator authenticator) http.Handler {
	return &Handler{repository: repository, authenticator: authenticator}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.repository == nil || h.authenticator == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Admin menu handler unavailable")
		return
	}
	actor, ok := h.authorize(w, r)
	if !ok {
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	if path != visibilityPath {
		// 不属于本投影的已知形状。如实报「未迁移」，不要伪装成别的错误。
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This admin menu API has not been migrated to the Go Gateway")
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.readVisibility(w, r)
	case http.MethodPut:
		h.saveVisibility(w, r, actor)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// authorize 验证 Go 自有会话并要求 super_admin。
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) (biz.Actor, bool) {
	identity, err := h.authenticator.Authenticate(r)
	if err != nil {
		adminauth.WriteDenial(w, err)
		return biz.Actor{}, false
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		adminauth.WriteDenial(w, shared.ErrUnauthenticated)
		return biz.Actor{}, false
	}
	if identity.Role != "super_admin" {
		adminauth.WriteDenial(w, adminauth.ErrForbidden)
		return biz.Actor{}, false
	}
	return biz.Actor{ID: identity.UserID, Role: identity.Role}, true
}

func (h *Handler) readVisibility(w http.ResponseWriter, r *http.Request) {
	visibility, err := h.repository.Get(r.Context())
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, visibilityView(visibility))
}

func (h *Handler) saveVisibility(w http.ResponseWriter, r *http.Request, actor biz.Actor) {
	payload, ok := decodePayload(w, r)
	if !ok {
		return
	}
	visibility, err := h.repository.Save(r.Context(), actor, payload.Overrides)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, visibilityView(visibility))
}

// visibilityPayload 是写入体。
//
// overrides 允许为空对象（含义是「全部按默认清单」），但不允许缺字段：
// 缺失与「显式清空」是两件事，后者是合法操作，前者基本是调用方写错了。
// 用指针区分，缺字段直接 400。
type visibilityPayload struct {
	Overrides *map[string]bool `json:"overrides"`
}

func decodePayload(w http.ResponseWriter, r *http.Request) (biz.Visibility, bool) {
	var payload visibilityPayload
	decoder := json.NewDecoder(r.Body)
	// 与 UTM 一致：未知字段直接拒绝，避免「多传一个字段」在两个实现下表现不同。
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid menu visibility payload")
		return biz.Visibility{}, false
	}
	if payload.Overrides == nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid menu visibility payload")
		return biz.Visibility{}, false
	}
	return biz.Visibility{Overrides: *payload.Overrides}, true
}

func visibilityView(visibility biz.Visibility) map[string]any {
	overrides := visibility.Overrides
	if overrides == nil {
		// 空配置要序列化成 {} 而不是 null —— 前端 `response.overrides ?? {}`
		// 虽然能兜住 null，但 null 会让「后端有没有配过」这件事看起来像异常。
		overrides = map[string]bool{}
	}
	return map[string]any{
		"overrides": overrides,
		"updatedAt": visibility.UpdatedAt,
		"updatedBy": visibility.UpdatedBy,
	}
}

func writeRepositoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrInvalid):
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid menu visibility overrides")
	default:
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Admin menu repository unavailable")
	}
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

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	failure(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
}
