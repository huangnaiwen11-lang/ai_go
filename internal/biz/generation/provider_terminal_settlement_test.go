package generation

import (
	"strings"
	"testing"
	"time"
)

func TestProviderTerminalSettlement接受OutboxURLSafeBase64租约(t *testing.T) {
	payload := ProviderTerminalSettlementEventPayload{
		CreationID: "creation-1", StepID: "step-1", Provider: "polarstar_b2b_v2", AccountRef: "account-1",
		JobID: "job-1", Capability: "text_to_image", Status: ProviderTerminalFailed, TerminalVersion: 1,
		TerminalDigest: ProviderTerminalSummaryDigest(ProviderTerminalFailed, ""),
	}
	settlement := ProviderTerminalSettlement{
		EventID: ProviderTerminalSettlementEventID(payload.StepID),
		// Outbox uses base64.RawURLEncoding; '-' and '_' are valid as its first
		// byte even though they are not valid at the start of a step identity.
		LeaseToken: "_" + strings.Repeat("a", 42),
		Target:     ProviderTerminalSettlementTarget{Payload: payload},
		SettledAt:  time.Date(2026, time.September, 19, 18, 0, 0, 0, time.UTC),
	}
	if _, err := settlement.Normalize(); err != nil {
		t.Fatalf("URL-safe Base64 outbox lease rejected: %v", err)
	}
}
