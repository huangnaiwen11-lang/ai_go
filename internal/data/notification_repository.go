package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	biznotification "ai-business-service/internal/biz/notification"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoNotificationRepository struct {
	collection  *mongo.Collection
	preferences *mongoNotificationPreferencesRepository
}

type mongoNotificationPreferencesRepository struct{ collection *mongo.Collection }

func NewNotificationRepository(data *Data) biznotification.Repository {
	return &mongoNotificationRepository{
		collection:  data.database.Collection(schema.CollectionNotifications),
		preferences: &mongoNotificationPreferencesRepository{collection: data.database.Collection(schema.CollectionNotificationPreferences)},
	}
}

func (repository *mongoNotificationRepository) List(ctx context.Context, query biznotification.ListQuery) ([]biznotification.Item, int, error) {
	filter := bson.D{{Key: "user_id", Value: query.UserID}}
	if query.UnreadOnly {
		filter = append(filter, bson.E{Key: "read", Value: false})
	}
	count, err := repository.collection.CountDocuments(ctx, bson.D{{Key: "user_id", Value: query.UserID}, {Key: "read", Value: false}})
	if err != nil {
		return nil, 0, fmt.Errorf("count notifications: %w", err)
	}
	cursor, err := repository.collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(int64(query.Limit)))
	if err != nil {
		return nil, 0, fmt.Errorf("list notifications: %w", err)
	}
	defer cursor.Close(ctx)
	var documents []model.NotificationDocument
	if err := cursor.All(ctx, &documents); err != nil {
		return nil, 0, fmt.Errorf("decode notifications: %w", err)
	}
	items := make([]biznotification.Item, 0, len(documents))
	for _, document := range documents {
		items = append(items, toNotification(document))
	}
	return items, int(count), nil
}

func (repository *mongoNotificationRepository) MarkRead(ctx context.Context, userID, id string, readAt time.Time) error {
	_, err := repository.collection.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "user_id", Value: userID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "read", Value: true}, {Key: "read_at", Value: readAt}}}})
	if err != nil {
		return fmt.Errorf("mark notification read: %w", err)
	}
	return nil
}
func (repository *mongoNotificationRepository) MarkAllRead(ctx context.Context, userID string, readAt time.Time) (int, error) {
	result, err := repository.collection.UpdateMany(ctx, bson.D{{Key: "user_id", Value: userID}, {Key: "read", Value: false}}, bson.D{{Key: "$set", Value: bson.D{{Key: "read", Value: true}, {Key: "read_at", Value: readAt}}}})
	if err != nil {
		return 0, fmt.Errorf("mark all notifications read: %w", err)
	}
	return int(result.ModifiedCount), nil
}
func (repository *mongoNotificationRepository) Delete(ctx context.Context, userID, id string) error {
	_, err := repository.collection.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "user_id", Value: userID}})
	if err != nil {
		return fmt.Errorf("delete notification: %w", err)
	}
	return nil
}

// DeleteRead 使用固定的 user_id + read=true 条件；调用方无法扩大删除范围。
func (repository *mongoNotificationRepository) DeleteRead(ctx context.Context, userID string) (int, error) {
	result, err := repository.collection.DeleteMany(ctx, bson.D{{Key: "user_id", Value: userID}, {Key: "read", Value: true}})
	if err != nil {
		return 0, fmt.Errorf("delete read notifications: %w", err)
	}
	return int(result.DeletedCount), nil
}

// GetPreferences 在用户尚未修改任何设置时返回明确的产品默认值，避免把“没有文档”误解为
// “全部关闭”。偏好文档仅属于自有 Go 用户域。
func (repository *mongoNotificationRepository) GetPreferences(ctx context.Context, userID string) (biznotification.Preferences, error) {
	if repository.preferences == nil || repository.preferences.collection == nil {
		return biznotification.Preferences{}, fmt.Errorf("notification preferences repository is not configured")
	}
	var document model.NotificationPreferencesDocument
	err := repository.preferences.collection.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return biznotification.DefaultPreferences(), nil
	}
	if err != nil {
		return biznotification.Preferences{}, fmt.Errorf("get notification preferences: %w", err)
	}
	return biznotification.Preferences{
		PushEnabled:                document.PushEnabled,
		EmailEnabled:               document.EmailEnabled,
		GenerationCompletedEnabled: document.GenerationCompletedEnabled,
	}, nil
}

// SavePreferences 使用 _id=userID 的 upsert。整份配置一次写入，既避免同一用户多文档，
// 也让三个可见开关始终来自同一个版本的用户选择。
func (repository *mongoNotificationRepository) SavePreferences(ctx context.Context, userID string, preferences biznotification.Preferences) error {
	if repository.preferences == nil || repository.preferences.collection == nil {
		return fmt.Errorf("notification preferences repository is not configured")
	}
	_, err := repository.preferences.collection.UpdateByID(ctx, userID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "push_enabled", Value: preferences.PushEnabled},
		{Key: "email_enabled", Value: preferences.EmailEnabled},
		{Key: "generation_completed_enabled", Value: preferences.GenerationCompletedEnabled},
		{Key: "updated_at", Value: time.Now().UTC()},
	}}}, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("save notification preferences: %w", err)
	}
	return nil
}

func toNotification(document model.NotificationDocument) biznotification.Item {
	return biznotification.Item{ID: document.ID, UserID: document.UserID, Type: document.Type, Title: document.Title, Body: document.Body, Data: document.Data, Read: document.Read, ReadAt: document.ReadAt, CreatedAt: document.CreatedAt}
}

var _ biznotification.Repository = (*mongoNotificationRepository)(nil)
