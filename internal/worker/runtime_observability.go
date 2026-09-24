package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ai-business-service/internal/biz/outbox"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// DefaultRuntimeObservabilityAddress is deliberately loopback-only. The
	// generation worker shares the Gateway network namespace; publishing this
	// port to the host or a public network would turn operational metadata into
	// an accidental API surface.
	DefaultRuntimeObservabilityAddress = "127.0.0.1:19082"

	runtimeMetricsPath = "/metrics"
	runtimeHealthPath  = "/healthz"
	runtimeReadyPath   = "/readyz"

	// Outbox snapshots read aggregate counts plus two ordered documents. The
	// Worker normally polls every second and can run a 20-item batch, so taking
	// this snapshot per item would turn passive metrics into sustained MongoDB
	// load. A bounded 15-second cadence is sufficient for the stated alert
	// windows (minutes) without competing with delivery writes.
	OutboxBacklogSnapshotInterval = 15 * time.Second
)

// WorkerUnit is a bounded execution unit name. It is used as a metric label;
// callers must never pass event IDs, provider names, or dynamic route data.
type WorkerUnit string

const (
	WorkerUnitSubmission  WorkerUnit = "submission"
	WorkerUnitCancel      WorkerUnit = "cancel"
	WorkerUnitInbox       WorkerUnit = "inbox"
	WorkerUnitReconcile   WorkerUnit = "reconcile"
	WorkerUnitSettlement  WorkerUnit = "settlement"
	WorkerUnitMaterialize WorkerUnit = "materialize"
	workerUnitUnknown     WorkerUnit = "unknown"
	metricOutcomeSuccess             = "success"
	metricOutcomeError               = "error"
	metricErrorNone                  = "none"
	metricErrorContext               = "context"
	metricErrorInvalid               = "invalid"
	metricErrorInternal              = "internal"
)

// RuntimeObservability owns the loopback-only operational HTTP endpoint and
// the bounded metrics emitted by the generation worker. It has no business
// routes and must not receive user traffic.
type RuntimeObservability struct {
	address string
	logger  *slog.Logger

	registry *prometheus.Registry
	server   *http.Server

	mu                          sync.Mutex
	listener                    net.Listener
	started                     atomic.Bool
	ready                       atomic.Bool
	nextBacklogSnapshotUnixNano atomic.Int64

	runtimeUp    prometheus.Gauge
	runtimeReady prometheus.Gauge
	unitRuns     *prometheus.CounterVec
	unitDuration *prometheus.HistogramVec
	claims       *prometheus.CounterVec
	transitions  *prometheus.CounterVec
	leaseRecover *prometheus.CounterVec
	backlog      *prometheus.GaugeVec
	oldestAge    *prometheus.GaugeVec
	snapshotAt   prometheus.Gauge
	snapshotFail prometheus.Counter
}

// NewRuntimeObservability constructs the endpoint but does not bind its port.
// Start must be called only after the worker's configuration and dependencies
// are valid, so an HTTP 200 never masks a broken delivery process.
func NewRuntimeObservability(address string, logger *slog.Logger) (*RuntimeObservability, error) {
	address, err := normalizeRuntimeObservabilityAddress(address)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	registry := prometheus.NewRegistry()
	observability := &RuntimeObservability{
		address:  address,
		logger:   logger,
		registry: registry,
		runtimeUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "up",
			Help: "Whether the generation worker observability endpoint is running.",
		}),
		runtimeReady: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "ready",
			Help: "Whether the generation worker has completed at least one full successful delivery cycle.",
		}),
		unitRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "unit_runs_total",
			Help: "Completed generation worker unit invocations by bounded unit and outcome.",
		}, []string{"unit", "outcome", "error_class"}),
		unitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "unit_duration_seconds",
			Help:    "Generation worker unit invocation duration by bounded unit.",
			Buckets: prometheus.DefBuckets,
		}, []string{"unit"}),
		claims: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "outbox_claims_total",
			Help: "Outbox claim attempts by fixed event type and bounded result.",
		}, []string{"event_type", "result", "error_class"}),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "outbox_transitions_total",
			Help: "Successful outbox state transitions by fixed event type, outcome, and fixed attention reason.",
		}, []string{"event_type", "outcome", "attention_reason"}),
		leaseRecover: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "outbox_lease_recoveries_total",
			Help: "Outbox events reclaimed after an expired lease by fixed event type.",
		}, []string{"event_type"}),
		backlog: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "outbox_backlog",
			Help: "Current outbox backlog by bounded operational state.",
		}, []string{"state"}),
		oldestAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "outbox_oldest_event_age_seconds",
			Help: "Age of the oldest outbox event by bounded operational state.",
		}, []string{"state"}),
		snapshotAt: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "outbox_snapshot_last_success_unixtime",
			Help: "Unix time of the most recent successful aggregate outbox snapshot.",
		}),
		snapshotFail: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "cling", Subsystem: "generation_worker", Name: "outbox_snapshot_failures_total",
			Help: "Failures while reading the aggregate outbox snapshot for metrics.",
		}),
	}
	registry.MustRegister(
		observability.runtimeUp,
		observability.runtimeReady,
		observability.unitRuns,
		observability.unitDuration,
		observability.claims,
		observability.transitions,
		observability.leaseRecover,
		observability.backlog,
		observability.oldestAge,
		observability.snapshotAt,
		observability.snapshotFail,
	)

	mux := http.NewServeMux()
	mux.Handle(runtimeMetricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc(runtimeHealthPath, observability.health)
	mux.HandleFunc(runtimeReadyPath, observability.readiness)
	observability.server = &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return observability, nil
}

