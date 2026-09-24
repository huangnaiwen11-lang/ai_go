package migrate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	retentionDelivered = 14 * 24 * time.Hour
	retentionFailed    = 30 * 24 * time.Hour
	retentionAttention = 90 * 24 * time.Hour
)

type retentionRule struct {
	status    string
	retention time.Duration
}

func defaultOutboxRetentionPolicy() []retentionRule {
	return []retentionRule{
		{status: "delivered", retention: retentionDelivered},
		{status: "failed", retention: retentionFailed},
		{status: "needs_attention", retention: retentionAttention},
	}
}

func (rule retentionRule) cutoff(now time.Time) time.Time {
	return now.Add(-rule.retention)
}

// RetentionBucketReport 是脱敏的保留期预检结果，只包含数量和最早时间。
// 不返回事件 ID、aggregate ID、payload、provider URL 或账号信息。
type RetentionBucketReport struct {
	Status       string
	Retention    time.Duration
	TotalCount   int64
	ExpiredCount int64
	Oldest       *time.Time
}

// OutboxRetentionReport 是创建 TTL 前必须留存的预检结果。
type OutboxRetentionReport struct {
	CheckedAt time.Time
	Buckets   []RetentionBucketReport
}

// HasExpired 返回是否存在创建 TTL 后会立即被 MongoDB 清理的历史文档。
func (report OutboxRetentionReport) HasExpired() bool {
	for _, bucket := range report.Buckets {
		if bucket.ExpiredCount > 0 {
			return true
		}
	}
	return false
}

// OutboxRetentionMigrator 只操作 outbox_events 的受控保留期索引和历史清理。
// 普通业务启动路径不构造它。
type OutboxRetentionMigrator struct {
	database *mongo.Database
}

func NewOutboxRetentionMigrator(database *mongo.Database) *OutboxRetentionMigrator {
	return &OutboxRetentionMigrator{database: database}
}

// Preflight 统计三类终态数量、最早 updated_at 和当前已过期数量。
// pending/dispatching/reconciling 永远不在过滤范围内。
func (migrator *OutboxRetentionMigrator) Preflight(ctx context.Context, now time.Time) (OutboxRetentionReport, error) {
	if migrator == nil || migrator.database == nil {
		return OutboxRetentionReport{}, errors.New("outbox retention migrator is not configured")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	collection := migrator.database.Collection(schema.CollectionOutboxEvents)
	report := OutboxRetentionReport{CheckedAt: now, Buckets: make([]RetentionBucketReport, 0, len(defaultOutboxRetentionPolicy()))}
	for _, rule := range defaultOutboxRetentionPolicy() {
		statusFilter := bson.D{{Key: "delivery_status", Value: rule.status}}
		total, err := collection.CountDocuments(ctx, statusFilter)
		if err != nil {
			return OutboxRetentionReport{}, fmt.Errorf("count outbox status %q: %w", rule.status, err)
		}
		cutoff := rule.cutoff(now)
		expiredFilter := bson.D{
			{Key: "delivery_status", Value: rule.status},
			{Key: "updated_at", Value: bson.D{{Key: "$lte", Value: cutoff}}},
		}
		expired, err := collection.CountDocuments(ctx, expiredFilter)
		if err != nil {
			return OutboxRetentionReport{}, fmt.Errorf("count expired outbox status %q: %w", rule.status, err)
		}

		bucket := RetentionBucketReport{Status: rule.status, Retention: rule.retention, TotalCount: total, ExpiredCount: expired}
		var oldest struct {
			UpdatedAt time.Time `bson:"updated_at"`
		}
		err = collection.FindOne(ctx, statusFilter, options.FindOne().SetProjection(bson.D{{Key: "updated_at", Value: 1}}).SetSort(bson.D{{Key: "updated_at", Value: 1}})).Decode(&oldest)
		if err == nil {
			bucket.Oldest = &oldest.UpdatedAt
		} else if !errors.Is(err, mongo.ErrNoDocuments) {
			return OutboxRetentionReport{}, fmt.Errorf("find oldest outbox status %q: %w", rule.status, err)
		}
		report.Buckets = append(report.Buckets, bucket)
	}
	return report, nil
}

// CreateIndexes 显式创建三档 partial TTL 索引。调用方必须先执行 Preflight，
// 并自行确认没有会被立即清理的历史文档。
func (migrator *OutboxRetentionMigrator) CreateIndexes(ctx context.Context) error {
	if migrator == nil || migrator.database == nil {
		return errors.New("outbox retention migrator is not configured")
	}
	models := make([]mongo.IndexModel, 0, len(schema.OutboxRetentionIndexes()))
	for _, spec := range schema.OutboxRetentionIndexes() {
		indexOptions := options.Index().SetName(spec.Name).SetExpireAfterSeconds(*spec.ExpireAfterSeconds)
		if len(spec.PartialFilter) > 0 {
			indexOptions.SetPartialFilterExpression(spec.PartialFilter)
		}
		models = append(models, mongo.IndexModel{Keys: spec.Keys, Options: indexOptions})
	}
	if _, err := migrator.database.Collection(schema.CollectionOutboxEvents).Indexes().CreateMany(ctx, models); err != nil {
		return fmt.Errorf("create outbox retention indexes: %w", err)
	}
	return nil
}

// PurgeExpired 分批清理历史终态文档。它不会触碰任何活动状态，且每批最多删除
// batchSize 条，避免一次性删除压垮 MongoDB。必须由显式运维命令调用。
func (migrator *OutboxRetentionMigrator) PurgeExpired(ctx context.Context, now time.Time, batchSize int64) (int64, error) {
	if migrator == nil || migrator.database == nil {
		return 0, errors.New("outbox retention migrator is not configured")
	}
	if batchSize <= 0 {
		return 0, errors.New("outbox retention purge batch size must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	collection := migrator.database.Collection(schema.CollectionOutboxEvents)
	var deletedTotal int64
	for _, rule := range defaultOutboxRetentionPolicy() {
		filter := bson.D{
			{Key: "delivery_status", Value: rule.status},
			{Key: "updated_at", Value: bson.D{{Key: "$lte", Value: rule.cutoff(now)}}},
		}
		for {
			cursor, err := collection.Find(ctx, filter, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}).SetSort(bson.D{{Key: "updated_at", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(batchSize))
			if err != nil {
				return deletedTotal, fmt.Errorf("find expired outbox status %q: %w", rule.status, err)
			}
			var ids []string
			for cursor.Next(ctx) {
				var item struct {
					ID string `bson:"_id"`
				}
				if err := cursor.Decode(&item); err != nil {
					_ = cursor.Close(ctx)
					return deletedTotal, fmt.Errorf("decode expired outbox status %q: %w", rule.status, err)
				}
				ids = append(ids, item.ID)
			}
			if err := cursor.Close(ctx); err != nil {
				return deletedTotal, fmt.Errorf("close expired outbox cursor: %w", err)
			}
			if len(ids) == 0 {
				break
			}
			result, err := collection.DeleteMany(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "delivery_status", Value: rule.status}})
			if err != nil {
				return deletedTotal, fmt.Errorf("delete expired outbox status %q: %w", rule.status, err)
			}
			deletedTotal += result.DeletedCount
			if result.DeletedCount == 0 {
				break
			}
		}
	}
	return deletedTotal, nil
}
