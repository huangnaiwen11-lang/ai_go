package data

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"ai-business-service/internal/biz/adminimage"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	legacyGeneratedImages = "generatedimages"
	legacyFaceSwapTasks   = "faceswaptasks"
	legacyTryOnTasks      = "tryontasks"
	legacyUsers           = "users"
)

type mongoAdminImageRepository struct{ data *Data }

// NewAdminImageRepository reads the legacy collections from NewAdminData's
// explicitly configured staging database. It is intentionally not registered
// in ProviderSet: only the admin gateway is allowed to opt into this data.
func NewAdminImageRepository(data *Data) adminimage.Repository {
	return &mongoAdminImageRepository{data: data}
}

func (r *mongoAdminImageRepository) ready() error {
	if r == nil || r.data == nil || r.data.database == nil {
		return adminimage.ErrUnavailable
	}
	return nil
}

func (r *mongoAdminImageRepository) List(ctx context.Context, query adminimage.ListQuery) (adminimage.ListResult, error) {
	if err := r.ready(); err != nil {
		return adminimage.ListResult{}, err
	}
	generated, faceSwap, tryOn := imageFilters(query)
	counts := [3]int64{}
	var docs [3][]bson.M
	filters := []bson.M{generated, faceSwap, tryOn}
	collections := []string{legacyGeneratedImages, legacyFaceSwapTasks, legacyTryOnTasks}
	for index, filter := range filters {
		if filter == nil {
			continue
		}
		count, err := r.data.database.Collection(collections[index]).CountDocuments(ctx, filter)
		if err != nil {
			return adminimage.ListResult{}, fmt.Errorf("count %s: %w", collections[index], err)
		}
		counts[index] = count
		cursor, err := r.data.database.Collection(collections[index]).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(query.FetchLimit)))
		if err != nil {
			return adminimage.ListResult{}, fmt.Errorf("find %s: %w", collections[index], err)
		}
		docs[index], err = decodeDocuments(ctx, cursor)
		if err != nil {
			return adminimage.ListResult{}, fmt.Errorf("decode %s: %w", collections[index], err)
		}
	}
	images := make([]adminimage.Image, 0, len(docs[0])+len(docs[1])+len(docs[2]))
	for _, document := range docs[0] {
		images = append(images, generatedImage(document))
	}
	for _, document := range docs[1] {
		images = append(images, faceSwapImage(document))
	}
	for _, document := range docs[2] {
		images = append(images, tryOnImage(document))
	}
	sort.SliceStable(images, func(i, j int) bool { return images[i].CreatedAt.After(images[j].CreatedAt) })
	if query.Skip >= len(images) {
		images = images[:0]
	} else {
		end := query.Skip + query.Limit
		if end < query.Skip || end > len(images) {
			end = len(images)
		}
		images = images[query.Skip:end]
	}
	if err := r.attachCreators(ctx, images); err != nil {
		return adminimage.ListResult{}, err
	}
	return adminimage.ListResult{Images: images, Total: int(counts[0] + counts[1] + counts[2])}, nil
}

func (r *mongoAdminImageRepository) Stats(ctx context.Context, todayStart time.Time) (adminimage.Stats, error) {
	if err := r.ready(); err != nil {
		return adminimage.Stats{}, err
	}
	type rules struct{ base, active bson.M }
	sets := []rules{
		{bson.M{"generationStatus": "completed", "tool": bson.M{"$ne": "seo-agent"}}, bson.M{"status": bson.M{"$in": bson.A{"pending", "approved", "active"}}}},
		{bson.M{"type": "image", "generationStatus": "completed"}, activeOrMissing()},
		{bson.M{"generationStatus": "completed"}, activeOrMissing()},
	}
	collections := []string{legacyGeneratedImages, legacyFaceSwapTasks, legacyTryOnTasks}
	var total adminimage.Stats
	for index, set := range sets {
		active, err := r.count(ctx, collections[index], andFilter(set.base, set.active))
		if err != nil {
			return adminimage.Stats{}, err
		}
		hidden, err := r.count(ctx, collections[index], andFilter(set.base, bson.M{"status": bson.M{"$in": bson.A{"hidden", "rejected"}}}))
		if err != nil {
			return adminimage.Stats{}, err
		}
		deleted, err := r.count(ctx, collections[index], andFilter(set.base, bson.M{"status": "deleted"}))
		if err != nil {
			return adminimage.Stats{}, err
		}
		today, err := r.count(ctx, collections[index], andFilter(set.base, bson.M{"createdAt": bson.M{"$gte": todayStart}}))
		if err != nil {
			return adminimage.Stats{}, err
		}
		total.Active += active
		total.Hidden += hidden
		total.Deleted += deleted
		total.Today += today
	}
	total.Total = total.Active + total.Hidden + total.Deleted
	return total, nil
}

