package outbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

type repositoryContract struct{}

func (repositoryContract) Enqueue(context.Context, *Event) error { return nil }

func (repositoryContract) Claim(context.Context, string, time.Time, time.Time) (*Event, error) {
	return nil, nil
}

func (repositoryContract) ClaimByType(context.Context, string, EventType, time.Time, time.Time) (*Event, error) {
	return nil, nil
}

func (repositoryContract) ClaimByIDAndType(context.Context, string, string, EventType, time.Time, time.Time) (*Event, error) {
	return nil, nil
}

func (repositoryContract) Requeue(context.Context, string, string, time.Time) error { return nil }

func (repositoryContract) MarkDelivered(context.Context, string, string, time.Time) error { return nil }

func (repositoryContract) MarkFailed(context.Context, string, string, time.Time) error { return nil }

var _ Repository = repositoryContract{}

func TestSubmissionEventID为每个步骤生成稳定且隔离的事件ID(t *testing.T) {
	if got, want := SubmissionEventID("step-1"), "generation.submission:step-1"; got != want {
		t.Fatalf("SubmissionEventID() = %q, want %q", got, want)
	}
	if first, second := SubmissionEventID("step-1"), SubmissionEventID("step-2"); first == second {
		t.Fatalf("不同步骤的事件 ID 相同: %q", first)
	}
}

func TestEventTransition仅允许已领取事件结案或重新入队(t *testing.T) {
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event, err := NewPending(SubmissionEventID("step-1"), "creation-1", []byte(`{"step_id":"step-1"}`), now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	if event.DeliveryStatus != DeliveryStatusPending || event.AttemptCount != 0 || !event.NextAttemptAt.Equal(now) {
		t.Fatalf("NewPending() event = %#v, want pending with zero attempts", event)
	}
	if err := event.MarkDelivered(now.Add(time.Second)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pending MarkDelivered() error = %v, want ErrInvalidTransition", err)
	}

	leaseUntil := now.Add(time.Minute)
	if err := event.MarkDispatching("lease-token", "worker-1", leaseUntil, now.Add(time.Second)); err != nil {
		t.Fatalf("MarkDispatching() error = %v", err)
	}
	if err := event.MarkDelivered(now.Add(2 * time.Second)); err != nil {
		t.Fatalf("dispatching MarkDelivered() error = %v", err)
	}
	if event.DeliveryStatus != DeliveryStatusDelivered || event.LeaseToken != "" || !event.LeaseUntil.IsZero() {
		t.Fatalf("MarkDelivered() event = %#v, want delivered without lease", event)
	}
}

func TestNewPending复制载荷并拒绝非法领域输入(t *testing.T) {
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{"key":"original"}`)
	event, err := NewPending(SubmissionEventID("step-1"), "creation-1", payload, now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	payload[0] = '!'
	if string(event.Payload) != `{"key":"original"}` {
		t.Fatalf("Payload = %q, want independent copy", event.Payload)
	}

	for _, input := range []struct {
		name        string
		id          string
		aggregateID string
		payload     []byte
		now         time.Time
	}{
		{name: "空事件 ID", aggregateID: "creation-1", payload: []byte(`{}`), now: now},
		{name: "空聚合 ID", id: SubmissionEventID("step-1"), payload: []byte(`{}`), now: now},
		{name: "空载荷", id: SubmissionEventID("step-1"), aggregateID: "creation-1", now: now},
		{name: "零时间", id: SubmissionEventID("step-1"), aggregateID: "creation-1", payload: []byte(`{}`)},
	} {
		t.Run(input.name, func(t *testing.T) {
			_, err := NewPending(input.id, input.aggregateID, input.payload, input.now)
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("NewPending() error = %v, want ErrInvalidEvent", err)
			}
			if err != nil && string(input.payload) != "" && string(input.payload) == err.Error() {
				t.Fatalf("领域错误泄露载荷: %q", err)
			}
		})
	}
}

func TestNewPending拒绝非步骤派生的提交事件ID(t *testing.T) {
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	for _, eventID := range []string{
		"random-event-id",
		"generation.submission:",
		"generation.submission:step with whitespace",
	} {
		_, err := NewPending(eventID, "creation-1", []byte(`{"prompt":"x"}`), now)
		if !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("NewPending(%q) error = %v, want ErrInvalidEvent", eventID, err)
		}
	}
}

func TestEventRequeue只写固定安全摘要(t *testing.T) {
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event, err := NewPending(SubmissionEventID("step-1"), "creation-1", []byte(`{"prompt":"x"}`), now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	if err := event.MarkDispatching("lease-token", "worker-1", now.Add(time.Minute), now); err != nil {
		t.Fatalf("MarkDispatching() error = %v", err)
	}
	retryAt := now.Add(2 * time.Minute)
	if err := event.Requeue(retryAt); err != nil {
		t.Fatalf("Requeue() error = %v", err)
	}
	if event.LastError != "outbox: retry_scheduled" {
		t.Fatalf("Requeue() LastError = %q, want fixed safe summary", event.LastError)
	}
}
