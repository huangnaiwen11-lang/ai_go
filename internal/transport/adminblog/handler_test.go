package adminblog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	biz "ai-business-service/internal/biz/adminblog"
	"ai-business-service/internal/transport/sessionauth"
)

type stubAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (stub stubAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return stub.identity, stub.err
}

type memoryRepository struct {
	posts  map[string]biz.Post
	nextID int
	audit  []string
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{posts: map[string]biz.Post{}}
}

func (repository *memoryRepository) List(_ context.Context, query biz.Query) (biz.Page, error) {
	query, err := biz.NormalizeQuery(query)
	if err != nil {
		return biz.Page{}, err
	}
	posts := make([]biz.Post, 0, len(repository.posts))
	for _, post := range repository.posts {
		if query.Status != "" && post.Status != query.Status {
			continue
		}
		if query.Category != "" && post.Category != query.Category {
			continue
		}
		posts = append(posts, post)
	}
	return biz.Page{Posts: posts, Total: int64(len(posts))}, nil
}

func (repository *memoryRepository) Get(_ context.Context, id string) (biz.Post, error) {
	post, ok := repository.posts[id]
	if !ok {
		return biz.Post{}, biz.ErrNotFound
	}
	return post, nil
}

func (repository *memoryRepository) Create(_ context.Context, actor biz.Actor, input biz.Input) (biz.Post, error) {
	input, err := biz.NormalizeInput(input, true)
	if err != nil {
		return biz.Post{}, err
	}
	repository.nextID++
	id := "post-" + string(rune('0'+repository.nextID))
	now := time.Now().UTC()
	post := biz.Post{ID: id, Title: *input.Title, Content: *input.Content, Category: *input.Category, Status: "draft", CreatedAt: now, UpdatedAt: now, Keywords: []string{}, Tags: []string{}}
	if input.Slug != nil {
		post.Slug = *input.Slug
	} else {
		post.Slug = biz.SlugFromTitle(*input.Title)
	}
	if input.Author != nil && *input.Author != nil {
		post.Author = *input.Author
	}
	repository.posts[id] = post
	repository.audit = append(repository.audit, "create:"+actor.ID)
	return post, nil
}

func (repository *memoryRepository) Update(_ context.Context, actor biz.Actor, id string, input biz.Input) (biz.Post, error) {
	input, err := biz.NormalizeInput(input, false)
	if err != nil {
		return biz.Post{}, err
	}
	post, ok := repository.posts[id]
	if !ok {
		return biz.Post{}, biz.ErrNotFound
	}
	if input.Title != nil {
		post.Title = *input.Title
	}
	if input.Content != nil {
		post.Content = *input.Content
	}
	post.UpdatedAt = time.Now().UTC()
	repository.posts[id] = post
	repository.audit = append(repository.audit, "update:"+actor.ID)
	return post, nil
}

func (repository *memoryRepository) Delete(_ context.Context, actor biz.Actor, id string) error {
	if _, ok := repository.posts[id]; !ok {
		return biz.ErrNotFound
	}
	delete(repository.posts, id)
	repository.audit = append(repository.audit, "delete:"+actor.ID)
	return nil
}

func (repository *memoryRepository) Publish(_ context.Context, actor biz.Actor, id string) (biz.Post, error) {
	return repository.setStatus(actor, id, "published")
}

func (repository *memoryRepository) Unpublish(_ context.Context, actor biz.Actor, id string) (biz.Post, error) {
	return repository.setStatus(actor, id, "draft")
}

func (repository *memoryRepository) setStatus(actor biz.Actor, id, status string) (biz.Post, error) {
	post, ok := repository.posts[id]
	if !ok {
		return biz.Post{}, biz.ErrNotFound
	}
	post.Status = status
	now := time.Now().UTC()
	post.UpdatedAt = now
	if status == "published" {
		post.PublishedAt = &now
	}
	repository.posts[id] = post
	repository.audit = append(repository.audit, status+":"+actor.ID)
	return post, nil
}

func (repository *memoryRepository) Stats(context.Context) (biz.Stats, error) {
	stats := biz.Stats{ByCategory: map[string]int64{}}
	for _, post := range repository.posts {
		stats.Total++
		if post.Status == "published" {
			stats.Published++
		}
		if post.Status == "draft" {
			stats.Draft++
		}
		stats.TotalViews += post.ViewCount
		stats.ByCategory[post.Category]++
	}
	return stats, nil
}

func adminHandler(t *testing.T, repository *memoryRepository) http.Handler {
	t.Helper()
	return NewHandler(stubAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: "admin"}}, repository)
}

func do(t *testing.T, handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, reader))
	return recorder
}

func decodeData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, recorder.Body.String())
	}
	if !envelope.Success {
		t.Fatalf("响应未成功: %s", recorder.Body.String())
	}
	return envelope.Data
}