// Start binds the validated loopback address and serves only health, readiness,
// and metrics. An unexpected serve failure is logged with a fixed message; its
// raw error is intentionally not exported as a metric label.
func (observability *RuntimeObservability) Start() error {
	if observability == nil || observability.server == nil {
		return errors.New("generation worker observability is not configured")
	}
	observability.mu.Lock()
	defer observability.mu.Unlock()
	if observability.listener != nil {
		return errors.New("generation worker observability already started")
	}
	listener, err := net.Listen("tcp", observability.address)
	if err != nil {
		return fmt.Errorf("listen generation worker observability %q: %w", observability.address, err)
	}
	observability.listener = listener
	observability.started.Store(true)
	observability.runtimeUp.Set(1)
	go func() {
		if err := observability.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			observability.logger.Error("generation worker observability server stopped", "error", err)
			observability.started.Store(false)
			observability.ready.Store(false)
			observability.runtimeUp.Set(0)
			observability.runtimeReady.Set(0)
		}
	}()
	return nil
}

// Shutdown stops the loopback endpoint without changing delivery semantics.
func (observability *RuntimeObservability) Shutdown(ctx context.Context) error {
	if observability == nil || observability.server == nil {
		return nil
	}
	observability.started.Store(false)
	observability.ready.Store(false)
	observability.runtimeUp.Set(0)
	observability.runtimeReady.Set(0)
	return observability.server.Shutdown(ctx)
}

// MarkReady records a successful full delivery cycle. Once set, readiness
// remains true until the process stops; a fatal cycle error exits the process
// through the existing Runner fail-fast path rather than pretending to recover.
func (observability *RuntimeObservability) MarkReady() {
	if observability == nil {
		return
	}
	observability.ready.Store(true)
	observability.runtimeReady.Set(1)
}

// ObserveUnit records a bounded worker unit outcome. Raw error strings are
// deliberately discarded so prompts, URLs, credentials, and IDs cannot become
// metric labels or scrapeable output.
func (observability *RuntimeObservability) ObserveUnit(unit WorkerUnit, startedAt time.Time, err error) {
	if observability == nil {
		return
	}
	unit = normalizeWorkerUnit(unit)
	if !startedAt.IsZero() {
		elapsed := time.Since(startedAt).Seconds()
		if elapsed >= 0 {
			observability.unitDuration.WithLabelValues(string(unit)).Observe(elapsed)
		}
	}
	if err == nil {
		observability.unitRuns.WithLabelValues(string(unit), metricOutcomeSuccess, metricErrorNone).Inc()
		return
	}
	observability.unitRuns.WithLabelValues(string(unit), metricOutcomeError, classifyMetricError(err)).Inc()
}

// ObserveClaim records a claim outcome after the underlying repository returns.
func (observability *RuntimeObservability) ObserveClaim(eventType outbox.EventType, event *outbox.Event, err error) {
	if observability == nil {
		return
	}
	label := normalizeMetricEventType(eventType)
	if err != nil {
		observability.claims.WithLabelValues(label, metricOutcomeError, classifyMetricError(err)).Inc()
		return
	}
	if event == nil {
		observability.claims.WithLabelValues(label, "empty", metricErrorNone).Inc()
		return
	}
	observability.claims.WithLabelValues(label, "claimed", metricErrorNone).Inc()
	if event.DeliveryStatus == outbox.DeliveryStatusReconciling {
		observability.leaseRecover.WithLabelValues(label).Inc()
	}
}

// ObserveTransition records only a successful terminal/requeue transition.
func (observability *RuntimeObservability) ObserveTransition(eventType outbox.EventType, outcome string, reason outbox.AttentionReason) {
	if observability == nil {
		return
	}
	observability.transitions.WithLabelValues(
		normalizeMetricEventType(eventType),
		normalizeTransitionOutcome(outcome),
		normalizeAttentionReason(reason),
	).Inc()
}

