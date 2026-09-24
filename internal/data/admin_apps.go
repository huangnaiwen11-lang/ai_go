package data

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ai-business-service/internal/biz/adminapps"
	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const adminAppsCollection = schema.CollectionApps

// adminObjectFields 是 Node `toAdminJSON` 里「缺失时兜成空对象」的五个子文档。
// 前端按对象使用它们，所以不能省略键。
var adminObjectFields = []string{"packageConfig", "serverIntegrations", "nativeConfig", "storeSubmission", "vendorScope"}

// adminScalarFields 是 Node `toAdminJSON` 里「有则输出、无则整个键消失」的字段。
// 顺序与 Node 一致，只是为了让响应体的字段顺序稳定，便于比对。
var adminScalarFields = []string{
	"name", "description", "platform", "clientId", "bundleId", "packageName",
	"domain", "apiUrl", "version", "status", "iconUrl", "contact",
	"source", "partnerSiteId", "appKey", "nativeBuild", "stats", "createdBy",
	"createdAt", "updatedAt",
}

type mongoAdminAppsRepository struct{ data *Data }

func NewAdminAppsRepository(data *Data) adminapps.Repository {
	return &mongoAdminAppsRepository{data: data}
}

func (r *mongoAdminAppsRepository) collection() (*mongo.Collection, error) {
	if r == nil || r.data == nil || r.data.database == nil {
		return nil, fmt.Errorf("admin apps repository unavailable")
	}
	return r.data.database.Collection(adminAppsCollection), nil
}

func (r *mongoAdminAppsRepository) List(ctx context.Context, query adminapps.Query) (adminapps.Page, error) {
	query, err := adminapps.NormalizeQuery(query)
	if err != nil {
		return adminapps.Page{}, err
	}
	collection, err := r.collection()
	if err != nil {
		return adminapps.Page{}, err
	}
	filter := appFilter(query)
	total, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		return adminapps.Page{}, fmt.Errorf("count apps: %w", err)
	}
	cursor, err := collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).SetSkip(int64((query.Page-1)*query.Limit)).SetLimit(int64(query.Limit)))
	if err != nil {
		return adminapps.Page{}, fmt.Errorf("list apps: %w", err)
	}
	defer cursor.Close(ctx)
	apps := make([]adminapps.App, 0, query.Limit)
	for cursor.Next(ctx) {
		var raw bson.M
		if err := cursor.Decode(&raw); err != nil {
			return adminapps.Page{}, fmt.Errorf("decode app: %w", err)
		}
		apps = append(apps, appFromDocument(raw))
	}
	if err := cursor.Err(); err != nil {
		return adminapps.Page{}, fmt.Errorf("iterate apps: %w", err)
	}
	return adminapps.Page{Apps: apps, Total: total}, nil
}

func (r *mongoAdminAppsRepository) Get(ctx context.Context, id string) (adminapps.App, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return adminapps.App{}, adminapps.ErrInvalid
	}
	collection, err := r.collection()
	if err != nil {
		return adminapps.App{}, err
	}
	var raw bson.M
	if err := collection.FindOne(ctx, appIDFilter(id)).Decode(&raw); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return adminapps.App{}, adminapps.ErrNotFound
		}
		return adminapps.App{}, fmt.Errorf("get app: %w", err)
	}
	return appFromDocument(raw), nil
}

func appIDFilter(id string) bson.M {
	filter := bson.M{"_id": id}
	if objectID, err := bson.ObjectIDFromHex(id); err == nil {
		filter = bson.M{"$or": bson.A{bson.M{"_id": id}, bson.M{"_id": objectID}}}
	}
	return filter
}

