// Package adminutm 把后台 UTM 导量链接管理适配成 HTTP。
//
// 两条纪律写在这里，因为它们都是被实测踩出来的：
//
//  1. 授权口径必须与 Node 端 utm-links.admin.routes.js 一致 —— 五条路由（含两条读路由）
//     全部要求 super_admin。这里刻意不使用 adminauth.Authorizer（它放行 admin），
//     因为 Node 用的是 requireSuperAdmin；放宽会让同一条路径在两个实现下
//     得到不同的可访问性，属于语义分裂。
//
//  2. 响应形状在本层显式构造。Node 返回 camelCase（id / slug / utmSource / shortUrl…），
//     而 biz 模型没有 json tag，直接序列化会变成 PascalCase（ID / Slug / UtmSource）。
//     前端 UtmLinksPage 读的是 res.items 与 r.slug，PascalCase 会让它全部落空 ——
//     接口 200、表格却恒为空。这正是「能返回 HTTP 响应 ≠ 功能可用」的实例。
package adminutm

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	biz "ai-business-service/internal/biz/adminutm"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/adminauth"
	"ai-business-service/internal/transport/sessionauth"
)

const (
	linksPath   = "utm-links"
	sourcesPath = "utm-links/sources"
)

// objectIDPattern 与 Node 端 :id 参数的 ^[a-f0-9]{24}$ 校验一致。
var objectIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{24}$`)

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
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "UTM admin handler unavailable")
		return
	}
	actor, ok := h.authorize(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	if path == sourcesPath {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.listSources(w, r)
		return
	}
	if path == linksPath {
		switch r.Method {
		case http.MethodGet:
			h.listLinks(w, r)
		case http.MethodPost:
			h.createLink(w, r, actor)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return
	}
	id, found := strings.CutPrefix(path, linksPath+"/")
	if !found {
		// 不属于 utm-links 的任何已知形状。如实报「未迁移」，不要伪装成别的错误。
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This UTM API has not been migrated to the Go Gateway")
		return
	}
	if strings.Contains(id, "/") {
		// utm-links/<a>/<b>：Node 端不存在这种路由，Go 也没有迁移。
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This UTM API has not been migrated to the Go Gateway")
		return
	}
	if !objectIDPattern.MatchString(id) {
		// 单段但不是 24 位 hex：Node 端由 zod 在进入处理器之前挡下，返回 400。
		// 这是「入参非法」的真实陈述，不是把「没实现」伪装成别的状态码。
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid UTM link id")
		return
	}
	switch r.Method {
	case http.MethodPut:
		h.updateLink(w, r, actor, id)
	case http.MethodDelete:
		h.deleteLink(w, r, actor, id)
	default:
		// Node 端 :id 上只有 PUT / DELETE。GET 不存在，所以这不是「没迁移」，
		// 而是这个方法确实不被支持。
		methodNotAllowed(w, http.MethodPut, http.MethodDelete)
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

func (h *Handler) listLinks(w http.ResponseWriter, r *http.Request) {
	query := biz.Query{Keyword: r.URL.Query().Get("keyword")}
	if raw := r.URL.Query().Get("enabled"); raw != "" {
		enabled, ok := strictBool(raw)
		if !ok {
			failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid enabled")
			return
		}
		query.Enabled = &enabled
	}
	for key, target := range map[string]*int{"limit": &query.Limit, "skip": &query.Skip} {
		if raw := r.URL.Query().Get(key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid pagination")
				return
			}
			*target = n
		}
	}
	query, err := biz.NormalizeQuery(query)
	if err != nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid UTM query")
		return
	}
	page, err := h.repository.List(r.Context(), query)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, pageView(page))
}

func (h *Handler) listSources(w http.ResponseWriter, r *http.Request) {
	onlyEnabled := false
	if raw := r.URL.Query().Get("onlyEnabled"); raw != "" {
		var ok bool
		onlyEnabled, ok = strictBool(raw)
		if !ok {
			failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid onlyEnabled")
			return
		}
	}
	sources, err := h.repository.Sources(r.Context(), onlyEnabled)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, sourcesView(sources))
}

func (h *Handler) createLink(w http.ResponseWriter, r *http.Request, actor biz.Actor) {
	payload, ok := decodePayload(w, r)
	if !ok {
		return
	}
	normalized, err := biz.NormalizeInput(payload, true)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	link, err := h.repository.Create(r.Context(), actor, normalized)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, map[string]any{"link": linkView(link)})
}

func (h *Handler) updateLink(w http.ResponseWriter, r *http.Request, actor biz.Actor, id string) {
	payload, ok := decodePayload(w, r)
	if !ok {
		return
	}
	normalized, err := biz.NormalizeInput(payload, false)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	link, err := h.repository.Update(r.Context(), actor, id, normalized)
	if err != nil {
		writeRepositoryError(w, err)
		return
	}
	success(w, map[string]any{"link": linkView(link)})
}

func (h *Handler) deleteLink(w http.ResponseWriter, r *http.Request, actor biz.Actor, id string) {
	if err := h.repository.Delete(r.Context(), actor, id); err != nil {
		writeRepositoryError(w, err)
		return
	}
	// Node 端 deleteLink 返回 { id }，这里保持一致。
	success(w, map[string]any{"id": id})
}

// linkPayload 是写入请求的线格式。字段用指针以区分「未提供」与「显式置空」——
// 后台的启用开关只发 {enabled}，其余字段必须保持原值。
type linkPayload struct {
	Slug        *string           `json:"slug"`
	Label       *string           `json:"label"`
	TargetPath  *string           `json:"targetPath"`
	UtmSource   *string           `json:"utmSource"`
	UtmMedium   *string           `json:"utmMedium"`
	UtmCampaign *string           `json:"utmCampaign"`
	UtmContent  *string           `json:"utmContent"`
	UtmTerm     *string           `json:"utmTerm"`
	ExtraParams map[string]string `json:"extraParams"`
	Enabled     *bool             `json:"enabled"`
	Notes       *string           `json:"notes"`
}

func (p linkPayload) input() biz.Input {
	return biz.Input{
		Slug: p.Slug, Label: p.Label, TargetPath: p.TargetPath, UtmSource: p.UtmSource,
		UtmMedium: p.UtmMedium, UtmCampaign: p.UtmCampaign, UtmContent: p.UtmContent,
		UtmTerm: p.UtmTerm, Notes: p.Notes, ExtraParams: p.ExtraParams, Enabled: p.Enabled,
	}
}

// decodePayload 解码写入体。未知字段直接拒绝 —— Node 端的 zod schema 是 .strict()，
// 放行未知字段会让「前端多传了一个字段」在两个实现下表现不同。
func decodePayload(w http.ResponseWriter, r *http.Request) (biz.Input, bool) {
	var payload linkPayload
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid UTM link payload")
		return biz.Input{}, false
	}
	return payload.input(), true
}

// linkView 构造 Node 端的 toPublicDto 同款线格式。
func linkView(link biz.Link) map[string]any {
	extra := link.ExtraParams
	if extra == nil {
		extra = map[string]string{}
	}
	return map[string]any{
		"id": link.ID, "slug": link.Slug, "label": link.Label, "targetPath": link.TargetPath,
		"utmSource": link.UtmSource, "utmMedium": link.UtmMedium, "utmCampaign": link.UtmCampaign,
		"utmContent": link.UtmContent, "utmTerm": link.UtmTerm,
		"extraParams": extra, "enabled": link.Enabled, "notes": link.Notes, "clicks": link.Clicks,
		"lastClickAt": link.LastClickAt, "createdAt": link.CreatedAt, "updatedAt": link.UpdatedAt,
		"shortUrl": link.ShortURL, "fullUrl": link.FullURL,
	}
}

func pageView(page biz.Page) map[string]any {
	items := make([]map[string]any, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, linkView(item))
	}
	return map[string]any{"total": page.Total, "items": items}
}

func sourcesView(sources []biz.Source) map[string]any {
	rows := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		links := make([]map[string]any, 0, len(source.Links))
		for _, link := range source.Links {
			links = append(links, map[string]any{
				"id": link.ID, "slug": link.Slug, "label": link.Label, "enabled": link.Enabled,
				"utmMedium": link.UtmMedium, "utmCampaign": link.UtmCampaign,
				"utmContent": link.UtmContent, "utmTerm": link.UtmTerm,
			})
		}
		rows = append(rows, map[string]any{
			"utmSource": source.UtmSource, "enabled": source.Enabled,
			"totalClicks": source.TotalClicks, "links": links,
		})
	}
	return map[string]any{"sources": rows}
}

func strictBool(raw string) (bool, bool) {
	switch raw {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
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

func writeRepositoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrInvalid):
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid UTM request")
	case errors.Is(err, biz.ErrConflict):
		// Node 端重复 slug 走 ApiError.badRequest → 400。
		// 这里保持 400 而不是更符合直觉的 409，是为了让同一请求在两个实现下
		// 得到同样的状态码 —— 状态码分裂本身就是一种「口径分裂」。
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "UTM slug already taken")
	case errors.Is(err, biz.ErrNotFound):
		failure(w, http.StatusNotFound, "NOT_FOUND", "UTM link not found")
	default:
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	}
}