func (r *mongoAdminImageRepository) TemplateOptions(ctx context.Context) (adminimage.TemplateOptions, error) {
	if err := r.ready(); err != nil {
		return adminimage.TemplateOptions{}, err
	}
	type row struct {
		Value any   `bson:"_id"`
		Count int64 `bson:"count"`
	}
	rows := make(map[string]int64)
	result := adminimage.TemplateOptions{}
	queries := []struct {
		collection string
		filter     bson.M
		field      string
		blank      bool
	}{
		{legacyGeneratedImages, bson.M{"generationStatus": "completed", "tool": bson.M{"$ne": "seo-agent"}}, "$templateTitle", true},
		{legacyFaceSwapTasks, bson.M{"type": "image", "generationStatus": "completed"}, "$name", false},
		{legacyTryOnTasks, bson.M{"generationStatus": "completed"}, "$name", false},
	}
	for _, query := range queries {
		cursor, err := r.data.database.Collection(query.collection).Aggregate(ctx, mongo.Pipeline{
			{{Key: "$match", Value: query.filter}},
			{{Key: "$group", Value: bson.M{"_id": query.field, "count": bson.M{"$sum": 1}}}},
		})
		if err != nil {
			return adminimage.TemplateOptions{}, fmt.Errorf("aggregate templates %s: %w", query.collection, err)
		}
		for cursor.Next(ctx) {
			var item row
			if err := cursor.Decode(&item); err != nil {
				_ = cursor.Close(ctx)
				return adminimage.TemplateOptions{}, fmt.Errorf("decode template %s: %w", query.collection, err)
			}
			value := strings.TrimSpace(imageStringFrom(item.Value))
			if value == "" {
				if query.blank {
					result.WithoutTemplate += item.Count
				}
				continue
			}
			result.WithTemplate += item.Count
			rows[value] += item.Count
		}
		if err := cursor.Err(); err != nil {
			_ = cursor.Close(ctx)
			return adminimage.TemplateOptions{}, fmt.Errorf("iterate templates %s: %w", query.collection, err)
		}
		_ = cursor.Close(ctx)
	}
	result.Options = make([]adminimage.TemplateOption, 0, len(rows))
	for value, count := range rows {
		result.Options = append(result.Options, adminimage.TemplateOption{Value: value, Count: count})
	}
	sort.Slice(result.Options, func(i, j int) bool {
		if result.Options[i].Count == result.Options[j].Count {
			return result.Options[i].Value < result.Options[j].Value
		}
		return result.Options[i].Count > result.Options[j].Count
	})
	return result, nil
}

func (r *mongoAdminImageRepository) ProviderRollup(ctx context.Context, since time.Time) (map[string]adminimage.ProviderRollup, int64, error) {
	if err := r.ready(); err != nil {
		return nil, 0, err
	}
	type row struct {
		Provider  string   `bson:"_id"`
		Total     int64    `bson:"total"`
		Completed int64    `bson:"completed"`
		Failed    int64    `bson:"failed"`
		AvgMS     *float64 `bson:"avgMs"`
	}
	cursor, err := r.data.database.Collection(legacyGeneratedImages).Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"createdAt": bson.M{"$gte": since}}}},
		{{Key: "$group", Value: bson.M{
			"_id": "$provider", "total": bson.M{"$sum": 1},
			"completed": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$generationStatus", "completed"}}, 1, 0}}},
			"failed":    bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$generationStatus", "failed"}}, 1, 0}}},
			"avgMs":     bson.M{"$avg": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$generationStatus", "completed"}}, bson.M{"$subtract": bson.A{bson.M{"$ifNull": bson.A{"$completedAt", "$updatedAt"}}, "$createdAt"}}, nil}}},
		}}},
	})
	if err != nil {
		return nil, 0, fmt.Errorf("aggregate image provider rollup: %w", err)
	}
	defer cursor.Close(ctx)
	result := make(map[string]adminimage.ProviderRollup)
	for cursor.Next(ctx) {
		var item row
		if err := cursor.Decode(&item); err != nil {
			return nil, 0, fmt.Errorf("decode image provider rollup: %w", err)
		}
		key := strings.TrimSpace(item.Provider)
		if key == "" {
			key = "unknown"
		}
		var average *int64
		if item.AvgMS != nil && *item.AvgMS > 0 {
			value := int64(math.Round(*item.AvgMS / 1000))
			average = &value
		}
		result[key] = adminimage.ProviderRollup{Total1H: item.Total, Completed1H: item.Completed, Failed1H: item.Failed, AvgDurationSec: average}
	}
	if err := cursor.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate image provider rollup: %w", err)
	}
	generating, err := r.count(ctx, legacyGeneratedImages, bson.M{"generationStatus": "generating", "createdAt": bson.M{"$gte": since.Add(-time.Hour)}})
	if err != nil {
		return nil, 0, err
	}
	return result, generating, nil
}

