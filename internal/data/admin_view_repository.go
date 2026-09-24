package data

import (
	"context"
	"fmt"
	"time"

	"ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type mongoAdminViewRepository struct{ data *Data }

// NewAdminViewRepository 返回只读取 Go 自有集合的管理总览仓储。
func NewAdminViewRepository(data *Data) adminview.Repository {
	return &mongoAdminViewRepository{data: data}
}

func (repository *mongoAdminViewRepository) Overview(ctx context.Context) (adminview.Overview, error) {
	if repository == nil || repository.data == nil {
		return adminview.Overview{}, fmt.Errorf("admin view repository unavailable")
	}
	now := time.Now().UTC()
	today := adminview.DayStart(now)
	week := today.AddDate(0, 0, -6)
	month := today.AddDate(0, 0, -29)
	count := func(collection string, filter bson.D) (int64, error) {
		return repository.data.database.Collection(collection).CountDocuments(ctx, filter)
	}
	var result adminview.Overview
	var err error
	if result.TotalUsers, err = count(schema.CollectionUsers, bson.D{}); err != nil {
		return result, fmt.Errorf("count users: %w", err)
	}
	if result.TodayUsers, err = count(schema.CollectionUsers, bson.D{{Key: "created_at", Value: bson.D{{Key: "$gte", Value: today}}}}); err != nil {
		return result, fmt.Errorf("count today users: %w", err)
	}
	if result.WeeklyUsers, err = count(schema.CollectionUsers, bson.D{{Key: "created_at", Value: bson.D{{Key: "$gte", Value: week}}}}); err != nil {
		return result, fmt.Errorf("count weekly users: %w", err)
	}
	if result.MonthlyUsers, err = count(schema.CollectionUsers, bson.D{{Key: "created_at", Value: bson.D{{Key: "$gte", Value: month}}}}); err != nil {
		return result, fmt.Errorf("count monthly users: %w", err)
	}
	creationCount := func(output string, from time.Time, pending bool) (int64, error) {
		filter := bson.D{{Key: "product_output", Value: output}}
		if !from.IsZero() {
			filter = append(filter, bson.E{Key: "created_at", Value: bson.D{{Key: "$gte", Value: from}}})
		}
		if pending {
			filter = append(filter, bson.E{Key: "status", Value: "pending_submission"})
		}
		return count(schema.CollectionCreations, filter)
	}
	if result.TotalImages, err = creationCount("image", time.Time{}, false); err != nil {
		return result, fmt.Errorf("count images: %w", err)
	}
	if result.TodayImages, err = creationCount("image", today, false); err != nil {
		return result, fmt.Errorf("count today images: %w", err)
	}
	if result.PendingImages, err = creationCount("image", time.Time{}, true); err != nil {
		return result, fmt.Errorf("count pending images: %w", err)
	}
	if result.TotalVideos, err = creationCount("video", time.Time{}, false); err != nil {
		return result, fmt.Errorf("count videos: %w", err)
	}
	if result.TodayVideos, err = creationCount("video", today, false); err != nil {
		return result, fmt.Errorf("count today videos: %w", err)
	}
	if result.PendingVideos, err = creationCount("video", time.Time{}, true); err != nil {
		return result, fmt.Errorf("count pending videos: %w", err)
	}
	if result.TotalRevenueCents, err = repository.sumCompletedOrders(ctx, time.Time{}); err != nil {
		return result, err
	}
	if result.TodayRevenueCents, err = repository.sumCompletedOrders(ctx, today); err != nil {
		return result, err
	}
	return result, nil
}

func (repository *mongoAdminViewRepository) sumCompletedOrders(ctx context.Context, from time.Time) (int64, error) {
	filter := bson.D{{Key: "status", Value: "paid"}, {Key: "currency", Value: "USD"}}
	if !from.IsZero() {
		filter = append(filter, bson.E{Key: "updated_at", Value: bson.D{{Key: "$gte", Value: from}, {Key: "$lte", Value: time.Now().UTC()}}})
	}
	cursor, err := repository.data.database.Collection(schema.CollectionPaymentOrders).Aggregate(ctx, bson.A{bson.D{{Key: "$match", Value: filter}}, bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "total", Value: bson.D{{Key: "$sum", Value: "$amount_cents"}}}}}}})
	if err != nil {
		return 0, fmt.Errorf("sum completed orders: %w", err)
	}
	defer cursor.Close(ctx)
	var row struct {
		Total int64 `bson:"total"`
	}
	if !cursor.Next(ctx) {
		return 0, cursor.Err()
	}
	if err := cursor.Decode(&row); err != nil {
		return 0, fmt.Errorf("decode revenue sum: %w", err)
	}
	return row.Total, cursor.Err()
}
