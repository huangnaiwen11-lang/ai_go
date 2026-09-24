package data

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"ai-business-service/internal/biz/adminutm"
	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const adminUTMCollection = schema.CollectionUTMLinks

type mongoAdminUTMRepository struct{ data *Data }

func NewAdminUTMRepository(data *Data) adminutm.Repository {
	return &mongoAdminUTMRepository{data: data}
}

func (r *mongoAdminUTMRepository) collection() (*mongo.Collection, error) {
	if r == nil || r.data == nil || r.data.database == nil {
		return nil, fmt.Errorf("admin utm repository unavailable")
	}
	return r.data.database.Collection(adminUTMCollection), nil
}

func (r *mongoAdminUTMRepository) List(ctx context.Context, query adminutm.Query) (adminutm.Page, error) {
	query, err := adminutm.NormalizeQuery(query)
	if err != nil {
		return adminutm.Page{}, err
	}
	collection, err := r.collection()
	if err != nil {
		return adminutm.Page{}, err
	}
	filter := bson.M{}
	if query.Enabled != nil {
		filter["enabled"] = *query.Enabled
	}
	if query.Keyword != "" {
		pattern := bson.Regex{Pattern: regexp.QuoteMeta(query.Keyword), Options: "i"}
		filter["$or"] = bson.A{bson.M{"slug": pattern}, bson.M{"label": pattern}, bson.M{"utmSource": pattern}, bson.M{"utmCampaign": pattern}}
	}
	total, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		return adminutm.Page{}, fmt.Errorf("count utm links: %w", err)
	}
	cursor, err := collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).SetSkip(int64(query.Skip)).SetLimit(int64(query.Limit)))
	if err != nil {
		return adminutm.Page{}, fmt.Errorf("list utm links: %w", err)
	}
	defer cursor.Close(ctx)
	items := make([]adminutm.Link, 0, query.Limit)
	for cursor.Next(ctx) {
		var raw bson.M
		if err := cursor.Decode(&raw); err != nil {
			return adminutm.Page{}, fmt.Errorf("decode utm link: %w", err)
		}
		items = append(items, utmLinkFromDocument(raw))
	}
	if err := cursor.Err(); err != nil {
		return adminutm.Page{}, fmt.Errorf("iterate utm links: %w", err)
	}
	return adminutm.Page{Total: total, Items: items}, nil
}

func (r *mongoAdminUTMRepository) Sources(ctx context.Context, onlyEnabled bool) ([]adminutm.Source, error) {
	collection, err := r.collection()
	if err != nil {
		return nil, err
	}
	filter := bson.M{}
	if onlyEnabled {
		filter["enabled"] = true
	}
	cursor, err := collection.Find(ctx, filter, options.Find().SetProjection(bson.M{"_id": 1, "slug": 1, "label": 1, "utmSource": 1, "utmMedium": 1, "utmCampaign": 1, "utmContent": 1, "utmTerm": 1, "enabled": 1, "clicks": 1}))
	if err != nil {
		return nil, fmt.Errorf("list utm sources: %w", err)
	}
	defer cursor.Close(ctx)
	buckets := map[string]*adminutm.Source{}
	for cursor.Next(ctx) {
		var raw bson.M
		if err := cursor.Decode(&raw); err != nil {
			return nil, fmt.Errorf("decode utm source: %w", err)
		}
		source := strings.TrimSpace(valueString(raw["utmSource"]))
		if source == "" {
			continue
		}
		bucket := buckets[source]
		if bucket == nil {
			bucket = &adminutm.Source{UtmSource: source, Links: make([]adminutm.SourceLink, 0)}
			buckets[source] = bucket
		}
		enabled := valueBool(raw["enabled"])
		bucket.Enabled = bucket.Enabled || enabled
		bucket.TotalClicks += valueInt64(raw["clicks"])
		bucket.Links = append(bucket.Links, adminutm.SourceLink{ID: valueString(raw["_id"]), Slug: valueString(raw["slug"]), Label: valueString(raw["label"]), Enabled: enabled, UtmMedium: valueString(raw["utmMedium"]), UtmCampaign: valueString(raw["utmCampaign"]), UtmContent: valueString(raw["utmContent"]), UtmTerm: valueString(raw["utmTerm"])})
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate utm sources: %w", err)
	}
	result := make([]adminutm.Source, 0, len(buckets))
	for _, source := range buckets {
		result = append(result, *source)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Enabled != result[j].Enabled {
			return result[i].Enabled
		}
		if result[i].TotalClicks != result[j].TotalClicks {
			return result[i].TotalClicks > result[j].TotalClicks
		}
		return result[i].UtmSource < result[j].UtmSource
	})
	return result, nil
}

