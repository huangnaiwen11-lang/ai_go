package generation

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"ai-business-service/internal/biz/creations"
)

var (
	// ErrInvalidProviderResultPublication means a worker tried to publish a
	// result without the immutable identity or owned-object facts required to
	// make it user-visible.
	ErrInvalidProviderResultPublication = errors.New("generation: invalid provider result publication")
	// ErrProviderResultPublicationConflict means the durable terminal, result
	// event, reservation gate, or creation facts no longer agree. Retrying a
	// conflicting publication must not expose a provider URL or reverse charge.
	ErrProviderResultPublicationConflict = errors.New("generation: provider result publication conflict")
	// ErrProviderResultPublicationTopology means this first materializer only
	// received a single-step B2B result. Two-step video needs its own frozen
	// opening-frame hand-off and must not be marked successful prematurely.
	ErrProviderResultPublicationTopology = errors.New("generation: provider result publication topology unsupported")
)

// ProviderResultMaterializationTarget is the storage-verified view of a
// completed result event. The payload remains the source identity; Sequence is
// read from the creation step so an event cannot invent a final-step topology.
type ProviderResultMaterializationTarget struct {
	Payload  ProviderResultMaterializeEventPayload
	Sequence int32
	// NextB2B exists only when this completed result is the first image of a
	// two-step B2B text-to-video plan. It is loaded from the blocked second
	// step and its persisted deferred recipe, never from the event payload.
	NextB2B *ProviderB2BSecondStepTarget
}

// ProviderB2BSecondStepTarget is the durable, unbound second stage selected
// at admission. The result worker may only bind the first owned R2 object to
// this exact recipe; it must not recompile against current template state.
type ProviderB2BSecondStepTarget struct {
	StepID   string
	Route    creations.ExecutionRoute
	Deferred creations.DeferredB2BImageToVideo
}

func (target ProviderResultMaterializationTarget) Normalize() (ProviderResultMaterializationTarget, error) {
	if !target.Payload.Validate() || target.Sequence <= 0 {
		return ProviderResultMaterializationTarget{}, ErrInvalidProviderResultPublication
	}
	if target.NextB2B == nil {
		return target, nil
	}
	if target.Sequence != 1 || target.Payload.Capability != "text_to_image" || target.NextB2B.StepID == "" || target.NextB2B.StepID == target.Payload.StepID {
		return ProviderResultMaterializationTarget{}, ErrInvalidProviderResultPublication
	}
	route, err := creations.NormalizeExecutionRoute(target.NextB2B.Route)
	if err != nil || route.Provider != creations.PolarStarB2BProvider {
		return ProviderResultMaterializationTarget{}, ErrInvalidProviderResultPublication
	}
	deferred, err := target.NextB2B.Deferred.Normalize()
	if err != nil {
		return ProviderResultMaterializationTarget{}, ErrInvalidProviderResultPublication
	}
	target.NextB2B = &ProviderB2BSecondStepTarget{StepID: target.NextB2B.StepID, Route: route, Deferred: deferred}
	return target, nil
}

func (target ProviderResultMaterializationTarget) Validate() error {
	_, err := target.Normalize()
	return err
}

// Same reports semantic identity rather than Go pointer identity. The target
// is read once before object I/O and once inside the publication transaction;
// each read naturally allocates a fresh deferred-recipe pointer.
func (target ProviderResultMaterializationTarget) Same(other ProviderResultMaterializationTarget) bool {
	left, err := target.Normalize()
	if err != nil {
		return false
	}
	right, err := other.Normalize()
	if err != nil || left.Payload != right.Payload || left.Sequence != right.Sequence {
		return false
	}
	if left.NextB2B == nil || right.NextB2B == nil {
		return left.NextB2B == nil && right.NextB2B == nil
	}
	if left.NextB2B.StepID != right.NextB2B.StepID || left.NextB2B.Route != right.NextB2B.Route || left.NextB2B.Deferred.Digest != right.NextB2B.Deferred.Digest {
		return false
	}
	leftPayload, leftErr := left.NextB2B.Deferred.Recipe.Marshal()
	rightPayload, rightErr := right.NextB2B.Deferred.Recipe.Marshal()
	return leftErr == nil && rightErr == nil && string(leftPayload) == string(rightPayload)
}

