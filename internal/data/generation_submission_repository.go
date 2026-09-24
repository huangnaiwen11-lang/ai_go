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
	creations    *mongo.Collection
	steps        *mongo.Collection
	events       *mongo.Collection
	users        *mongo.Collection
	reservations *mongo.Collection
}

// NewGenerationSubmissionRepository 返回生成提交状态仓储，并复用调用方的事务上下文。
func NewGenerationSubmissionRepository(data *Data) generation.SubmissionStore {
	if data == nil || data.database == nil {
		return &mongoGenerationSubmissionRepository{}
	}
	return &mongoGenerationSubmissionRepository{
		creations:    data.database.Collection(schema.CollectionCreations),
		steps:        data.database.Collection(schema.CollectionCreationSteps),
		events:       data.database.Collection(schema.CollectionOutboxEvents),
		users:        data.database.Collection(schema.CollectionUsers),
		reservations: data.database.Collection(schema.CollectionReservations),
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
	err = repository.steps.FindOne(ctx, recoverableSubmissionStepFilter(document.AggregateID, stepID)).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, generation.ErrSubmissionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("read claimed submission step: %w", err)
	}
	if step.SubmitStatus == "submitting" {
		// 已授权步骤只能从有效的冻结意图恢复；不能把缺失/损坏意图当首次提交。
		intent, err := repository.ReadProviderSubmission(ctx, stepID)
		if err != nil {
			return nil, err
		}
		if intent == nil {
			return nil, generation.ErrSubmissionConflict
		}
	}
	var creation model.CreationDocument
	err = repository.creations.FindOne(ctx, bson.D{
		{Key: "_id", Value: document.AggregateID},
		{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(creations.CreationStatusPendingSubmission),
			string(creations.CreationStatusCancelling),
		}}}},
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
	route, err := creations.NormalizeExecutionRoute(creations.ExecutionRoute{Provider: step.Provider, AccountRef: step.AccountRef, ContractVersion: step.ContractVersion, MappingVersion: step.MappingVersion})
	if err != nil {
		return nil, generation.ErrSubmissionConflict
	}
	return &generation.SubmissionRecord{
		Route:            route,
		EventID:          document.ID,
		CreationID:       document.AggregateID,
		StepID:           step.ID,
		CreationStatus:   creations.CreationStatus(creation.Status),
		UserID:           creation.UserID,
		ContentAccess:    user.ContentAccess,
		LeaseToken:       document.LeaseToken,
		LeaseOwner:       document.LeaseOwner,
		Fence:            document.AttemptCount,
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
// 再次可领取的时刻取自命令，而不是「当前时刻」：提交结果未知往往伴随
// 429/503 的 Retry-After，立刻放回队列等于无视平台给出的等待要求。
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
			{Key: "next_attempt_at", Value: normalized.NextAttemptAt},
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
	if err := repository.updateStepStatusWithCause(ctx, event.AggregateID, stepID, creations.StepSubmitStatusSubmissionFailed, normalized.ProviderRejectionCause); err != nil {
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
	result, err := repository.steps.UpdateOne(ctx, recoverableSubmissionStepFilter(creationID, stepID), bson.D{{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(status)}}}})
	if err != nil {
		return fmt.Errorf("update creation step submission status: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	return nil
}

// updateStepStatusWithCause 仅供 MarkRejected 使用。空 cause 刻意不写入/清除已有值：
// 重入或并发终态不能抹掉已经持久化的 provider_payment_required 事实。
func (repository *mongoGenerationSubmissionRepository) updateStepStatusWithCause(ctx context.Context, creationID, stepID string, status creations.StepSubmitStatus, cause generation.ProviderRejectionCause) error {
	set := bson.D{{Key: "submit_status", Value: string(status)}}
	if cause != "" {
		set = append(set, bson.E{Key: "provider_rejection_cause", Value: string(cause)})
	}
	result, err := repository.steps.UpdateOne(ctx, recoverableSubmissionStepFilter(creationID, stepID), bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return fmt.Errorf("update creation step submission status with cause: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	return nil
}

// local 的状态集合不变。只有尚未绑定任务号、带 B2B 提交意图的 submitting
// 才能进入恢复路径；读取时还需重验冻结请求，授权仍由独立事务 CAS 控制。
func recoverableSubmissionStepFilter(creationID, stepID string) bson.D {
	return bson.D{
		{Key: "_id", Value: stepID}, {Key: "creation_id", Value: creationID},
		{Key: "$or", Value: bson.A{
			bson.M{"submit_status": bson.M{"$in": bson.A{string(creations.StepSubmitStatusReady), string(creations.StepSubmitStatusReconciling)}}},
			bson.M{"submit_status": "submitting", "provider": creations.PolarStarB2BProvider,
				"contract_version": creations.B2BContractVersion, "submission_intent": bson.M{"$type": "object"},
				"external_execution_id": bson.M{"$exists": false}},
		}},
	}
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
