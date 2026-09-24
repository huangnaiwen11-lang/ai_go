package data

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"ai-business-service/internal/biz/adminreview"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const adminReviewProjectionStateID = "global"

type mongoAdminReviewRepository struct{ data *Data }

// NewAdminReviewRepository returns a read-only repository over the explicit Go
// moderation projection.  It never queries legacy GeneratedImage/GeneratedVideo
// collections, because those collections have no Go-owned contract.
func NewAdminReviewRepository(data *Data) adminreview.Repository {
	return &mongoAdminReviewRepository{data: data}
}

func (r *mongoAdminReviewRepository) ready(ctx context.Context) error {
	if r == nil || r.data == nil || r.data.database == nil {
		return adminreview.ErrProjectionUnavailable
	}
	var state model.AdminReviewProjectionStateDocument
	err := r.data.database.Collection(schema.CollectionAdminReviewProjectionState).FindOne(ctx, bson.M{"_id": adminReviewProjectionStateID}).Decode(&state)
	if errors.Is(err, mongo.ErrNoDocuments) || err != nil || !state.Ready {
		return adminreview.ErrProjectionUnavailable
	}
	return nil
}

func (r *mongoAdminReviewRepository) ListReviewItems(ctx context.Context, query adminreview.ListQuery) (adminreview.ListResult, error) {
	if err := r.ready(ctx); err != nil {
		return adminreview.ListResult{}, err
	}
	filter := bson.M{"media_type": query.MediaType}
	if query.MediaType == adminreview.MediaTypeVideo {
		filter["generation_status"] = adminreview.GenerationStatusSucceeded
		applyVideoListFilter(filter, query)
	}
	if query.Status != "all" {
		filter["review_status"] = query.Status
	}
	collection := r.data.database.Collection(schema.CollectionAdminReviewItems)
	total, err := collection.CountDocuments(ctx, filter)
	if err != nil {
		return adminreview.ListResult{}, fmt.Errorf("count admin review items: %w", err)
	}
	sortOrder := bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}
	if query.MediaType == adminreview.MediaTypeVideo && query.SortBy == "outputAt" {
		sortOrder = bson.D{{Key: "output_at", Value: -1}, {Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}
	}
	cursor, err := collection.Find(ctx, filter, options.Find().SetSort(sortOrder).SetSkip(int64((query.Page-1)*query.Limit)).SetLimit(int64(query.Limit)))
	if err != nil {
		return adminreview.ListResult{}, fmt.Errorf("list admin review items: %w", err)
	}
	defer cursor.Close(ctx)
	items := make([]adminreview.ReviewItem, 0, query.Limit)
	for cursor.Next(ctx) {
		var document model.AdminReviewItemDocument
		if err := cursor.Decode(&document); err != nil {
			return adminreview.ListResult{}, fmt.Errorf("decode admin review item: %w", err)
		}
		items = append(items, reviewItemFromDocument(document))
	}
	if err := cursor.Err(); err != nil {
		return adminreview.ListResult{}, fmt.Errorf("iterate admin review items: %w", err)
	}
	pages := int(math.Ceil(float64(total) / float64(query.Limit)))
	return adminreview.ListResult{Items: items, Total: total, Page: query.Page, Limit: query.Limit, TotalPages: pages}, nil
}

func applyVideoListFilter(filter bson.M, query adminreview.ListQuery) {
	sources := videoReviewSources(query.Source)
	switch query.VideoPool {
	case "external":
		sources = intersectReviewSources(sources, []string{"legacy.animates", "animate", "legacy.faceswaptasks", "faceswap"})
	case "unknown":
		sources = intersectReviewSources(sources, []string{"legacy.generatedvideos", "generated"})
	}
	if sources != nil {
		filter["source"] = bson.M{"$in": sources}
	}
	switch query.Template {
	case "all":
	case "__none__":
		filter["template_title"] = bson.M{"$in": bson.A{nil, ""}}
	case "__any__":
		filter["template_title"] = bson.M{"$nin": bson.A{nil, ""}}
	default:
		filter["template_title"] = query.Template
	}
	if query.DateRange == "today" {
		filter["created_at"] = bson.M{"$gte": shanghaiStartOfDayUTC(time.Now().UTC())}
	}
}

func videoReviewSources(source string) []string {
	switch source {
	case "generated":
		return []string{"legacy.generatedvideos", "generated"}
	case "animate":
		return []string{"legacy.animates", "animate"}
	case "faceswap":
		return []string{"legacy.faceswaptasks", "faceswap"}
	default:
		return nil
	}
}

func intersectReviewSources(left, right []string) []string {
	if left == nil {
		return right
	}
	accepted := make(map[string]struct{}, len(right))
	for _, value := range right {
		accepted[value] = struct{}{}
	}
	result := make([]string, 0, len(left))
	for _, value := range left {
		if _, ok := accepted[value]; ok {
			result = append(result, value)
		}
	}
	return result
}