func (r *mongoAdminAppsRepository) Overview(ctx context.Context) (adminapps.Overview, error) {
	collection, err := r.collection()
	if err != nil {
		return adminapps.Overview{}, err
	}
	total, err := collection.CountDocuments(ctx, bson.M{})
	if err != nil {
		return adminapps.Overview{}, fmt.Errorf("count apps overview: %w", err)
	}
	active, err := collection.CountDocuments(ctx, bson.M{"status": "active"})
	if err != nil {
		return adminapps.Overview{}, fmt.Errorf("count active apps: %w", err)
	}
	cursor, err := collection.Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"status": "active"}},
		bson.M{"$group": bson.M{"_id": "$platform", "count": bson.M{"$sum": 1}}},
	})
	if err != nil {
		return adminapps.Overview{}, fmt.Errorf("aggregate app platforms: %w", err)
	}
	defer cursor.Close(ctx)
	byPlatform := map[string]int64{}
	for cursor.Next(ctx) {
		var row struct {
			Platform string `bson:"_id"`
			Count    int64  `bson:"count"`
		}
		if err := cursor.Decode(&row); err != nil {
			return adminapps.Overview{}, fmt.Errorf("decode app platform: %w", err)
		}
		if row.Platform != "" {
			byPlatform[row.Platform] = row.Count
		}
	}
	if err := cursor.Err(); err != nil {
		return adminapps.Overview{}, fmt.Errorf("iterate app platforms: %w", err)
	}
	return adminapps.Overview{Total: total, Active: active, Suspended: total - active, ByPlatform: byPlatform}, nil
}

// Create 新建 App，与 admin_audit 审计同事务落库。
func (r *mongoAdminAppsRepository) Create(ctx context.Context, actor adminapps.Actor, input map[string]any) (adminapps.App, error) {
	document, err := adminapps.ValidateCreate(input)
	if err != nil {
		return adminapps.App{}, err
	}
	collection, err := r.collection()
	if err != nil {
		return adminapps.App{}, err
	}
	if err := r.assertCreateIdentifiersFree(ctx, collection, document); err != nil {
		return adminapps.App{}, err
	}

	now := adminAppsNow()
	id := bson.NewObjectID()
	record := bson.M{"_id": id}
	for key, value := range document {
		// nil 表示「提交了空值」，在 Node 里落成 undefined —— 字段根本不入库。
		if value == nil {
			continue
		}
		record[key] = value
	}
	// Node 的 `App.create` 硬编码 `packageConfig: data.packageConfig || {}`，
	// 而 packageConfig 不在 zod createBody 里，所以创建时恒为空对象。
	record["packageConfig"] = bson.M{}
	if identifiers := adminapps.NativeIdentifiers(document); len(identifiers) > 0 {
		record["nativeIdentifiers"] = identifiers
	}
	record["createdBy"] = actor.ID
	record["createdAt"] = now
	record["updatedAt"] = now

	var result adminapps.App
	err = NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		if _, err := collection.InsertOne(tx, record); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return &adminapps.ConflictError{Message: "App identifier already registered"}
			}
			return fmt.Errorf("create app: %w", err)
		}
		if err := r.audit(tx, actor, "app_create", id.Hex(), now); err != nil {
			return err
		}
		result = appFromDocument(record)
		return nil
	})
	if err != nil {
		return adminapps.App{}, err
	}
	return result, nil
}

