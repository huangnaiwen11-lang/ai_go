package outbox

import (
	"context"
	"time"
)

// BacklogSnapshot contains only aggregate operational facts. It intentionally
// excludes event IDs, aggregate IDs, payloads, provider locations, and lease
// tokens so it is safe to surface through a metrics endpoint.
type BacklogSnapshot struct {
	Pending        int64
	Dispatching    int64
	Reconciling    int64
	NeedsAttention int64

	// OldestActiveCreatedAt is the oldest still-automatic event among pending,
	// dispatching, and reconciling work. NeedsAttention has its own age because
	// it is terminal for automation and is governed by an operator SLA.
	OldestActiveCreatedAt    time.Time
	OldestAttentionCreatedAt time.Time
}

// BacklogReader is a narrow, read-only boundary used only for operational
// metrics. It is deliberately separate from Repository so existing delivery
// fakes and transactional writers do not gain a monitoring obligation.
type BacklogReader interface {
	ReadOutboxBacklog(context.Context) (BacklogSnapshot, error)
}
