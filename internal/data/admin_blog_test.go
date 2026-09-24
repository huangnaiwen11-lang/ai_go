package data

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ai-business-service/internal/biz/adminblog"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func newBlogRepository(t *testing.T) (*mongoAdminBlogRepository, context.Context) {
	t.Helper()
	client := newLocalMongoClient(t)
	db := client.Database("admin_blog_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	ctx := context.Background()
	t.Cleanup(func() { _ = db.Drop(ctx) })
	// 唯一索引是 slug 冲突的唯一权威来源，测试库必须与生产同样初始化 schema。
	if err := migrate.NewInitializer(db).Ensure(ctx); err != nil {
		t.Fatalf("初始化测试 schema: %v", err)
	}
	return &mongoAdminBlogRepository{data: &Data{client: client, database: db}}, ctx
}

func stringPtr(value string) *string { return &value }

func TestAdminBlogRepositoryFullLifecycle(t *testing.T) {
	repository, ctx := newBlogRepository(t)
	actor := adminblog.Actor{ID: "admin-1"}

	created, err := repository.Create(ctx, actor, adminblog.Input{
		Title:    stringPtr("Hello World"),
		Content:  stringPtr("body"),
		Category: stringPtr("guides"),
		Author:   func() **adminblog.Author { a := &adminblog.Author{Name: "Cling AI Team"}; return &a }(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Slug != "hello-world" || created.Status != "draft" || created.Author == nil {
		t.Fatalf("created = %#v", created)
	}
	if _, err := repository.Get(ctx, created.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}

	// slug 唯一：同一 slug 再次创建必须冲突，而不是静默覆盖。
	if _, err := repository.Create(ctx, actor, adminblog.Input{
		Slug: stringPtr("hello-world"), Title: stringPtr("Other"), Content: stringPtr("b"), Category: stringPtr("news"),
	}); !errors.Is(err, adminblog.ErrConflict) {
		t.Fatalf("重复 slug err = %v，期望 ErrConflict", err)
	}

	updated, err := repository.Update(ctx, actor, created.ID, adminblog.Input{Title: stringPtr("Hello Again"), IsFeatured: func() *bool { v := true; return &v }()})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Title != "Hello Again" || !updated.IsFeatured || updated.Slug != "hello-world" {
		t.Fatalf("updated = %#v", updated)
	}

	published, err := repository.Publish(ctx, actor, created.ID)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.Status != "published" || published.PublishedAt == nil {
		t.Fatalf("published = %#v", published)
	}
	unpublished, err := repository.Unpublish(ctx, actor, created.ID)
	if err != nil || unpublished.Status != "draft" {
		t.Fatalf("Unpublish = %#v, err = %v", unpublished, err)
	}

	stats, err := repository.Stats(ctx)
	if err != nil || stats.Total != 1 || stats.Draft != 1 || stats.ByCategory["guides"] != 1 {
		t.Fatalf("stats = %#v, err = %v", stats, err)
	}

	// 审计必须与写入同事务落库：创建 / 更新 / 发布 / 下架共 4 条。
	audits, err := repository.data.database.Collection(schema.CollectionAdminAudit).CountDocuments(ctx, bson.M{"target_id": created.ID})
	if err != nil || audits != 4 {
		t.Fatalf("审计条数 = %d, err = %v，期望 4", audits, err)
	}

	if err := repository.Delete(ctx, actor, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	audits, err = repository.data.database.Collection(schema.CollectionAdminAudit).CountDocuments(ctx, bson.M{"target_id": created.ID})
	if err != nil || audits != 5 {
		t.Fatalf("删除后审计条数 = %d, err = %v，期望 5", audits, err)
	}
	if _, err := repository.Get(ctx, created.ID); !errors.Is(err, adminblog.ErrNotFound) {
		t.Fatalf("删除后 Get err = %v，期望 ErrNotFound", err)
	}
	if err := repository.Delete(ctx, actor, created.ID); !errors.Is(err, adminblog.ErrNotFound) {
		t.Fatalf("重复删除 err = %v，期望 ErrNotFound", err)
	}
}

func TestAdminBlogRepositoryListFiltersAndPagination(t *testing.T) {
	repository, ctx := newBlogRepository(t)
	actor := adminblog.Actor{ID: "admin-1"}
	for _, input := range []adminblog.Input{
		{Title: stringPtr("Alpha guide"), Content: stringPtr("b"), Category: stringPtr("guides")},
		{Title: stringPtr("Beta news"), Content: stringPtr("b"), Category: stringPtr("news"), Status: stringPtr("published")},
		{Title: stringPtr("Gamma guide"), Content: stringPtr("b"), Category: stringPtr("guides")},
	} {
		if _, err := repository.Create(ctx, actor, input); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	page, err := repository.List(ctx, adminblog.Query{Category: "guides", Limit: 10})
	if err != nil || page.Total != 2 || len(page.Posts) != 2 {
		t.Fatalf("按分类筛选 page = %#v, err = %v", page, err)
	}
	page, err = repository.List(ctx, adminblog.Query{Status: "published", Limit: 10})
	if err != nil || page.Total != 1 || page.Posts[0].Title != "Beta news" {
		t.Fatalf("按状态筛选 page = %#v, err = %v", page, err)
	}
	page, err = repository.List(ctx, adminblog.Query{Search: "gamma", Limit: 10})
	if err != nil || page.Total != 1 {
		t.Fatalf("按关键字搜索 page = %#v, err = %v", page, err)
	}
	page, err = repository.List(ctx, adminblog.Query{Limit: 2, Page: 2})
	if err != nil || page.Total != 3 || len(page.Posts) != 1 {
		t.Fatalf("分页 page = %#v, err = %v", page, err)
	}
	if _, err := repository.List(ctx, adminblog.Query{Status: "nope"}); !errors.Is(err, adminblog.ErrInvalid) {
		t.Fatalf("非法状态 err = %v，期望 ErrInvalid", err)
	}
}

func TestAdminBlogRepositoryRejectsUnknownPost(t *testing.T) {
	repository, ctx := newBlogRepository(t)
	if _, err := repository.Get(ctx, "missing"); !errors.Is(err, adminblog.ErrNotFound) {
		t.Fatalf("Get err = %v，期望 ErrNotFound", err)
	}
	if _, err := repository.Publish(ctx, adminblog.Actor{ID: "a"}, "missing"); !errors.Is(err, adminblog.ErrNotFound) {
		t.Fatalf("Publish err = %v，期望 ErrNotFound", err)
	}
}
