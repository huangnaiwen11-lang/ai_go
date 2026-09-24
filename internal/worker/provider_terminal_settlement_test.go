package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
)

var terminalSettlementNow = time.Date(2026, time.September, 19, 17, 0, 0, 0, time.UTC)

func TestProviderTerminalSettlementWorker冲正后原子结算失败终态(t *testing.T) {
	target := terminalSettlementTarget(t, generation.ProviderTerminalFailed)
	payload, err := generation.MarshalProviderTerminalSettlementEventPayload(target.Payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{
		ID: generation.ProviderTerminalSettlementEventID(target.Payload.StepID), AggregateID: target.Payload.CreationID,
		EventType: outbox.EventType(generation.ProviderTerminalSettlementEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusPending, NextAttemptAt: terminalSettlementNow, CreatedAt: terminalSettlementNow, UpdatedAt: terminalSettlementNow,
	}
	queue := &materializerOutbox{event: event}
	store := &terminalSettlementStore{target: target}
	reverser := &terminalSettlementReverser{}
	worker := NewProviderTerminalSettlementWorker(queue, store, reverser, immediateTx{}, "terminal-settler", func() time.Time { return terminalSettlementNow })

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if reverser.creationID != target.Payload.CreationID || reverser.reason != ledger.ReversalReasonGenerationFailed || !reverser.at.Equal(terminalSettlementNow) {
		t.Fatalf("reverse command = %#v", reverser)
	}
	if len(store.settlements) != 1 {
		t.Fatalf("settlement count = %d, want 1", len(store.settlements))
	}
	settlement := store.settlements[0]
	if settlement.EventID != event.ID || settlement.LeaseToken != event.LeaseToken || settlement.Target != target || !settlement.SettledAt.Equal(terminalSettlementNow) {
		t.Fatalf("settlement = %#v", settlement)
	}
	if len(queue.delivered) != 0 || len(queue.failed) != 0 || len(queue.requeued) != 0 {
		t.Fatalf("store must own atomic event settlement: %#v", queue)
	}
}

func TestProviderTerminalSettlementWorker冲突进入失败队列且不冲正(t *testing.T) {
	target := terminalSettlementTarget(t, generation.ProviderTerminalCancelled)
	payload, err := generation.MarshalProviderTerminalSettlementEventPayload(target.Payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{
		ID: generation.ProviderTerminalSettlementEventID(target.Payload.StepID), AggregateID: target.Payload.CreationID,
		EventType: outbox.EventType(generation.ProviderTerminalSettlementEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusPending, NextAttemptAt: terminalSettlementNow, CreatedAt: terminalSettlementNow, UpdatedAt: terminalSettlementNow,
	}
	queue := &materializerOutbox{event: event}
	store := &terminalSettlementStore{target: target, loadErr: generation.ErrProviderTerminalSettlementConflict}
	reverser := &terminalSettlementReverser{}
	worker := NewProviderTerminalSettlementWorker(queue, store, reverser, immediateTx{}, "terminal-settler", func() time.Time { return terminalSettlementNow })

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if reverser.creationID != "" || len(store.settlements) != 0 || len(queue.failed) != 1 || len(queue.requeued) != 0 {
		t.Fatalf("conflicting terminal was retried or refunded: reverser=%#v settlements=%d queue=%#v", reverser, len(store.settlements), queue)
	}
}

func terminalSettlementTarget(t *testing.T, status generation.ProviderTerminalStatus) generation.ProviderTerminalSettlementTarget {
	t.Helper()
	payload := generation.ProviderTerminalSettlementEventPayload{
		CreationID: "creation-settlement-1", StepID: "step-settlement-1", Provider: "polarstar_b2b_v2", AccountRef: "account-main",
		JobID: "job-settlement-1", Capability: "text_to_image", Status: status, TerminalVersion: 1,
		TerminalDigest: generation.ProviderTerminalSummaryDigest(status, ""),
	}
	target := generation.ProviderTerminalSettlementTarget{Payload: payload}
	if err := target.Validate(); err != nil {
		t.Fatalf("invalid settlement target: %v", err)
	}
	return target
}

type terminalSettlementStore struct {
	target      generation.ProviderTerminalSettlementTarget
	loadErr     error
	settleErr   error
	settlements []generation.ProviderTerminalSettlement
}

func (store *terminalSettlementStore) LoadProviderTerminalSettlement(_ context.Context, payload generation.ProviderTerminalSettlementEventPayload) (generation.ProviderTerminalSettlementTarget, error) {
	if store.loadErr != nil {
		return generation.ProviderTerminalSettlementTarget{}, store.loadErr
	}
	if payload != store.target.Payload {
		return generation.ProviderTerminalSettlementTarget{}, generation.ErrProviderTerminalSettlementConflict
	}
	return store.target, nil
}

func (store *terminalSettlementStore) SettleProviderTerminal(_ context.Context, settlement generation.ProviderTerminalSettlement) error {
	if store.settleErr != nil {
		return store.settleErr
	}
	store.settlements = append(store.settlements, settlement)
	return nil
}

var _ generation.ProviderTerminalSettlementStore = (*terminalSettlementStore)(nil)

type terminalSettlementReverser struct {
	creationID string
	reason     ledger.ReversalReason
	at         time.Time
	err        error
}

func (reverser *terminalSettlementReverser) ReverseInTx(_ context.Context, creationID string, reason ledger.ReversalReason, at time.Time) (*ledger.Reservation, error) {
	reverser.creationID, reverser.reason, reverser.at = creationID, reason, at
	if reverser.err != nil {
		return nil, reverser.err
	}
	return &ledger.Reservation{CreationID: creationID, Status: ledger.ReservationStatusReversed}, nil
}

var _ providerTerminalSettlementReverser = (*terminalSettlementReverser)(nil)

func TestProviderTerminalSettlementPermanentFailure分类(t *testing.T) {
	if !providerTerminalSettlementPermanentFailure(generation.ErrProviderTerminalSettlementConflict) ||
		!providerTerminalSettlementPermanentFailure(generation.ErrInvalidProviderTerminalSettlement) ||
		providerTerminalSettlementPermanentFailure(errors.New("temporary mongo timeout")) {
		t.Fatal("terminal settlement failure classifier is unsafe")
	}
}

func TestProviderTerminalSettlementWorker依赖失败超过最大事件年龄转入人工关注(t *testing.T) {
	createdAt := terminalSettlementNow.Add(-outbox.MaxEventAge)
	target := terminalSettlementTarget(t, generation.ProviderTerminalFailed)
	payload, err := generation.MarshalProviderTerminalSettlementEventPayload(target.Payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{
		ID: generation.ProviderTerminalSettlementEventID(target.Payload.StepID), AggregateID: target.Payload.CreationID,
		EventType: outbox.EventType(generation.ProviderTerminalSettlementEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusPending, AttemptCount: 1,
		NextAttemptAt: createdAt, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	queue := &materializerOutbox{event: event}
	store := &terminalSettlementStore{target: target, loadErr: errors.New("temporary mongo timeout")}
	reverser := &terminalSettlementReverser{}
	worker := NewProviderTerminalSettlementWorker(queue, store, reverser, immediateTx{}, "terminal-settler", func() time.Time { return terminalSettlementNow })

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(queue.attended) != 1 || queue.attended[0] != outbox.AttentionReasonEventAge {
		t.Fatalf("attended = %v, want 事件超龄", queue.attended)
	}
	// 人工关注不是失败：不得冲正、不得继续重排。
	if reverser.creationID != "" || len(store.settlements) != 0 || len(queue.failed) != 0 || len(queue.requeued) != 0 {
		t.Fatalf("超龄依赖失败被误处理: reverser=%#v settlements=%d queue=%#v", reverser, len(store.settlements), queue)
	}
}

func TestProviderTerminalSettlementWorker转入人工关注后发出可采集告警(t *testing.T) {
	createdAt := terminalSettlementNow.Add(-outbox.MaxEventAge)
	target := terminalSettlementTarget(t, generation.ProviderTerminalFailed)
	payload, err := generation.MarshalProviderTerminalSettlementEventPayload(target.Payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{
		ID: generation.ProviderTerminalSettlementEventID(target.Payload.StepID), AggregateID: target.Payload.CreationID,
		EventType: outbox.EventType(generation.ProviderTerminalSettlementEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusPending, AttemptCount: 1,
		NextAttemptAt: createdAt, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	queue := &materializerOutbox{event: event}
	store := &terminalSettlementStore{target: target, loadErr: errors.New("temporary mongo timeout")}
	alerter := &recordingAttentionAlerter{}
	worker := NewProviderTerminalSettlementWorker(queue, store, &terminalSettlementReverser{}, immediateTx{}, "terminal-settler", func() time.Time { return terminalSettlementNow })
	worker.SetAttentionAlerter(alerter)

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(alerter.events) != 1 {
		t.Fatalf("alert count = %d, want exactly one signal per transition", len(alerter.events))
	}
	got := alerter.events[0]
	if got.EventID != event.ID || got.Reason != outbox.AttentionReasonEventAge || got.Age != outbox.MaxEventAge || !got.At.Equal(terminalSettlementNow) {
		t.Fatalf("attention = %#v", got)
	}
}

func TestProviderTerminalSettlementWorker年龄内依赖失败继续重排(t *testing.T) {
	target := terminalSettlementTarget(t, generation.ProviderTerminalFailed)
	payload, err := generation.MarshalProviderTerminalSettlementEventPayload(target.Payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{
		ID: generation.ProviderTerminalSettlementEventID(target.Payload.StepID), AggregateID: target.Payload.CreationID,
		EventType: outbox.EventType(generation.ProviderTerminalSettlementEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusPending, AttemptCount: 1,
		NextAttemptAt: terminalSettlementNow, CreatedAt: terminalSettlementNow.Add(-time.Minute), UpdatedAt: terminalSettlementNow,
	}
	queue := &materializerOutbox{event: event}
	store := &terminalSettlementStore{target: target, loadErr: errors.New("temporary mongo timeout")}
	worker := NewProviderTerminalSettlementWorker(queue, store, &terminalSettlementReverser{}, immediateTx{}, "terminal-settler", func() time.Time { return terminalSettlementNow })

	if err := worker.DeliverOnce(context.Background(), event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(queue.requeued) != 1 || len(queue.attended) != 0 || len(queue.failed) != 0 {
		t.Fatalf("年龄内依赖失败应继续重排: requeued=%v attended=%v failed=%v", queue.requeued, queue.attended, queue.failed)
	}
}