// Update 局部更新：只写补丁里显式出现的字段，未提供的保持原值。
func (r *mongoAdminAppsRepository) Update(ctx context.Context, actor adminapps.Actor, id string, input map[string]any) (adminapps.App, error) {
	collection, err := r.collection()
	if err != nil {
		return adminapps.App{}, err
	}
	var raw bson.M
	if err := collection.FindOne(ctx, appIDFilter(strings.TrimSpace(id))).Decode(&raw); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return adminapps.App{}, adminapps.ErrNotFound
		}
		return adminapps.App{}, fmt.Errorf("load app for update: %w", err)
	}
	// 既有文档必须先从 bson 形态转成纯 map：驱动会把嵌套文档解码成有序 bson.D，
	// 直接喂给 biz 会让 nativeBuild / nativeConfig 这类子对象在类型断言上落空。
	existing := documentFromRaw(raw)

	patch, err := adminapps.ValidateUpdate(input, existing)
	if err != nil {
		return adminapps.App{}, err
	}
	effective := adminapps.Merge(existing, patch)
	if err := r.assertNativeIdentifiersFree(ctx, collection, effective, objectIDOrNil(id)); err != nil {
		return adminapps.App{}, err
	}

	now := adminAppsNow()
	set := bson.M{}
	unset := bson.M{}
	for key, value := range patch {
		if value == nil {
			// 与 Node 一致：提交空字符串等价于把这个字段摘掉，而不是写成 null。
			unset[key] = ""
			continue
		}
		set[key] = value
	}
	if identifiers := adminapps.NativeIdentifiers(effective); len(identifiers) > 0 {
		set["nativeIdentifiers"] = identifiers
	} else {
		// 标识清空后必须真正移除字段，否则唯一部分索引仍然把这条文档算作「已占用」。
		unset["nativeIdentifiers"] = ""
	}
	set["updatedAt"] = now

	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}

	var result adminapps.App
	err = NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		var updated bson.M
		err := collection.FindOneAndUpdate(
			tx,
			appIDFilter(strings.TrimSpace(id)),
			update,
			options.FindOneAndUpdate().SetReturnDocument(options.After),
		).Decode(&updated)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return adminapps.ErrNotFound
		}
		if err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return &adminapps.ConflictError{Message: "App identifier already registered"}
			}
			return fmt.Errorf("update app: %w", err)
		}
		if err := r.audit(tx, actor, "app_update", strings.TrimSpace(id), now); err != nil {
			return err
		}
		result = appFromDocument(updated)
		return nil
	})
	if err != nil {
		return adminapps.App{}, err
	}
	return result, nil
}

// Delete 是**软删除**：把 status 置为 deprecated，与 Node 的 `deleteApp` 一致。
// 不是物理删除 —— 物理删除会让引用该 App 的创作/订单数据失去归属。
func (r *mongoAdminAppsRepository) Delete(ctx context.Context, actor adminapps.Actor, id string) (adminapps.App, error) {
	collection, err := r.collection()
	if err != nil {
		return adminapps.App{}, err
	}
	now := adminAppsNow()
	var result adminapps.App
	err = NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		var updated bson.M
		err := collection.FindOneAndUpdate(
			tx,
			appIDFilter(strings.TrimSpace(id)),
			bson.M{"$set": bson.M{"status": "deprecated", "updatedAt": now}},
			options.FindOneAndUpdate().SetReturnDocument(options.After),
		).Decode(&updated)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return adminapps.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete app: %w", err)
		}
		if err := r.audit(tx, actor, "app_delete", strings.TrimSpace(id), now); err != nil {
			return err
		}
		result = appFromDocument(updated)
		return nil
	})
	if err != nil {
		return adminapps.App{}, err
	}
	return result, nil
}

// PackageConfig exports the exact native build fields stored in Mongo. It is
// deliberately a local read: build execution and signing belong to an
// externally configured CI integration, never this admin API.
func (r *mongoAdminAppsRepository) PackageConfig(ctx context.Context, id string) (adminapps.Document, error) {
	collection, err := r.collection()
	if err != nil {
		return nil, err
	}
	var raw bson.M
	if err := collection.FindOne(ctx, appIDFilter(strings.TrimSpace(id))).Decode(&raw); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, adminapps.ErrNotFound
		}
		return nil, fmt.Errorf("get app package config: %w", err)
	}
	return adminapps.BuildPackageConfig(documentFromRaw(raw))
}

