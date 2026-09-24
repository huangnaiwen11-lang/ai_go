package worker

import (
	"context"
	"errors"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

const defaultProviderCancelWorkerID = "provider-cancel-worker"

var ErrProviderCancelWorkerDependenciesUnavailable = errors.New("provider cancel worker dependencies are unavailable")

// providerCancelRouteResolver resolves the exact provider/account frozen in
// the cancellation outbox payload. It deliberately has no new-task selector:
// changing a rollout configuration must never redirect an accepted cancel
// request to another account.
type providerCancelRouteResolver interface {
	ProviderForRoute(provider, accountRef string) (platform.ProviderHandle, error)
}

// ProviderCancelWorker delivers a durable B2B cancellation request. A provider
// acknowledgement only closes this outbox item; it never writes a terminal,
// publishes a result, or touches the reservation. Those facts remain owned by
// the callback/reconciliation paths.
type ProviderCancelWorker struct {
	outbox    outbox.Repository
	providers providerCancelRouteResolver
	workerID  string
	now       func() time.Time
}

// NewProviderCancelWorker constructs one cancellable outbox delivery unit. It
// performs no I/O until DeliverOnce is invoked by the explicit worker process.
func NewProviderCancelWorker(
	outboxRepository outbox.Repository,
	providers providerCancelRouteResolver,
	workerID string,
	now func() time.Time,
) *ProviderCancelWorker {
	if now == nil {
		now = time.Now
	}
	return &ProviderCancelWorker{outbox: outboxRepository, providers: providers, workerID: workerID, now: now}
}

// NewDefaultProviderCancelWorker provides the only production composition for
// cancellation delivery. It is intentionally not a Wire provider: HTTP
// processes may accept cancellation requests but must never drain their
// provider-facing outbox.
func NewDefaultProviderCancelWorker(outboxRepository outbox.Repository, registry *platform.ProviderRegistry) *ProviderCancelWorker {
	return NewProviderCancelWorker(outboxRepository, registry, defaultProviderCancelWorkerID, time.Now)
}

// DeliverOnce delivers at most one cancellation event. Successful delivery is
// only an acknowledgement of the provider Cancel request; provider callback or
// lookup remains the canonical terminal authority.
func (worker *ProviderCancelWorker) DeliverOnce(ctx context.Context, expectedEventID string) error {
	if err := worker.ready(); err != nil {
		return err
	}
	now := worker.nowUTC()
	eventType := outbox.EventType(generation.ProviderCancelEventType)
	var event *outbox.Event
	var err error
	if expectedEventID == "" {
		event, err = worker.outbox.ClaimByType(ctx, worker.workerID, eventType, now, now.Add(defaultLeaseDuration))
	} else {
		event, err = worker.outbox.ClaimByIDAndType(ctx, worker.workerID, expectedEventID, eventType, now, now.Add(defaultLeaseDuration))
	}
	if err != nil || event == nil {
		return err
	}
	if event.EventType != eventType || (expectedEventID != "" && event.ID != expectedEventID) {
		return ErrUnexpectedClaimedEvent
	}

	payload, err := generation.ParseProviderCancelEventPayload(event.Payload)
	if err != nil || event.ID != generation.ProviderCancelEventID(payload.StepID) || event.AggregateID != payload.CreationID {
		return worker.abandon(ctx, event)
	}
	handle, err := worker.providers.ProviderForRoute(payload.Provider, payload.AccountRef)
	if err != nil || !handle.IsB2B() || handle.B2B == nil || handle.AccountRef != payload.AccountRef {
		// A malformed/frozen route must never be repaired by guessing another
		// account. It is a durable integrity failure, not a reason to POST.
		return worker.abandon(ctx, event)
	}
	receipt, err := handle.B2B.Cancel(ctx, payload.JobID)
	if err != nil {
		if errors.Is(err, polarstarb2b.ErrNotSent) {
			// The adapter proved this request was not sent (for example local
			// validation/context failure). Requeueing cannot make frozen bytes
			// valid and would hide an integrity fault indefinitely.
			return worker.abandon(ctx, event)
		}
		// Every remote response failure remains outcome-unknown from this
		// worker's perspective. In particular, do not turn a 4xx/5xx into a
		// local terminal or refund; the callback/reconciliation record is the
		// sole authority. Requeue with the shared bounded backoff instead.
		return worker.requeue(ctx, event, err)
	}
	if receipt.JobID != payload.JobID || !receipt.RequestAcknowledged {
		return worker.abandon(ctx, event)
	}
	return worker.deliver(ctx, event)
}

func (worker *ProviderCancelWorker) deliver(ctx context.Context, event *outbox.Event) error {
	return settleOutboxError(worker.outbox.MarkDelivered(ctx, event.ID, event.LeaseToken, worker.nowUTC()))
}

func (worker *ProviderCancelWorker) abandon(ctx context.Context, event *outbox.Event) error {
	return settleOutboxError(worker.outbox.MarkFailed(ctx, event.ID, event.LeaseToken, worker.nowUTC()))
}

func (worker *ProviderCancelWorker) requeue(ctx context.Context, event *outbox.Event, cause error) error {
	now := worker.nowUTC()
	return settleOutboxError(worker.outbox.Requeue(ctx, event.ID, event.LeaseToken, nextSubmissionAttempt(now, event.AttemptCount, cause)))
}

func (worker *ProviderCancelWorker) ready() error {
	if worker == nil || worker.outbox == nil || worker.providers == nil || worker.workerID == "" {
		return ErrProviderCancelWorkerDependenciesUnavailable
	}
	return nil
}

func (worker *ProviderCancelWorker) nowUTC() time.Time {
	if worker.now == nil {
		return time.Now().UTC()
	}
	return worker.now().UTC()
}

var _ onceDeliverer = (*ProviderCancelWorker)(nil)
