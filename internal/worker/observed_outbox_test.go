package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/outbox"
)

func TestObservedOutboxRepositoryExportsBoundedClaimAndAttentionMetrics(t *testing.T) {
	observability, err := NewRuntimeObservability("127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRuntimeObservability() error = %v", err)
	}
	delegate := &observedOutboxStub{
		claimed: &outbox.Event{
			ID:             "event-with-sensitive-id",
			EventType:      outbox.EventType("generation.result-materialize"),
			DeliveryStatus: outbox.DeliveryStatusReconciling,
		},
	}
	repository := NewObservedOutboxRepository(delegate, observability, outbox.EventType("generation.result-materialize"))
	if _, err := repository.ClaimByType(context.Background(), "worker", outbox.EventType("generation.result-materialize"), time.Now(), time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("ClaimByType() error = %v", err)
	}
	if err := repository.MarkNeedsAttention(context.Background(), "event-with-sensitive-id", "lease-with-sensitive-token", outbox.AttentionReasonEventAge, time.Now()); err != nil {
		t.Fatalf("MarkNeedsAttention() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, runtimeMetricsPath, nil)
	response := httptest.NewRecorder()
	observability.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", response.Code)
	}
	metrics := response.Body.String()
	for _, expected := range []string{
		`event_type="generation.result-materialize",result="claimed"`,
		`event_type="generation.result-materialize"} 1`,
		`attention_reason="provider_result_event_age_exceeded",event_type="generation.result-materialize",outcome="needs_attention"`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Errorf("metrics does not contain %q:\n%s", expected, metrics)
		}
	}
	for _, forbidden := range []string{"event-with-sensitive-id", "lease-with-sensitive-token"} {
		if strings.Contains(metrics, forbidden) {
			t.Errorf("metrics leaked %q:\n%s", forbidden, metrics)
		}
	}
}

type observedOutboxStub struct {
	claimed *outbox.Event
}

func (stub *observedOutboxStub) Enqueue(context.Context, *outbox.Event) error { return nil }

func (stub *observedOutboxStub) Claim(context.Context, string, time.Time, time.Time) (*outbox.Event, error) {
	return stub.claimed, nil
}

func (stub *observedOutboxStub) ClaimByType(context.Context, string, outbox.EventType, time.Time, time.Time) (*outbox.Event, error) {
	return stub.claimed, nil
}

func (stub *observedOutboxStub) ClaimByIDAndType(context.Context, string, string, outbox.EventType, time.Time, time.Time) (*outbox.Event, error) {
	return stub.claimed, nil
}

func (stub *observedOutboxStub) RenewLease(context.Context, string, string, time.Time, time.Time) error {
	return nil
}

func (stub *observedOutboxStub) Requeue(context.Context, string, string, time.Time) error { return nil }

func (stub *observedOutboxStub) MarkDelivered(context.Context, string, string, time.Time) error {
	return nil
}

func (stub *observedOutboxStub) MarkFailed(context.Context, string, string, time.Time) error {
	return nil
}

func (stub *observedOutboxStub) MarkNeedsAttention(context.Context, string, string, outbox.AttentionReason, time.Time) error {
	return nil
}

func (stub *observedOutboxStub) RedriveAttention(context.Context, outbox.RedriveCommand) (*outbox.Event, error) {
	return nil, errors.New("not implemented")
}

var _ outbox.Repository = (*observedOutboxStub)(nil)
