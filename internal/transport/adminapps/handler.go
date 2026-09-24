// Package adminapps 把管理后台的 Apps 投影适配成 HTTP。
//
// 鉴权口径：读（GET）要求 admin / super_admin，与其它只读投影一致；
// 写（POST / PATCH / DELETE）要求 **super_admin**。
//
// 为什么写比读严：Node 的写接口各自挂 `requireAppFactoryAccess` /
// `requireAppScopedOperator` / `PERMISSIONS.MANAGE_APPS` 这类细粒度权限，
// 而 Go 侧没有管理权限模型，无法判定这些门禁。判不了的时候 fail-closed 的选择是
// 要求**最高**角色，而不是退到最弱的 admin —— 后者等于把「有 admin 角色」
// 直接放大成「能增删 App」，而 App 记录是后续平台配置、域名、支付分成的挂载点。
package adminapps

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	biz "ai-business-service/internal/biz/adminapps"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/adminauth"
	"ai-business-service/internal/transport/sessionauth"
)

// maxWriteBodyBytes 是写请求体的上限。
//
// nativeBuild 里的两个文件字段各有 20000 字符上限，加上 nativeConfig / serverIntegrations，
// 正常请求远小于 1 MiB。设上限是为了不把「读多少由调用方决定」留给外部。
const maxWriteBodyBytes = 2 << 20

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
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Apps admin handler unavailable")
		return
	}

	// 先鉴权再分派：匿名调用者不该从「501 还是 404」里推断出哪些端点存在。
	// 平台配置包含跨 App 的运行时策略，即使读取也只给 super_admin。
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	requireSuperAdmin := isWriteMethod(r.Method) || path == "config/platforms" || strings.HasPrefix(path, "config/platforms/")
	actor, ok := h.authorize(w, r, requireSuperAdmin)
	if !ok {
		return
	}

	switch {
	case path == "apps":
		switch r.Method {
		case http.MethodGet:
			h.list(w, r)
		case http.MethodPost:
			h.create(w, r, actor)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
	case path == "apps/overview":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.overview(w, r)
	case strings.HasPrefix(path, "apps/"):
		h.dispatchApp(w, r, actor, strings.TrimPrefix(path, "apps/"))
	case path == "config/platforms" || strings.HasPrefix(path, "config/platforms/"):
		h.dispatchPlatforms(w, r, actor, strings.TrimPrefix(path, "config/platforms"))
	default:
		notMigrated(w)
	}
}

func (h *Handler) dispatchApp(w http.ResponseWriter, r *http.Request, actor biz.Actor, id string) {
	parts := strings.Split(id, "/")
	if len(parts) > 1 {
		h.dispatchAppResource(w, r, actor, parts)
		return
	}
	if strings.Contains(id, "/") {
		// apps/<id>/<子资源> 尚未迁移。必须如实报 501，不能伪装成 404「应用不存在」——
		// 把「没实现」说成「资源缺失」会让前端与排障都得出错误结论。
		notMigrated(w)
		return
	}
	if id == "" {
		// Node 的 `/admin/apps/` 不匹配 `/:id` 路由，落到 404。
		failure(w, http.StatusNotFound, "NOT_FOUND", "App not found")
		return
	}
	if !biz.IsObjectID(id) {
		// Node 的 idParams 是 `^[a-fA-F0-9]{24}$`：格式不对是 400（参数非法），
		// 不是 404（资源不存在）。两者对调用方的含义不同。
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Apps request")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.get(w, r, id)
	case http.MethodPatch:
		h.update(w, r, actor, id)
	case http.MethodDelete:
		h.delete(w, r, actor, id)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
	}
}