func (r *mongoAdminImageRepository) ExternalToday(ctx context.Context, todayStart time.Time) (int64, error) {
	if err := r.ready(); err != nil {
		return 0, err
	}
	return r.count(ctx, legacyFaceSwapTasks, bson.M{"type": "image", "generationStatus": "completed", "createdAt": bson.M{"$gte": todayStart}})
}

func (r *mongoAdminImageRepository) Mutate(ctx context.Context, id string, mutation adminimage.Mutation) error {
	if err := r.ready(); err != nil {
		return err
	}
	objectID, err := bson.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("%w: invalid image id", adminimage.ErrInvalidQuery)
	}
	collections := []string{legacyGeneratedImages, legacyFaceSwapTasks, legacyTryOnTasks}
	for _, collection := range collections {
		update := bson.M{"$set": bson.M{"status": string(mutation)}}
		if collection == legacyGeneratedImages && (mutation == adminimage.MutationHide || mutation == adminimage.MutationDelete) {
			update = bson.M{"$set": bson.M{"status": string(mutation), "isPublic": false}}
		}
		result, err := r.data.database.Collection(collection).UpdateOne(ctx, bson.M{"_id": objectID}, update)
		if err != nil {
			return fmt.Errorf("update %s: %w", collection, err)
		}
		if result.MatchedCount > 0 {
			return nil
		}
	}
	return adminimage.ErrNotFound
}

func (r *mongoAdminImageRepository) BatchHide(ctx context.Context, ids []string) error {
	if err := r.ready(); err != nil {
		return err
	}
	objectIDs := make([]bson.ObjectID, 0, len(ids))
	for _, id := range ids {
		objectID, err := bson.ObjectIDFromHex(strings.TrimSpace(id))
		if err != nil {
			return fmt.Errorf("%w: invalid image id", adminimage.ErrInvalidQuery)
		}
		objectIDs = append(objectIDs, objectID)
	}
	updates := []struct {
		collection string
		update     bson.M
	}{
		{legacyGeneratedImages, bson.M{"$set": bson.M{"status": "hidden", "isPublic": false}}},
		{legacyFaceSwapTasks, bson.M{"$set": bson.M{"status": "hidden"}}},
		{legacyTryOnTasks, bson.M{"$set": bson.M{"status": "hidden"}}},
	}
	for _, item := range updates {
		if _, err := r.data.database.Collection(item.collection).UpdateMany(ctx, bson.M{"_id": bson.M{"$in": objectIDs}}, item.update); err != nil {
			return fmt.Errorf("batch hide %s: %w", item.collection, err)
		}
	}
	return nil
}

func (r *mongoAdminImageRepository) count(ctx context.Context, collection string, filter bson.M) (int64, error) {
	result, err := r.data.database.Collection(collection).CountDocuments(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", collection, err)
	}
	return result, nil
}

