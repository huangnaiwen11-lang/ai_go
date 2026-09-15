package data

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"ai-business-service/internal/executionv2"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	generationCallbackReceiptSource = "generation.execution.v2"
	callbackVersionV2               = int64(2)
	callbackAssetOwnerType          = "creation_step"
	callbackAssetKindResult         = "result"
	callbackAssetStatusAvailable    = "available"
)

// mongoGenerationCallbackRepository 在调用方事务内同时实现可信步骤关联和回调终态写入。
type mongoGenerationCallbackRepository struct {
	creations *mongo.Collection
	steps     *mongo.Collection
	recipes   *mongo.Collection
	assets    *mongo.Collection
	receipts  *mongo.Collection
	events    *mongo.Collection
}

// NewGenerationCallbackRepository 返回生成回调的关联器和终态仓储实现。
func NewGenerationCallbackRepository(data *Data) *mongoGenerationCallbackRepository {
	if data == nil || data.database == nil {
		return &mongoGenerationCallbackRepository{}
	}
	return &mongoGenerationCallbackRepository{
		creations: data.database.Collection(schema.CollectionCreations),
		steps:     data.database.Collection(schema.CollectionCreationSteps),
		recipes:   data.database.Collection(schema.CollectionGenerationStepRecipes),
		assets:    data.database.Collection(schema.CollectionAssets),
		receipts:  data.database.Collection(schema.CollectionCallbackReceipts),
		events:    data.database.Collection(schema.CollectionOutboxEvents),
	}
}

// NewGenerationCallbackStore 将 Mongo 回调仓储收窄为终态写入边界。
func NewGenerationCallbackStore(repository *mongoGenerationCallbackRepository) generation.CallbackStore {
	return repository
}

// NewGenerationCallbackLinker 将 Mongo 回调仓储收窄为可信步骤关联边界。
func NewGenerationCallbackLinker(repository *mongoGenerationCallbackRepository) generation.CallbackLinker {
	return repository
}

// Link 仅从已保存步骤读取创作归属，并核验任务标识和技术原子。
func (repository *mongoGenerationCallbackRepository) Link(ctx context.Context, event generation.VerifiedCallbackEvent) (generation.LinkedCallback, error) {
	if err := repository.ready(); err != nil {
		return generation.LinkedCallback{}, err
	}
	if event.StepID == "" || event.JobID == "" || event.ExternalRef != event.StepID || !callbackCapabilityMatchesAtom(event.Capability, "") {
		return generation.LinkedCallback{}, generation.ErrInvalidCallbackEvent
	}
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: event.StepID},
		{Key: "external_execution_id", Value: event.JobID},
		{Key: "atom", Value: callbackAtom(event.Capability)},
	}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.LinkedCallback{}, generation.ErrInvalidCallbackEvent
	}
	if err != nil || step.CreationID == "" {
		if err != nil {
			return generation.LinkedCallback{}, fmt.Errorf("link generation callback step: %w", err)
		}
		return generation.LinkedCallback{}, generation.ErrInvalidCallbackEvent
	}
	return generation.NewLinkedCallback(step.CreationID, event)
}

// Complete 先写回执，再用 submitted 状态和回调版本执行终态 CAS。
func (repository *mongoGenerationCallbackRepository) Complete(ctx context.Context, command generation.CompletedCallback) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if !callbackTransactionActive(ctx) {
		return generation.ErrInvalidCallbackEvent
	}
	if !validCompletedCommand(command) {
		return generation.ErrInvalidCallbackEvent
	}
	if err := repository.insertReceipt(ctx, completedReceipt(command)); err != nil {
		return err
	}

	matched, err := repository.updateTerminalStep(ctx, command.CreationID, command.StepID, command.JobID, command.Capability, creations.StepSubmitStatusSucceeded, command.At)
	if err != nil {
		return err
	}
	if !matched {
		return repository.existingCompleted(ctx, command)
	}
	if err := repository.insertResultAsset(ctx, command.StepID, command.ResultURL, command.At); err != nil {
		return err
	}

	finalStep, err := repository.isFinalCallbackStep(ctx, command.CreationID, command.StepID)
	if err != nil {
		return err
	}
	if finalStep {
		return repository.markCreationSucceeded(ctx, command.CreationID, command.At)
	}
	return repository.activateSecondStep(ctx, command)
}

