package data

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	generationcancel "ai-business-service/internal/biz/generationcancel"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// mongoGenerationCancelStore is deliberately a narrow store.  It owns only
// the cancellation request transition and provider-cancel outbox rows; it has
// no terminal, publication or ledger write methods, so an HTTP request cannot
// create a local refund or terminal by accident.
type mongoGenerationCancelStore struct {
	creations *mongo.Collection
	steps     *mongo.Collection
	events    *mongo.Collection
}

// NewGenerationCancelStore exposes the owner-scoped B2B cancellation boundary.
// The caller must provide the transaction through generationcancel.Usecase.
func NewGenerationCancelStore(data *Data) generationcancel.Store {
	if data == nil || data.database == nil {
		return &mongoGenerationCancelStore{}
	}
	return &mongoGenerationCancelStore{
		creations: data.database.Collection(schema.CollectionCreations),
		steps:     data.database.Collection(schema.CollectionCreationSteps),
		events:    data.database.Collection(schema.CollectionOutboxEvents),
	}
}

func (store *mongoGenerationCancelStore) RequestCancellation(ctx context.Context, command generationcancel.Command) (generationcancel.Result, error) {
	if err := command.Validate(); err != nil || command.At.IsZero() {
		return generationcancel.Result{}, generationcancel.ErrInvalidCommand
	}
	if store == nil || store.creations == nil || store.steps == nil || store.events == nil {
		return generationcancel.Result{}, generationcancel.ErrDependenciesUnavailable
	}
	session := mongo.SessionFromContext(ctx)
	if session == nil || !session.TransactionRunning() {
		return generationcancel.Result{}, errors.New("generation cancellation requires an active transaction")
	}

	var creation model.CreationDocument
	err := store.creations.FindOne(ctx, bson.D{{Key: "_id", Value: command.CreationID}, {Key: "user_id", Value: command.UserID}}).Decode(&creation)
	if errors.Is(err, mongo.ErrNoDocuments) {
		// Intentionally do not distinguish an absent creation from another user's
		// creation: otherwise this endpoint becomes an ownership oracle.
		return generationcancel.Result{}, generationcancel.ErrNotFound
	}
	if err != nil {
		return generationcancel.Result{}, fmt.Errorf("read cancellation creation: %w", err)
	}

	switch creations.CreationStatus(creation.Status) {
	case creations.CreationStatusCancelling:
		count, err := store.countCancelEvents(ctx, command.CreationID)
		if err != nil {
			return generationcancel.Result{}, err
		}
		return generationcancel.Result{CreationID: command.CreationID, Accepted: true, CancelEventCount: int(count), Replayed: true}, nil
	case creations.CreationStatusPendingSubmission:
		// Continue below.  This is the only pre-terminal state in which a
		// cancellation request may claim durable ownership of the creation.
	default:
		return generationcancel.Result{}, generationcancel.ErrConflict
	}

	candidates, sawSubmission, err := store.cancelCandidates(ctx, command.CreationID)
	if err != nil {
		return generationcancel.Result{}, err
	}
	if !sawSubmission {
		// A ready/blocked local step is not evidence that any provider request
		// left the process.  Do not turn it into a fake cancelled terminal.
		return generationcancel.Result{}, generationcancel.ErrNotReady
	}

	for _, candidate := range candidates {
		if err := store.ensureCancelEvent(ctx, command.CreationID, candidate, command.At); err != nil {
			return generationcancel.Result{}, err
		}
	}

	result, err := store.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: command.CreationID},
		{Key: "user_id", Value: command.UserID},
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.CreationStatusCancelling)},
		{Key: "updated_at", Value: command.At.UTC()},
	}}})
	if err != nil {
		return generationcancel.Result{}, fmt.Errorf("mark creation cancelling: %w", err)
	}
	if result.MatchedCount != 1 {
		return generationcancel.Result{}, generationcancel.ErrConflict
	}
	return generationcancel.Result{CreationID: command.CreationID, Accepted: true, CancelEventCount: len(candidates)}, nil
}

type providerCancelCandidate struct {
	stepID     string
	provider   string
	accountRef string
	jobID      string
	capability string
}