// assertCreateIdentifiersFree 复刻 Node `createApp` 落库前的四次唯一性查询。
//
// 这几次查询与写入之间没有事务边界（Node 也没有）。真正的兜底是
// `uniq_android_native_identifiers` 这个唯一部分索引 —— 并发插入会由它挡下，
// 再被上面的 `mongo.IsDuplicateKeyError` 映射成 409。
func (r *mongoAdminAppsRepository) assertCreateIdentifiersFree(ctx context.Context, collection *mongo.Collection, document adminapps.Document) error {
	if err := r.assertNativeIdentifiersFree(ctx, collection, document, bson.NilObjectID); err != nil {
		return err
	}
	platform := adminapps.FieldString(document, "platform")

	if clientID := adminapps.FieldString(document, "clientId"); clientID != "" && platform != "android" {
		if err := assertAppFieldFree(ctx, collection, bson.M{"platform": platform, "clientId": clientID}, "clientId already registered"); err != nil {
			return err
		}
	}
	if bundleID := adminapps.FieldString(document, "bundleId"); bundleID != "" {
		if err := assertAppFieldFree(ctx, collection, bson.M{"bundleId": bundleID}, "Bundle ID already registered"); err != nil {
			return err
		}
	}
	if packageName := adminapps.FieldString(document, "packageName"); packageName != "" && platform != "android" {
		if err := assertAppFieldFree(ctx, collection, bson.M{"packageName": packageName}, "Package name already registered"); err != nil {
			return err
		}
	}
	return nil
}

// assertNativeIdentifiersFree 复刻 Node `assertNativeAndroidIdentifiersAvailable`。
//
// 跨三个字段做 OR，且用大小写不敏感的锚定正则 —— 与 Node 的 `exactIdentifierPattern` 一致。
func (r *mongoAdminAppsRepository) assertNativeIdentifiersFree(
	ctx context.Context,
	collection *mongo.Collection,
	document map[string]any,
	excludeID bson.ObjectID,
) error {
	identifiers := adminapps.NativeIdentifiers(document)
	if len(identifiers) == 0 {
		return nil
	}
	patterns := make(bson.A, 0, len(identifiers))
	for _, identifier := range identifiers {
		patterns = append(patterns, bson.Regex{Pattern: "^" + regexp.QuoteMeta(identifier) + "$", Options: "i"})
	}
	filter := bson.M{"$or": bson.A{
		bson.M{"platform": "android", "clientId": bson.M{"$in": patterns}},
		bson.M{"packageName": bson.M{"$in": patterns}},
		bson.M{"nativeBuild.android.applicationId": bson.M{"$in": patterns}},
	}}
	if !excludeID.IsZero() {
		filter["_id"] = bson.M{"$ne": excludeID}
	}
	count, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		return fmt.Errorf("check app identifiers: %w", err)
	}
	if count > 0 {
		return &adminapps.ConflictError{Message: "App identifier already registered"}
	}
	return nil
}

func assertAppFieldFree(ctx context.Context, collection *mongo.Collection, filter bson.M, message string) error {
	count, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		return fmt.Errorf("check app field availability: %w", err)
	}
	if count > 0 {
		return &adminapps.ConflictError{Message: message}
	}
	return nil
}

func objectIDOrNil(id string) bson.ObjectID {
	objectID, err := bson.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return bson.NilObjectID
	}
	return objectID
}

// adminAppsNow 返回截断到毫秒的 UTC 时间。
//
// BSON DateTime 只有毫秒精度，不截断会让「刚写完的响应」和「再读一次」
// 在同一个字段上给出不同的值。
func adminAppsNow() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}

func (r *mongoAdminAppsRepository) audit(ctx context.Context, actor adminapps.Actor, action, target string, now time.Time) error {
	_, err := r.data.database.Collection(schema.CollectionAdminAudit).InsertOne(ctx, bson.M{
		"_id": uuid.NewString(), "actor_id": actor.ID, "target_id": target, "action": action, "created_at": now,
	})
	if err != nil {
		return fmt.Errorf("write app audit: %w", err)
	}
	return nil
}

