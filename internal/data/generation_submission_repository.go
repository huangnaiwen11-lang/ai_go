package data

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type mongoGenerationSubmissionRepository struct {
	creations *mongo.Collection
	steps     *mongo.Collection
	events    *mongo.Collection
	users     *mongo.Collection
}

// NewGenerationSubmissionRepository 返回生成提交状态仓储，并复用调用方的事务上下文。
func NewGenerationSubmissionRepository(data *Data) generation.SubmissionStore {
	if data == nil || data.database == nil {
		return &mongoGenerationSubmissionRepository{}
	}
	return &mongoGenerationSubmissionRepository{
		creations: data.database.Collection(schema.CollectionCreations),
		steps:     data.database.Collection(schema.CollectionCreationSteps),
		events:    data.database.Collection(schema.CollectionOutboxEvents),
		users:     data.database.Collection(schema.CollectionUsers),
	}
}

// ClaimedSubmission 读取当前已领取事件对应的最小技术提交事实。
func (repository *mongoGenerationSubmissionRepository) ClaimedSubmission(ctx context.Context, eventID string) (*generation.SubmissionRecord, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	if !isGenerationSubmissionEventID(eventID) {
		return nil, generation.ErrInvalidSubmissionCommand
	}
	document, stepID, err := repository.findDispatchingEvent(ctx, eventID, "")
	if err != nil {
		return nil, err
	}
	var step model.CreationStepDocument
	err = repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: stepID},
		{Key: "creation_id", Value: document.AggregateID},
		{Key: "submit_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(creations.StepSubmitStatusReady),
			string(creations.StepSubmitStatusReconciling),
		}}}},
	}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, generation.ErrSubmissionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("read claimed submission step: %w", err)
	}
	var creation model.CreationDocument
	err = repository.creations.FindOne(ctx, bson.D{
		{Key: "_id", Value: document.AggregateID},
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
	}).Decode(&creation)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, generation.ErrSubmissionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("read claimed submission creation: %w", err)
	}
	var user model.UserDocument
	err = repository.users.FindOne(ctx, bson.D{{Key: "_id", Value: creation.UserID}}).Decode(&user)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, generation.ErrSubmissionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("read claimed submission user: %w", err)
	}
	if user.ContentAccess != identity.ContentAccessStandard && user.ContentAccess != identity.ContentAccessReviewRestricted {
		return nil, generation.ErrSubmissionConflict
	}
	return &generation.SubmissionRecord{
		EventID:          document.ID,
		CreationID:       document.AggregateID,
		StepID:           step.ID,
		UserID:           creation.UserID,
		ContentAccess:    user.ContentAccess,
		LeaseToken:       document.LeaseToken,
		LeaseUntil:       document.LeaseUntil,
		ExecutionPayload: append([]byte(nil), document.Payload...),
	}, nil
}

// ExistingSubmission 在条件更新冲突后读取既有事件事实。
// 它不改变任务、预留或账本，工作者据此确认不得再次执行冲正。
func (repository *mongoGenerationSubmissionRepository) ExistingSubmission(ctx context.Context, eventID string) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if !isGenerationSubmissionEventID(eventID) {
		return generation.ErrInvalidSubmissionCommand
	}
	var event model.OutboxEventDocument
	err := repository.events.FindOne(ctx, bson.D{
		{Key: "_id", Value: eventID},
		{Key: "event_type", Value: string(outbox.EventTypeGenerationSubmission)},
	}).Decode(&event)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrSubmissionConflict
	}
	if err != nil {
		return fmt.Errorf("read existing submission event: %w", err)
	}
	return nil
}