// Fail 先写回执，再以 submitted 状态将对应步骤和创作收敛到技术失败。
func (repository *mongoGenerationCallbackRepository) Fail(ctx context.Context, command generation.FailedCallback) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if !callbackTransactionActive(ctx) {
		return generation.ErrInvalidCallbackEvent
	}
	if !validFailedCommand(command) {
		return generation.ErrInvalidCallbackEvent
	}
	if err := repository.insertReceipt(ctx, failedReceipt(command)); err != nil {
		return err
	}

	matched, err := repository.updateTerminalStep(ctx, command.CreationID, command.StepID, command.JobID, command.Capability, creations.StepSubmitStatusGenerationFailed, command.At)
	if err != nil {
		return err
	}
	if !matched {
		return repository.existingFailed(ctx, command)
	}
	result, err := repository.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: command.CreationID},
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.CreationStatusGenerationFailed)},
		{Key: "updated_at", Value: command.At.UTC()},
	}}})
	if err != nil {
		return fmt.Errorf("mark generation failed creation: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrInvalidCallbackEvent
	}
	return nil
}

type callbackReceipt struct {
	nonceHash       string
	payloadDigest   string
	creationID      string
	stepID          string
	externalRef     string
	jobID           string
	capability      executionv2.Capability
	terminal        string
	mediaType       string
	resultURL       string
	callbackVersion string
	at              time.Time
}

func completedReceipt(command generation.CompletedCallback) callbackReceipt {
	return callbackReceipt{nonceHash: command.NonceHash, payloadDigest: command.PayloadDigest, creationID: command.CreationID, stepID: command.StepID, externalRef: command.ExternalRef, jobID: command.JobID, capability: command.Capability, terminal: "completed", mediaType: command.MediaType, resultURL: command.ResultURL, callbackVersion: command.CallbackVersion, at: command.At}
}

func failedReceipt(command generation.FailedCallback) callbackReceipt {
	return callbackReceipt{nonceHash: command.NonceHash, payloadDigest: command.PayloadDigest, creationID: command.CreationID, stepID: command.StepID, externalRef: command.ExternalRef, jobID: command.JobID, capability: command.Capability, terminal: string(command.Terminal), callbackVersion: command.CallbackVersion, at: command.At}
}

func (repository *mongoGenerationCallbackRepository) insertReceipt(ctx context.Context, receipt callbackReceipt) error {
	result, err := repository.receipts.UpdateOne(ctx, bson.D{
		{Key: "source", Value: generationCallbackReceiptSource},
		{Key: "nonce_hash", Value: receipt.nonceHash},
	}, bson.D{{Key: "$setOnInsert", Value: model.CallbackReceiptDocument{
		ID:              "generation.callback:" + receipt.nonceHash,
		Source:          generationCallbackReceiptSource,
		NonceHash:       receipt.nonceHash,
		ReceivedAt:      receipt.at.UTC(),
		PayloadDigest:   receipt.payloadDigest,
		CreationID:      receipt.creationID,
		StepID:          receipt.stepID,
		ExternalRef:     receipt.externalRef,
		JobID:           receipt.jobID,
		Capability:      string(receipt.capability),
		Terminal:        receipt.terminal,
		MediaType:       receipt.mediaType,
		ResultURL:       receipt.resultURL,
		CallbackVersion: receipt.callbackVersion,
	}}}, options.UpdateOne().SetUpsert(true))
	if err == nil {
		if result.UpsertedCount == 1 {
			return nil
		}
	} else if !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("upsert generation callback receipt: %w", err)
	}
	var existing model.CallbackReceiptDocument
	err = repository.receipts.FindOne(ctx, bson.D{
		{Key: "source", Value: generationCallbackReceiptSource},
		{Key: "nonce_hash", Value: receipt.nonceHash},
	}).Decode(&existing)
	if err != nil {
		return fmt.Errorf("read existing generation callback receipt: %w", err)
	}
	if !sameCallbackReceipt(existing, receipt) {
		return generation.ErrInvalidCallbackEvent
	}
	return generation.ErrCallbackAlreadyConfirmed
}

