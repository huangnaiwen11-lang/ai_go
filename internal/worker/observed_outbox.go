package worker

import (
	"context"
	"time"

	"ai-business-service/internal/biz/outbox"
)

// NewObservedOutboxRepository decorates one worker's fixed event type with
// bounded metrics. It never inspects or exports payloads, IDs, provider URLs,
// or raw errors, and it preserves every repository call unchanged.
func NewObservedOutboxRepository(repository outbox.Repository, observability *RuntimeObservability, eventType outbox.EventType) outbox.Repository {
	if repository == nil || observability == nil {
		return repository
	}
	return &observedOutboxRepository{repository: repository, observability: observability, eventType: eventType}
}

type observedOutboxRepository struct {
	repository    outbox.Repository
	observability *RuntimeObservability
	eventType     outbox.EventType
}

func (repository *observedOutboxRepository) Enqueue(ctx context.Context, event *outbox.Event) error {
	return repository.repository.Enqueue(ctx, event)
}

func (repository *observedOutboxRepository) Claim(ctx context.Context, workerID string, now, leaseUntil time.Time) (*outbox.Event, error) {
	event, err := repository.repository.Claim(ctx, workerID, now, leaseUntil)
	repository.observability.ObserveClaim(repository.eventType, event, err)
	return event, err
}

func (repository *observedOutboxRepository) ClaimByType(ctx context.Context, workerID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	event, err := repository.repository.ClaimByType(ctx, workerID, eventType, now, leaseUntil)
	repository.observability.ObserveClaim(repository.eventType, event, err)
	return event, err
}

func (repository *observedOutboxRepository) ClaimByIDAndType(ctx context.Context, workerID, eventID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	event, err := repository.repository.ClaimByIDAndType(ctx, workerID, eventID, eventType, now, leaseUntil)
	repository.observability.ObserveClaim(repository.eventType, event, err)
	return event, err
}

func (repository *observedOutboxRepository) RenewLease(ctx context.Context, eventID, leaseToken string, now, leaseUntil time.Time) error {
	return repository.repository.RenewLease(ctx, eventID, leaseToken, now, leaseUntil)
}

func (repository *observedOutboxRepository) Requeue(ctx context.Context, eventID, leaseToken string, nextAttemptAt time.Time) error {
	err := repository.repository.Requeue(ctx, eventID, leaseToken, nextAttemptAt)
	if err == nil {
		repository.observability.ObserveTransition(repository.eventType, "requeued", "")
	}
	return err
}

func (repository *observedOutboxRepository) MarkDelivered(ctx context.Context, eventID, leaseToken string, now time.Time) error {
	err := repository.repository.MarkDelivered(ctx, eventID, leaseToken, now)
	if err == nil {
		repository.observability.ObserveTransition(repository.eventType, "delivered", "")
	}
	return err
}

func (repository *observedOutboxRepository) MarkFailed(ctx context.Context, eventID, leaseToken string, now time.Time) error {
	err := repository.repository.MarkFailed(ctx, eventID, leaseToken, now)
	if err == nil {
		repository.observability.ObserveTransition(repository.eventType, "failed", "")
	}
	return err
}

func (repository *observedOutboxRepository) MarkNeedsAttention(ctx context.Context, eventID, leaseToken string, reason outbox.AttentionReason, now time.Time) error {
	err := repository.repository.MarkNeedsAttention(ctx, eventID, leaseToken, reason, now)
	if err == nil {
		repository.observability.ObserveTransition(repository.eventType, "needs_attention", reason)
	}
	return err
}

func (repository *observedOutboxRepository) RedriveAttention(ctx context.Context, command outbox.RedriveCommand) (*outbox.Event, error) {
	event, err := repository.repository.RedriveAttention(ctx, command)
	if err == nil {
		repository.observability.ObserveTransition(repository.eventType, "redriven", "")
	}
	return event, err
}

var _ outbox.Repository = (*observedOutboxRepository)(nil)
