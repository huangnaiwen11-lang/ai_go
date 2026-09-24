// Package adminblog 把后台博客内容管理适配成 HTTP。
//
// 它只做协议转换与身份提取：授权要求 admin/super_admin，写入以管理员身份记账审计，
// 其余校验与持久化都在 biz/data 层完成。
package adminblog

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	biz "ai-business-service/internal/biz/adminblog"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type handler struct {
	authenticator authenticator
	repository    biz.Repository
}

func NewHandler(authenticator authenticator, repository biz.Repository) http.Handler {
	return &handler{authenticator: authenticator, repository: repository}
}

const prefix = "/api/admin/blog"

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.authenticator == nil || h.repository == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Blog admin handler unavailable")
		return
	}
	identity, err := h.authenticator.Authenticate(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		failure(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
		return
	}
	// 博客管理面向 editor 角色开放，但这里只接受 Go 自有会话；
	// 不允许把 editor 之外的写权限放宽到 user。
	if identity.Role != "admin" && identity.Role != "super_admin" && identity.Role != "editor" {
		failure(w, http.StatusForbidden, "FORBIDDEN", "Admin access required")
		return
	}
	actor := biz.Actor{ID: identity.UserID}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case path == "" && r.Method == http.MethodGet:
		h.list(w, r)
	case path == "" && r.Method == http.MethodPost:
		h.create(w, r, actor)
	case path == "/stats" && r.Method == http.MethodGet:
		h.stats(w, r)
	case strings.Count(path, "/") == 1 && r.Method == http.MethodGet:
		h.get(w, r, strings.TrimPrefix(path, "/"))
	case strings.Count(path, "/") == 1 && r.Method == http.MethodPut:
		h.update(w, r, actor, strings.TrimPrefix(path, "/"))
	case strings.Count(path, "/") == 1 && r.Method == http.MethodDelete:
		h.remove(w, r, actor, strings.TrimPrefix(path, "/"))
	case strings.HasSuffix(path, "/publish") && r.Method == http.MethodPost:
		h.publish(w, r, actor, strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/publish"))
	case strings.HasSuffix(path, "/unpublish") && r.Method == http.MethodPost:
		h.unpublish(w, r, actor, strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/unpublish"))
	default:
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This blog API has not been migrated to the Go Gateway")
	}
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	query := biz.Query{Status: r.URL.Query().Get("status"), Category: r.URL.Query().Get("category"), Search: r.URL.Query().Get("search")}
	for key, target := range map[string]*int{"page": &query.Page, "limit": &query.Limit} {
		if raw := r.URL.Query().Get(key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid pagination")
				return
			}
			*target = value
		}
	}
	page, err := h.repository.List(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	normalized, err := biz.NormalizeQuery(query)
	if err != nil {
		writeError(w, err)
		return
	}
	posts := make([]map[string]any, 0, len(page.Posts))
	for _, post := range page.Posts {
		posts = append(posts, postView(post))
	}
	success(w, map[string]any{
		"posts": posts,
		"pagination": map[string]any{
			"page":  normalized.Page,
			"limit": normalized.Limit,
			"total": page.Total,
			"pages": (page.Total + int64(normalized.Limit) - 1) / int64(normalized.Limit),
		},
	})
}

func (h *handler) get(w http.ResponseWriter, r *http.Request, id string) {
	post, err := h.repository.Get(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	success(w, map[string]any{"post": postView(post)})
}

func (h *handler) create(w http.ResponseWriter, r *http.Request, actor biz.Actor) {
	input, ok := decodeInput(w, r)
	if !ok {
		return
	}
	post, err := h.repository.Create(r.Context(), actor, input)
	if err != nil {
		writeError(w, err)
		return
	}
	success(w, map[string]any{"post": postView(post)})
}

func (h *handler) update(w http.ResponseWriter, r *http.Request, actor biz.Actor, id string) {
	input, ok := decodeInput(w, r)
	if !ok {
		return
	}
	post, err := h.repository.Update(r.Context(), actor, id, input)
	if err != nil {
		writeError(w, err)
		return
	}
	success(w, map[string]any{"post": postView(post)})
}

func (h *handler) remove(w http.ResponseWriter, r *http.Request, actor biz.Actor, id string) {
	if err := h.repository.Delete(r.Context(), actor, id); err != nil {
		writeError(w, err)
		return
	}
	success(w, map[string]any{"success": true})
}

func (h *handler) publish(w http.ResponseWriter, r *http.Request, actor biz.Actor, id string) {
	post, err := h.repository.Publish(r.Context(), actor, id)
	if err != nil {
		writeError(w, err)
		return
	}
	success(w, map[string]any{"post": postView(post)})
}

func (h *handler) unpublish(w http.ResponseWriter, r *http.Request, actor biz.Actor, id string) {
	post, err := h.repository.Unpublish(r.Context(), actor, id)
	if err != nil {
		writeError(w, err)
		return
	}
	success(w, map[string]any{"post": postView(post)})
}

func (h *handler) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.repository.Stats(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	byCategory := map[string]int64{}
	for key, value := range stats.ByCategory {
		byCategory[key] = value
	}
	success(w, map[string]any{
		"total":      stats.Total,
		"published":  stats.Published,
		"draft":      stats.Draft,
		"byCategory": byCategory,
		"totalViews": stats.TotalViews,
	})
}

// body 只声明后台编辑器真正会提交的字段；未列出的字段一律拒绝，
// 避免把前端表单的额外状态静默写进内容表。
type body struct {
	Slug            *string         `json:"slug"`
	Title           *string         `json:"title"`
	MetaDescription *string         `json:"metaDescription"`
	Keywords        *[]string       `json:"keywords"`
	Excerpt         *string         `json:"excerpt"`
	Content         *string         `json:"content"`
	CoverImage      *string         `json:"coverImage"`
	Author          json.RawMessage `json:"author"`
	Category        *string         `json:"category"`
	Tags            *[]string       `json:"tags"`
	Status          *string         `json:"status"`
	Language        *string         `json:"language"`
	IsFeatured      *bool           `json:"isFeatured"`
	ReadingTime     *int64          `json:"readingTime"`
}

func decodeInput(w http.ResponseWriter, r *http.Request) (biz.Input, bool) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var payload body
	if err := decoder.Decode(&payload); err != nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid blog payload")
		return biz.Input{}, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid blog payload")
		return biz.Input{}, false
	}
	author, ok := decodeAuthor(payload.Author)
	if !ok {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid author")
		return biz.Input{}, false
	}
	return biz.Input{
		Slug:            payload.Slug,
		Title:           payload.Title,
		MetaDescription: payload.MetaDescription,
		Keywords:        payload.Keywords,
		Excerpt:         payload.Excerpt,
		Content:         payload.Content,
		CoverImage:      payload.CoverImage,
		Author:          author,
		Category:        payload.Category,
		Tags:            payload.Tags,
		Status:          payload.Status,
		Language:        payload.Language,
		IsFeatured:      payload.IsFeatured,
		ReadingTime:     payload.ReadingTime,
	}, true
}