func (repository *mongoGenerationCallbackRepository) updateTerminalStep(ctx context.Context, creationID, stepID, jobID string, capability executionv2.Capability, status creations.StepSubmitStatus, atTime time.Time) (bool, error) {
	result, err := repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: stepID},
		{Key: "creation_id", Value: creationID},
		{Key: "external_execution_id", Value: jobID},
		{Key: "atom", Value: callbackAtom(capability)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSubmitted)},
		{Key: "callback_version", Value: int64(0)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "submit_status", Value: string(status)},
		{Key: "callback_version", Value: callbackVersionV2},
	}}})
	if err != nil {
		return false, fmt.Errorf("compare-and-set callback step at %s: %w", atTime.UTC().Format(time.RFC3339), err)
	}
	return result.MatchedCount == 1, nil
}

func (repository *mongoGenerationCallbackRepository) insertResultAsset(ctx context.Context, stepID, resultURL string, atTime time.Time) error {
	_, err := repository.assets.InsertOne(ctx, model.AssetDocument{
		ID: "asset:" + stepID + ":result", OwnerType: callbackAssetOwnerType, OwnerID: stepID,
		AssetKind: callbackAssetKindResult, StorageKey: resultURL, Status: callbackAssetStatusAvailable, CreatedAt: atTime.UTC(),
	})
	if err == nil {
		return nil
	}
	if mongo.IsDuplicateKeyError(err) {
		return generation.ErrInvalidCallbackEvent
	}
	return fmt.Errorf("insert callback result asset: %w", err)
}

func (repository *mongoGenerationCallbackRepository) isFinalCallbackStep(ctx context.Context, creationID, stepID string) (bool, error) {
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{{Key: "_id", Value: stepID}, {Key: "creation_id", Value: creationID}}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return false, fmt.Errorf("read callback step topology: %w", err)
	}
	count, err := repository.steps.CountDocuments(ctx, bson.D{{Key: "creation_id", Value: creationID}, {Key: "sequence", Value: bson.D{{Key: "$gt", Value: step.Sequence}}}})
	if err != nil {
		return false, fmt.Errorf("count callback creation steps: %w", err)
	}
	return count == 0, nil
}

func (repository *mongoGenerationCallbackRepository) markCreationSucceeded(ctx context.Context, creationID string, atTime time.Time) error {
	result, err := repository.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: creationID},
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.CreationStatusSucceeded)},
		{Key: "updated_at", Value: atTime.UTC()},
	}}})
	if err != nil {
		return fmt.Errorf("mark generation succeeded creation: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrInvalidCallbackEvent
	}
	return nil
}