// cancelCandidates returns only already-bound provider jobs.  A prepared
// intent without a job deliberately counts as durable submission evidence but
// emits no cancel event: the submission worker will Lookup only, and
// BindProviderJob schedules this exact event if it later learns the job ID.
func (store *mongoGenerationCancelStore) cancelCandidates(ctx context.Context, creationID string) ([]providerCancelCandidate, bool, error) {
	cursor, err := store.steps.Find(ctx, bson.D{{Key: "creation_id", Value: creationID}})
	if err != nil {
		return nil, false, fmt.Errorf("read cancellation steps: %w", err)
	}
	defer cursor.Close(ctx)

	var candidates []providerCancelCandidate
	sawSubmission := false
	for cursor.Next(ctx) {
		var step struct {
			ID                  string                                  `bson:"_id"`
			Atom                string                                  `bson:"atom"`
			Provider            string                                  `bson:"provider"`
			AccountRef          string                                  `bson:"account_ref"`
			ContractVersion     string                                  `bson:"contract_version"`
			MappingVersion      string                                  `bson:"mapping_version"`
			ExternalExecutionID string                                  `bson:"external_execution_id"`
			TerminalStatus      string                                  `bson:"terminal_status"`
			Intent              *model.ProviderSubmissionIntentDocument `bson:"submission_intent"`
		}
		if err := cursor.Decode(&step); err != nil {
			return nil, false, fmt.Errorf("decode cancellation step: %w", err)
		}
		route, err := creations.NormalizeExecutionRoute(creations.ExecutionRoute{
			Provider: step.Provider, AccountRef: step.AccountRef, ContractVersion: step.ContractVersion, MappingVersion: step.MappingVersion,
		})
		if err != nil || route.Provider != creations.PolarStarB2BProvider {
			continue
		}
		if step.Intent == nil && step.ExternalExecutionID == "" {
			continue
		}
		// A bound job with a missing or malformed intent is corrupt durable
		// state, not a reason to guess a provider request or local outcome.
		if step.Intent == nil || step.Intent.PreparedAt.IsZero() || step.Intent.Fence <= 0 {
			return nil, false, generationcancel.ErrConflict
		}
		sawSubmission = true
		if step.TerminalStatus != "" {
			return nil, false, generationcancel.ErrConflict
		}
		if step.ExternalExecutionID == "" {
			continue
		}
		payload := generation.ProviderCancelEventPayload{
			CreationID: creationID, StepID: step.ID, Provider: route.Provider, AccountRef: route.AccountRef,
			JobID: step.ExternalExecutionID, Capability: step.Atom,
		}
		if !payload.Validate() {
			return nil, false, generationcancel.ErrConflict
		}
		candidates = append(candidates, providerCancelCandidate{
			stepID: step.ID, provider: route.Provider, accountRef: route.AccountRef, jobID: step.ExternalExecutionID, capability: step.Atom,
		})
	}
	if err := cursor.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate cancellation steps: %w", err)
	}
	return candidates, sawSubmission, nil
}

func (store *mongoGenerationCancelStore) ensureCancelEvent(ctx context.Context, creationID string, candidate providerCancelCandidate, at time.Time) error {
	payload, err := generation.MarshalProviderCancelEventPayload(generation.ProviderCancelEventPayload{
		CreationID: creationID, StepID: candidate.stepID, Provider: candidate.provider, AccountRef: candidate.accountRef,
		JobID: candidate.jobID, Capability: candidate.capability,
	})
	if err != nil {
		return generationcancel.ErrConflict
	}
	eventID := generation.ProviderCancelEventID(candidate.stepID)
	if eventID == "" {
		return generationcancel.ErrConflict
	}
	_, err = store.events.InsertOne(ctx, model.OutboxEventDocument{
		ID: eventID, AggregateID: creationID, EventType: generation.ProviderCancelEventType, Payload: payload,
		DeliveryStatus: string(outbox.DeliveryStatusPending), AttemptCount: 0, NextAttemptAt: at.UTC(), CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	})
	if err == nil {
		return nil
	}
	if !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("insert provider cancel event: %w", err)
	}
	var existing model.OutboxEventDocument
	if readErr := store.events.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&existing); readErr != nil {
		return fmt.Errorf("read duplicate provider cancel event: %w", readErr)
	}
	if existing.AggregateID != creationID || existing.EventType != generation.ProviderCancelEventType || !bytes.Equal(existing.Payload, payload) {
		return generationcancel.ErrConflict
	}
	return nil
}

func (store *mongoGenerationCancelStore) countCancelEvents(ctx context.Context, creationID string) (int64, error) {
	count, err := store.events.CountDocuments(ctx, bson.D{{Key: "aggregate_id", Value: creationID}, {Key: "event_type", Value: generation.ProviderCancelEventType}})
	if err != nil {
		return 0, fmt.Errorf("count provider cancel events: %w", err)
	}
	return count, nil
}

var _ generationcancel.Store = (*mongoGenerationCancelStore)(nil)