// ProviderB2BSecondStepActivation is the sole result-worker handoff allowed
// to make the second stage ready. Recipe must be the exact deferred product
// after binding the owned first R2 frame as opening_frame.
type ProviderB2BSecondStepActivation struct {
	StepID string
	Route  creations.ExecutionRoute
	Recipe creations.B2BProductRecipe
}

func (activation ProviderB2BSecondStepActivation) Normalize() (ProviderB2BSecondStepActivation, error) {
	if activation.StepID == "" {
		return ProviderB2BSecondStepActivation{}, ErrInvalidProviderResultPublication
	}
	route, err := creations.NormalizeExecutionRoute(activation.Route)
	if err != nil || route.Provider != creations.PolarStarB2BProvider {
		return ProviderB2BSecondStepActivation{}, ErrInvalidProviderResultPublication
	}
	recipe, err := activation.Recipe.Normalize()
	if err != nil {
		return ProviderB2BSecondStepActivation{}, ErrInvalidProviderResultPublication
	}
	activation.Route, activation.Recipe = route, recipe
	return activation, nil
}

// ProviderResultPublication is committed only after an external worker has
// safely copied the result to a verified immutable R2 object. It carries no
// provider credential or raw result bytes, and StorageURL must be the owned
// public R2 URL rather than the transient provider ResultRef.
type ProviderResultPublication struct {
	EventID       string
	LeaseToken    string
	Target        ProviderResultMaterializationTarget
	StorageURL    string
	ContentType   string
	ContentLength int64
	ContentSHA256 string
	PublishedAt   time.Time
	// NextB2B is absent for a final result. It is required only for the first
	// result of a two-step B2B video and is compared against Target.NextB2B.
	NextB2B *ProviderB2BSecondStepActivation
}

func (publication ProviderResultPublication) Normalize() (ProviderResultPublication, error) {
	target, err := publication.Target.Normalize()
	if err != nil || !providerTerminalIdentity(publication.EventID) || !validProviderOutboxLeaseToken(publication.LeaseToken) ||
		publication.EventID != ProviderResultMaterializeEventID(publication.Target.Payload.StepID) ||
		publication.ContentLength <= 0 ||
		!providerInboxDigest(publication.ContentSHA256) || publication.PublishedAt.IsZero() ||
		!validOwnedResultURL(publication.StorageURL) || publication.StorageURL == publication.Target.Payload.ResultRef ||
		!validResultContentType(publication.Target.Payload.Capability, publication.ContentType) {
		return ProviderResultPublication{}, ErrInvalidProviderResultPublication
	}
	publication.Target = target
	if target.NextB2B == nil {
		if publication.NextB2B != nil {
			return ProviderResultPublication{}, ErrInvalidProviderResultPublication
		}
	} else {
		if publication.NextB2B == nil {
			return ProviderResultPublication{}, ErrInvalidProviderResultPublication
		}
		activation, err := publication.NextB2B.Normalize()
		if err != nil || activation.StepID != target.NextB2B.StepID || activation.Route != target.NextB2B.Route {
			return ProviderResultPublication{}, ErrInvalidProviderResultPublication
		}
		expected, err := target.NextB2B.Deferred.BindOwnedOpeningFrame(publication.StorageURL)
		if err != nil || !sameResultPublicationRecipe(expected, activation.Recipe) {
			return ProviderResultPublication{}, ErrInvalidProviderResultPublication
		}
		publication.NextB2B = &activation
	}
	publication.PublishedAt = publication.PublishedAt.UTC()
	return publication, nil
}

