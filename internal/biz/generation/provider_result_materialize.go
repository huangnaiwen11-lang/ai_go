package generation

import (
	"encoding/json"
)

// ProviderResultMaterializeEventType is emitted exactly once by the winning
// completed-terminal transaction. A result worker may retry this event, but a
// duplicate terminal must never allocate another materialization identity.
const ProviderResultMaterializeEventType = "generation.result-materialize"

// ProviderResultMaterializeEventPayload is the immutable hand-off from a
// verified B2B terminal to the long-running materializer. ResultRef remains a
// provider locator, not an authorization decision: the worker must still
// enforce its allowlist, DNS, redirect and media limits before fetching it.
type ProviderResultMaterializeEventPayload struct {
	CreationID      string `json:"creationId"`
	StepID          string `json:"stepId"`
	Provider        string `json:"provider"`
	AccountRef      string `json:"accountRef"`
	JobID           string `json:"jobId"`
	Capability      string `json:"capability"`
	ResultRef       string `json:"resultRef"`
	TerminalVersion int64  `json:"terminalVersion"`
	TerminalDigest  string `json:"terminalDigest"`
}

// ProviderResultMaterializeEventID is stable per step. The terminal version
// and digest live in the immutable payload so a stale worker cannot claim a
// different provider result under the same step identity.
func ProviderResultMaterializeEventID(stepID string) string {
	if !providerTerminalIdentity(stepID) {
		return ""
	}
	return ProviderResultMaterializeEventType + ":" + stepID
}

func MarshalProviderResultMaterializeEventPayload(payload ProviderResultMaterializeEventPayload) ([]byte, error) {
	if !payload.Validate() {
		return nil, ErrInvalidProviderTerminal
	}
	return json.Marshal(payload)
}

func ParseProviderResultMaterializeEventPayload(raw []byte) (ProviderResultMaterializeEventPayload, error) {
	var payload ProviderResultMaterializeEventPayload
	if err := json.Unmarshal(raw, &payload); err != nil || !payload.Validate() {
		return ProviderResultMaterializeEventPayload{}, ErrInvalidProviderTerminal
	}
	canonical, err := MarshalProviderResultMaterializeEventPayload(payload)
	if err != nil || string(canonical) != string(raw) {
		return ProviderResultMaterializeEventPayload{}, ErrInvalidProviderTerminal
	}
	return payload, nil
}

func (payload ProviderResultMaterializeEventPayload) Validate() bool {
	return providerTerminalIdentity(payload.CreationID) && ProviderResultMaterializeEventID(payload.StepID) != "" &&
		providerTerminalIdentity(payload.Provider) && providerTerminalIdentity(payload.AccountRef) && providerTerminalIdentity(payload.JobID) &&
		isFrozenCapability(payload.Capability) && validProviderResultRef(payload.ResultRef) && payload.TerminalVersion > 0 &&
		providerInboxDigest(payload.TerminalDigest) && payload.TerminalDigest == ProviderTerminalSummaryDigest(ProviderTerminalCompleted, payload.ResultRef)
}