// Create 新建 UTM 链接，与 admin_audit 审计同事务落库。
func (r *mongoAdminUTMRepository) Create(ctx context.Context, actor adminutm.Actor, in adminutm.Input) (adminutm.Link, error) {
	// 仓储是持久化边界，归一化在这里再走一次（与 List 里调 NormalizeQuery 同一思路）：
	// 调用方漏调 NormalizeInput 时不能让 utm_* 键混进 extraParams 或留下空 slug。
	in, err := adminutm.NormalizeInput(in, true)
	if err != nil {
		return adminutm.Link{}, err
	}
	collection, err := r.collection()
	if err != nil {
		return adminutm.Link{}, err
	}
	now := utmNow()
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	// _id 必须是 ObjectId 而不是 UUID：Node 端 UtmLink 模型用默认 ObjectId，
	// 且路由对 :id 有 ^[a-f0-9]{24}$ 的校验。用 UUID 会让 Go 建出来的链接
	// 在 Node 侧（短链跳转 /r/:slug 的回查）以及任何按 24 位 hex 校验的调用方那里失配。
	id := bson.NewObjectID()
	document := bson.M{
		"_id": id, "slug": *in.Slug, "label": *in.Label,
		"targetPath": normalizeTargetPath(utmStringOr(in.TargetPath, adminutm.DefaultTargetPath)),
		"utmSource":  *in.UtmSource, "utmMedium": utmStringOr(in.UtmMedium, adminutm.DefaultUtmMedium),
		"utmCampaign": utmStringOr(in.UtmCampaign, ""), "utmContent": utmStringOr(in.UtmContent, ""),
		"utmTerm": utmStringOr(in.UtmTerm, ""), "notes": utmStringOr(in.Notes, ""),
		"extraParams": utmParamsOrEmpty(in.ExtraParams), "enabled": enabled,
		"createdBy": actor.ID, "clicks": int64(0), "createdAt": now, "updatedAt": now,
	}
	var result adminutm.Link
	err = NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		if _, err := collection.InsertOne(tx, document); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return adminutm.ErrConflict
			}
			return fmt.Errorf("create utm link: %w", err)
		}
		if err := r.audit(tx, actor, "utm_link_create", id.Hex(), now); err != nil {
			return err
		}
		result = utmLinkFromDocument(document)
		return nil
	})
	if err != nil {
		return adminutm.Link{}, err
	}
	return result, nil
}