// appFromDocument 按 Node `toAdminJSON` 的白名单投影。
//
// 必须用白名单，而不是「读回整份文档再删几个键」：
//   - `nativeIdentifiers` 在 Node 里是 `select: false`，`__v` 是 Mongoose 内部字段，
//     两者都不该出现在响应里；
//   - 五个子文档在 Node 里缺失时兜成 `{}`，前端按对象使用它们。
func appFromDocument(raw bson.M) adminapps.App {
	document := documentFromRaw(raw)
	id := valueString(raw["_id"])

	fields := make(map[string]any, len(adminScalarFields)+len(adminObjectFields)+2)
	fields["id"] = id
	for _, key := range adminScalarFields {
		value, exists := document[key]
		if !exists || value == nil {
			continue
		}
		fields[key] = value
	}
	for _, key := range adminObjectFields {
		if object, ok := document[key].(map[string]any); ok {
			fields[key] = object
			continue
		}
		fields[key] = map[string]any{}
	}
	fields["resolvedApiUrl"] = adminapps.ResolveAPIURL(document)

	return adminapps.App{
		ID:             id,
		Name:           valueString(document["name"]),
		Description:    valueString(document["description"]),
		Platform:       valueString(document["platform"]),
		Status:         valueString(document["status"]),
		ClientID:       valueString(document["clientId"]),
		BundleID:       valueString(document["bundleId"]),
		PackageName:    valueString(document["packageName"]),
		Domain:         valueString(document["domain"]),
		APIURL:         valueString(document["apiUrl"]),
		ResolvedAPIURL: adminapps.ResolveAPIURL(document),
		Fields:         fields,
		CreatedAt:      valueTime(document["createdAt"]),
		UpdatedAt:      valueTime(document["updatedAt"]),
	}
}

func appFilter(query adminapps.Query) bson.M {
	filter := bson.M{}
	if query.Platform != "" {
		filter["platform"] = query.Platform
	}
	if query.Status != "" {
		filter["status"] = query.Status
	}
	if query.Search != "" {
		pattern := primitiveRegex(query.Search)
		filter["$or"] = bson.A{
			bson.M{"name": pattern}, bson.M{"bundleId": pattern}, bson.M{"packageName": pattern},
			bson.M{"domain": pattern}, bson.M{"apiUrl": pattern}, bson.M{"clientId": pattern},
		}
	}
	return filter
}

func primitiveRegex(value string) bson.Regex {
	return bson.Regex{Pattern: regexp.QuoteMeta(value), Options: "i"}
}

func valueString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case bson.ObjectID:
		return typed.Hex()
	default:
		return ""
	}
}

func valueTime(value any) time.Time {
	switch typed := value.(type) {
	case time.Time:
		return typed
	case bson.DateTime:
		return typed.Time()
	default:
		return time.Time{}
	}
}

// documentFromRaw 把驱动解码出来的 bson 形态递归转成纯 map / slice。
//
// 必须处理 bson.D：驱动把 `bson.M` 里的嵌套文档解码成**有序 bson.D**
// （empty_interface_codec 的行为）。只认 bson.M 会让 `nativeBuild`、`nativeConfig`
// 这类子对象在 biz 的类型断言上全部落空 —— 表现为「写进去的配置读回来是空的」，
// 而错误信息一条都没有。
func documentFromRaw(raw bson.M) map[string]any {
	document := make(map[string]any, len(raw))
	for key, value := range raw {
		document[key] = normalizeBSONValue(value)
	}
	return document
}

func normalizeBSONValue(value any) any {
	switch typed := value.(type) {
	case bson.ObjectID:
		return typed.Hex()
	case bson.DateTime:
		return typed.Time()
	case bson.M:
		return documentFromRaw(typed)
	case bson.D:
		document := make(map[string]any, len(typed))
		for _, entry := range typed {
			document[entry.Key] = normalizeBSONValue(entry.Value)
		}
		return document
	case bson.A:
		rows := make([]any, len(typed))
		for index := range typed {
			rows[index] = normalizeBSONValue(typed[index])
		}
		return rows
	case []any:
		rows := make([]any, len(typed))
		for index := range typed {
			rows[index] = normalizeBSONValue(typed[index])
		}
		return rows
	default:
		return value
	}
}

var _ adminapps.Repository = (*mongoAdminAppsRepository)(nil)