func (publication ProviderResultPublication) Validate() error {
	_, err := publication.Normalize()
	return err
}

// ProviderResultUploadPendingReason is the only persisted, fixed summary for
// the "provider confirmed completion but the bytes are not in owned storage"
// fact. Like the outbox attention reasons it is an enum, never free text: the
// step row is operator-visible and must not leak a provider URL or object key.
type ProviderResultUploadPendingReason string

const (
	// ProviderResultUploadPendingBudgetExhausted means the material upload leg
	// used up the frozen automatic retry budget.
	ProviderResultUploadPendingBudgetExhausted ProviderResultUploadPendingReason = "material_upload_budget_exhausted"
	// ProviderResultUploadPendingAgeExceeded means the event exceeded its
	// allowed non-terminal age while the upload leg was still failing.
	ProviderResultUploadPendingAgeExceeded ProviderResultUploadPendingReason = "event_age_exceeded"
)

// ValidProviderResultUploadPendingReason reports whether a reason is registered.
func ValidProviderResultUploadPendingReason(reason ProviderResultUploadPendingReason) bool {
	switch reason {
	case ProviderResultUploadPendingBudgetExhausted, ProviderResultUploadPendingAgeExceeded:
		return true
	default:
		return false
	}
}

// ProviderResultUploadPending records the durable, non-silent fact that a
// completed provider result has not been copied into owned storage. It exists
// because the object store and Mongo share no transaction: the alternative to
// recording this fact is either publishing nothing at all (an invisible stuck
// task) or publishing the provider URL as a user asset (forbidden).
type ProviderResultUploadPending struct {
	Payload ProviderResultMaterializeEventPayload
	Reason  ProviderResultUploadPendingReason
	At      time.Time
}

func (pending ProviderResultUploadPending) Normalize() (ProviderResultUploadPending, error) {
	if !pending.Payload.Validate() || !ValidProviderResultUploadPendingReason(pending.Reason) || pending.At.IsZero() {
		return ProviderResultUploadPending{}, ErrInvalidProviderResultPublication
	}
	pending.At = pending.At.UTC()
	return pending, nil
}

func (pending ProviderResultUploadPending) Validate() error {
	_, err := pending.Normalize()
	return err
}

// ProviderResultMaterializationStore is the durable boundary for a result
// worker. Load checks the event identity against the terminal slot before any
// provider URL is fetched; Publish must run inside the caller's transaction
// and settles the materialization event in that same transaction.
//
// MarkProviderResultUploadPending is deliberately outside that transaction: it
// describes a state the automatic path has already given up on, so it must be
// writable after the publish transaction has repeatedly failed. It must be
// idempotent, must never publish an asset, and must never expose the transient
// provider ResultRef as a user-visible storage key.
type ProviderResultMaterializationStore interface {
	LoadProviderResultMaterialization(context.Context, ProviderResultMaterializeEventPayload) (ProviderResultMaterializationTarget, error)
	PublishProviderResultMaterialization(context.Context, ProviderResultPublication) error
	MarkProviderResultUploadPending(context.Context, ProviderResultUploadPending) error
}

func validOwnedResultURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && raw == strings.TrimSpace(raw) && parsed.Scheme == "https" && parsed.Host != "" &&
		parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path != "" && parsed.String() == raw
}

func sameResultPublicationRecipe(left, right creations.B2BProductRecipe) bool {
	leftBytes, leftErr := left.Marshal()
	rightBytes, rightErr := right.Marshal()
	return leftErr == nil && rightErr == nil && string(leftBytes) == string(rightBytes)
}

func validResultContentType(capability, contentType string) bool {
	switch capability {
	case "text_to_image", "image_edit":
		return contentType == "image/jpeg" || contentType == "image/png" || contentType == "image/webp"
	case "image_to_video":
		return contentType == "video/mp4" || contentType == "video/quicktime" || contentType == "video/webm"
	default:
		return false
	}
}
