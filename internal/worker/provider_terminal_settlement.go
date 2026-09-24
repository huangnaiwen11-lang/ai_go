package worker

import (
	"context"
	"errors"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
)

const defaultProviderTerminalSettlementWorkerID = "provider-terminal-settlement-worker"

var ErrProviderTerminalSettlementDependenciesUnavailable = errors.New("provider terminal settlement worker dependencies are unavailable")

// providerTerminalSettlementReverser is deliberately smaller than ledger's
// full usecase. The worker only needs the already-transactional state change;
// it cannot reserve, confiscate or query balances.
type providerTerminalSettlementReverser interface {
	ReverseInTx(context.Context, string, ledger.ReversalReason, time.Time) (*ledger.Reservation, error)
}

// ProviderTerminalSettlementWorker turns a persisted failed/cancelled B2B
// terminal into one deterministic reservation reversal. It owns no provider
// credentials and makes no HTTP calls; callbacks and reconciliation merely
// create its durable event, so their acknowledgement latency is isolated from
// ledger storage work.
type ProviderTerminalSettlementWorker struct {
	outbox    outbox.Repository
	store     generation.ProviderTerminalSettlementStore
	ledger    providerTerminalSettlementReverser
	tx        shared.TxRunner
	workerID  string
	now       func() time.Time
	attention AttentionAlerter
}

// SetAttentionAlerter installs the operator signal for events this worker gives
// up on. It must be called before the first DeliverOnce; leaving it unset keeps
// the worker silent, which is the pre-existing behaviour and stays valid in
// tests that only exercise the settlement state machine.
func (worker *ProviderTerminalSettlementWorker) SetAttentionAlerter(alerter AttentionAlerter) {
	if worker != nil {
		worker.attention = alerter
	}
}

func NewProviderTerminalSettlementWorker(
	outboxRepository outbox.Repository,
	store generation.ProviderTerminalSettlementStore,
	ledgerUsecase providerTerminalSettlementReverser,
	tx shared.TxRunner,
	workerID string,
	now func() time.Time,
) *ProviderTerminalSettlementWorker {
	if now == nil {
		now = time.Now
	}
	return &ProviderTerminalSettlementWorker{
		outbox: outboxRepository, store: store, ledger: ledgerUsecase, tx: tx, workerID: workerID, now: now,
	}
}

// NewDefaultProviderTerminalSettlementWorker is the explicit worker-process
// composition helper. It is intentionally not in a Wire provider set: HTTP
// processes must never drain billing events.
func NewDefaultProviderTerminalSettlementWorker(
	outboxRepository outbox.Repository,
	store generation.ProviderTerminalSettlementStore,
	ledgerUsecase *ledger.Usecase,
	tx shared.TxRunner,
) *ProviderTerminalSettlementWorker {
	return NewProviderTerminalSettlementWorker(outboxRepository, store, ledgerUsecase, tx, defaultProviderTerminalSettlementWorkerID, time.Now)
}

// DeliverOnce claims at most one failure-settlement event. A successful store
// call settles the event itself in the same transaction as the ledger and
// creation state; this worker therefore must not issue MarkDelivered after a
// successful transaction.
func (worker *ProviderTerminalSettlementWorker) DeliverOnce(ctx context.Context, expectedEventID string) error {
	if err := worker.ready(); err != nil {
		return err
	}
	now := worker.nowUTC()
	eventType := outbox.EventType(generation.ProviderTerminalSettlementEventType)
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
	payload, err := generation.ParseProviderTerminalSettlementEventPayload(event.Payload)
	if err != nil || event.ID != generation.ProviderTerminalSettlementEventID(payload.StepID) || event.AggregateID != payload.CreationID {
		return worker.abandon(ctx, event)
	}
	target, err := worker.store.LoadProviderTerminalSettlement(ctx, payload)
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	if target.Payload != payload || target.Validate() != nil {
		return worker.abandon(ctx, event)
	}
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	err = worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		if _, err := worker.ledger.ReverseInTx(txCtx, payload.CreationID, ledger.ReversalReasonGenerationFailed, now); err != nil {
			return err
		}
		return worker.store.SettleProviderTerminal(txCtx, generation.ProviderTerminalSettlement{
			EventID: event.ID, LeaseToken: event.LeaseToken, Target: target, SettledAt: now,
		})
	})
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	return nil
}

// retryOrAbandon mirrors the result materializer's three-way shape on purpose:
// the two workers drive the same outbox state machine, so a change to terminate
// conditions here must be made there too, otherwise only half the failure paths
// have a bound.
//
// Every failure in this worker is a dependency failure (reading the durable
// terminal, writing the ledger and settlement transaction). None of them
// consume the material-upload budget, so the only terminal condition is the
// maximum event age. Without it this worker would re-queue a permanently
// broken reversal forever.
func (worker *ProviderTerminalSettlementWorker) retryOrAbandon(ctx context.Context, event *outbox.Event, cause error) error {
	if providerTerminalSettlementPermanentFailure(cause) {
		return worker.abandon(ctx, event)
	}
	now := worker.nowUTC()
	// 年龄按「当前自动重试窗口」计算：人工重驱会开启新窗口，否则超过 24 小时
	// 的事件在重驱后第一次失败就会立刻再次终态。本 worker 没有预算判定。
	startedAt, _ := event.AttentionWindow()
	if outbox.EventAgeExceeded(startedAt, now) {
		if err := settleOutboxError(worker.outbox.MarkNeedsAttention(ctx, event.ID, event.LeaseToken, outbox.AttentionReasonEventAge, now)); err != nil {
			return err
		}
		// The signal is emitted only after the transition is durable, so an
		// operator investigating an alert always finds the event in
		// needs_attention.
		alertAttention(worker.attention, ctx, event, outbox.AttentionReasonEventAge, now)
		return nil
	}
	return worker.requeue(ctx, event, cause)
}

func providerTerminalSettlementPermanentFailure(cause error) bool {
	return errors.Is(cause, generation.ErrInvalidProviderTerminalSettlement) ||
		errors.Is(cause, generation.ErrProviderTerminalSettlementConflict) ||
		errors.Is(cause, ledger.ErrInvalidReservationCommand) ||
		errors.Is(cause, ledger.ErrReservationNotFound) ||
		errors.Is(cause, ledger.ErrReservationStateConflict) ||
		errors.Is(cause, ledger.ErrLedgerDependenciesUnavailable)
}

func (worker *ProviderTerminalSettlementWorker) abandon(ctx context.Context, event *outbox.Event) error {
	return settleOutboxError(worker.outbox.MarkFailed(ctx, event.ID, event.LeaseToken, worker.nowUTC()))
}

func (worker *ProviderTerminalSettlementWorker) requeue(ctx context.Context, event *outbox.Event, cause error) error {
	now := worker.nowUTC()
	return settleOutboxError(worker.outbox.Requeue(ctx, event.ID, event.LeaseToken, nextSubmissionAttempt(now, event.AttemptCount, cause)))
}

func (worker *ProviderTerminalSettlementWorker) ready() error {
	if worker == nil || worker.outbox == nil || worker.store == nil || worker.ledger == nil || worker.tx == nil || worker.workerID == "" {
		return ErrProviderTerminalSettlementDependenciesUnavailable
	}
	return nil
}

func (worker *ProviderTerminalSettlementWorker) persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), submissionPersistenceTimeout)
}

func (worker *ProviderTerminalSettlementWorker) nowUTC() time.Time {
	if worker.now == nil {
		return time.Now().UTC()
	}
	return worker.now().UTC()
}

var _ onceDeliverer = (*ProviderTerminalSettlementWorker)(nil)
