package generation

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func terminalFixture() (ProviderTerminalSlot, ProviderTerminalFact) {
	now := time.Date(2026, 9, 18, 2, 3, 4, 0, time.UTC)
	slot := ProviderTerminalSlot{
		Provider: "polarstar_b2b_v2", AccountRef: "account-a", StepID: "step-1",
		ExternalExecutionID: "job-1", Capability: "text_to_image", AttemptFence: 7,
		LeaseToken: "lease-7", TerminalVersion: 0,
	}
	fact := ProviderTerminalFact{
		Provider: "polarstar_b2b_v2", AccountRef: "account-a", StepID: "step-1",
		ExternalExecutionID: "job-1", Capability: "text_to_image", Status: ProviderTerminalCompleted,
		ResultRef: "https://results.example.test/step-1.png", PayloadDigest: strings.Repeat("b", 64),
		AttemptFence: 7, LeaseToken: "lease-7", ExpectedVersion: 0, ObservedAt: now,
	}
	fact.TerminalDigest = ProviderTerminalSummaryDigest(fact.Status, fact.ResultRef)
	return slot, fact
}

func TestProviderTerminalQuarantinedPendingCannotApply(t *testing.T) {
	slot, fact := terminalFixture()
	slot.Quarantined = true
	slot.QuarantineReason = ErrProviderTerminalConflict.Error()
	updated, result, err := ApplyProviderTerminal(&slot, fact)
	if !errors.Is(err, ErrProviderTerminalConflict) || result != ProviderTerminalQuarantined || updated != slot {
		t.Fatalf("quarantined slot advanced: %#v / %s / %v", updated, result, err)
	}
}

func TestApplyProviderTerminalFirstWriterIncrementsVersion(t *testing.T) {
	slot, fact := terminalFixture()
	got, result, err := ApplyProviderTerminal(&slot, fact)
	if err != nil || result != ProviderTerminalApplied {
		t.Fatalf("got=%#v result=%s err=%v", got, result, err)
	}
	if got.TerminalVersion != 1 || got.Status != ProviderTerminalCompleted || got.ConfirmedAt.IsZero() {
		t.Fatalf("terminal slot not committed: %#v", got)
	}
	if slot.TerminalVersion != 0 || slot.Status != "" {
		t.Fatalf("apply mutated input slot: %#v", slot)
	}
}

func TestApplyProviderTerminalNilSlotRequiresInitialVersion(t *testing.T) {
	_, fact := terminalFixture()
	fact.ExpectedVersion = 1
	if _, result, err := ApplyProviderTerminal(nil, fact); !errors.Is(err, ErrProviderTerminalVersion) || result != ProviderTerminalQuarantined {
		t.Fatalf("nil stale version result=%s err=%v", result, err)
	}
}

func TestApplyProviderTerminalSameFactIsNoopEvenWithStaleVersionRead(t *testing.T) {
	slot, fact := terminalFixture()
	first, _, err := ApplyProviderTerminal(&slot, fact)
	if err != nil {
		t.Fatal(err)
	}
	replay := fact
	// A replay commonly carries the version read before the first callback won.
	replay.ExpectedVersion = 0
	replay.PayloadDigest = strings.Repeat("d", 64)
	replay.LeaseToken = ""
	got, result, err := ApplyProviderTerminal(&first, replay)
	if err != nil || result != ProviderTerminalNoop || got.TerminalVersion != 1 || got.Quarantined {
		t.Fatalf("got=%#v result=%s err=%v", got, result, err)
	}
}

func TestApplyProviderTerminalSignedQueryRefreshIsSemanticReplay(t *testing.T) {
	slot, fact := terminalFixture()
	first, result, err := ApplyProviderTerminal(&slot, fact)
	if err != nil || result != ProviderTerminalApplied {
		t.Fatalf("first terminal got=%#v result=%s err=%v", first, result, err)
	}
	replay := fact
	replay.ResultRef = "https://results.example.test/step-1.png?signature=rotated&expires=999"
	replay.TerminalDigest = ProviderTerminalSummaryDigest(replay.Status, replay.ResultRef)
	got, result, err := ApplyProviderTerminal(&first, replay)
	if err != nil || result != ProviderTerminalNoop || got.Quarantined {
		t.Fatalf("signed query refresh must replay: got=%#v result=%s err=%v", got, result, err)
	}
}

