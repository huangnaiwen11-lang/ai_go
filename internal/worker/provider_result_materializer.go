package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/r2"
	"ai-business-service/internal/integrations/safefetch"
)

const (
	defaultProviderResultMaterializerWorkerID = "provider-result-materializer-worker"
	// The materializer lease lasts one minute. Renewing every third keeps two
	// full renewal opportunities in reserve for a slow provider download or R2
	// multipart upload without turning ordinary result delivery into a hot loop.
	providerResultLeaseRenewalInterval = defaultLeaseDuration / 3
)

var (
	// ErrProviderResultMaterializerDependenciesUnavailable means an event would
	// otherwise be handled without an owned storage or transaction boundary.
	ErrProviderResultMaterializerDependenciesUnavailable = errors.New("provider result materializer dependencies are unavailable")
	// ErrInvalidProviderResultMaterializeEvent is a malformed work item. It is
	// deterministic and must be failed for operator review, not retried.
	ErrInvalidProviderResultMaterializeEvent = errors.New("provider result materializer: invalid materialization event")
	// ErrInvalidProviderResultMedia rejects an untrusted provider response before
	// it is written into R2. The result worker never sniffs a type into a more
	// permissive content type than the provider declared.
	ErrInvalidProviderResultMedia = errors.New("provider result materializer: invalid result media")
)

// providerResultRouteResolver resolves the frozen execution route. It is
// deliberately not a new-task selector: historical results must retain their
// provider/account binding even while rollout configuration changes.
type providerResultRouteResolver interface {
	ProviderForRoute(provider, accountRef string) (platform.ProviderHandle, error)
}

// b2bMaterializerConfigSource is intentionally optional. A normal one-step
// result only needs the frozen provider handle and R2 policy; a two-step B2B
// result additionally needs the same catalog/callback configuration used by
// the submission worker to validate its newly bound second-stage product.
type b2bMaterializerConfigSource interface {
	B2BSubmission() (platform.B2BSubmissionConfig, bool)
}

// providerResultFetcher is narrow enough to test the worker without a real
// network. The production implementation creates a safefetch downloader from
// the persisted B2B account policy for every result fetch.
type providerResultFetcher interface {
	Fetch(context.Context, platform.ProviderHandle, string) (*safefetch.Response, error)
}

// immutableResultWriter is the only object-store capability required by the
// worker. r2.Client streams the body straight into an immutable object, so the
// worker never stages a provider result on its own disk, and the client owns
// the conditional write, the abort of a partial upload, and the independent
// verification.
type immutableResultWriter interface {
	StreamImmutable(context.Context, r2.ImmutableStreamInput, io.Reader) (r2.ImmutableStreamedObject, error)
}

// materializerLeaseGuard owns cancellation of the external download/R2 leg.
// A failed renewal is indistinguishable from ownership loss: continuing I/O
// could let a reclaimed event publish from a stale Worker, so it is always
// treated as lost and the I/O context is cancelled immediately.
type materializerLeaseGuard struct {
	ctx           context.Context
	ioCancel      context.CancelFunc
	renewalCancel context.CancelFunc
	done          chan struct{}
	stopped       chan struct{}
	stopOnce      sync.Once
	stopping      atomic.Bool
	lost          atomic.Bool
}

func (guard *materializerLeaseGuard) Stop() bool {
	if guard == nil {
		return true
	}
	guard.stopOnce.Do(func() {
		guard.stopping.Store(true)
		close(guard.done)
		guard.renewalCancel()
		guard.ioCancel()
		<-guard.stopped
	})
	return guard.lost.Load()
}

// ProviderResultMaterializerWorker turns a completed B2B terminal into an
// owned R2 asset. It has no callback HTTP surface and it never posts to the
// provider: it only consumes the durable materialization event emitted by the
// terminal CAS transaction.
type ProviderResultMaterializerWorker struct {
	outbox    outbox.Repository
	store     generation.ProviderResultMaterializationStore
	tx        shared.TxRunner
	providers providerResultRouteResolver
	fetcher   providerResultFetcher
	objects   immutableResultWriter
	r2        r2.Config
	workerID  string
	now       func() time.Time
	attention AttentionAlerter
}

// SetAttentionAlerter installs the operator signal for events this worker gives
// up on. It must be called before the first DeliverOnce; leaving it unset keeps
// the worker silent, which is the pre-existing behaviour and stays valid in
// tests that only exercise the delivery state machine.
func (worker *ProviderResultMaterializerWorker) SetAttentionAlerter(alerter AttentionAlerter) {
	if worker != nil {
		worker.attention = alerter
	}
}

