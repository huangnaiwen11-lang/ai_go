package generation

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const ProviderTerminalSettlementEventType = "generation.terminal-settle"

var (
	// ErrInvalidProviderTerminalSettlement rejects an event or persistence
	// command that is not bound to a failed/cancelled terminal fact.
	ErrInvalidProviderTerminalSettlement = errors.New("generation: invalid provider terminal settlement")
	// ErrProviderTerminalSettlementConflict means the terminal slot, creation,
	// reservation gate, or worker lease no longer authorizes a refund. It is
	// deliberately distinct from a transport error: retrying must never turn a
	// possibly published result into a refund.
	ErrProviderTerminalSettlementConflict = errors.New("generation: provider terminal settlement conflict")
)

// ProviderTerminalSettlementEventPayload is the immutable hand-off emitted
// by a winning failed/cancelled terminal CAS. It contains no provider payload
// or result URL: the only permitted effect is to settle the already-persisted
// terminal conclusion and its frozen reservation.
type ProviderTerminalSettlementEventPayload struct {
	CreationID      string                 `json:"creationId"`
	StepID          string                 `json:"stepId"`
	Provider        string                 `json:"provider"`
	AccountRef      string                 `json:"accountRef"`
	JobID           string                 `json:"jobId"`
	Capability      string                 `json:"capability"`
	Status          ProviderTerminalStatus `json:"status"`
	TerminalVersion int64                  `json:"terminalVersion"`
	TerminalDigest  string                 `json:"terminalDigest"`
}

// ProviderTerminalSettlementEventID is stable per step. A terminal has one
// terminal slot, so a replay must never create a second refund authority.
func ProviderTerminalSettlementEventID(stepID string) string {
	if !providerTerminalIdentity(stepID) {
		return ""
	}
	return ProviderTerminalSettlementEventType + ":" + stepID
}

func MarshalProviderTerminalSettlementEventPayload(payload ProviderTerminalSettlementEventPayload) ([]byte, error) {
	if !payload.Validate() {
		return nil, ErrInvalidProviderTerminalSettlement
	}
	return json.Marshal(payload)
}

func ParseProviderTerminalSettlementEventPayload(raw []byte) (ProviderTerminalSettlementEventPayload, error) {
	var payload ProviderTerminalSettlementEventPayload
	if err := json.Unmarshal(raw, &payload); err != nil || !payload.Validate() {
		return ProviderTerminalSettlementEventPayload{}, ErrInvalidProviderTerminalSettlement
	}
	canonical, err := MarshalProviderTerminalSettlementEventPayload(payload)
	if err != nil || string(canonical) != string(raw) {
		return ProviderTerminalSettlementEventPayload{}, ErrInvalidProviderTerminalSettlement
	}
	return payload, nil
}

func (payload ProviderTerminalSettlementEventPayload) Validate() bool {
	return providerTerminalIdentity(payload.CreationID) && ProviderTerminalSettlementEventID(payload.StepID) != "" &&
		providerTerminalIdentity(payload.Provider) && providerTerminalIdentity(payload.AccountRef) && providerTerminalIdentity(payload.JobID) &&
		isFrozenCapability(payload.Capability) && (payload.Status == ProviderTerminalFailed || payload.Status == ProviderTerminalCancelled) &&
		payload.TerminalVersion > 0 && providerInboxDigest(payload.TerminalDigest) &&
		payload.TerminalDigest == ProviderTerminalSummaryDigest(payload.Status, "")
}

// ProviderTerminalSettlementTarget is the storage-verified terminal snapshot.
// The worker must load it before it begins a transaction; Settle re-reads it
// inside that transaction before it can mark a creation failed.
type ProviderTerminalSettlementTarget struct {
	Payload ProviderTerminalSettlementEventPayload
}

func (target ProviderTerminalSettlementTarget) Validate() error {
	if !target.Payload.Validate() {
		return ErrInvalidProviderTerminalSettlement
	}
	return nil
}

// ProviderTerminalSettlement records the lease holder that has already
// reversed the reservation in its surrounding transaction.
type ProviderTerminalSettlement struct {
	EventID    string
	LeaseToken string
	Target     ProviderTerminalSettlementTarget
	SettledAt  time.Time
}

func (settlement ProviderTerminalSettlement) Normalize() (ProviderTerminalSettlement, error) {
	if !providerTerminalIdentity(settlement.EventID) || !validProviderOutboxLeaseToken(settlement.LeaseToken) ||
		settlement.EventID != ProviderTerminalSettlementEventID(settlement.Target.Payload.StepID) ||
		settlement.Target.Validate() != nil || settlement.SettledAt.IsZero() {
		return ProviderTerminalSettlement{}, ErrInvalidProviderTerminalSettlement
	}
	settlement.SettledAt = settlement.SettledAt.UTC()
	return settlement, nil
}

func (settlement ProviderTerminalSettlement) Validate() error {
	_, err := settlement.Normalize()
	return err
}

// ProviderTerminalSettlementStore is the persistence boundary shared by the
// refund worker and its exact terminal CAS. The worker must call ledger
// ReverseInTx before SettleProviderTerminal in the same transaction.
type ProviderTerminalSettlementStore interface {
	LoadProviderTerminalSettlement(context.Context, ProviderTerminalSettlementEventPayload) (ProviderTerminalSettlementTarget, error)
	SettleProviderTerminal(context.Context, ProviderTerminalSettlement) error
}