func shanghaiStartOfDayUTC(now time.Time) time.Time {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		location = time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).UTC()
}

func (r *mongoAdminReviewRepository) CountReviewStatuses(ctx context.Context, query adminreview.StatsQuery) (adminreview.StatusCounts, error) {
	if err := r.ready(ctx); err != nil {
		return adminreview.StatusCounts{}, err
	}
	filter := bson.M{}
	if query.MediaType != "" {
		filter["media_type"] = query.MediaType
	}
	if query.MediaType == adminreview.MediaTypeVideo {
		filter["generation_status"] = adminreview.GenerationStatusSucceeded
	}
	collection := r.data.database.Collection(schema.CollectionAdminReviewItems)
	count := func(status string) (int64, error) {
		statusFilter := bson.M{}
		for key, value := range filter {
			statusFilter[key] = value
		}
		statusFilter["review_status"] = status
		return collection.CountDocuments(ctx, statusFilter)
	}
	var result adminreview.StatusCounts
	var err error
	if result.Pending, err = count(adminreview.ReviewStatusPending); err != nil {
		return result, fmt.Errorf("count pending review items: %w", err)
	}
	if result.Approved, err = count(adminreview.ReviewStatusApproved); err != nil {
		return result, fmt.Errorf("count approved review items: %w", err)
	}
	if result.Rejected, err = count(adminreview.ReviewStatusRejected); err != nil {
		return result, fmt.Errorf("count rejected review items: %w", err)
	}
	if result.Deleted, err = count(adminreview.ReviewStatusDeleted); err != nil {
		return result, fmt.Errorf("count deleted review items: %w", err)
	}
	result.Total = result.Pending + result.Approved + result.Rejected
	location, locationErr := time.LoadLocation("Asia/Shanghai")
	if locationErr != nil {
		location = time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	now := time.Now().In(location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location).UTC()
	todayFilter := bson.M{"created_at": bson.M{"$gte": today}}
	for key, value := range filter {
		todayFilter[key] = value
	}
	result.Today, err = collection.CountDocuments(ctx, todayFilter)
	if err != nil {
		return result, fmt.Errorf("count today review items: %w", err)
	}
	return result, nil
}

func (r *mongoAdminReviewRepository) ImageTemplateOptions(ctx context.Context) (adminreview.ImageTemplateOptions, error) {
	if err := r.ready(ctx); err != nil {
		return adminreview.ImageTemplateOptions{}, err
	}
	type row struct {
		Title *string `bson:"_id"`
		Count int64   `bson:"count"`
	}
	cursor, err := r.data.database.Collection(schema.CollectionAdminReviewItems).Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"media_type": adminreview.MediaTypeImage, "generation_status": adminreview.GenerationStatusSucceeded}}},
		{{Key: "$group", Value: bson.M{"_id": "$template_title", "count": bson.M{"$sum": 1}}}},
	})
	if err != nil {
		return adminreview.ImageTemplateOptions{}, fmt.Errorf("aggregate admin image template options: %w", err)
	}
	defer cursor.Close(ctx)
	result := adminreview.ImageTemplateOptions{Options: make([]adminreview.ImageTemplateOption, 0)}
	for cursor.Next(ctx) {
		var value row
		if err := cursor.Decode(&value); err != nil {
			return adminreview.ImageTemplateOptions{}, fmt.Errorf("decode admin image template option: %w", err)
		}
		title := ""
		if value.Title != nil {
			title = strings.TrimSpace(*value.Title)
		}
		if title == "" {
			result.WithoutTemplate += value.Count
			continue
		}
		result.WithTemplate += value.Count
		result.Options = append(result.Options, adminreview.ImageTemplateOption{Value: title, Count: value.Count})
	}
	if err := cursor.Err(); err != nil {
		return adminreview.ImageTemplateOptions{}, fmt.Errorf("iterate admin image template options: %w", err)
	}
	sort.Slice(result.Options, func(i, j int) bool {
		if result.Options[i].Count == result.Options[j].Count {
			return result.Options[i].Value < result.Options[j].Value
		}
		return result.Options[i].Count > result.Options[j].Count
	})
	return result, nil
}

func (r *mongoAdminReviewRepository) ImageRuntime(ctx context.Context, query adminreview.ImageRuntimeQuery) (adminreview.ImageRuntime, error) {
	if err := r.ready(ctx); err != nil {
		return adminreview.ImageRuntime{}, err
	}
	collection := r.data.database.Collection(schema.CollectionAdminReviewItems)
	generating, err := collection.CountDocuments(ctx, bson.M{
		"media_type":        adminreview.MediaTypeImage,
		"generation_status": adminreview.GenerationStatusGenerating,
		"created_at":        bson.M{"$gte": query.GeneratingSince},
	})
	if err != nil {
		return adminreview.ImageRuntime{}, fmt.Errorf("count generating admin images: %w", err)
	}
	externalToday, err := collection.CountDocuments(ctx, bson.M{
		"media_type":        adminreview.MediaTypeImage,
		"source":            bson.M{"$in": []string{"legacy.faceswaptasks", "faceswap"}},
		"generation_status": adminreview.GenerationStatusSucceeded,
		"created_at":        bson.M{"$gte": query.TodayStart},
	})
	if err != nil {
		return adminreview.ImageRuntime{}, fmt.Errorf("count external admin images today: %w", err)
	}
	return adminreview.ImageRuntime{Generating: generating, ExternalToday: externalToday}, nil
}