// NewProviderResultMaterializerWorker constructs one delivery unit without
// network I/O. R2 configuration is validated at construction so an enabled
// worker cannot silently use a different object-storage topology.
func NewProviderResultMaterializerWorker(
	outboxRepository outbox.Repository,
	store generation.ProviderResultMaterializationStore,
	tx shared.TxRunner,
	providers providerResultRouteResolver,
	fetcher providerResultFetcher,
	objects immutableResultWriter,
	storage r2.Config,
	workerID string,
	now func() time.Time,
) (*ProviderResultMaterializerWorker, error) {
	if err := storage.Validate(); err != nil {
		return nil, fmt.Errorf("validate provider result R2 config: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	return &ProviderResultMaterializerWorker{
		outbox: outboxRepository, store: store, tx: tx, providers: providers, fetcher: fetcher,
		objects: objects, r2: storage, workerID: workerID, now: now,
	}, nil
}

// NewSafeProviderResultFetcher returns the production fetcher. It is kept as
// a constructor instead of a process-wide downloader because ResultHostAllowlist
// and MaxResultBytes are account-scoped facts carried by each frozen route.
func NewSafeProviderResultFetcher() providerResultFetcher { return safeProviderResultFetcher{} }

type safeProviderResultFetcher struct{}

func (safeProviderResultFetcher) Fetch(ctx context.Context, handle platform.ProviderHandle, rawURL string) (*safefetch.Response, error) {
	if !handle.IsB2B() || handle.MaxResultBytes <= 0 || len(handle.ResultHostAllowlist) == 0 {
		return nil, ErrProviderResultMaterializerDependenciesUnavailable
	}
	downloader, err := safefetch.NewDownloader(safefetch.Options{
		MaxBytes: handle.MaxResultBytes, AllowedHosts: handle.ResultHostAllowlist,
	})
	if err != nil {
		return nil, err
	}
	// Closing idle connections does not close the active response body; the
	// caller owns Body.Close and still reads the fully policy-checked stream.
	defer downloader.CloseIdleConnections()
	return downloader.Fetch(ctx, rawURL)
}

// DeliverOnce processes at most one materialization event. A successful
// publication is settled by store.PublishProviderResultMaterialization in the
// same Mongo transaction as the asset and billing gate; this worker must not
// issue a second MarkDelivered afterwards.
func (worker *ProviderResultMaterializerWorker) DeliverOnce(ctx context.Context, expectedEventID string) error {
	if err := worker.ready(); err != nil {
		return err
	}
	now := worker.nowUTC()
	eventType := outbox.EventType(generation.ProviderResultMaterializeEventType)
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
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil || event.ID != generation.ProviderResultMaterializeEventID(payload.StepID) || event.AggregateID != payload.CreationID {
		return worker.abandon(ctx, event)
	}
	target, err := worker.store.LoadProviderResultMaterialization(ctx, payload)
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	if target.Payload != payload || target.Validate() != nil {
		return worker.abandon(ctx, event)
	}
	handle, err := worker.providers.ProviderForRoute(payload.Provider, payload.AccountRef)
	if err != nil {
		// A temporary configuration rollback must not make an already completed
		// task disappear; wait for the correct account configuration to return.
		return worker.requeue(ctx, event, err)
	}
	if !handle.IsB2B() || handle.TenantID == "" || handle.MaxResultBytes <= 0 || len(handle.ResultHostAllowlist) == 0 {
		return worker.abandon(ctx, event)
	}
	lease := worker.startLeaseGuard(ctx, event)
	publication, err := worker.materialize(lease.ctx, event, target, handle)
	if lease.Stop() {
		// The final state belongs to the worker that currently owns the lease.
		// Do not requeue, mark pending or publish with a token whose ownership
		// was not confirmed; the current/reclaiming owner will converge it.
		return nil
	}
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	// Stop the asynchronous guard before opening the Mongo transaction, then
	// take one synchronous fence immediately before publication. This prevents
	// a concurrent renewal from fighting the transaction and gives the short
	// persistence window a fresh full lease.
	if err := worker.renewLease(ctx, event); err != nil {
		return nil
	}
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	err = worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		return worker.store.PublishProviderResultMaterialization(txCtx, publication)
	})
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	return nil
}