func (h *Handler) dispatchAppResource(w http.ResponseWriter, r *http.Request, actor biz.Actor, parts []string) {
	id := parts[0]
	if !biz.IsObjectID(id) {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Apps request")
		return
	}
	resource := strings.Join(parts[1:], "/")
	switch resource {
	case "package-config":
		switch r.Method {
		case http.MethodGet:
			h.packageConfig(w, r, id)
		case http.MethodPut:
			payload, ok := decodeWritePayload(w, r)
			if !ok {
				return
			}
			app, config, err := h.repository.UpdatePackageConfig(r.Context(), actor, id, payload)
			if err != nil {
				writeRepositoryError(w, err)
				return
			}
			success(w, http.StatusOK, map[string]any{"app": app.Fields, "packageConfig": config})
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
	case "native-config-template":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		value, err := h.repository.NativeConfigTemplate(r.Context(), id)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, map[string]any{"nativeConfig": value})
	case "review-mode":
		if r.Method != http.MethodPatch {
			methodNotAllowed(w, http.MethodPatch)
			return
		}
		payload, ok := decodeWritePayload(w, r)
		if !ok {
			return
		}
		value, err := h.repository.SetReviewMode(r.Context(), actor, id, payload)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, value)
	case "integration-check":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		value, err := h.repository.IntegrationCheck(r.Context(), id)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, value)
	case "build-request":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		payload, ok := decodeWritePayload(w, r)
		if !ok {
			return
		}
		value, err := h.repository.CreateBuildRequest(r.Context(), actor, id, payload)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusAccepted, value)
	case "api-domain/candidates":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		payload, ok := decodeWritePayload(w, r)
		if !ok {
			return
		}
		value, err := h.repository.APIDomainCandidates(r.Context(), id, payload)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, value)
	case "api-domain/register":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		payload, ok := decodeWritePayload(w, r)
		if !ok {
			return
		}
		value, err := h.repository.RegisterAPIDomain(r.Context(), actor, id, payload)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusAccepted, value)
	default:
		h.dispatchLegalPages(w, r, actor, id, resource)
	}
}

func (h *Handler) dispatchLegalPages(w http.ResponseWriter, r *http.Request, actor biz.Actor, id, resource string) {
	if resource == "legal-pages" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		value, err := h.repository.LegalPages(r.Context(), id)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, value)
		return
	}
	if resource == "legal-pages/publishing-binding" {
		if r.Method != http.MethodPut {
			methodNotAllowed(w, http.MethodPut)
			return
		}
		payload, ok := decodeWritePayload(w, r)
		if !ok {
			return
		}
		value, err := h.repository.UpdateLegalPublishing(r.Context(), actor, id, payload)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, value)
		return
	}
	if resource == "legal-pages/publishing-binding/managed" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		value, err := h.repository.EnableManagedLegalPublishing(r.Context(), actor, id)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusAccepted, value)
		return
	}
	if resource == "legal-pages/publishing-binding/verify" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		writeRepositoryError(w, fmt.Errorf("%w: publisher binding live verification requires an external origin", biz.ErrExternalUnavailable))
		return
	}
	parts := strings.Split(resource, "/")
	if len(parts) < 2 || parts[0] != "legal-pages" || !biz.IsLegalPageType(parts[1]) {
		notMigrated(w)
		return
	}
	kind := parts[1]
	if len(parts) == 2 && r.Method == http.MethodDelete {
		payload, ok := decodeWritePayload(w, r)
		if !ok {
			return
		}
		if payload["confirmation"] != "DISABLE_LEGAL_PAGE" {
			failure(w, http.StatusBadRequest, "INVALID_REQUEST", "confirmation must be DISABLE_LEGAL_PAGE")
			return
		}
		value, err := h.repository.LegalAction(r.Context(), actor, id, "disable", kind, payload)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, value)
		return
	}
	// HTML upload, content readback, public-origin verification, and rollback
	// are intentionally unavailable without the frozen R2/legal-origin setup.
	if (len(parts) == 2 && r.Method == http.MethodPost) || (len(parts) == 3 && r.Method == http.MethodPost && (parts[2] == "verify" || parts[2] == "rollback")) {
		writeRepositoryError(w, fmt.Errorf("%w: legal page storage and public verification are not configured", biz.ErrExternalUnavailable))
		return
	}
	notMigrated(w)
}

func (h *Handler) packageConfig(w http.ResponseWriter, r *http.Request, id string) {
	config, err := h.repository.PackageConfig(r.Context(), id)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, http.StatusOK, config)
}