func (repository *mongoGenerationCallbackRepository) activateSecondStep(ctx context.Context, command generation.CompletedCallback) error {
	var second model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "creation_id", Value: command.CreationID},
		{Key: "sequence", Value: int32(2)},
		{Key: "atom", Value: string(creations.AtomImageToVideo)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusBlocked)},
	}).Decode(&second)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read blocked second step: %w", err)
	}
	var recipe model.DeferredRecipeDocument
	err = repository.recipes.FindOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "step_id", Value: second.ID},
		{Key: "creation_id", Value: command.CreationID}, {Key: "atom", Value: string(creations.AtomImageToVideo)},
		{Key: "status", Value: string(creations.DeferredRecipeStatusPending)},
	}).Decode(&recipe)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read deferred recipe: %w", err)
	}
	deferred, err := executionv2.CompileDeferredImageToVideo(recipe.ModelSKU, recipe.InputTemplate)
	if err != nil {
		return generation.ErrInvalidCallbackEvent
	}
	if deferred.Digest != recipe.Digest {
		return generation.ErrInvalidCallbackEvent
	}
	snapshot, err := deferred.BindOpeningFrame(command.ResultURL)
	if err != nil {
		return generation.ErrInvalidCallbackEvent
	}
	payload, err := snapshot.MarshalSubmissionPayload()
	if err != nil {
		return generation.ErrInvalidCallbackEvent
	}
	event, err := outbox.NewPending(outbox.SubmissionEventID(second.ID), command.CreationID, payload, command.At.UTC())
	if err != nil {
		return generation.ErrInvalidCallbackEvent
	}

	result, err := repository.recipes.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "status", Value: string(creations.DeferredRecipeStatusPending)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.DeferredRecipeStatusConsumed)}, {Key: "updated_at", Value: command.At.UTC()},
	}}})
	if err != nil {
		return fmt.Errorf("consume deferred recipe: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrInvalidCallbackEvent
	}
	result, err = repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "creation_id", Value: command.CreationID},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusBlocked)},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(creations.StepSubmitStatusReady)}}}})
	if err != nil {
		return fmt.Errorf("activate second generation step: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrInvalidCallbackEvent
	}
	_, err = repository.events.InsertOne(ctx, model.OutboxEventDocument{
		ID: event.ID, AggregateID: event.AggregateID, EventType: string(event.EventType), Payload: event.Payload,
		DeliveryStatus: string(event.DeliveryStatus), AttemptCount: event.AttemptCount, NextAttemptAt: event.NextAttemptAt,
		CreatedAt: event.CreatedAt, UpdatedAt: event.UpdatedAt,
	})
	if err == nil {
		return nil
	}
	if mongo.IsDuplicateKeyError(err) {
		return generation.ErrInvalidCallbackEvent
	}
	return fmt.Errorf("enqueue second generation submission: %w", err)
}

func (repository *mongoGenerationCallbackRepository) existingCompleted(ctx context.Context, command generation.CompletedCallback) error {
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: command.StepID}, {Key: "creation_id", Value: command.CreationID},
		{Key: "external_execution_id", Value: command.JobID}, {Key: "atom", Value: callbackAtom(command.Capability)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSucceeded)}, {Key: "callback_version", Value: callbackVersionV2},
	}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read existing completed callback step: %w", err)
	}
	var receipt model.CallbackReceiptDocument
	err = repository.receipts.FindOne(ctx, receiptFactFilter(completedReceipt(command), command.NonceHash)).Decode(&receipt)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read existing completed callback receipt: %w", err)
	}
	var asset model.AssetDocument
	err = repository.assets.FindOne(ctx, bson.D{
		{Key: "_id", Value: "asset:" + command.StepID + ":result"},
		{Key: "owner_type", Value: callbackAssetOwnerType}, {Key: "owner_id", Value: command.StepID},
		{Key: "asset_kind", Value: callbackAssetKindResult}, {Key: "storage_key", Value: command.ResultURL},
		{Key: "status", Value: callbackAssetStatusAvailable},
	}).Decode(&asset)
	if err == nil {
		return generation.ErrCallbackAlreadyConfirmed
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	return fmt.Errorf("read existing callback result asset: %w", err)
}

func (repository *mongoGenerationCallbackRepository) existingFailed(ctx context.Context, command generation.FailedCallback) error {
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: command.StepID}, {Key: "creation_id", Value: command.CreationID},
		{Key: "external_execution_id", Value: command.JobID}, {Key: "atom", Value: callbackAtom(command.Capability)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusGenerationFailed)}, {Key: "callback_version", Value: callbackVersionV2},
	}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read existing failed callback step: %w", err)
	}
	var receipt model.CallbackReceiptDocument
	err = repository.receipts.FindOne(ctx, receiptFactFilter(failedReceipt(command), command.NonceHash)).Decode(&receipt)
	if err == nil {
		return generation.ErrCallbackAlreadyConfirmed
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	return fmt.Errorf("read existing failed callback receipt: %w", err)
}

func (repository *mongoGenerationCallbackRepository) ready() error {
	if repository == nil || repository.creations == nil || repository.steps == nil || repository.recipes == nil || repository.assets == nil || repository.receipts == nil || repository.events == nil {
		return errors.New("generation callback repository is not configured")
	}
	return nil
}