func TestBlog创建更新发布删除全链路(t *testing.T) {
	repository := newMemoryRepository()
	handler := adminHandler(t, repository)

	created := do(t, handler, http.MethodPost, "/api/admin/blog", `{"title":"Hello World","content":"body","category":"guides","author":{"name":"Cling AI Team"}}`)
	if created.Code != http.StatusOK {
		t.Fatalf("创建状态码 = %d (%s)", created.Code, created.Body.String())
	}
	post := decodeData(t, created)["post"].(map[string]any)
	id, _ := post["_id"].(string)
	if id == "" || post["slug"] != "hello-world" {
		t.Fatalf("创建结果异常: %#v", post)
	}
	if post["status"] != "draft" {
		t.Fatalf("默认状态 = %v，期望 draft", post["status"])
	}

	published := do(t, handler, http.MethodPost, "/api/admin/blog/"+id+"/publish", "")
	if published.Code != http.StatusOK {
		t.Fatalf("发布状态码 = %d (%s)", published.Code, published.Body.String())
	}
	publishedPost := decodeData(t, published)["post"].(map[string]any)
	if publishedPost["status"] != "published" || publishedPost["publishedAt"] == nil {
		t.Fatalf("发布结果异常: %#v", publishedPost)
	}

	updated := do(t, handler, http.MethodPut, "/api/admin/blog/"+id, `{"title":"Hello Again"}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("更新状态码 = %d (%s)", updated.Code, updated.Body.String())
	}
	if got := decodeData(t, updated)["post"].(map[string]any)["title"]; got != "Hello Again" {
		t.Fatalf("更新后标题 = %v", got)
	}

	stats := decodeData(t, do(t, handler, http.MethodGet, "/api/admin/blog/stats", ""))
	if stats["total"].(float64) != 1 || stats["published"].(float64) != 1 {
		t.Fatalf("统计异常: %#v", stats)
	}

	removed := do(t, handler, http.MethodDelete, "/api/admin/blog/"+id, "")
	if removed.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d (%s)", removed.Code, removed.Body.String())
	}
	if missing := do(t, handler, http.MethodGet, "/api/admin/blog/"+id, ""); missing.Code != http.StatusNotFound {
		t.Fatalf("删除后读取状态码 = %d，期望 404", missing.Code)
	}
	for _, action := range []string{"create:admin-1", "update:admin-1", "published:admin-1", "delete:admin-1"} {
		found := false
		for _, entry := range repository.audit {
			if entry == action {
				found = true
			}
		}
		if !found {
			t.Fatalf("审计缺失 %q，实际 %#v", action, repository.audit)
		}
	}
}

func TestBlog拒绝未定义字段与非法枚举(t *testing.T) {
	handler := adminHandler(t, newMemoryRepository())
	unknown := do(t, handler, http.MethodPost, "/api/admin/blog", `{"title":"a","content":"b","category":"guides","unexpected":1}`)
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("未定义字段状态码 = %d，期望 400", unknown.Code)
	}
	badCategory := do(t, handler, http.MethodPost, "/api/admin/blog", `{"title":"a","content":"b","category":"nope"}`)
	if badCategory.Code != http.StatusBadRequest {
		t.Fatalf("非法分类状态码 = %d，期望 400", badCategory.Code)
	}
	badPagination := do(t, handler, http.MethodGet, "/api/admin/blog?limit=999", "")
	if badPagination.Code != http.StatusBadRequest {
		t.Fatalf("越界分页状态码 = %d，期望 400", badPagination.Code)
	}
}

func TestBlog列表返回分页与空数组(t *testing.T) {
	repository := newMemoryRepository()
	handler := adminHandler(t, repository)
	do(t, handler, http.MethodPost, "/api/admin/blog", `{"title":"First","content":"b","category":"news"}`)
	data := decodeData(t, do(t, handler, http.MethodGet, "/api/admin/blog", ""))
	pagination, ok := data["pagination"].(map[string]any)
	if !ok || pagination["total"].(float64) != 1 {
		t.Fatalf("分页异常: %#v", data)
	}
	post := data["posts"].([]any)[0].(map[string]any)
	if _, ok := post["keywords"].([]any); !ok {
		t.Fatalf("keywords 必须是数组: %#v", post["keywords"])
	}
}

func TestBlog越权与未认证被拒绝(t *testing.T) {
	repository := newMemoryRepository()
	user := NewHandler(stubAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "u1", Role: "user"}}, repository)
	if recorder := do(t, user, http.MethodGet, "/api/admin/blog", ""); recorder.Code != http.StatusForbidden {
		t.Fatalf("普通用户状态码 = %d，期望 403", recorder.Code)
	}
	anonymous := NewHandler(stubAuthenticator{identity: nil}, repository)
	if recorder := do(t, anonymous, http.MethodGet, "/api/admin/blog", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("匿名状态码 = %d，期望 401", recorder.Code)
	}
}

func TestBlog未迁移的方法返回501(t *testing.T) {
	handler := adminHandler(t, newMemoryRepository())
	recorder := do(t, handler, http.MethodPatch, "/api/admin/blog/abc", `{}`)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501", recorder.Code)
	}
}