func (worker *ProviderResultMaterializerWorker) materialize(ctx context.Context, event *outbox.Event, target generation.ProviderResultMaterializationTarget, handle platform.ProviderHandle) (generation.ProviderResultPublication, error) {
	response, err := worker.fetcher.Fetch(ctx, handle, target.Payload.ResultRef)
	if err != nil {
		return generation.ProviderResultPublication{}, materializerMediaFailure{cause: err}
	}
	if response == nil || response.Body == nil {
		return generation.ProviderResultPublication{}, ErrInvalidProviderResultMedia
	}
	defer func() { _ = response.Body.Close() }()
	contentType, extension, err := resultContentTypeAndExtension(target.Payload.Capability, response.Header.Get("Content-Type"))
	if err != nil {
		return generation.ProviderResultPublication{}, err
	}
	tenantSum := sha256.Sum256([]byte(handle.TenantID))
	tenantSHA256 := hex.EncodeToString(tenantSum[:])
	// The destination key must be fixed before the first byte leaves the socket,
	// because a multipart upload freezes it at CreateMultipartUpload. It is
	// therefore namespaced by the step rather than derived from the hash of the
	// body; the content hash comes back from the client and is recorded on the
	// published asset row, which is where readers look it up.
	key := "gen/" + target.Payload.Capability + "/" + tenantSHA256 + "/" + target.Payload.StepID + "." + extension
	committed, err := worker.objects.StreamImmutable(ctx, r2.ImmutableStreamInput{
		Key: key, ContentType: contentType, TenantSHA256: tenantSHA256, MaxBytes: handle.MaxResultBytes,
	}, response.Body)
	if err != nil {
		if errors.Is(err, r2.ErrImmutableStreamTooLarge) {
			// The frozen route ceiling is a property of this provider response, not
			// of the attempt: a retry would exceed it again, so fail closed instead
			// of consuming the material-upload budget.
			return generation.ProviderResultPublication{}, fmt.Errorf("%w: %v", ErrInvalidProviderResultMedia, err)
		}
		return generation.ProviderResultPublication{}, materializerMediaFailure{cause: err}
	}
	publication := generation.ProviderResultPublication{
		EventID: event.ID, LeaseToken: event.LeaseToken, Target: target,
		StorageURL: worker.r2.PublicObjectURL(committed.Key), ContentType: contentType, ContentLength: committed.ContentLength,
		ContentSHA256: committed.ContentSHA256, PublishedAt: worker.nowUTC(),
	}
	if target.NextB2B != nil {
		configSource, ok := worker.providers.(b2bMaterializerConfigSource)
		if !ok {
			return generation.ProviderResultPublication{}, ErrB2BSubmissionUnavailable
		}
		config, ok := configSource.B2BSubmission()
		if !ok || !config.Ready() {
			return generation.ProviderResultPublication{}, ErrB2BSubmissionUnavailable
		}
		secondRecipe, err := target.NextB2B.Deferred.BindOwnedOpeningFrame(publication.StorageURL)
		if err != nil {
			return generation.ProviderResultPublication{}, generation.ErrInvalidProviderResultPublication
		}
		// Map exactly once here as a preflight. The ordinary submission worker
		// will map the same canonical recipe again when it grants POST intent;
		// that second validation protects against any catalog-storage repair
		// between materialization and dispatch without changing route version.
		if _, err := mapB2BRecipe(ctx, config, target.NextB2B.Route, target.NextB2B.StepID, secondRecipe); err != nil {
			return generation.ProviderResultPublication{}, err
		}
		publication.NextB2B = &generation.ProviderB2BSecondStepActivation{
			StepID: target.NextB2B.StepID, Route: target.NextB2B.Route, Recipe: secondRecipe,
		}
	}
	return publication.Normalize()
}

func resultContentTypeAndExtension(capability, raw string) (string, string, error) {
	contentType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", "", ErrInvalidProviderResultMedia
	}
	contentType = strings.ToLower(contentType)
	switch capability {
	case string(creations.AtomTextToImage), string(creations.AtomImageEdit):
		switch contentType {
		case "image/jpeg":
			return contentType, "jpg", nil
		case "image/png":
			return contentType, "png", nil
		case "image/webp":
			return contentType, "webp", nil
		}
	case string(creations.AtomImageToVideo):
		switch contentType {
		case "video/mp4", "video/quicktime":
			return contentType, "mp4", nil
		case "video/webm":
			return contentType, "webm", nil
		}
	}
	return "", "", ErrInvalidProviderResultMedia
}