func callbackTransactionActive(ctx context.Context) bool {
	session := mongo.SessionFromContext(ctx)
	return session != nil && session.TransactionRunning()
}

func sameCallbackReceipt(existing model.CallbackReceiptDocument, expected callbackReceipt) bool {
	return existing.Source == generationCallbackReceiptSource && existing.NonceHash == expected.nonceHash &&
		existing.PayloadDigest == expected.payloadDigest && existing.CreationID == expected.creationID && existing.StepID == expected.stepID &&
		existing.ExternalRef == expected.externalRef && existing.JobID == expected.jobID && existing.Capability == string(expected.capability) &&
		existing.Terminal == expected.terminal && existing.MediaType == expected.mediaType && existing.ResultURL == expected.resultURL && existing.CallbackVersion == expected.callbackVersion
}

func receiptFactFilter(receipt callbackReceipt, excludeNonceHash string) bson.D {
	filter := bson.D{
		{Key: "source", Value: generationCallbackReceiptSource}, {Key: "payload_digest", Value: receipt.payloadDigest},
		{Key: "creation_id", Value: receipt.creationID}, {Key: "step_id", Value: receipt.stepID}, {Key: "external_ref", Value: receipt.externalRef},
		{Key: "job_id", Value: receipt.jobID}, {Key: "capability", Value: string(receipt.capability)}, {Key: "terminal", Value: receipt.terminal},
		{Key: "media_type", Value: receipt.mediaType}, {Key: "result_url", Value: receipt.resultURL}, {Key: "callback_version", Value: receipt.callbackVersion},
	}
	if excludeNonceHash != "" {
		filter = append(filter, bson.E{Key: "nonce_hash", Value: bson.D{{Key: "$ne", Value: excludeNonceHash}}})
	}
	return filter
}

func validCompletedCommand(command generation.CompletedCallback) bool {
	return validCallbackBase(command.CreationID, command.StepID, command.JobID, command.ExternalRef, command.Capability, command.NonceHash, command.PayloadDigest, command.CallbackVersion, command.At) &&
		callbackMediaMatches(command.Capability, command.MediaType) && callbackResultURL(command.ResultURL)
}

func validFailedCommand(command generation.FailedCallback) bool {
	return validCallbackBase(command.CreationID, command.StepID, command.JobID, command.ExternalRef, command.Capability, command.NonceHash, command.PayloadDigest, command.CallbackVersion, command.At) &&
		(command.Terminal == generation.CallbackTerminalFailed || command.Terminal == generation.CallbackTerminalCancelled)
}

func validCallbackBase(creationID, stepID, jobID, externalRef string, capability executionv2.Capability, nonceHash, payloadDigest, version string, atTime time.Time) bool {
	return callbackIdentifier(creationID) && callbackIdentifier(stepID) && callbackIdentifier(jobID) && externalRef == stepID &&
		callbackIdentifier(nonceHash) && callbackIdentifier(payloadDigest) && version == "2" && !atTime.IsZero() && callbackCapabilityMatchesAtom(capability, "")
}

func callbackIdentifier(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, " \t\r\n") && len(value) <= 512
}

func callbackCapabilityMatchesAtom(capability executionv2.Capability, atom string) bool {
	expected := callbackAtom(capability)
	return expected != "" && (atom == "" || atom == expected)
}

func callbackAtom(capability executionv2.Capability) string {
	switch capability {
	case executionv2.CapabilityTextToImage:
		return string(creations.AtomTextToImage)
	case executionv2.CapabilityImageEdit:
		return string(creations.AtomImageEdit)
	case executionv2.CapabilityImageToVideo:
		return string(creations.AtomImageToVideo)
	default:
		return ""
	}
}

func callbackMediaMatches(capability executionv2.Capability, mediaType string) bool {
	if capability == executionv2.CapabilityImageToVideo {
		return mediaType == "video"
	}
	return mediaType == "image"
}

func callbackResultURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && raw == strings.TrimSpace(raw) && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

var _ generation.CallbackStore = (*mongoGenerationCallbackRepository)(nil)
var _ generation.CallbackLinker = (*mongoGenerationCallbackRepository)(nil)