func (r *mongoAdminReviewRepository) VideoTemplateOptions(ctx context.Context) (adminreview.VideoTemplateOptions, error) {
	if err := r.ready(ctx); err != nil {
		return adminreview.VideoTemplateOptions{}, err
	}
	type row struct {
		Title string `bson:"_id"`
		Count int64  `bson:"count"`
	}
	cursor, err := r.data.database.Collection(schema.CollectionAdminReviewItems).Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"media_type": adminreview.MediaTypeVideo, "generation_status": adminreview.GenerationStatusSucceeded,
			"source": bson.M{"$nin": []string{"legacy.faceswaptasks", "faceswap"}},
		}}},
		{{Key: "$group", Value: bson.M{"_id": "$template_title", "count": bson.M{"$sum": 1}}}},
	})
	if err != nil {
		return adminreview.VideoTemplateOptions{}, fmt.Errorf("aggregate admin video template options: %w", err)
	}
	defer cursor.Close(ctx)
	result := adminreview.VideoTemplateOptions{Options: make([]adminreview.VideoTemplateOption, 0)}
	for cursor.Next(ctx) {
		var value row
		if err := cursor.Decode(&value); err != nil {
			return adminreview.VideoTemplateOptions{}, fmt.Errorf("decode admin video template option: %w", err)
		}
		value.Title = strings.TrimSpace(value.Title)
		if value.Title == "" {
			result.WithoutTemplate += value.Count
			continue
		}
		result.WithTemplate += value.Count
		result.Options = append(result.Options, adminreview.VideoTemplateOption{Value: value.Title, Count: value.Count})
	}
	if err := cursor.Err(); err != nil {
		return adminreview.VideoTemplateOptions{}, fmt.Errorf("iterate admin video template options: %w", err)
	}
	sort.Slice(result.Options, func(i, j int) bool {
		if result.Options[i].Count == result.Options[j].Count {
			return result.Options[i].Value < result.Options[j].Value
		}
		return result.Options[i].Count > result.Options[j].Count
	})
	return result, nil
}

func (r *mongoAdminReviewRepository) VideoRuntime(ctx context.Context, query adminreview.VideoRuntimeQuery) (adminreview.VideoRuntime, error) {
	if err := r.ready(ctx); err != nil {
		return adminreview.VideoRuntime{}, err
	}
	collection := r.data.database.Collection(schema.CollectionAdminReviewItems)
	generating, err := collection.CountDocuments(ctx, bson.M{
		"media_type":        adminreview.MediaTypeVideo,
		"generation_status": adminreview.GenerationStatusGenerating,
		"created_at":        bson.M{"$gte": query.GeneratingSince},
	})
	if err != nil {
		return adminreview.VideoRuntime{}, fmt.Errorf("count generating admin videos: %w", err)
	}
	externalToday, err := collection.CountDocuments(ctx, bson.M{
		"media_type":        adminreview.MediaTypeVideo,
		"source":            bson.M{"$in": []string{"legacy.animates", "animate", "legacy.faceswaptasks", "faceswap"}},
		"generation_status": adminreview.GenerationStatusSucceeded,
		"created_at":        bson.M{"$gte": query.TodayStart},
	})
	if err != nil {
		return adminreview.VideoRuntime{}, fmt.Errorf("count external admin videos today: %w", err)
	}
	return adminreview.VideoRuntime{Generating: generating, ExternalToday: externalToday}, nil
}

func reviewItemFromDocument(document model.AdminReviewItemDocument) adminreview.ReviewItem {
	return adminreview.ReviewItem{
		ID: document.ID, MediaType: document.MediaType, Source: document.Source,
		LegacySourceID: document.LegacySourceID, CreationID: document.CreationID, UserID: document.UserID, AssetID: document.AssetID,
		OutputRef: document.OutputRef, ReviewStatus: document.ReviewStatus,
		VisibilityStatus: document.VisibilityStatus, GenerationStatus: document.GenerationStatus,
		Prompt: document.Prompt, NegativePrompt: document.NegativePrompt,
		TemplateID: document.TemplateID, TemplateTitle: document.TemplateTitle,
		ReviewedBy: document.ReviewedBy, ReviewedAt: document.ReviewedAt,
		RejectReason: document.RejectReason, Version: document.Version,
		CreatedAt: document.CreatedAt, OutputAt: document.OutputAt, UpdatedAt: document.UpdatedAt,
	}
}