// MarkSubmitted 在同一调用方事务中将步骤与事件结案为已提交。
func (repository *mongoGenerationSubmissionRepository) MarkSubmitted(ctx context.Context, command generation.SubmittedCommand) error {
	if err := repository.ready(); err != nil {
		return err
	}
	normalized, err := command.Normalize()
	if err != nil {
		return err
	}
	event, stepID, err := repository.findDispatchingEvent(ctx, normalized.EventID, normalized.LeaseToken)
	if err != nil {
		return err
	}
	if err := repository.updateStepSubmitted(ctx, event.AggregateID, stepID, normalized); err != nil {
		return err
	}
	result, err := repository.events.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: normalized.EventID},
		{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(outbox.DeliveryStatusDispatching),
			string(outbox.DeliveryStatusReconciling),
		}}}},
		{Key: "lease_token", Value: normalized.LeaseToken},
		{Key: "job_id", Value: bson.D{{Key: "$exists", Value: false}}},
	}, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "delivery_status", Value: string(outbox.DeliveryStatusDelivered)},
			{Key: "job_id", Value: normalized.JobID},
			{Key: "updated_at", Value: normalized.At},
		}},
		{Key: "$unset", Value: bson.D{
			{Key: "lease_token", Value: ""},
			{Key: "lease_until", Value: ""},
			{Key: "lease_owner", Value: ""},
		}},
	})
	if err != nil {
		return fmt.Errorf("mark submission delivered: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	return nil
}

// MarkReconciling 将未知结果退回可再次领取的核对状态，并清除已完成的租约。
func (repository *mongoGenerationSubmissionRepository) MarkReconciling(ctx context.Context, command generation.ReconcilingCommand) error {
	if err := repository.ready(); err != nil {
		return err
	}
	normalized, err := command.Normalize()
	if err != nil {
		return err
	}
	event, stepID, err := repository.findDispatchingEvent(ctx, normalized.EventID, normalized.LeaseToken)
	if err != nil {
		return err
	}
	if err := repository.updateStepStatus(ctx, event.AggregateID, stepID, creations.StepSubmitStatusReconciling); err != nil {
		return err
	}
	result, err := repository.events.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: normalized.EventID},
		{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(outbox.DeliveryStatusDispatching),
			string(outbox.DeliveryStatusReconciling),
		}}}},
		{Key: "lease_token", Value: normalized.LeaseToken},
	}, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "delivery_status", Value: string(outbox.DeliveryStatusReconciling)},
			{Key: "next_attempt_at", Value: normalized.At},
			{Key: "updated_at", Value: normalized.At},
		}},
		{Key: "$unset", Value: bson.D{
			{Key: "lease_token", Value: ""},
			{Key: "lease_until", Value: ""},
			{Key: "lease_owner", Value: ""},
		}},
	})
	if err != nil {
		return fmt.Errorf("mark submission reconciling: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	return nil
}

// MarkRejected 只改变创作和步骤状态，供外层事务继续执行账本冲正和发件箱失败结案。
func (repository *mongoGenerationSubmissionRepository) MarkRejected(ctx context.Context, command generation.RejectedCommand) error {
	if err := repository.ready(); err != nil {
		return err
	}
	normalized, err := command.Normalize()
	if err != nil {
		return err
	}
	event, stepID, err := repository.findDispatchingEvent(ctx, normalized.EventID, normalized.LeaseToken)
	if err != nil {
		return err
	}
	if err := repository.updateStepStatus(ctx, event.AggregateID, stepID, creations.StepSubmitStatusSubmissionFailed); err != nil {
		return err
	}
	result, err := repository.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: event.AggregateID},
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.CreationStatusSubmissionFailed)},
		{Key: "updated_at", Value: normalized.At},
	}}})
	if err != nil {
		return fmt.Errorf("mark creation submission failed: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	return nil
}

// MarkConfiscated 只改变审核拒绝对应的创作和步骤状态。
// 预留没收与发件箱失败必须由工作者在同一外层事务内继续完成。
func (repository *mongoGenerationSubmissionRepository) MarkConfiscated(ctx context.Context, command generation.ConfiscatedCommand) error {
	if err := repository.ready(); err != nil {
		return err
	}
	normalized, err := command.Normalize()
	if err != nil {
		return err
	}
	event, stepID, err := repository.findDispatchingEvent(ctx, normalized.EventID, normalized.LeaseToken)
	if err != nil {
		return err
	}
	if err := repository.updateStepStatus(ctx, event.AggregateID, stepID, creations.StepSubmitStatusConfiscated); err != nil {
		return err
	}
	result, err := repository.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: event.AggregateID},
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.CreationStatusConfiscated)},
		{Key: "updated_at", Value: normalized.At},
	}}})
	if err != nil {
		return fmt.Errorf("mark creation confiscated: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	return nil
}

func (repository *mongoGenerationSubmissionRepository) updateStepSubmitted(ctx context.Context, creationID, stepID string, command generation.SubmittedCommand) error {
	result, err := repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: stepID},
		{Key: "creation_id", Value: creationID},
		{Key: "submit_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(creations.StepSubmitStatusReady),
			string(creations.StepSubmitStatusReconciling),
		}}}},
		{Key: "external_execution_id", Value: bson.D{{Key: "$exists", Value: false}}},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSubmitted)},
		{Key: "external_execution_id", Value: command.JobID},
	}}})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return generation.ErrSubmissionConflict
		}
		return fmt.Errorf("mark creation step submitted: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	return nil
}

