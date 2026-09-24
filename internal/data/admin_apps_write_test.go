package data

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/adminapps"
	"ai-business-service/internal/data/migrate"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// newAdminAppsTestRepository 建一个隔离库并铺上真实 schema（含唯一索引）。
//
// 不能像只读用例那样省掉 Ensure：写路径的兜底是 uniq_android_native_identifiers，
// 索引不在，冲突用例就变成在测「内存里的一次查询」而不是「数据库的裁决」。
func newAdminAppsTestRepository(t *testing.T) (adminapps.Repository, *mongo.Database) {
	t.Helper()
	client := newLocalMongoClient(t)
	db := client.Database("admin_apps_write_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	t.Cleanup(func() { _ = db.Drop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := migrate.NewInitializer(db).Ensure(ctx); err != nil {
		t.Fatalf("铺建隔离库 schema: %v", err)
	}
	return NewAdminAppsRepository(&Data{client: client, database: db}), db
}

func adminAppsAuditCount(t *testing.T, db *mongo.Database, action string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count, err := db.Collection("admin_audit").CountDocuments(ctx, bson.M{"action": action})
	if err != nil {
		t.Fatalf("统计审计记录: %v", err)
	}
	return count
}

// isEmptyDocument 同时接受 bson.M 与 bson.D：驱动把嵌套文档解码成 bson.D，
// 断言只认 bson.M 会在「明明是空对象」时判失败。
func isEmptyDocument(value any) bool {
	switch typed := value.(type) {
	case bson.M:
		return len(typed) == 0
	case bson.D:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func TestAdminAppsRepositoryCreatesAppWithAuditInSameTransaction(t *testing.T) {
	repository, db := newAdminAppsTestRepository(t)
	actor := adminapps.Actor{ID: "actor-create", Role: "super_admin"}

	app, err := repository.Create(context.Background(), actor, map[string]any{
		"name":        "  Cling Android  ",
		"platform":    "android",
		"clientId":    "com.example.Create",
		"packageName": "com.example.create.pkg",
		"apiUrl":      "HTTPS://API.Example.Test:443/base/path",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if app.Name != "Cling Android" {
		t.Errorf("name = %q, want 去空格后的 %q", app.Name, "Cling Android")
	}
	if app.Status != "active" {
		t.Errorf("status = %q, want 默认 active", app.Status)
	}
	// apiUrl 归一化成 origin：小写、去路径、默认端口不保留。
	if app.APIURL != "https://api.example.test" {
		t.Errorf("apiUrl = %q, want %q", app.APIURL, "https://api.example.test")
	}
	if app.ResolvedAPIURL != "https://api.example.test" {
		t.Errorf("resolvedApiUrl = %q, want %q", app.ResolvedAPIURL, "https://api.example.test")
	}

	// 落库文档：nativeIdentifiers 由 clientId + packageName 派生并小写去重；
	// packageConfig 恒为空对象（Node `App.create` 的硬编码）。
	var stored bson.M
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	objectID, err := bson.ObjectIDFromHex(app.ID)
	if err != nil {
		t.Fatalf("Create() 返回的 id 不是 ObjectID: %q", app.ID)
	}
	if err := db.Collection("apps").FindOne(ctx, bson.M{"_id": objectID}).Decode(&stored); err != nil {
		t.Fatalf("读回落库文档: %v", err)
	}
	identifiers, _ := stored["nativeIdentifiers"].(bson.A)
	if len(identifiers) != 2 || identifiers[0] != "com.example.create" || identifiers[1] != "com.example.create.pkg" {
		t.Errorf("nativeIdentifiers = %#v, want [com.example.create com.example.create.pkg]", stored["nativeIdentifiers"])
	}
	// 注意驱动把嵌套文档解码成有序 bson.D（不是 bson.M），这里只断言「是空文档」。
	if !isEmptyDocument(stored["packageConfig"]) {
		t.Errorf("packageConfig = %#v, want 空对象", stored["packageConfig"])
	}
	if stored["createdBy"] != "actor-create" {
		t.Errorf("createdBy = %#v, want actor-create", stored["createdBy"])
	}

	// 审计必须与写入同事务，且目标指向新文档。
	if got := adminAppsAuditCount(t, db, "app_create"); got != 1 {
		t.Errorf("app_create 审计条数 = %d, want 1", got)
	}

	// 投影不得泄漏 nativeIdentifiers —— Node 里它是 select: false。
	if _, leaked := app.Fields["nativeIdentifiers"]; leaked {
		t.Error("响应投影泄漏了 nativeIdentifiers")
	}
	if _, ok := app.Fields["packageConfig"].(map[string]any); !ok {
		t.Errorf("投影里的 packageConfig = %#v, want 空对象", app.Fields["packageConfig"])
	}
}

// TestAdminAppsRepositoryNormalizesWebDomainOnly 钉住 Node 的**平台条件**归一化：
// domain 只在 platform === 'web' 时被压成主机名，其余平台原样保留。
// 把这条条件去掉（无差别归一化）会让 iOS/Android 的 domain 被悄悄改写。
func TestAdminAppsRepositoryNormalizesWebDomainOnly(t *testing.T) {
	repository, _ := newAdminAppsTestRepository(t)
	actor := adminapps.Actor{ID: "actor-domain", Role: "super_admin"}

	web, err := repository.Create(context.Background(), actor, map[string]any{
		"name": "Web", "platform": "web", "domain": "https://Create.Example.Test/some/path",
	})
	if err != nil {
		t.Fatalf("创建 web App error = %v", err)
	}
	if web.Domain != "create.example.test" {
		t.Errorf("web domain = %q, want %q", web.Domain, "create.example.test")
	}
	if web.ResolvedAPIURL != "https://create.example.test" {
		t.Errorf("web resolvedApiUrl = %q, want %q", web.ResolvedAPIURL, "https://create.example.test")
	}

	ios, err := repository.Create(context.Background(), actor, map[string]any{
		"name": "iOS", "platform": "ios", "domain": "https://Raw.Example.Test/some/path",
	})
	if err != nil {
		t.Fatalf("创建 iOS App error = %v", err)
	}
	if ios.Domain != "https://Raw.Example.Test/some/path" {
		t.Errorf("iOS domain = %q, want 原样保留", ios.Domain)
	}
}

func TestAdminAppsRepositoryRejectsDuplicateNativeIdentifiers(t *testing.T) {
	repository, db := newAdminAppsTestRepository(t)
	actor := adminapps.Actor{ID: "actor-conflict", Role: "super_admin"}
	payload := map[string]any{
		"name":     "First",
		"platform": "android",
		"clientId": "com.example.duplicate",
	}
	if _, err := repository.Create(context.Background(), actor, payload); err != nil {
		t.Fatalf("首次 Create() error = %v", err)
	}

	payload["name"] = "Second"
	_, err := repository.Create(context.Background(), actor, payload)
	if !errors.Is(err, adminapps.ErrConflict) {
		t.Fatalf("重复标识 Create() error = %v, want ErrConflict", err)
	}
	var conflict *adminapps.ConflictError
	if !errors.As(err, &conflict) || conflict.Message == "" {
		t.Fatalf("冲突错误未携带对外文案: %#v", err)
	}

	// 冲突必须在事务内整体回滚：既没有第二份文档，也没有第二行审计。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	total, err := db.Collection("apps").CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("统计 apps: %v", err)
	}
	if total != 1 {
		t.Errorf("apps 文档数 = %d, want 1", total)
	}
	if got := adminAppsAuditCount(t, db, "app_create"); got != 1 {
		t.Errorf("app_create 审计条数 = %d, want 1（失败的那次不得留下审计）", got)
	}
}

// TestAdminAppsRepositoryAllowsAppsWithoutNativeIdentifiers 是部分唯一索引的回归闸门：
// 两个没有 clientId / packageName 的 web App 必须都能建出来。
// 少了 partialFilterExpression，第二个会被索引当成「标识为 null 的重复」拒掉。
func TestAdminAppsRepositoryAllowsAppsWithoutNativeIdentifiers(t *testing.T) {
	repository, _ := newAdminAppsTestRepository(t)
	actor := adminapps.Actor{ID: "actor-web", Role: "super_admin"}

	for _, name := range []string{"Web One", "Web Two"} {
		if _, err := repository.Create(context.Background(), actor, map[string]any{
			"name":     name,
			"platform": "web",
		}); err != nil {
			t.Fatalf("创建 web App %q error = %v", name, err)
		}
	}
}

func TestAdminAppsRepositoryUpdatesOnlyProvidedFields(t *testing.T) {
	repository, db := newAdminAppsTestRepository(t)
	actor := adminapps.Actor{ID: "actor-update", Role: "super_admin"}
	created, err := repository.Create(context.Background(), actor, map[string]any{
		"name":        "Before",
		"platform":    "ios",
		"description": "keep me",
		"bundleId":    "com.example.before",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	updated, err := repository.Update(context.Background(), actor, created.ID, map[string]any{
		"name": "After",
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Name != "After" {
		t.Errorf("name = %q, want After", updated.Name)
	}
	// 补丁里没出现的字段必须原样保留。
	if updated.Description != "keep me" || updated.BundleID != "com.example.before" {
		t.Errorf("未提供的字段被改动了: description=%q bundleId=%q", updated.Description, updated.BundleID)
	}
	if got := adminAppsAuditCount(t, db, "app_update"); got != 1 {
		t.Errorf("app_update 审计条数 = %d, want 1", got)
	}

	// 未知字段必须显式拒绝，而不是静默丢弃。
	if _, err := repository.Update(context.Background(), actor, created.ID, map[string]any{"notAField": 1}); !errors.Is(err, adminapps.ErrInvalid) {
		t.Fatalf("未知字段 Update() error = %v, want ErrInvalid", err)
	}
	// 空补丁同样拒绝。
	if _, err := repository.Update(context.Background(), actor, created.ID, map[string]any{}); !errors.Is(err, adminapps.ErrInvalid) {
		t.Fatalf("空补丁 Update() error = %v, want ErrInvalid", err)
	}
	// 不存在的 App 返回 ErrNotFound，且不写审计。
	if _, err := repository.Update(context.Background(), actor, bson.NewObjectID().Hex(), map[string]any{"name": "Ghost"}); !errors.Is(err, adminapps.ErrNotFound) {
		t.Fatalf("不存在的 App Update() error = %v, want ErrNotFound", err)
	}
	if got := adminAppsAuditCount(t, db, "app_update"); got != 1 {
		t.Errorf("失败更新留下了审计: app_update 条数 = %d, want 1", got)
	}
}

func TestAdminAppsRepositorySoftDeletesApp(t *testing.T) {
	repository, db := newAdminAppsTestRepository(t)
	actor := adminapps.Actor{ID: "actor-delete", Role: "super_admin"}
	created, err := repository.Create(context.Background(), actor, map[string]any{
		"name":     "Doomed",
		"platform": "web",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	deleted, err := repository.Delete(context.Background(), actor, created.ID)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if deleted.Status != "deprecated" {
		t.Errorf("status = %q, want deprecated", deleted.Status)
	}

	// 软删除：文档仍在，只是状态变了 —— 物理删除会让引用它的创作数据失去归属。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	total, err := db.Collection("apps").CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatalf("统计 apps: %v", err)
	}
	if total != 1 {
		t.Errorf("apps 文档数 = %d, want 1（软删除不得删文档）", total)
	}
	if got := adminAppsAuditCount(t, db, "app_delete"); got != 1 {
		t.Errorf("app_delete 审计条数 = %d, want 1", got)
	}
	if _, err := repository.Delete(context.Background(), actor, bson.NewObjectID().Hex()); !errors.Is(err, adminapps.ErrNotFound) {
		t.Fatalf("删除不存在的 App error = %v, want ErrNotFound", err)
	}
	if got := adminAppsAuditCount(t, db, "app_delete"); got != 1 {
		t.Errorf("失败删除留下了审计: app_delete 条数 = %d, want 1", got)
	}
}

// TestAdminAppsRepositoryClearingIdentifiersFreesThem 验证「把标识清空」真的把唯一索引的
// 占用一起释放。仓储层用 $unset 而不是写空数组，正是因为留着空数组仍会被
// uniq_android_native_identifiers 视为占用。
func TestAdminAppsRepositoryClearingIdentifiersFreesThem(t *testing.T) {
	repository, _ := newAdminAppsTestRepository(t)
	actor := adminapps.Actor{ID: "actor-release", Role: "super_admin"}
	clientID := "com.example.release-" + uuid.NewString()

	first, err := repository.Create(context.Background(), actor, map[string]any{
		"name": "Holder", "platform": "android", "clientId": clientID,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := repository.Update(context.Background(), actor, first.ID, map[string]any{"clientId": ""}); err != nil {
		t.Fatalf("清空 clientId error = %v", err)
	}
	// 释放后同一个标识必须能被另一个 App 拿走。
	if _, err := repository.Create(context.Background(), actor, map[string]any{
		"name": "Taker", "platform": "android", "clientId": clientID,
	}); err != nil {
		t.Fatalf("复用已释放标识 Create() error = %v", err)
	}
}

func TestAdminAppsRepositoryDisablesLegalPageAtomically(t *testing.T) {
	repository, db := newAdminAppsTestRepository(t)
	actor := adminapps.Actor{ID: "actor-legal-disable", Role: "super_admin"}
	id := bson.NewObjectID()
	ctx := context.Background()
	if _, err := db.Collection("apps").InsertOne(ctx, bson.M{
		"_id": id, "name": "Legal app", "platform": "web",
		"nativeConfig": bson.M{"legal": bson.M{"privacyUrl": "https://legal.example.com/privacy"}},
		"legalPages": bson.M{"privacy": bson.M{
			"status": "active", "currentVersionId": "privacy-v2", "publicUrl": "https://legal.example.com/privacy-v2",
			"versions": bson.A{
				bson.M{"versionId": "privacy-v1", "publicUrl": "https://legal.example.com/privacy-v1", "storageKey": "private/v1", "status": "superseded", "uploadedBy": "actor-old"},
				bson.M{"versionId": "privacy-v2", "publicUrl": "https://legal.example.com/privacy-v2", "storageKey": "private/v2", "status": "active", "uploadedBy": "actor-old"},
			},
		}},
	}); err != nil {
		t.Fatalf("插入法律页应用: %v", err)
	}

	response, err := repository.LegalAction(ctx, actor, id.Hex(), "disable", "privacy", map[string]any{})
	if err != nil {
		t.Fatalf("LegalAction(disable) error = %v", err)
	}
	pages := adminapps.FieldObject(response, "pages")
	privacy := adminapps.FieldObject(pages, "privacy")
	if privacy["status"] != "disabled" || privacy["current"] != nil || privacy["publicUrl"] != nil {
		t.Fatalf("disable response=%#v", privacy)
	}
	versions := adminapps.FieldArray(privacy, "versions")
	if len(versions) != 2 || adminapps.FieldString(nestedDocument(versions[0]), "versionId") != "privacy-v2" {
		t.Fatalf("versions=%#v, want latest-first", versions)
	}
	if _, leaked := nestedDocument(versions[0])["storageKey"]; leaked {
		t.Fatalf("法律页响应泄漏 storageKey: %#v", versions[0])
	}

	var stored bson.M
	if err := db.Collection("apps").FindOne(ctx, bson.M{"_id": id}).Decode(&stored); err != nil {
		t.Fatalf("读回 disable 后应用: %v", err)
	}
	document := adminapps.Document(documentFromRaw(stored))
	legal := adminapps.FieldObject(adminapps.FieldObject(document, "nativeConfig"), "legal")
	if _, exists := legal["privacyUrl"]; exists {
		t.Fatalf("nativeConfig.legal.privacyUrl 仍存在: %#v", legal)
	}
	slot := adminapps.FieldObject(adminapps.FieldObject(document, "legalPages"), "privacy")
	if adminapps.FieldString(slot, "status") != "disabled" || slot["currentVersionId"] != nil || slot["publicUrl"] != nil {
		t.Fatalf("disable 后 slot=%#v", slot)
	}
	if adminapps.FieldString(slot, "disabledBy") != actor.ID || adminapps.FieldString(slot, "lastAction") != "disable" || adminapps.FieldString(slot, "lastActionBy") != actor.ID || slot["disabledAt"] == nil || slot["lastActionAt"] == nil {
		t.Fatalf("disable 审计元数据缺失: %#v", slot)
	}
	storedVersions := adminapps.FieldArray(slot, "versions")
	if adminapps.FieldString(nestedDocument(storedVersions[1]), "status") != "inactive" {
		t.Fatalf("current version 未标 inactive: %#v", storedVersions)
	}
	if got := adminAppsAuditCount(t, db, "app_legal_page_disable"); got != 1 {
		t.Fatalf("app_legal_page_disable 审计数=%d want=1", got)
	}
}

func TestAdminAppsRepositoryManagedLegalPublishingDoesNotPersistPlaceholder(t *testing.T) {
	repository, db := newAdminAppsTestRepository(t)
	id := bson.NewObjectID()
	if _, err := db.Collection("apps").InsertOne(context.Background(), bson.M{"_id": id, "name": "Managed legal", "platform": "web"}); err != nil {
		t.Fatalf("插入应用: %v", err)
	}
	_, err := repository.EnableManagedLegalPublishing(context.Background(), adminapps.Actor{ID: "actor-managed", Role: "super_admin"}, id.Hex())
	if !errors.Is(err, adminapps.ErrExternalUnavailable) {
		t.Fatalf("EnableManagedLegalPublishing error=%v, want ErrExternalUnavailable", err)
	}
	var stored bson.M
	if err := db.Collection("apps").FindOne(context.Background(), bson.M{"_id": id}).Decode(&stored); err != nil {
		t.Fatalf("读回应用: %v", err)
	}
	if _, exists := stored["legalPublishing"]; exists {
		t.Fatalf("managed unavailable 不应写入 legalPublishing: %#v", stored["legalPublishing"])
	}
	if got := adminAppsAuditCount(t, db, "app_legal_managed_publishing_requested"); got != 0 {
		t.Fatalf("managed unavailable 不应写审计，got=%d", got)
	}
}

func TestAdminAppsRepositoryLocksActiveLegalPublishingPolicy(t *testing.T) {
	repository, db := newAdminAppsTestRepository(t)
	id := bson.NewObjectID()
	if _, err := db.Collection("apps").InsertOne(context.Background(), bson.M{
		"_id": id, "name": "Bound legal", "platform": "web",
		"legalPublishing": bson.M{"approvedBrandDomain": "legal.example.com", "publisherBinding": bson.M{"status": "verified", "provider": "cloudflare_r2"}},
		"legalPages":      bson.M{"privacy": bson.M{"status": "active", "currentVersionId": "privacy-v1", "publicUrl": "https://legal.example.com/privacy-v1"}},
	}); err != nil {
		t.Fatalf("插入应用: %v", err)
	}
	actor := adminapps.Actor{ID: "actor-binding", Role: "super_admin"}
	same := map[string]any{"approvedBrandDomain": "legal.example.com", "publisherBinding": map[string]any{"status": "pending", "provider": "cloudflare_r2"}}
	if _, err := repository.UpdateLegalPublishing(context.Background(), actor, id.Hex(), same); err != nil {
		t.Fatalf("同策略 pending 应是幂等读取，error=%v", err)
	}
	if got := adminAppsAuditCount(t, db, "app_legal_publishing_update"); got != 0 {
		t.Fatalf("同策略 pending 不应写审计，got=%d", got)
	}
	_, err := repository.UpdateLegalPublishing(context.Background(), actor, id.Hex(), map[string]any{"approvedBrandDomain": "other.example.com", "publisherBinding": map[string]any{"status": "pending", "provider": "cloudflare_r2"}})
	var conflict *adminapps.ConflictError
	if !errors.As(err, &conflict) || conflict.Message != "Disable active legal pages before changing the approved publisher binding" {
		t.Fatalf("更换 active publisher error=%v, want policy conflict", err)
	}
	if got := adminAppsAuditCount(t, db, "app_legal_publishing_update"); got != 0 {
		t.Fatalf("冲突不应写审计，got=%d", got)
	}
}
