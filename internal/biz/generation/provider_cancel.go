package generation

import (
	"bytes"
	"encoding/json"
	"errors"

	"ai-business-service/internal/biz/creations"
)

// ProviderCancelEventType is emitted once per already-bound B2B step after an
// owner has requested cancellation.  Delivery only asks the provider to stop;
// it never creates a local terminal or changes the reservation.
const ProviderCancelEventType = "generation.provider-cancel"

var ErrInvalidProviderCancel = errors.New("generation: invalid provider cancel event")

// ProviderCancelEventPayload is the immutable hand-off to the provider
// cancellation worker.  Every routing identity is copied from the already
// frozen step/job binding, never taken from an HTTP request.
type ProviderCancelEventPayload struct {
	AccountRef string `json:"accountRef"`
	Capability string `json:"capability"`
	CreationID string `json:"creationId"`
	JobID      string `json:"jobId"`
	Provider   string `json:"provider"`
	StepID     string `json:"stepId"`
}

// ProviderCancelEventID returns the stable, per-step cancellation outbox ID.
// A repeated user request therefore cannot dispatch a second cancel operation.
func ProviderCancelEventID(stepID string) string {
	if !providerJobIdentifier(stepID) {
		return ""
	}
	return ProviderCancelEventType + ":" + stepID
}

func (payload ProviderCancelEventPayload) Validate() bool {
	return providerJobIdentifier(payload.CreationID) &&
		providerJobIdentifier(payload.StepID) &&
		payload.Provider == creations.PolarStarB2BProvider &&
		providerJobIdentifier(payload.AccountRef) &&
		providerJobIdentifier(payload.JobID) &&
		validProviderCapability(payload.Capability) &&
		ProviderCancelEventID(payload.StepID) != ""
}

// MarshalProviderCancelEventPayload produces canonical JSON.  The byte form
// is used as a Mongo identity guard, so equivalent semantic payloads with a
// different encoding are rejected rather than silently normalized later.
func MarshalProviderCancelEventPayload(payload ProviderCancelEventPayload) ([]byte, error) {
	if !payload.Validate() {
		return nil, ErrInvalidProviderCancel
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrInvalidProviderCancel
	}
	return raw, nil
}

// ParseProviderCancelEventPayload only accepts the exact canonical encoding
// created above.  This prevents an outbox row from smuggling unknown data into
// the worker or changing the frozen route through duplicate JSON keys.
func ParseProviderCancelEventPayload(raw []byte) (ProviderCancelEventPayload, error) {
	var payload ProviderCancelEventPayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return ProviderCancelEventPayload{}, ErrInvalidProviderCancel
	}
	if decoder.More() {
		return ProviderCancelEventPayload{}, ErrInvalidProviderCancel
	}
	canonical, err := MarshalProviderCancelEventPayload(payload)
	if err != nil || !bytes.Equal(canonical, raw) {
		return ProviderCancelEventPayload{}, ErrInvalidProviderCancel
	}
	return payload, nil
}
