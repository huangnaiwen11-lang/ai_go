package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/adminmenu"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const menuTestActor = "menu-integration-actor"

func newAdminMenuTestRepository(t *testing.T) (adminmenu.Repository, *mongo.Database) {
	t.Helper()
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("ensure local schema: %v", err)
	}
	// 全局配置只有一份，用例之间必须互不残留。
	cleanupAdminMenu(t, database)
	return NewAdminMenuRepository(&Data{client: client, database: database}), database
}

func cleanupAdminMenu(t *testing.T, database *mongo.Database) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = database.Collection(schema.CollectionAdminMenuVisibility).
		DeleteMany(ctx, bson.M{"_id": adminmenu.SettingsDocumentID})
	_, _ = database.Collection(schema.CollectionAdminAudit).
		DeleteMany(ctx, bson.M{"actor_id": menuTestActor})
}

func menuActor() adminmenu.Actor {
	return adminmenu.Actor{ID: menuTestActor, Role: "super_admin"}
}

// 「还没人配置过」必须返回空配置而不是错误 —— 这是正常状态，不是异常。
func TestMongoAdminMenuGetWithoutDocumentReturnsEmptyOverrides(t *testing.T) {
	repository, database := newAdminMenuTestRepository(t)
	defer cleanupAdminMenu(t, database)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	visibility, err := repository.Get(ctx)
	if err != nil {
		t.Fatalf("Get() 错误 = %v", err)
	}
	if len(visibility.Overrides) != 0 {
		t.Fatalf("Overrides = %#v, want 空", visibility.Overrides)
	}
	if visibility.UpdatedAt != nil {
		t.Fatalf("UpdatedAt = %v, want nil", visibility.UpdatedAt)
	}
}

func TestMongoAdminMenuSaveAndRoundTrip(t *testing.T) {
	repository, database := newAdminMenuTestRepository(t)
	defer cleanupAdminMenu(t, database)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	overrides := map[string]bool{"/loras": true, "/images": false}
	saved, err := repository.Save(ctx, menuActor(), overrides)
	if err != nil {
		t.Fatalf("Save() 错误 = %v", err)
	}
	if saved.UpdatedBy != menuTestActor {
		t.Fatalf("UpdatedBy = %q, want %q", saved.UpdatedBy, menuTestActor)
	}
	if saved.UpdatedAt == nil {
		t.Fatal("UpdatedAt 不应为空")
	}
	// BSON DateTime 只有毫秒精度：响应值必须与「再读一次」完全一致，
	// 否则同一个字段在写入前后会给出不同的值。
	if saved.UpdatedAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("UpdatedAt 未截断到毫秒: %v", saved.UpdatedAt)
	}

	read, err := repository.Get(ctx)
	if err != nil {
		t.Fatalf("Get() 错误 = %v", err)
	}
	if read.Overrides["/loras"] != true || read.Overrides["/images"] != false {
		t.Fatalf("Overrides = %#v, want {/loras:true, /images:false}", read.Overrides)
	}
	if read.UpdatedAt == nil || !read.UpdatedAt.Equal(*saved.UpdatedAt) {
		t.Fatalf("写后读的 UpdatedAt = %v, 写入响应 = %v", read.UpdatedAt, saved.UpdatedAt)
	}
}

// 整体覆盖而不是逐键合并：前端提交的是完整草稿，
// 逐键合并会让「把某个节点改回默认」无法表达。
func TestMongoAdminMenuSaveReplacesInsteadOfMerging(t *testing.T) {
	repository, database := newAdminMenuTestRepository(t)
	defer cleanupAdminMenu(t, database)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := repository.Save(ctx, menuActor(), map[string]bool{"/loras": true, "/images": true}); err != nil {
		t.Fatalf("首次 Save() 错误 = %v", err)
	}
	if _, err := repository.Save(ctx, menuActor(), map[string]bool{"/loras": true}); err != nil {
		t.Fatalf("二次 Save() 错误 = %v", err)
	}

	read, err := repository.Get(ctx)
	if err != nil {
		t.Fatalf("Get() 错误 = %v", err)
	}
	if _, exists := read.Overrides["/images"]; exists {
		t.Fatalf("Overrides = %#v, /images 应已被移除", read.Overrides)
	}
	if read.Overrides["/loras"] != true {
		t.Fatalf("Overrides = %#v, /loras 应保留", read.Overrides)
	}
}

// 单文档存储：无论 Save 多少次，集合里只能有一条配置。
func TestMongoAdminMenuSaveKeepsSingleDocument(t *testing.T) {
	repository, database := newAdminMenuTestRepository(t)
	defer cleanupAdminMenu(t, database)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for index := 0; index < 3; index++ {
		if _, err := repository.Save(ctx, menuActor(), map[string]bool{"/loras": true}); err != nil {
			t.Fatalf("第 %d 次 Save() 错误 = %v", index+1, err)
		}
	}
	count, err := database.Collection(schema.CollectionAdminMenuVisibility).
		CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("CountDocuments() 错误 = %v", err)
	}
	if count != 1 {
		t.Fatalf("集合文档数 = %d, want 1", count)
	}
}

func TestMongoAdminMenuSaveWritesAudit(t *testing.T) {
	repository, database := newAdminMenuTestRepository(t)
	defer cleanupAdminMenu(t, database)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := repository.Save(ctx, menuActor(), map[string]bool{"/loras": true}); err != nil {
		t.Fatalf("Save() 错误 = %v", err)
	}
	var audit bson.M
	err := database.Collection(schema.CollectionAdminAudit).
		FindOne(ctx, bson.M{"actor_id": menuTestActor}).Decode(&audit)
	if err != nil {
		t.Fatalf("未写入审计: %v", err)
	}
	if audit["action"] != "admin_menu_visibility_update" {
		t.Fatalf("审计 action = %v", audit["action"])
	}
	if audit["target_id"] != adminmenu.SettingsDocumentID {
		t.Fatalf("审计 target_id = %v", audit["target_id"])
	}
}

// 非法覆盖项必须在落库前被挡下，不能留下半份配置。
func TestMongoAdminMenuSaveRejectsInvalidOverrides(t *testing.T) {
	repository, database := newAdminMenuTestRepository(t)
	defer cleanupAdminMenu(t, database)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := repository.Save(ctx, menuActor(), map[string]bool{"loras": true}); !errors.Is(err, adminmenu.ErrInvalid) {
		t.Fatalf("错误 = %v, want ErrInvalid", err)
	}
	count, err := database.Collection(schema.CollectionAdminMenuVisibility).
		CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("CountDocuments() 错误 = %v", err)
	}
	if count != 0 {
		t.Fatalf("非法输入后集合文档数 = %d, want 0", count)
	}
}

// 空覆盖项是合法输入，含义是「全部按默认清单」。
func TestMongoAdminMenuSaveAcceptsEmptyOverrides(t *testing.T) {
	repository, database := newAdminMenuTestRepository(t)
	defer cleanupAdminMenu(t, database)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := repository.Save(ctx, menuActor(), map[string]bool{"/loras": true}); err != nil {
		t.Fatalf("首次 Save() 错误 = %v", err)
	}
	if _, err := repository.Save(ctx, menuActor(), map[string]bool{}); err != nil {
		t.Fatalf("清空 Save() 错误 = %v", err)
	}

	read, err := repository.Get(ctx)
	if err != nil {
		t.Fatalf("Get() 错误 = %v", err)
	}
	if len(read.Overrides) != 0 {
		t.Fatalf("Overrides = %#v, want 空", read.Overrides)
	}
}
