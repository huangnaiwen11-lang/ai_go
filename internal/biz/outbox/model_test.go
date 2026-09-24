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

func (repositoryContract) RenewLease(context.Context, string, string, time.Time, time.Time) error {
	return nil
}

func (repositoryContract) Requeue(context.Context, string, string, time.Time) error { return nil }

func (repositoryContract) MarkDelivered(context.Context, string, string, time.Time) error { return nil }

func (repositoryContract) MarkFailed(context.Context, string, string, time.Time) error { return nil }

func (repositoryContract) MarkNeedsAttention(context.Context, string, string, AttentionReason, time.Time) error {
	return nil
}

// RedriveAttention 在本文件里只用于满足 Repository 接口；重驱语义由
// internal/data 的 Mongo 实现与 outbox 的重驱用例测试覆盖。
func (repositoryContract) RedriveAttention(context.Context, RedriveCommand) (*Event, error) {
	return nil, ErrRedriveNotEligible
}

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

func TestEventMarkNeedsAttention终结事件并保留关注原因(t *testing.T) {
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event, err := NewPending(SubmissionEventID("step-1"), "creation-1", []byte(`{"prompt":"x"}`), now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	if err := event.MarkDispatching("lease-token", "worker-1", now.Add(time.Minute), now); err != nil {
		t.Fatalf("MarkDispatching() error = %v", err)
	}
	if err := event.Requeue(now.Add(time.Minute)); err != nil {
		t.Fatalf("Requeue() error = %v", err)
	}
	if err := event.MarkDispatching("lease-token-2", "worker-1", now.Add(2*time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatalf("second MarkDispatching() error = %v", err)
	}

	attentionAt := now.Add(3 * time.Minute)
	if err := event.MarkNeedsAttention(AttentionReasonMaterialUploadBudget, attentionAt); err != nil {
		t.Fatalf("MarkNeedsAttention() error = %v", err)
	}
	if event.DeliveryStatus != DeliveryStatusNeedsAttention || event.AttentionReason != AttentionReasonMaterialUploadBudget {
		t.Fatalf("MarkNeedsAttention() event = %#v", event)
	}
	if event.LeaseToken != "" || event.LeaseOwner != "" || !event.LeaseUntil.IsZero() {
		t.Fatalf("MarkNeedsAttention() 未清除租约: %#v", event)
	}
	if !event.UpdatedAt.Equal(attentionAt) {
		t.Fatalf("UpdatedAt = %v, want %v", event.UpdatedAt, attentionAt)
	}
	// 关注原因必须与 LastError 分开：Requeue 每轮都会覆盖 LastError，
	// 把原因塞进去会让它在重新入队之后消失。
	if event.LastError != "outbox: retry_scheduled" {
		t.Fatalf("MarkNeedsAttention() 改写了 LastError = %q", event.LastError)
	}
	if event.DeliveryStatus == DeliveryStatusFailed {
		t.Fatal("needs_attention 不得被实现为 failed")
	}
}

func TestEventMarkNeedsAttention拒绝非法状态与非法原因(t *testing.T) {
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event, err := NewPending(SubmissionEventID("step-1"), "creation-1", []byte(`{"prompt":"x"}`), now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	if err := event.MarkNeedsAttention(AttentionReasonEventAge, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pending MarkNeedsAttention() error = %v, want ErrInvalidTransition", err)
	}
	if err := event.MarkDispatching("lease-token", "worker-1", now.Add(time.Minute), now); err != nil {
		t.Fatalf("MarkDispatching() error = %v", err)
	}
	for _, reason := range []AttentionReason{"", "material upload failed: https://provider.example/x.png", "made-up"} {
		if err := event.MarkNeedsAttention(reason, now.Add(time.Second)); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("MarkNeedsAttention(%q) error = %v, want ErrInvalidEvent", reason, err)
		}
	}
	if err := event.MarkNeedsAttention(AttentionReasonEventAge, time.Time{}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("zero-time MarkNeedsAttention() error = %v, want ErrInvalidEvent", err)
	}
	if event.DeliveryStatus != DeliveryStatusDispatching || event.AttentionReason != "" {
		t.Fatalf("被拒的迁移改动了事件: %#v", event)
	}
}

func TestNewPending拒绝预置关注原因(t *testing.T) {
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event, err := NewPending(SubmissionEventID("step-1"), "creation-1", []byte(`{"prompt":"x"}`), now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	event.AttentionReason = AttentionReasonEventAge
	if err := ValidatePending(event); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("ValidatePending() error = %v, want ErrInvalidEvent", err)
	}
}

func TestRetryBudgetExhausted按已领取次数判定(t *testing.T) {
	for _, tc := range []struct {
		attempt int32
		budget  int32
		want    bool
	}{
		{attempt: 0, budget: 5, want: false},
		{attempt: 4, budget: 5, want: false},
		{attempt: 5, budget: 5, want: true},
		{attempt: 6, budget: 5, want: true},
		{attempt: 9, budget: 0, want: false},
	} {
		if got := RetryBudgetExhausted(tc.attempt, tc.budget); got != tc.want {
			t.Fatalf("RetryBudgetExhausted(%d, %d) = %v, want %v", tc.attempt, tc.budget, got, tc.want)
		}
	}
}

func TestEventAgeExceeded按创建时间判定(t *testing.T) {
	created := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "刚创建", now: created, want: false},
		{name: "差一秒到上限", now: created.Add(MaxEventAge - time.Second), want: false},
		{name: "正好到上限", now: created.Add(MaxEventAge), want: true},
		{name: "超过上限", now: created.Add(MaxEventAge + time.Second), want: true},
		{name: "零时间不误判", now: time.Time{}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EventAgeExceeded(created, tc.now); got != tc.want {
				t.Fatalf("EventAgeExceeded(%v, %v) = %v, want %v", created, tc.now, got, tc.want)
			}
		})
	}
	if EventAgeExceeded(time.Time{}, created) {
		t.Fatal("零创建时间不得被判为超龄")
	}
}
