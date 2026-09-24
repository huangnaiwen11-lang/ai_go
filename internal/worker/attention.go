package worker

import (
	"context"
	"log/slog"
	"time"

	"ai-business-service/internal/biz/outbox"
)

// AttentionAlert is the stable, greppable marker for the operator signal. It is
// a separate attribute rather than part of the free-text message so a log
// pipeline can match it without parsing prose.
const AttentionAlert = "outbox_event_needs_attention"

// AttentionEvent is the operator-facing identity of one event the automatic
// path has given up on.
//
// It carries identifiers and the frozen reason only. An alert must never carry
// a provider URL, an event payload or a credential: this signal leaves the
// process, while the payload is untrusted provider data.
type AttentionEvent struct {
	EventID     string
	AggregateID string
	EventType   string
	Reason      outbox.AttentionReason
	Attempt     int32
	Age         time.Duration
	At          time.Time
}

// AttentionAlerter emits the operator signal for a durable needs_attention
// transition.
//
// It is deliberately a single synchronous call: the delivery loop invokes it
// once per transition, never once per attempt, so an implementation that blocks
// would stall the only worker able to finish that event. Implementations must
// therefore not perform network I/O. A nil alerter means "no alerting
// configured" and must stay silent rather than panic.
type AttentionAlerter interface {
	AttentionRequired(context.Context, AttentionEvent)
}

// NewLogAttentionAlerter returns the production alerter: one structured WARN
// line on the process log.
//
// There is no metrics or alerting stack in this deployment, and the worker
// already writes to stdout, so a stable structured line is the only signal an
// external collector can act on without new infrastructure. A nil logger
// returns a nil alerter, which keeps "alerting not configured" expressible as
// silence instead of a failure.
func NewLogAttentionAlerter(logger *slog.Logger) AttentionAlerter {
	if logger == nil {
		return nil
	}
	return logAttentionAlerter{logger: logger}
}

type logAttentionAlerter struct{ logger *slog.Logger }

func (alerter logAttentionAlerter) AttentionRequired(ctx context.Context, attention AttentionEvent) {
	alerter.logger.WarnContext(ctx, "outbox event requires operator attention",
		"alert", AttentionAlert,
		"event_id", attention.EventID,
		"aggregate_id", attention.AggregateID,
		"event_type", attention.EventType,
		"attention_reason", string(attention.Reason),
		"attempt_count", attention.Attempt,
		"event_age_seconds", int64(attention.Age.Seconds()),
		"at", attention.At,
	)
}

// alertAttention emits the operator signal for an event that is now durably in
// needs_attention.
//
// Callers must invoke it only after MarkNeedsAttention succeeded. An alert that
// fired on a failed marker would send an operator looking for an event that is
// in fact still being retried, which is worse than staying silent.
func alertAttention(alerter AttentionAlerter, ctx context.Context, event *outbox.Event, reason outbox.AttentionReason, now time.Time) {
	if alerter == nil || event == nil {
		return
	}
	age := now.Sub(event.CreatedAt)
	if age < 0 {
		// A negative age would only mean a clock step; reporting it as an age is
		// less useful to an operator than reporting zero.
		age = 0
	}
	alerter.AttentionRequired(ctx, AttentionEvent{
		EventID:     event.ID,
		AggregateID: event.AggregateID,
		EventType:   string(event.EventType),
		Reason:      reason,
		Attempt:     event.AttemptCount,
		Age:         age,
		At:          now,
	})
}
