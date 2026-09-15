package data

import (
	"context"
	"fmt"

	"ai-business-service/internal/biz/feedback"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

type mongoFeedbackRepository struct{ collection *mongo.Collection }

// NewFeedbackRepository 返回用户反馈写入仓储；回复、状态和运营字段不暴露给用户端。
func NewFeedbackRepository(data *Data) feedback.Repository {
	return &mongoFeedbackRepository{collection: data.database.Collection(schema.CollectionFeedbacks)}
}

func (repository *mongoFeedbackRepository) Create(ctx context.Context, submission feedback.Submission) error {
	document := model.FeedbackDocument{ID: submission.ID, UserID: submission.UserID, Type: submission.Type, Message: submission.Message, Email: submission.Email, CreatedAt: submission.CreatedAt}
	document.Attachments = make([]model.FeedbackAttachment, 0, len(submission.Attachments))
	for _, attachment := range submission.Attachments {
		document.Attachments = append(document.Attachments, model.FeedbackAttachment{ID: attachment.ID, DownloadURL: attachment.DownloadURL})
	}
	if _, err := repository.collection.InsertOne(ctx, document); err != nil {
		return fmt.Errorf("create feedback: %w", err)
	}
	return nil
}

var _ feedback.Repository = (*mongoFeedbackRepository)(nil)