func TestApplyProviderTerminalBusinessQueryChangeQuarantines(t *testing.T) {
	slot, fact := terminalFixture()
	first, result, err := ApplyProviderTerminal(&slot, fact)
	if err != nil || result != ProviderTerminalApplied {
		t.Fatalf("first terminal got=%#v result=%s err=%v", first, result, err)
	}
	replay := fact
	replay.ResultRef = "https://results.example.test/step-1.png?token=business-variant"
	replay.TerminalDigest = ProviderTerminalSummaryDigest(replay.Status, replay.ResultRef)
	got, result, err := ApplyProviderTerminal(&first, replay)
	if !errors.Is(err, ErrProviderTerminalConflict) || result != ProviderTerminalQuarantined || !got.Quarantined {
		t.Fatalf("business query change must quarantine: got=%#v result=%s err=%v", got, result, err)
	}
}

func TestApplyProviderTerminalDifferentResultObjectQuarantines(t *testing.T) {
	slot, fact := terminalFixture()
	first, _, err := ApplyProviderTerminal(&slot, fact)
	if err != nil {
		t.Fatal(err)
	}
	for _, resultRef := range []string{
		"https://results.example.test/other.png?signature=rotated",
		"https://other.example.test/step-1.png?signature=rotated",
	} {
		replay := fact
		replay.ResultRef = resultRef
		replay.TerminalDigest = ProviderTerminalSummaryDigest(replay.Status, replay.ResultRef)
		got, result, err := ApplyProviderTerminal(&first, replay)
		if !errors.Is(err, ErrProviderTerminalConflict) || result != ProviderTerminalQuarantined || !got.Quarantined {
			t.Fatalf("different result object must quarantine: ref=%q got=%#v result=%s err=%v", resultRef, got, result, err)
		}
	}
}

func TestApplyProviderTerminalCompletedFailedRaceHasSingleWinner(t *testing.T) {
	slot, completed := terminalFixture()
	first, result, err := ApplyProviderTerminal(&slot, completed)
	if err != nil || result != ProviderTerminalApplied {
		t.Fatalf("first terminal got=%#v result=%s err=%v", first, result, err)
	}
	failed := completed
	failed.Status = ProviderTerminalFailed
	failed.ResultRef = ""
	failed.TerminalDigest = ProviderTerminalSummaryDigest(failed.Status, failed.ResultRef)
	failed.ExpectedVersion = 0
	got, result, err := ApplyProviderTerminal(&first, failed)
	if !errors.Is(err, ErrProviderTerminalConflict) || result != ProviderTerminalQuarantined || !got.Quarantined || got.Status != ProviderTerminalCompleted {
		t.Fatalf("race got=%#v result=%s err=%v", got, result, err)
	}
}

func TestApplyProviderTerminalFencesStaleLeaseAndVersion(t *testing.T) {
	slot, fact := terminalFixture()
	staleLease := fact
	staleLease.LeaseToken = "lease-old"
	got, result, err := ApplyProviderTerminal(&slot, staleLease)
	if !errors.Is(err, ErrProviderTerminalFence) || result != "" || got != slot {
		t.Fatalf("lease got=%#v result=%s err=%v", got, result, err)
	}

	// Stale worker state must not poison the slot used by a valid callback.
	slot, fact = terminalFixture()
	staleVersion := fact
	staleVersion.ExpectedVersion = 1
	got, result, err = ApplyProviderTerminal(&slot, staleVersion)
	if !errors.Is(err, ErrProviderTerminalVersion) || result != "" || got != slot {
		t.Fatalf("version got=%#v result=%s err=%v", got, result, err)
	}
}

func TestApplyProviderTerminalConflictingIdentityIsQuarantined(t *testing.T) {
	slot, fact := terminalFixture()
	fact.ExternalExecutionID = "job-other"
	got, result, err := ApplyProviderTerminal(&slot, fact)
	if !errors.Is(err, ErrProviderTerminalConflict) || result != ProviderTerminalQuarantined || !got.Quarantined {
		t.Fatalf("got=%#v result=%s err=%v", got, result, err)
	}
}

func TestProviderTerminalValidationRequiresNormalizedDigests(t *testing.T) {
	_, fact := terminalFixture()
	fact.TerminalDigest = "terminal"
	if !errors.Is(fact.Validate(), ErrInvalidProviderTerminal) {
		t.Fatal("short terminal digest accepted")
	}
	fact, _ = func() (ProviderTerminalFact, ProviderTerminalSlot) {
		slot, fact := terminalFixture()
		return fact, slot
	}()
	fact.AttemptFence = 0
	if !errors.Is(fact.Validate(), ErrInvalidProviderTerminal) {
		t.Fatal("zero attempt fence accepted")
	}
}
