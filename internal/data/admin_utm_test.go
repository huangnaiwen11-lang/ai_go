package data

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"ai-business-service/internal/biz/adminutm"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var utmTestObjectIDPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)

func newAdminUTMTestRepository(t *testing.T) (adminutm.Repository, *mongo.Database) {
	t.Helper()
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	// 唯一索引 ux_utmlinks_slug 是 slug 冲突返回 ErrConflict 的前提。
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("ensure local schema: %v", err)
	}
	return NewAdminUTMRepository(&Data{client: client, database: database}), database
}

// newUTMTestSlug 生成一个本次用例独占的 slug，避免与库里既有数据或并行用例撞车。
func newUTMTestSlug(t *testing.T) string {
	t.Helper()
	return "t-" + uuid.NewString()[:20]
}

func utmTestInput(slug string) adminutm.Input {
	label, source, medium := "集成测试链接", "integration_test", "banner"
	return adminutm.Input{Slug: &slug, Label: &label, UtmSource: &source, UtmMedium: &medium}
}

// cleanupUTMLinks 清掉本用例写入的链接与审计，保持本地库可重复跑。
func cleanupUTMLinks(t *testing.T, database *mongo.Database, slugs []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, slug := range slugs {
		_ = database.Collection(schema.CollectionUTMLinks).FindOneAndDelete(ctx, bson.M{"slug": slug})
	}
	_, _ = database.Collection(schema.CollectionAdminAudit).DeleteMany(ctx, bson.M{"action": bson.M{"$regex": "^utm_link_"}, "actor_id": "utm-integration-actor"})
}

// 这条用例钉住两个曾经真实存在的静默缺陷：
//  1. _id 用 UUID 生成 —— Node 端路由对 :id 有 ^[a-f0-9]{24}$ 校验，UUID 会让写路由整条失配。
//  2. extraParams 读回来是 {} —— 驱动把嵌套文档解码成 bson.D，而读取端只认 bson.M。
func TestMongoAdminUTMCreateUsesObjectIDAndRoundTripsExtraParams(t *testing.T) {
	repository, database := newAdminUTMTestRepository(t)
	slug := newUTMTestSlug(t)
	t.Cleanup(func() { cleanupUTMLinks(t, database, []string{slug}) })
	ctx, cancel := newMongoTestContext()
	defer cancel()

	input := utmTestInput(slug)
	input.ExtraParams = map[string]string{"from": "probe", "utm_source": "应被丢弃"}
	created, err := repository.Create(ctx, adminutm.Actor{ID: "utm-integration-actor", Role: "super_admin"}, input)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !utmTestObjectIDPattern.MatchString(created.ID) {
		t.Fatalf("_id 不是 24 位 ObjectId hex: %q", created.ID)
	}
	// utm_* 前缀的键必须被丢弃：它们由专门的字段负责。
	if created.ExtraParams["from"] != "probe" || len(created.ExtraParams) != 1 {
		t.Fatalf("创建响应里的 extraParams = %v", created.ExtraParams)
	}

	page, err := repository.List(ctx, adminutm.Query{Keyword: slug})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("List() total=%d items=%d", page.Total, len(page.Items))
	}
	if page.Items[0].ExtraParams["from"] != "probe" {
		t.Fatalf("从库里读回的 extraParams = %v，嵌套文档被静默丢成空对象", page.Items[0].ExtraParams)
	}
	if page.Items[0].ID != created.ID {
		t.Fatalf("List 返回的 id=%q 与 Create 的 %q 不一致", page.Items[0].ID, created.ID)
	}

	auditCount, err := database.Collection(schema.CollectionAdminAudit).CountDocuments(ctx, bson.M{"action": "utm_link_create", "target_id": created.ID})
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("审计行数 = %d，写操作必须与 admin_audit 同事务落库", auditCount)
	}
}

// 后台的启用开关只发 {enabled}，其余字段必须保持原值。
func TestMongoAdminUTMPartialUpdateKeepsOtherFields(t *testing.T) {
	repository, database := newAdminUTMTestRepository(t)
	slug := newUTMTestSlug(t)
	t.Cleanup(func() { cleanupUTMLinks(t, database, []string{slug}) })
	ctx, cancel := newMongoTestContext()
	defer cancel()

	created, err := repository.Create(ctx, adminutm.Actor{ID: "utm-integration-actor", Role: "super_admin"}, utmTestInput(slug))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	disabled := false
	updated, err := repository.Update(ctx, adminutm.Actor{ID: "utm-integration-actor", Role: "super_admin"}, created.ID, adminutm.Input{Enabled: &disabled})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Enabled {
		t.Fatalf("enabled 未生效: %+v", updated)
	}
	if updated.Label != created.Label || updated.UtmSource != created.UtmSource || updated.Slug != created.Slug {
		t.Fatalf("未提供的字段被改写: %+v", updated)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("createdAt 被改写: %v -> %v", created.CreatedAt, updated.CreatedAt)
	}
}

func TestMongoAdminUTMSlugConflictReturnsErrConflict(t *testing.T) {
	repository, database := newAdminUTMTestRepository(t)
	slug := newUTMTestSlug(t)
	t.Cleanup(func() { cleanupUTMLinks(t, database, []string{slug}) })
	ctx, cancel := newMongoTestContext()
	defer cancel()

	if _, err := repository.Create(ctx, adminutm.Actor{ID: "utm-integration-actor", Role: "super_admin"}, utmTestInput(slug)); err != nil {
		t.Fatalf("首次 Create() error = %v", err)
	}
	_, err := repository.Create(ctx, adminutm.Actor{ID: "utm-integration-actor", Role: "super_admin"}, utmTestInput(slug))
	if !errors.Is(err, adminutm.ErrConflict) {
		t.Fatalf("重复 slug error = %v，期望 ErrConflict", err)
	}
}

func TestMongoAdminUTMDeleteAndMissingSemantics(t *testing.T) {
	repository, database := newAdminUTMTestRepository(t)
	slug := newUTMTestSlug(t)
	t.Cleanup(func() { cleanupUTMLinks(t, database, []string{slug}) })
	ctx, cancel := newMongoTestContext()
	defer cancel()

	created, err := repository.Create(ctx, adminutm.Actor{ID: "utm-integration-actor", Role: "super_admin"}, utmTestInput(slug))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	actor := adminutm.Actor{ID: "utm-integration-actor", Role: "super_admin"}
	if err := repository.Delete(ctx, actor, created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := repository.Delete(ctx, actor, created.ID); !errors.Is(err, adminutm.ErrNotFound) {
		t.Fatalf("重复 Delete() error = %v，期望 ErrNotFound", err)
	}

	ghost := bson.NewObjectID().Hex()
	enabled := true
	if _, err := repository.Update(ctx, actor, ghost, adminutm.Input{Enabled: &enabled}); !errors.Is(err, adminutm.ErrNotFound) {
		t.Fatalf("更新不存在的链接 error = %v，期望 ErrNotFound", err)
	}
	// 格式非法的 id 是「入参非法」，不是「资源不存在」——两者对调用方含义不同。
	if _, err := repository.Update(ctx, actor, "not-an-objectid", adminutm.Input{Enabled: &enabled}); !errors.Is(err, adminutm.ErrInvalid) {
		t.Fatalf("非法 id error = %v，期望 ErrInvalid", err)
	}
}
