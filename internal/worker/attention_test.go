package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/outbox"
)

// recordingAttentionAlerter is the shared test double for the operator signal.
// The worker package is single-threaded per event, so it needs no lock.
type recordingAttentionAlerter struct{ events []AttentionEvent }

func (recorder *recordingAttentionAlerter) AttentionRequired(_ context.Context, attention AttentionEvent) {
	recorder.events = append(recorder.events, attention)
}

func TestLogAttentionAlerter输出可采集的单行结构化告警且不泄露定位符(t *testing.T) {
	var buffer bytes.Buffer
	alerter := NewLogAttentionAlerter(slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if alerter == nil {
		t.Fatal("NewLogAttentionAlerter() = nil for a non-nil logger")
	}
	at := time.Date(2026, 9, 20, 6, 0, 0, 0, time.UTC)
	alerter.AttentionRequired(context.Background(), AttentionEvent{
		EventID: "generation.provider_result.materialize:step-1", AggregateID: "creation-1",
		EventType: "generation.provider_result.materialize", Reason: outbox.AttentionReasonMaterialUploadBudget,
		Attempt: outbox.MaterialUploadRetryBudget, Age: 42 * time.Minute, At: at,
	})

	line := strings.TrimSpace(buffer.String())
	if strings.Contains(line, "\n") {
		t.Fatalf("alert must be exactly one log record, got %q", buffer.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("alert is not one structured record: %v (%q)", err, line)
	}
	// 外部采集按固定标记匹配，不解析自然语言。
	if got := decoded["alert"]; got != AttentionAlert {
		t.Fatalf("alert marker = %#v, want %q", got, AttentionAlert)
	}
	if got := decoded["level"]; got != "WARN" {
		t.Fatalf("alert level = %#v, want WARN", got)
	}
	if got := decoded["attention_reason"]; got != string(outbox.AttentionReasonMaterialUploadBudget) {
		t.Fatalf("attention_reason = %#v", got)
	}
	if got := decoded["event_id"]; got != "generation.provider_result.materialize:step-1" {
		t.Fatalf("event_id = %#v", got)
	}
	if got := decoded["aggregate_id"]; got != "creation-1" {
		t.Fatalf("aggregate_id = %#v", got)
	}
	if got := decoded["attempt_count"]; got != float64(outbox.MaterialUploadRetryBudget) {
		t.Fatalf("attempt_count = %#v", got)
	}
	if got := decoded["event_age_seconds"]; got != float64(int64((42 * time.Minute).Seconds())) {
		t.Fatalf("event_age_seconds = %#v", got)
	}
	// 告警会离开进程；事件载荷、provider 定位符与凭据一律不得出现在告警里。
	// （事件类型名本身含 "provider_result"，所以只禁具体泄露形态，不禁该词。）
	for _, forbidden := range []string{"http://", "https://", "x-amz", "authorization", "secret", "access_key", "payload"} {
		if strings.Contains(strings.ToLower(line), forbidden) {
			t.Fatalf("alert leaked %q: %s", forbidden, line)
		}
	}
}

func TestNewLogAttentionAlerter无logger时保持静默而不是失败(t *testing.T) {
	if alerter := NewLogAttentionAlerter(nil); alerter != nil {
		t.Fatalf("NewLogAttentionAlerter(nil) = %#v, want nil", alerter)
	}
	// 未安装告警的 Worker 仍然必须能跑完整个投递循环。
	alertAttention(nil, context.Background(), &outbox.Event{ID: "event-1"}, outbox.AttentionReasonEventAge, time.Now())
}

func TestAlertAttention只上报标识与冻结原因并把负年龄归零(t *testing.T) {
	now := time.Date(2026, 9, 20, 6, 0, 0, 0, time.UTC)
	recorder := &recordingAttentionAlerter{}
	alertAttention(recorder, context.Background(), &outbox.Event{
		ID: "event-1", AggregateID: "creation-1", EventType: outbox.EventType("generation.provider_result.materialize"),
		AttemptCount: 3,
		// 事件时间晚于当前时刻只可能来自时钟回拨；把负年龄报给运维没有意义。
		CreatedAt: now.Add(time.Minute),
	}, outbox.AttentionReasonEventAge, now)

	if len(recorder.events) != 1 {
		t.Fatalf("alert count = %d, want 1", len(recorder.events))
	}
	got := recorder.events[0]
	if got.Age != 0 {
		t.Fatalf("Age = %s, want 0 for a future-created event", got.Age)
	}
	if got.EventID != "event-1" || got.AggregateID != "creation-1" || got.EventType != string(outbox.EventType("generation.provider_result.materialize")) ||
		got.Reason != outbox.AttentionReasonEventAge || got.Attempt != 3 || !got.At.Equal(now) {
		t.Fatalf("attention = %#v", got)
	}

	// 空事件不得产生告警。
	alertAttention(recorder, context.Background(), nil, outbox.AttentionReasonEventAge, now)
	if len(recorder.events) != 1 {
		t.Fatalf("nil event produced an alert: %#v", recorder.events)
	}
}