// decodeAuthor 接受三种形态：缺省、null、{name,avatar}，以及历史遗留的纯字符串。
func decodeAuthor(raw json.RawMessage) (**biz.Author, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	if string(raw) == "null" {
		var empty *biz.Author
		return &empty, true
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		author := &biz.Author{Name: asString}
		return &author, true
	}
	var asObject struct {
		Name   string `json:"name"`
		Avatar string `json:"avatar"`
	}
	if err := json.Unmarshal(raw, &asObject); err != nil {
		return nil, false
	}
	author := &biz.Author{Name: asObject.Name, Avatar: asObject.Avatar}
	return &author, true
}

func postView(post biz.Post) map[string]any {
	var author any
	if post.Author != nil {
		author = map[string]any{"name": post.Author.Name, "avatar": post.Author.Avatar}
	}
	var publishedAt any
	if post.PublishedAt != nil {
		publishedAt = post.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{
		"_id":             post.ID,
		"slug":            post.Slug,
		"title":           post.Title,
		"metaDescription": post.MetaDescription,
		"keywords":        emptyIfNil(post.Keywords),
		"excerpt":         post.Excerpt,
		"content":         post.Content,
		"coverImage":      post.CoverImage,
		"author":          author,
		"category":        post.Category,
		"tags":            emptyIfNil(post.Tags),
		"status":          post.Status,
		"publishedAt":     publishedAt,
		"readingTime":     post.ReadingTime,
		"viewCount":       post.ViewCount,
		"isFeatured":      post.IsFeatured,
		"language":        post.Language,
		"createdAt":       post.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updatedAt":       post.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func emptyIfNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
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
	for _, entry := range []struct {
		err           error
		status        int
		code, message string
	}{
		{biz.ErrInvalid, 400, "INVALID_REQUEST", "Invalid blog request"},
		{biz.ErrNotFound, 404, "NOT_FOUND", "Blog post not found"},
		{biz.ErrConflict, 409, "CONFLICT", "Blog slug already exists"},
	} {
		if errors.Is(err, entry.err) {
			failure(w, entry.status, entry.code, entry.message)
			return
		}
	}
	if errors.Is(err, shared.ErrUnauthenticated) {
		failure(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
		return
	}
	failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
}