// materializerMediaFailure marks a failure inside the "move provider bytes into
// owned storage" leg: the download and the immutable object write.
//
// Only this leg consumes the frozen material-upload retry budget. It is a
// wrapper type rather than an error taxonomy because the adapters below
// (safefetch, the S3-compatible client) surface raw transport/SDK errors;
// inferring the leg from error types silently misclassifies whenever an adapter
// changes its wrapping.
type materializerMediaFailure struct{ cause error }

func (failure materializerMediaFailure) Error() string { return failure.cause.Error() }

func (failure materializerMediaFailure) Unwrap() error { return failure.cause }

func isMaterializerMediaFailure(cause error) bool {
	var failure materializerMediaFailure
	return errors.As(cause, &failure)
}

func (worker *ProviderResultMaterializerWorker) retryOrAbandon(ctx context.Context, event *outbox.Event, cause error) error {
	if providerResultMaterializationPermanentFailure(cause) {
		return worker.abandon(ctx, event)
	}
	mediaFailure := isMaterializerMediaFailure(cause)
	reason, terminal := materializerAttentionReason(event, mediaFailure, worker.nowUTC())
	if !terminal {
		if mediaFailure {
			return worker.requeueMaterialUpload(ctx, event)
		}
		return worker.requeue(ctx, event, cause)
	}
	if mediaFailure {
		// Record the durable fact before giving up the lease: the order matters,
		// because the reverse order can leave an unclaimable event whose cause is
		// nowhere recorded. A failed marker must not consume the attention
		// terminal either -- better to keep retrying than to open that hole.
		if err := worker.markUploadPending(ctx, event, reason); err != nil {
			return worker.requeueMaterialUpload(ctx, event)
		}
	}
	attentionAt := worker.nowUTC()
	if err := settleOutboxError(worker.outbox.MarkNeedsAttention(ctx, event.ID, event.LeaseToken, reason, attentionAt)); err != nil {
		return err
	}
	// The signal is emitted only after the transition is durable, so an operator
	// investigating an alert always finds the event in needs_attention.
	alertAttention(worker.attention, ctx, event, reason, attentionAt)
	return nil
}

// materializerAttentionReason decides whether the automatic path must give up.
//
// The two terminal conditions are deliberately separate. Dependency failures
// (reading the publication target, writing the publication transaction, mapping
// the frozen recipe) do not consume the material-upload budget: a temporary
// configuration rollback must not turn an already completed provider task into
// an operator ticket. They converge through the maximum event age instead, so
// no failure mode is left unbounded.
func materializerAttentionReason(event *outbox.Event, mediaFailure bool, now time.Time) (outbox.AttentionReason, bool) {
	// 预算与年龄都按「当前自动重试窗口」计算。人工重驱会开启一个新窗口，
	// 如果这里仍然用 AttemptCount / CreatedAt 原始值，重驱后第一次失败就会
	// 立刻再次终态，重驱等于空操作。
	startedAt, attemptBase := event.AttentionWindow()
	if mediaFailure && outbox.RetryBudgetExhausted(event.AttemptCount-attemptBase, outbox.MaterialUploadRetryBudget) {
		return outbox.AttentionReasonMaterialUploadBudget, true
	}
	if outbox.EventAgeExceeded(startedAt, now) {
		return outbox.AttentionReasonEventAge, true
	}
	return "", false
}

// markUploadPending persists the "provider confirmed completion, bytes not yet
// owned" fact on the step row. It runs outside the publication transaction on
// purpose: the publication transaction failing is precisely the case that leads
// here.
func (worker *ProviderResultMaterializerWorker) markUploadPending(ctx context.Context, event *outbox.Event, reason outbox.AttentionReason) error {
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		return err
	}
	pendingReason, ok := materializerUploadPendingReason(reason)
	if !ok {
		return generation.ErrInvalidProviderResultPublication
	}
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	return worker.store.MarkProviderResultUploadPending(persistenceCtx, generation.ProviderResultUploadPending{
		Payload: payload, Reason: pendingReason, At: worker.nowUTC(),
	})
}

func materializerUploadPendingReason(reason outbox.AttentionReason) (generation.ProviderResultUploadPendingReason, bool) {
	switch reason {
	case outbox.AttentionReasonMaterialUploadBudget:
		return generation.ProviderResultUploadPendingBudgetExhausted, true
	case outbox.AttentionReasonEventAge:
		return generation.ProviderResultUploadPendingAgeExceeded, true
	default:
		return "", false
	}
}