// Update 局部更新 UTM 链接：只写显式提供的字段，未提供的保持原值。
func (r *mongoAdminUTMRepository) Update(ctx context.Context, actor adminutm.Actor, id string, in adminutm.Input) (adminutm.Link, error) {
	objectID, err := utmObjectID(id)
	if err != nil {
		return adminutm.Link{}, err
	}
	in, err = adminutm.NormalizeInput(in, false)
	if err != nil {
		return adminutm.Link{}, err
	}
	collection, err := r.collection()
	if err != nil {
		return adminutm.Link{}, err
	}
	fields := bson.M{}
	if in.Slug != nil {
		fields["slug"] = *in.Slug
	}
	if in.Label != nil {
		fields["label"] = *in.Label
	}
	if in.TargetPath != nil {
		fields["targetPath"] = normalizeTargetPath(*in.TargetPath)
	}
	if in.UtmSource != nil {
		fields["utmSource"] = *in.UtmSource
	}
	if in.UtmMedium != nil {
		fields["utmMedium"] = *in.UtmMedium
	}
	if in.UtmCampaign != nil {
		fields["utmCampaign"] = *in.UtmCampaign
	}
	if in.UtmContent != nil {
		fields["utmContent"] = *in.UtmContent
	}
	if in.UtmTerm != nil {
		fields["utmTerm"] = *in.UtmTerm
	}
	if in.Notes != nil {
		fields["notes"] = *in.Notes
	}
	if in.ExtraParams != nil {
		fields["extraParams"] = in.ExtraParams
	}
	if in.Enabled != nil {
		fields["enabled"] = *in.Enabled
	}
	if len(fields) == 0 {
		return adminutm.Link{}, adminutm.ErrInvalid
	}
	now := utmNow()
	fields["updatedAt"] = now
	var result adminutm.Link
	err = NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		var updated bson.M
		err := collection.FindOneAndUpdate(tx,
			bson.M{"_id": objectID}, bson.M{"$set": fields},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return adminutm.ErrNotFound
		}
		if err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return adminutm.ErrConflict
			}
			return fmt.Errorf("update utm link: %w", err)
		}
		if err := r.audit(tx, actor, "utm_link_update", objectID.Hex(), now); err != nil {
			return err
		}
		result = utmLinkFromDocument(updated)
		return nil
	})
	if err != nil {
		return adminutm.Link{}, err
	}
	return result, nil
}

// Delete 删除 UTM 链接，与审计同事务。
func (r *mongoAdminUTMRepository) Delete(ctx context.Context, actor adminutm.Actor, id string) error {
	objectID, err := utmObjectID(id)
	if err != nil {
		return err
	}
	collection, err := r.collection()
	if err != nil {
		return err
	}
	now := utmNow()
	return NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		outcome, err := collection.DeleteOne(tx, bson.M{"_id": objectID})
		if err != nil {
			return fmt.Errorf("delete utm link: %w", err)
		}
		if outcome.DeletedCount == 0 {
			return adminutm.ErrNotFound
		}
		return r.audit(tx, actor, "utm_link_delete", objectID.Hex(), now)
	})
}

// utmNow 返回截断到毫秒的 UTC 时间。
//
// BSON DateTime 的精度就是毫秒。若不截断，Create/Update 的响应会带着纳秒尾数，
// 而同一份数据重新查出来只剩毫秒 —— 同一个字段在「刚写完」和「再读一次」两个时刻
// 值不一样，排障时会以为是数据被改过。
func utmNow() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}

// utmObjectID 把路径里的 24 位 hex 解析成 ObjectId。
// 格式不对按「输入非法」处理，而不是当成「不存在」——两者对调用方的含义不同。
func utmObjectID(id string) (bson.ObjectID, error) {
	objectID, err := bson.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return bson.NilObjectID, adminutm.ErrInvalid
	}
	return objectID, nil
}

func (r *mongoAdminUTMRepository) audit(ctx context.Context, actor adminutm.Actor, action, target string, now time.Time) error {
	_, err := r.data.database.Collection(schema.CollectionAdminAudit).InsertOne(ctx, bson.M{
		"_id": uuid.NewString(), "actor_id": actor.ID, "target_id": target, "action": action, "created_at": now,
	})
	if err != nil {
		return fmt.Errorf("write utm audit: %w", err)
	}
	return nil
}

func utmStringOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func utmParamsOrEmpty(params map[string]string) map[string]string {
	if params == nil {
		return map[string]string{}
	}
	return params
}