func (repository *mongoGenerationSubmissionRepository) updateStepStatus(ctx context.Context, creationID, stepID string, status creations.StepSubmitStatus) error {
	result, err := repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: stepID},
		{Key: "creation_id", Value: creationID},
		{Key: "submit_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(creations.StepSubmitStatusReady),
			string(creations.StepSubmitStatusReconciling),
		}}}},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(status)}}}})
	if err != nil {
		return fmt.Errorf("update creation step submission status: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	return nil
}

func (repository *mongoGenerationSubmissionRepository) findDispatchingEvent(ctx context.Context, eventID, leaseToken string) (model.OutboxEventDocument, string, error) {
	if !isGenerationSubmissionEventID(eventID) || (leaseToken != "" && !isSubmissionLeaseToken(leaseToken)) {
		return model.OutboxEventDocument{}, "", generation.ErrInvalidSubmissionCommand
	}
	filter := bson.D{
		{Key: "_id", Value: eventID},
		{Key: "event_type", Value: string(outbox.EventTypeGenerationSubmission)},
		{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(outbox.DeliveryStatusDispatching),
			string(outbox.DeliveryStatusReconciling),
		}}}},
	}
	if leaseToken != "" {
		filter = append(filter, bson.E{Key: "lease_token", Value: leaseToken})
	}
	var event model.OutboxEventDocument
	err := repository.events.FindOne(ctx, filter).Decode(&event)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return model.OutboxEventDocument{}, "", generation.ErrSubmissionConflict
	}
	if err != nil {
		return model.OutboxEventDocument{}, "", fmt.Errorf("read dispatching submission event: %w", err)
	}
	stepID := strings.TrimPrefix(eventID, "generation.submission:")
	if outbox.SubmissionEventID(stepID) != eventID || event.AggregateID == "" {
		return model.OutboxEventDocument{}, "", generation.ErrSubmissionConflict
	}
	return event, stepID, nil
}

func (repository *mongoGenerationSubmissionRepository) ready() error {
	if repository == nil || repository.creations == nil || repository.steps == nil || repository.events == nil || repository.users == nil {
		return errors.New("generation submission repository is not configured")
	}
	return nil
}

func isGenerationSubmissionEventID(eventID string) bool {
	if eventID == "" || eventID != strings.TrimSpace(eventID) || len(eventID) > 512 {
		return false
	}
	stepID := strings.TrimPrefix(eventID, "generation.submission:")
	return outbox.SubmissionEventID(stepID) == eventID
}

func isSubmissionLeaseToken(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 512
}

var _ generation.SubmissionStore = (*mongoGenerationSubmissionRepository)(nil)