func providerResultMaterializationPermanentFailure(cause error) bool {
	if errors.Is(cause, generation.ErrInvalidProviderResultPublication) ||
		errors.Is(cause, generation.ErrProviderResultPublicationConflict) ||
		errors.Is(cause, generation.ErrProviderResultPublicationTopology) ||
		errors.Is(cause, ErrInvalidProviderResultMedia) ||
		errors.Is(cause, r2.ErrInvalidObjectRequest) ||
		safefetch.IsPolicyError(cause) {
		return true
	}
	var status *safefetch.StatusError
	return errors.As(cause, &status) && !safefetch.RetryableStatus(status.StatusCode)
}

func (worker *ProviderResultMaterializerWorker) abandon(ctx context.Context, event *outbox.Event) error {
	return settleOutboxError(worker.outbox.MarkFailed(ctx, event.ID, event.LeaseToken, worker.nowUTC()))
}

func (worker *ProviderResultMaterializerWorker) requeue(ctx context.Context, event *outbox.Event, cause error) error {
	now := worker.nowUTC()
	return settleOutboxError(worker.outbox.Requeue(ctx, event.ID, event.LeaseToken, nextSubmissionAttempt(now, event.AttemptCount, cause)))
}

// requeueMaterialUpload re-schedules through the material-upload backoff rather
// than the submission one. The two legs have different frozen rhythms and the
// submission leg additionally honours a provider Retry-After that has no
// authority over an object-store write.
func (worker *ProviderResultMaterializerWorker) requeueMaterialUpload(ctx context.Context, event *outbox.Event) error {
	now := worker.nowUTC()
	return settleOutboxError(worker.outbox.Requeue(ctx, event.ID, event.LeaseToken, nextMaterialUploadAttempt(now, event.AttemptCount)))
}

func (worker *ProviderResultMaterializerWorker) startLeaseGuard(ctx context.Context, event *outbox.Event) *materializerLeaseGuard {
	ioCtx, ioCancel := context.WithCancel(ctx)
	renewalCtx, renewalCancel := context.WithCancel(context.WithoutCancel(ctx))
	guard := &materializerLeaseGuard{
		ctx: ioCtx, ioCancel: ioCancel, renewalCancel: renewalCancel,
		done: make(chan struct{}), stopped: make(chan struct{}),
	}
	go func() {
		defer close(guard.stopped)
		attempt := func() bool {
			if err := worker.renewLeaseWithContext(renewalCtx, event); err != nil {
				// Stop() cancels the renewal context as cleanup after I/O has already
				// returned. That is not a lease loss; the final synchronous renewal
				// below is the fence for that hand-off.
				if !guard.stopping.Load() {
					guard.lost.Store(true)
					guard.ioCancel()
				}
				return false
			}
			return true
		}
		// Renew immediately in a separate goroutine. The Claim itself is still
		// the initial ownership proof, while this makes a failed renewal cancel a
		// stream even when it begins before the first ticker interval elapses.
		if !attempt() {
			return
		}
		ticker := time.NewTicker(providerResultLeaseRenewalInterval)
		defer ticker.Stop()
		for {
			select {
			case <-guard.done:
				return
			case <-ticker.C:
				if !attempt() {
					return
				}
			}
		}
	}()
	return guard
}

func (worker *ProviderResultMaterializerWorker) renewLease(ctx context.Context, event *outbox.Event) error {
	return worker.renewLeaseWithContext(context.WithoutCancel(ctx), event)
}

func (worker *ProviderResultMaterializerWorker) renewLeaseWithContext(ctx context.Context, event *outbox.Event) error {
	if event == nil {
		return outbox.ErrInvalidEvent
	}
	now := worker.nowUTC()
	persistenceCtx, cancel := context.WithTimeout(ctx, submissionPersistenceTimeout)
	defer cancel()
	return worker.outbox.RenewLease(persistenceCtx, event.ID, event.LeaseToken, now, now.Add(defaultLeaseDuration))
}

func (worker *ProviderResultMaterializerWorker) ready() error {
	if worker == nil || worker.outbox == nil || worker.store == nil || worker.tx == nil || worker.providers == nil || worker.fetcher == nil ||
		worker.objects == nil || worker.workerID == "" || worker.r2.Validate() != nil {
		return ErrProviderResultMaterializerDependenciesUnavailable
	}
	return nil
}

func (worker *ProviderResultMaterializerWorker) persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), submissionPersistenceTimeout)
}

func (worker *ProviderResultMaterializerWorker) nowUTC() time.Time {
	if worker.now == nil {
		return time.Now().UTC()
	}
	return worker.now().UTC()
}

var _ onceDeliverer = (*ProviderResultMaterializerWorker)(nil)
