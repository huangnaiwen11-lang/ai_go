package worker

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
)

// providerCancelOutbox reuses the battle-tested lease simulator from
// generation_submission_test but records the only state changes a cancellation
// worker is allowed to make: delivered, failed, or requeued. It deliberately
// has no terminal or ledger capability.
type providerCancelOutbox struct {
	*memoryOutboxRepository
	delivered []string
	requeued  []time.Time
}

func (queue *providerCancelOutbox) MarkDelivered(_ context.Context, eventID, leaseToken string, _ time.Time) error {
	if queue.event == nil || eventID != queue.event.ID || leaseToken != queue.event.LeaseToken {
		return outbox.ErrLeaseConflict
	}
	queue.delivered = append(queue.delivered, eventID)
	queue.event.DeliveryStatus = outbox.DeliveryStatusDelivered
	queue.event.LeaseToken = ""
	return nil
}

func (queue *providerCancelOutbox) Requeue(_ context.Context, eventID, leaseToken string, next time.Time) error {
	if queue.event == nil || eventID != queue.event.ID || leaseToken != queue.event.LeaseToken {
		return outbox.ErrLeaseConflict
	}
	queue.requeued = append(queue.requeued, next)
	queue.event.DeliveryStatus = outbox.DeliveryStatusPending
	queue.event.NextAttemptAt = next
	queue.event.LeaseToken = ""
	return nil
}

func TestProviderCancelWorker确认取消请求后只结案Outbox(t *testing.T) {
	calls := 0
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/jobs/job-cancel-1/cancel" {
			t.Fatalf("cancel request = %s %s", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(b2bWorkerJobBody("job-cancel-1", "cancelled")))
	})
	queue := providerCancelQueue(t, fixture, generation.ProviderCancelEventPayload{JobID: "job-cancel-1"})
	worker := NewProviderCancelWorker(queue, fixture.router, "provider-cancel-test", func() time.Time { return fixture.now })

	if err := worker.DeliverOnce(context.Background(), queue.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if calls != 1 || len(queue.delivered) != 1 || len(queue.requeued) != 0 || queue.failedCalls != 0 {
		t.Fatalf("cancel outcome calls/delivered/requeued/failed = %d/%v/%v/%d, want 1/1/0/0", calls, queue.delivered, queue.requeued, queue.failedCalls)
	}
	if queue.event.DeliveryStatus != outbox.DeliveryStatusDelivered {
		t.Fatalf("cancel event status = %q, want delivered", queue.event.DeliveryStatus)
	}
}

func TestProviderCancelWorker非法载荷不出站并失败结案(t *testing.T) {
	calls := 0
	fixture := newB2BWorkerFixture(t, func(http.ResponseWriter, *http.Request) { calls++ })
	queue := providerCancelQueue(t, fixture, generation.ProviderCancelEventPayload{JobID: "job-cancel-1"})
	queue.event.Payload = []byte(`{"creationId":"creation-b2b-worker-test"}`)
	worker := NewProviderCancelWorker(queue, fixture.router, "provider-cancel-test", func() time.Time { return fixture.now })

	if err := worker.DeliverOnce(context.Background(), queue.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if calls != 0 || len(queue.delivered) != 0 || len(queue.requeued) != 0 || queue.failedCalls != 1 {
		t.Fatalf("malformed cancel outcome calls/delivered/requeued/failed = %d/%v/%v/%d, want 0/0/0/1", calls, queue.delivered, queue.requeued, queue.failedCalls)
	}
}

func TestProviderCancelWorker路由不匹配不出站并失败结案(t *testing.T) {
	calls := 0
	fixture := newB2BWorkerFixture(t, func(http.ResponseWriter, *http.Request) { calls++ })
	queue := providerCancelQueue(t, fixture, generation.ProviderCancelEventPayload{AccountRef: "another-account", JobID: "job-cancel-1"})
	worker := NewProviderCancelWorker(queue, fixture.router, "provider-cancel-test", func() time.Time { return fixture.now })

	if err := worker.DeliverOnce(context.Background(), queue.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if calls != 0 || len(queue.delivered) != 0 || len(queue.requeued) != 0 || queue.failedCalls != 1 {
		t.Fatalf("route mismatch cancel outcome calls/delivered/requeued/failed = %d/%v/%v/%d, want 0/0/0/1", calls, queue.delivered, queue.requeued, queue.failedCalls)
	}
}

func TestProviderCancelWorker未知结果按退避重排(t *testing.T) {
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/jobs/job-cancel-1/cancel" {
			t.Fatalf("cancel request = %s %s", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"success":false,"code":"SERVICE_UNAVAILABLE"}`))
	})
	queue := providerCancelQueue(t, fixture, generation.ProviderCancelEventPayload{JobID: "job-cancel-1"})
	worker := NewProviderCancelWorker(queue, fixture.router, "provider-cancel-test", func() time.Time { return fixture.now })

	if err := worker.DeliverOnce(context.Background(), queue.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(queue.delivered) != 0 || len(queue.requeued) != 1 || queue.failedCalls != 0 {
		t.Fatalf("unknown cancel outcome delivered/requeued/failed = %v/%v/%d, want 0/1/0", queue.delivered, queue.requeued, queue.failedCalls)
	}
	if !queue.requeued[0].After(fixture.now) {
		t.Fatalf("requeue time = %s, want after %s", queue.requeued[0], fixture.now)
	}
}

func TestProviderCancelWorker缺少依赖FailClosed(t *testing.T) {
	worker := NewProviderCancelWorker(nil, nil, "provider-cancel-test", time.Now)
	if err := worker.DeliverOnce(context.Background(), ""); !errors.Is(err, ErrProviderCancelWorkerDependenciesUnavailable) {
		t.Fatalf("DeliverOnce() error = %v, want ErrProviderCancelWorkerDependenciesUnavailable", err)
	}
}

func providerCancelQueue(t *testing.T, fixture *b2bWorkerFixture, partial generation.ProviderCancelEventPayload) *providerCancelOutbox {
	t.Helper()
	payload := generation.ProviderCancelEventPayload{
		CreationID: fixture.event.AggregateID,
		StepID:     b2bWorkerStepID,
		Provider:   b2bWorkerRoute().Provider,
		AccountRef: b2bWorkerAccount,
		JobID:      "job-cancel-default",
		Capability: "text_to_image",
	}
	if partial.AccountRef != "" {
		payload.AccountRef = partial.AccountRef
	}
	if partial.JobID != "" {
		payload.JobID = partial.JobID
	}
	raw, err := generation.MarshalProviderCancelEventPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	event := &outbox.Event{
		ID: generation.ProviderCancelEventID(payload.StepID), AggregateID: payload.CreationID,
		EventType: outbox.EventType(generation.ProviderCancelEventType), Payload: raw,
		DeliveryStatus: outbox.DeliveryStatusPending, NextAttemptAt: fixture.now, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}
	return &providerCancelOutbox{memoryOutboxRepository: &memoryOutboxRepository{event: event, recorder: &operationRecorder{}}}
}

var _ outbox.Repository = (*providerCancelOutbox)(nil)