func utmLinkFromDocument(raw bson.M) adminutm.Link {
	link := adminutm.Link{
		ID: valueString(raw["_id"]), Slug: strings.ToLower(valueString(raw["slug"])), Label: valueString(raw["label"]), TargetPath: normalizeTargetPath(valueString(raw["targetPath"])),
		UtmSource: valueString(raw["utmSource"]), UtmMedium: valueString(raw["utmMedium"]), UtmCampaign: valueString(raw["utmCampaign"]), UtmContent: valueString(raw["utmContent"]), UtmTerm: valueString(raw["utmTerm"]),
		Enabled: valueBool(raw["enabled"]), Notes: valueString(raw["notes"]), Clicks: valueInt64(raw["clicks"]), CreatedAt: valueTime(raw["createdAt"]), UpdatedAt: valueTime(raw["updatedAt"]),
	}
	if raw["lastClickAt"] != nil {
		last := valueTime(raw["lastClickAt"])
		if !last.IsZero() {
			link.LastClickAt = &last
		}
	}
	link.ExtraParams = valueStringMap(raw["extraParams"])
	base := strings.TrimRight(getAdminFrontendURL(), "/")
	link.ShortURL = base + "/api/v1/r/" + url.PathEscape(link.Slug)
	link.FullURL = buildAdminUTMURL(base, link)
	return link
}

func getAdminFrontendURL() string {
	if value := strings.TrimSpace(getenv("FRONTEND_URL")); value != "" {
		return value
	}
	return "https://cling-ai.com"
}

func normalizeTargetPath(value string) string {
	if value == "" {
		return "/"
	}
	if strings.HasPrefix(value, "/") {
		return value
	}
	return "/" + value
}

func buildAdminUTMURL(base string, link adminutm.Link) string {
	u, err := url.Parse(base + normalizeTargetPath(link.TargetPath))
	if err != nil {
		return ""
	}
	query := u.Query()
	for key, value := range map[string]string{"utm_source": link.UtmSource, "utm_medium": link.UtmMedium, "utm_campaign": link.UtmCampaign, "utm_content": link.UtmContent, "utm_term": link.UtmTerm} {
		if value != "" {
			query.Set(key, value)
		}
	}
	for key, value := range link.ExtraParams {
		if key != "" && !strings.HasPrefix(strings.ToLower(key), "utm_") && query.Get(key) == "" {
			query.Set(key, value)
		}
	}
	u.RawQuery = query.Encode()
	return u.String()
}

func valueBool(value any) bool {
	v, ok := value.(bool)
	return ok && v
}

func valueInt64(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return 0
	}
}

// valueStringMap 读取 extraParams 子文档。
//
// 必须同时处理三种形态，少一种就会静默丢数据：
//   - bson.D：驱动把 bson.M 里的嵌套文档解码成**有序 bson.D**（empty_interface_codec 的行为）。
//     只认 bson.M 会让从库里读回来的 extraParams 变成 {} —— 这个静默丢失曾在实机上被实测抓到
//     （写进去的 from=probe 读回来是空对象）。
//   - map[string]string：Create/Update 回填结果时 document 里装的就是这个类型，
//     少这个分支会让「刚创建成功的响应」里 extraParams 是空的，而重新拉列表又有值。
func valueStringMap(value any) map[string]string {
	result := map[string]string{}
	switch v := value.(type) {
	case bson.D:
		for _, entry := range v {
			if text := valueString(entry.Value); text != "" {
				result[entry.Key] = text
			}
		}
	case bson.M:
		for key, item := range v {
			if text := valueString(item); text != "" {
				result[key] = text
			}
		}
	case map[string]any:
		for key, item := range v {
			if text := valueString(item); text != "" {
				result[key] = text
			}
		}
	case map[string]string:
		for key, item := range v {
			if item != "" {
				result[key] = item
			}
		}
	}
	return result
}

func getenv(key string) string {
	return os.Getenv(key)
}

var _ adminutm.Repository = (*mongoAdminUTMRepository)(nil)