func (r *mongoAdminImageRepository) attachCreators(ctx context.Context, images []adminimage.Image) error {
	ids := make([]bson.ObjectID, 0, len(images))
	positions := make(map[string][]int)
	for index := range images {
		id, err := bson.ObjectIDFromHex(images[index].UserID)
		if err == nil {
			key := id.Hex()
			if _, exists := positions[key]; !exists {
				ids = append(ids, id)
			}
			positions[key] = append(positions[key], index)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	cursor, err := r.data.database.Collection(legacyUsers).Find(ctx, bson.M{"_id": bson.M{"$in": ids}}, options.Find().SetProjection(bson.M{"email": 1, "displayName": 1, "avatarUrl": 1}))
	if err != nil {
		return fmt.Errorf("find image creators: %w", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var user struct {
			ID          bson.ObjectID `bson:"_id"`
			Email       string        `bson:"email"`
			DisplayName string        `bson:"displayName"`
			AvatarURL   string        `bson:"avatarUrl"`
		}
		if err := cursor.Decode(&user); err != nil {
			return fmt.Errorf("decode image creator: %w", err)
		}
		creator := adminimage.Creator{ID: user.ID.Hex(), Email: user.Email, DisplayName: user.DisplayName, AvatarURL: user.AvatarURL}
		for _, index := range positions[user.ID.Hex()] {
			copy := creator
			images[index].Creator = &copy
		}
	}
	if err := cursor.Err(); err != nil {
		return fmt.Errorf("iterate image creators: %w", err)
	}
	return nil
}

func decodeDocuments(ctx context.Context, cursor *mongo.Cursor) ([]bson.M, error) {
	defer cursor.Close(ctx)
	result := make([]bson.M, 0)
	for cursor.Next(ctx) {
		var document bson.M
		if err := cursor.Decode(&document); err != nil {
			return nil, err
		}
		result = append(result, document)
	}
	return result, cursor.Err()
}

func imageFilters(query adminimage.ListQuery) (bson.M, bson.M, bson.M) {
	generatedTools := map[string]bool{"t2i": true, "i2i": true, "undress": true, "doubleAction": true, "sexPose": true}
	isSubTool := generatedTools[query.Source]
	includeGenerated := query.Source == "all" || query.Source == "generated" || isSubTool
	includeFaceSwap := query.MultiImage != "true" && (query.ImagePool == "all" || query.ImagePool == "external") && (query.Source == "all" || query.Source == "faceswap")
	includeTryOn := query.MultiImage != "true" && (query.ImagePool == "all" || query.ImagePool == "qwen-edit-default") && (query.Source == "all" || query.Source == "tryon")
	var generated, faceSwap, tryOn bson.M
	if includeGenerated {
		generated = statusFilter(query.Status, "generated")
		if isSubTool {
			if query.Source == "t2i" {
				generated = andFilter(generated, bson.M{"$or": bson.A{bson.M{"tool": "t2i"}, bson.M{"tool": bson.M{"$exists": false}}, bson.M{"tool": nil}}})
			} else {
				generated = andFilter(generated, bson.M{"tool": query.Source})
			}
		}
		if query.MultiImage == "true" {
			generated = andFilter(generated, bson.M{"$expr": bson.M{"$gte": bson.A{bson.M{"$size": bson.M{"$ifNull": bson.A{"$inputImages", bson.A{}}}}, 2}}})
		}
		generated = templateFilter(generated, query.Template, "templateTitle", true)
		generated = andFilter(generated, clientSourceFilter(query.ClientSource))
		generated = andFilter(generated, imagePoolFilter(query.ImagePool))
	}
	if includeFaceSwap {
		faceSwap = statusFilter(query.Status, "faceswap")
		faceSwap = templateFilter(faceSwap, query.Template, "name", false)
		faceSwap = andFilter(faceSwap, clientSourceFilter(query.ClientSource))
	}
	if includeTryOn {
		tryOn = statusFilter(query.Status, "tryon")
		tryOn = templateFilter(tryOn, query.Template, "name", false)
		tryOn = andFilter(tryOn, clientSourceFilter(query.ClientSource))
	}
	for _, filter := range []*bson.M{&generated, &faceSwap, &tryOn} {
		if *filter != nil && !query.TodayStart.IsZero() {
			*filter = andFilter(*filter, bson.M{"createdAt": bson.M{"$gte": query.TodayStart}})
		}
	}
	return generated, faceSwap, tryOn
}

func statusFilter(status, collection string) bson.M {
	var base bson.M
	switch collection {
	case "generated":
		base = bson.M{"generationStatus": "completed", "tool": bson.M{"$ne": "seo-agent"}}
	case "faceswap":
		base = bson.M{"type": "image", "generationStatus": "completed"}
	default:
		base = bson.M{"generationStatus": "completed"}
	}
	if status == "" || status == "all" {
		return base
	}
	if collection == "generated" {
		switch status {
		case "active":
			return andFilter(base, bson.M{"status": bson.M{"$in": bson.A{"pending", "approved", "active"}}})
		case "hidden":
			return andFilter(base, bson.M{"status": bson.M{"$in": bson.A{"hidden", "rejected"}}})
		default:
			return andFilter(base, bson.M{"status": status})
		}
	}
	if status == "active" {
		return andFilter(base, activeOrMissing())
	}
	return andFilter(base, bson.M{"status": status})
}

func activeOrMissing() bson.M {
	return bson.M{"$or": bson.A{bson.M{"status": "active"}, bson.M{"status": bson.M{"$exists": false}}}}
}

func templateFilter(filter bson.M, template, field string, supportsNone bool) bson.M {
	if filter == nil || template == "" || template == "all" {
		return filter
	}
	if template == "__none__" {
		if !supportsNone {
			return nil
		}
		return andFilter(filter, bson.M{field: bson.M{"$in": bson.A{nil, ""}}})
	}
	if template == "__any__" {
		return andFilter(filter, bson.M{field: bson.M{"$nin": bson.A{nil, ""}}})
	}
	return andFilter(filter, bson.M{field: template})
}

func imagePoolFilter(pool string) bson.M {
	if pool == "" || pool == "all" {
		return nil
	}
	fallbacks := map[string][]string{
		"qwen-edit-default":     {"comfyui-qwen-edit", "runpod-qwen", "qwen-edit"},
		"image-flex-default":    {"comfyui-anime", "comfyui-anime-image", "image-router", "comfyui-sd", "comfyui", "runpod", "fal", "novita"},
		"sensenova-u15-default": {"sensenova-u15"},
		"external":              {"a2e", "external"},
	}
	if pool == "unknown" {
		known := make([]string, 0)
		for _, providers := range fallbacks {
			known = append(known, providers...)
		}
		return bson.M{"imagePool": bson.M{"$in": bson.A{nil, "unknown"}}, "provider": bson.M{"$nin": known}}
	}
	providers, ok := fallbacks[pool]
	if !ok {
		return nil
	}
	provider := any(providers[0])
	if len(providers) > 1 {
		provider = bson.M{"$in": providers}
	}
	return bson.M{"$or": bson.A{bson.M{"imagePool": pool}, bson.M{"provider": provider, "imagePool": bson.M{"$in": bson.A{nil, "unknown"}}}}}
}

func clientSourceFilter(value string) bson.M {
	key := normalizeClientSource(value)
	if key == "" || key == "all" {
		return nil
	}
	if strings.HasPrefix(key, "surface:") {
		switch strings.TrimPrefix(key, "surface:") {
		case "web", "ios":
			return bson.M{"clientSource.platform": strings.TrimPrefix(key, "surface:")}
		case "apk":
			return andFilter(bson.M{"clientSource.platform": "android"}, bson.M{"clientSource.appId": bson.M{"$in": bson.A{"com.clingai.offstore"}}})
		case "android":
			return andFilter(bson.M{"clientSource.platform": "android"}, bson.M{"clientSource.appId": bson.M{"$nin": bson.A{"com.clingai.offstore"}}})
		default:
			return bson.M{"_id": nil}
		}
	}
	if key == "clingai" {
		return bson.M{"$or": bson.A{bson.M{"clientSource.key": bson.M{"$in": bson.A{"clingai", "web:cling-ai.com", "web:www.cling-ai.com"}}}, bson.M{"clientSource.host": bson.M{"$in": bson.A{"cling-ai.com", "www.cling-ai.com"}}}}}
	}
	if key == "clingai-apk" {
		key = "app:com.clingai.offstore"
	}
	if strings.HasPrefix(key, "partner:") {
		domain := strings.TrimPrefix(key, "partner:")
		if domain == "" {
			return bson.M{"_id": nil}
		}
		return bson.M{"$or": bson.A{bson.M{"clientSource.key": bson.M{"$in": bson.A{key, "web:" + domain, "web:www." + domain}}}, bson.M{"clientSource.host": bson.M{"$in": bson.A{domain, "www." + domain}}}}}
	}
	if strings.HasPrefix(key, "app:") {
		identifier := strings.TrimPrefix(key, "app:")
		if identifier == "" {
			return bson.M{"_id": nil}
		}
		regex := bson.Regex{Pattern: "^" + regexpQuote(identifier) + "$", Options: "i"}
		return bson.M{"$or": bson.A{bson.M{"clientSource.key": bson.M{"$in": bson.A{key, "android:" + identifier, "ios:" + identifier}}}, bson.M{"clientSource.appId": regex}, bson.M{"clientSource.clientId": regex}, bson.M{"clientSource.identifier": regex}}}
	}
	return bson.M{"clientSource.key": key}
}

func normalizeClientSource(value string) string {
	raw := strings.ToLower(strings.TrimSpace(value))
	if raw == "" {
		return ""
	}
	if raw == "b2b" || strings.HasPrefix(raw, "b2b:") || strings.HasPrefix(raw, "app:") || strings.HasPrefix(raw, "surface:") {
		return raw
	}
	if raw == "clingai-apk" || raw == "clingai apk" || raw == "com.clingai.offstore" {
		return "clingai-apk"
	}
	withoutPartner := strings.TrimPrefix(raw, "partner:")
	withoutProtocol := strings.TrimPrefix(strings.TrimPrefix(withoutPartner, "https://"), "http://")
	host := strings.TrimPrefix(strings.Split(withoutProtocol, "/")[0], "www.")
	if host == "web" || host == "clingai" || host == "cling-ai.com" {
		return "clingai"
	}
	if strings.HasPrefix(raw, "partner:") || strings.Contains(host, ".") {
		return "partner:" + host
	}
	return raw
}

func andFilter(left, right bson.M) bson.M {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	return bson.M{"$and": bson.A{left, right}}
}

func generatedImage(document bson.M) adminimage.Image {
	result := baseImage(document)
	result.Prompt = imageStringValue(document["prompt"])
	result.ImageURL = imageStringValue(document["imageUrl"])
	result.Style = imageStringPointer(document["style"])
	result.Status = defaultValue(imageStringValue(document["status"]), "active")
	result.IsPublic = boolDefault(document["isPublic"], true)
	result.Likes = int64Value(document["likes"])
	result.Views = int64Value(document["views"])
	result.Source = defaultValue(imageStringValue(document["tool"]), "t2i")
	result.APIProvider = defaultValue(imageStringValue(document["provider"]), "unknown")
	result.ComfyNode = imageStringPointer(document["comfyNode"])
	result.ImagePool = resolveImagePool(imageStringValue(document["imagePool"]), "", nil, imageStringValue(document["imageProfile"]), result.APIProvider, "")
	result.ImageProfile = imageStringPointer(document["imageProfile"])
	result.TemplateTitle = imageStringPointer(document["templateTitle"])
	result.AdditionalImageCount = int64(len(nonEmptyValues(document["inputImages"])))
	if completed, ok := timeValue(document["completedAt"]); ok {
		result.GenerationMS = durationMS(result.CreatedAt, completed)
	} else if updated, ok := timeValue(document["updatedAt"]); ok {
		result.GenerationMS = durationMS(result.CreatedAt, updated)
	}
	return result
}

func faceSwapImage(document bson.M) adminimage.Image {
	result := baseImage(document)
	result.Prompt = defaultValue(imageStringValue(document["name"]), "Face Swap")
	result.ImageURL = imageStringValue(document["resultImageUrl"])
	result.Status = defaultValue(imageStringValue(document["status"]), "active")
	result.IsPublic, result.Source, result.APIProvider, result.ImagePool = true, "faceswap", "a2e", "external"
	result.TemplateTitle = imageStringPointer(document["name"])
	if updated, ok := timeValue(document["updatedAt"]); ok {
		result.GenerationMS = durationMS(result.CreatedAt, updated)
	}
	return result
}

func tryOnImage(document bson.M) adminimage.Image {
	result := baseImage(document)
	result.Prompt = defaultValue(imageStringValue(document["name"]), "Dress Up")
	result.ImageURL = imageStringValue(document["resultImageUrl"])
	result.Status = defaultValue(imageStringValue(document["status"]), "active")
	result.IsPublic, result.Source, result.APIProvider, result.ImagePool = true, "tryon", "comfyui", "qwen-edit-default"
	profile := "qwen-image-edit-2511"
	result.ImageProfile, result.TemplateTitle = &profile, imageStringPointer(document["name"])
	if updated, ok := timeValue(document["updatedAt"]); ok {
		result.GenerationMS = durationMS(result.CreatedAt, updated)
	}
	return result
}

func baseImage(document bson.M) adminimage.Image {
	id, _ := objectIDFrom(document["_id"])
	created, _ := timeValue(document["createdAt"])
	return adminimage.Image{ID: id.Hex(), CreatedAt: created, ClientSource: document["clientSource"], UserID: imageUserID(document["userId"])}
}

func resolveImagePool(declared, nodePool string, nodePools []string, profile, provider, role string) string {
	known := map[string]bool{"qwen-edit-default": true, "image-flex-default": true, "sensenova-u15-default": true, "external": true, "unknown": true}
	if known[declared] {
		return declared
	}
	if known[nodePool] && nodePool != "external" && nodePool != "unknown" {
		return nodePool
	}
	for _, pool := range nodePools {
		if known[pool] && pool != "external" && pool != "unknown" {
			return pool
		}
	}
	profile = strings.ToLower(strings.TrimSpace(profile))
	if profile == "sensenova-u15-8b-mot-8step" || strings.Contains(profile, "sensenova-u15") {
		return "sensenova-u15-default"
	}
	if profile == "qwen-image-edit-2511" || strings.Contains(profile, "qwen-image-edit") {
		return "qwen-edit-default"
	}
	if map[string]bool{"qwen-image-t2i": true, "hidream-o1-dev-2604": true, "krea2-turbo-oss-2026": true, "krea2-moody-v5": true}[profile] {
		return "image-flex-default"
	}
	for pool, providers := range map[string][]string{"qwen-edit-default": {"comfyui-qwen-edit", "runpod-qwen", "qwen-edit"}, "image-flex-default": {"comfyui-anime", "comfyui-anime-image", "image-router", "comfyui-sd", "comfyui", "runpod", "fal", "novita"}, "sensenova-u15-default": {"sensenova-u15"}, "external": {"a2e", "external"}} {
		for _, candidate := range providers {
			if strings.EqualFold(candidate, provider) {
				return pool
			}
		}
	}
	if role == "qwen-edit" {
		return "qwen-edit-default"
	}
	if role == "anime" || role == "sd" {
		return "image-flex-default"
	}
	return "unknown"
}

func regexpQuote(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", ".", "\\.", "*", "\\*", "+", "\\+", "?", "\\?", "^", "\\^", "$", "\\$", "(", "\\(", ")", "\\)", "[", "\\[", "]", "\\]", "{", "\\{", "}", "\\}", "|", "\\|")
	return replacer.Replace(value)
}

func imageStringFrom(value any) string { return imageStringValue(value) }
func imageStringValue(value any) string {
	if stringValue, ok := value.(string); ok {
		return stringValue
	}
	return ""
}
func imageStringPointer(value any) *string {
	valueString := strings.TrimSpace(imageStringValue(value))
	if valueString == "" {
		return nil
	}
	return &valueString
}
func boolDefault(value any, fallback bool) bool {
	valueBool, ok := value.(bool)
	if ok {
		return valueBool
	}
	return fallback
}
func int64Value(value any) int64 {
	switch value := value.(type) {
	case int:
		return int64(value)
	case int32:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case float32:
		return int64(value)
	default:
		return 0
	}
}
func timeValue(value any) (time.Time, bool) { valueTime, ok := value.(time.Time); return valueTime, ok }
func objectIDFrom(value any) (bson.ObjectID, bool) {
	objectID, ok := value.(bson.ObjectID)
	return objectID, ok
}
func defaultValue(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
func durationMS(start, end time.Time) *int64 {
	if start.IsZero() || end.Before(start) {
		return nil
	}
	value := end.Sub(start).Milliseconds()
	return &value
}
func nonEmptyValues(value any) []any {
	values, ok := value.(bson.A)
	if !ok {
		if valuesSlice, sliceOK := value.([]any); sliceOK {
			values = bson.A(valuesSlice)
		}
	}
	result := make([]any, 0, len(values))
	for _, item := range values {
		if item == nil {
			continue
		}
		if text, ok := item.(string); ok && text == "" {
			continue
		}
		result = append(result, item)
	}
	return result
}

func imageUserID(value any) string {
	if id, ok := objectIDFrom(value); ok {
		return id.Hex()
	}
	return imageStringValue(value)
}

var _ adminimage.Repository = (*mongoAdminImageRepository)(nil)