// ObserveBacklog publishes aggregate counts and oldest-event ages after a
// successful snapshot. CreatedAt is the immutable event fact and therefore
// remains meaningful even while retry bookkeeping updates UpdatedAt.
func (observability *RuntimeObservability) ObserveBacklog(snapshot outbox.BacklogSnapshot, now time.Time) {
	if observability == nil {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	observability.backlog.WithLabelValues("pending").Set(float64(snapshot.Pending))
	observability.backlog.WithLabelValues("dispatching").Set(float64(snapshot.Dispatching))
	observability.backlog.WithLabelValues("reconciling").Set(float64(snapshot.Reconciling))
	observability.backlog.WithLabelValues("needs_attention").Set(float64(snapshot.NeedsAttention))
	observability.oldestAge.WithLabelValues("active").Set(metricAgeSeconds(now, snapshot.OldestActiveCreatedAt))
	observability.oldestAge.WithLabelValues("needs_attention").Set(metricAgeSeconds(now, snapshot.OldestAttentionCreatedAt))
	observability.snapshotAt.Set(float64(now.Unix()))
}

// ShouldSampleBacklog rate-limits aggregate Mongo reads independently of the
// delivery batch size. It is safe for multiple goroutines and records the
// next slot before the query starts, so slow snapshots cannot create a burst
// of concurrent duplicate aggregation work.
func (observability *RuntimeObservability) ShouldSampleBacklog(now time.Time) bool {
	if observability == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowUnixNano := now.UnixNano()
	for {
		nextUnixNano := observability.nextBacklogSnapshotUnixNano.Load()
		if nextUnixNano > nowUnixNano {
			return false
		}
		if observability.nextBacklogSnapshotUnixNano.CompareAndSwap(nextUnixNano, now.Add(OutboxBacklogSnapshotInterval).UnixNano()) {
			return true
		}
	}
}

// ObserveBacklogFailure keeps observability failures visible without changing
// the delivery path. The underlying worker remains fail-fast for business
// delivery failures; a metrics aggregate failure must not strand user work.
func (observability *RuntimeObservability) ObserveBacklogFailure(err error) {
	if observability == nil || err == nil {
		return
	}
	observability.snapshotFail.Inc()
	observability.logger.Warn("generation worker outbox metrics snapshot failed", "error_class", classifyMetricError(err))
}

// Handler exposes the server handler for focused tests. Production code must
// use Start so the loopback-address validation remains in force.
func (observability *RuntimeObservability) Handler() http.Handler {
	if observability == nil || observability.server == nil {
		return http.NotFoundHandler()
	}
	return observability.server.Handler
}

// Address returns the validated operational bind address for startup logs.
func (observability *RuntimeObservability) Address() string {
	if observability == nil {
		return ""
	}
	return observability.address
}

func (observability *RuntimeObservability) health(writer http.ResponseWriter, _ *http.Request) {
	if observability != nil && observability.started.Load() {
		writer.WriteHeader(http.StatusOK)
		return
	}
	http.Error(writer, "generation worker observability is not running", http.StatusServiceUnavailable)
}

func (observability *RuntimeObservability) readiness(writer http.ResponseWriter, _ *http.Request) {
	if observability != nil && observability.started.Load() && observability.ready.Load() {
		writer.WriteHeader(http.StatusOK)
		return
	}
	http.Error(writer, "generation worker is not ready", http.StatusServiceUnavailable)
}

func normalizeRuntimeObservabilityAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		address = DefaultRuntimeObservabilityAddress
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return "", fmt.Errorf("generation worker observability address must be host:port: %q", address)
	}
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return address, nil
	default:
		return "", fmt.Errorf("generation worker observability address must bind loopback: %q", address)
	}
}

func normalizeWorkerUnit(unit WorkerUnit) WorkerUnit {
	switch unit {
	case WorkerUnitSubmission, WorkerUnitCancel, WorkerUnitInbox, WorkerUnitReconcile, WorkerUnitSettlement, WorkerUnitMaterialize:
		return unit
	default:
		return workerUnitUnknown
	}
}

func normalizeMetricEventType(eventType outbox.EventType) string {
	switch string(eventType) {
	case string(outbox.EventTypeGenerationSubmission),
		"generation.provider-cancel",
		"generation.inbox.consume",
		"generation.reconcile",
		"generation.terminal-settle",
		"generation.result-materialize":
		return string(eventType)
	default:
		return "unknown"
	}
}

func normalizeTransitionOutcome(outcome string) string {
	switch outcome {
	case "requeued", "delivered", "failed", "needs_attention", "redriven":
		return outcome
	default:
		return "unknown"
	}
}

func normalizeAttentionReason(reason outbox.AttentionReason) string {
	switch reason {
	case "":
		return "none"
	case outbox.AttentionReasonMaterialUploadBudget, outbox.AttentionReasonEventAge:
		return string(reason)
	default:
		return "unknown"
	}
}

func classifyMetricError(err error) string {
	switch {
	case err == nil:
		return metricErrorNone
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return metricErrorContext
	case errors.Is(err, outbox.ErrInvalidEvent), errors.Is(err, outbox.ErrInvalidTransition), errors.Is(err, outbox.ErrLeaseConflict):
		return metricErrorInvalid
	default:
		return metricErrorInternal
	}
}

func metricAgeSeconds(now, createdAt time.Time) float64 {
	if createdAt.IsZero() {
		return 0
	}
	age := now.Sub(createdAt).Seconds()
	if age < 0 {
		return 0
	}
	return age
}