// authorize 验证 Go 自有会话并返回执行者身份。
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, requireSuperAdmin bool) (biz.Actor, bool) {
	identity, err := h.authenticator.Authenticate(r)
	if err != nil {
		adminauth.WriteDenial(w, err)
		return biz.Actor{}, false
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		adminauth.WriteDenial(w, shared.ErrUnauthenticated)
		return biz.Actor{}, false
	}
	if identity.Role != "admin" && identity.Role != "super_admin" {
		adminauth.WriteDenial(w, adminauth.ErrForbidden)
		return biz.Actor{}, false
	}
	if requireSuperAdmin && identity.Role != "super_admin" {
		adminauth.WriteDenial(w, adminauth.ErrForbidden)
		return biz.Actor{}, false
	}
	return biz.Actor{ID: identity.UserID, Role: identity.Role}, true
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	overview, err := h.repository.Overview(r.Context())
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, http.StatusOK, map[string]any{
		"total": overview.Total, "active": overview.Active,
		"suspended": overview.Suspended, "byPlatform": overview.ByPlatform,
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	query, err := parseQuery(r)
	if err != nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Apps query")
		return
	}
	page, err := h.repository.List(r.Context(), query)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(page.Apps))
	for _, app := range page.Apps {
		rows = append(rows, app.Fields)
	}
	success(w, http.StatusOK, map[string]any{
		"apps": rows,
		"pagination": map[string]any{
			"page": query.Page, "limit": query.Limit, "total": page.Total,
			"totalPages": (page.Total + int64(query.Limit) - 1) / int64(query.Limit),
		},
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, id string) {
	app, err := h.repository.Get(r.Context(), id)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, http.StatusOK, map[string]any{"app": app.Fields})
}

// create 对应 Node `POST /admin/apps`：成功是 **201**，响应体形如 `{app}`。
func (h *Handler) create(w http.ResponseWriter, r *http.Request, actor biz.Actor) {
	payload, ok := decodeWritePayload(w, r)
	if !ok {
		return
	}
	app, err := h.repository.Create(r.Context(), actor, payload)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, http.StatusCreated, map[string]any{"app": app.Fields})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request, actor biz.Actor, id string) {
	payload, ok := decodeWritePayload(w, r)
	if !ok {
		return
	}
	app, err := h.repository.Update(r.Context(), actor, id, payload)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, http.StatusOK, map[string]any{"app": app.Fields})
}

// delete 是软删除（status 置为 deprecated），响应体与 Node 一样是 `{success: true}`。
func (h *Handler) delete(w http.ResponseWriter, r *http.Request, actor biz.Actor, id string) {
	if _, err := h.repository.Delete(r.Context(), actor, id); err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, http.StatusOK, map[string]any{"success": true})
}

// decodeWritePayload 只做「是不是一个 JSON 对象」这一层判断。
//
// 字段级校验（未知键、长度、枚举）全部交给 biz —— 让校验规则只有一份，
// 不会出现「传输层放行、业务层拒绝」或反过来的分裂。
func decodeWritePayload(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWriteBodyBytes))
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Apps payload")
		return nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Apps payload")
		return nil, false
	}
	return payload, true
}

func parseQuery(r *http.Request) (biz.Query, error) {
	values := r.URL.Query()
	for _, value := range values {
		if len(value) != 1 || len(value[0]) > 512 {
			return biz.Query{}, biz.ErrInvalid
		}
	}
	query := biz.Query{Platform: values.Get("platform"), Status: values.Get("status"), Search: values.Get("search")}
	if raw := values.Get("page"); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil {
			return biz.Query{}, biz.ErrInvalid
		}
		query.Page = page
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			return biz.Query{}, biz.ErrInvalid
		}
		query.Limit = limit
	}
	return biz.NormalizeQuery(query)
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

func success(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
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

func notMigrated(w http.ResponseWriter) {
	failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This Apps API has not been migrated to the Go Gateway")
}

func writeRepositoryError(w http.ResponseWriter, err error) {
	// 冲突要带上 Node 的具体文案（'Bundle ID already registered' 之类），
	// 前端直接把 message 显示给管理员，统一成一句「冲突」等于丢掉排障信息。
	var conflict *biz.ConflictError
	if errors.As(err, &conflict) {
		failure(w, http.StatusConflict, "CONFLICT", conflict.Message)
		return
	}
	switch {
	case errors.Is(err, biz.ErrConflict):
		failure(w, http.StatusConflict, "CONFLICT", "App identifier already registered")
	case errors.Is(err, biz.ErrInvalid):
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid Apps request")
	case errors.Is(err, biz.ErrNotFound):
		failure(w, http.StatusNotFound, "NOT_FOUND", "App not found")
	case errors.Is(err, biz.ErrExternalUnavailable):
		failure(w, http.StatusServiceUnavailable, "EXTERNAL_INTEGRATION_UNAVAILABLE", err.Error())
	default:
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	}
}
