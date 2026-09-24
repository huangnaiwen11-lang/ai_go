package generation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func inboxFixture(t *testing.T) ProviderInboxRecord {
	t.Helper()
	payload := []byte(`{"jobId":"job-1","status":"completed","output":{"resultUrl":"https://cdn.example/a.png"}}`)
	return ProviderInboxRecord{
		Source: "polarstar.b2b.v2", AccountRef: "account-a", DeliveryID: "delivery-1", StepID: "step-1", JobID: "job-1",
		PayloadDigest: digestInbox(payload), Payload: payload, Status: ProviderInboxPending, Attempts: 1,
		CreatedAt: time.Date(2026, 9, 18, 1, 2, 3, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 18, 1, 2, 3, 0, time.UTC),
	}
}

func digestInbox(payload []byte) string { s := sha256.Sum256(payload); return hex.EncodeToString(s[:]) }

func TestNewProviderInboxRecordValidatesAndCopiesPayload(t *testing.T) {
	payload := []byte(`{"jobId":"job-1","status":"processing"}`)
	r, err := NewProviderInboxRecord("account-a", "delivery-1", "step-1", "job-1", "", payload, 2, time.Date(2026, 9, 18, 1, 2, 3, 0, time.FixedZone("UTC+8", 8*60*60)))
	if err != nil {
		t.Fatal(err)
	}
	payload[0] = '!'
	if r.Status != ProviderInboxPending || r.Source != "polarstar.b2b.v2" || r.PayloadDigest != digestInbox(r.Payload) || r.CreatedAt.Location() != time.UTC || !bytes.Equal(r.Payload, []byte(`{"jobId":"job-1","status":"processing"}`)) {
		t.Fatalf("unexpected record: %#v", r)
	}
}

func TestProviderInboxRecordRejectsInvalidFacts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ProviderInboxRecord)
	}{
		{"source", func(r *ProviderInboxRecord) { r.Source = "generation.callback.v2" }},
		{"account whitespace", func(r *ProviderInboxRecord) { r.AccountRef = " account-a" }},
		{"delivery control", func(r *ProviderInboxRecord) { r.DeliveryID = "delivery\n1" }},
		{"empty job", func(r *ProviderInboxRecord) { r.JobID = "" }},
		{"payload digest", func(r *ProviderInboxRecord) { r.PayloadDigest = strings.Repeat("0", 64) }},
		{"uppercase digest", func(r *ProviderInboxRecord) { r.PayloadDigest = strings.ToUpper(r.PayloadDigest) }},
		{"terminal digest format", func(r *ProviderInboxRecord) { r.TerminalDigest = "terminal" }},
		{"payload too large", func(r *ProviderInboxRecord) {
			r.Payload = bytes.Repeat([]byte("x"), 1<<20+1)
			r.PayloadDigest = digestInbox(r.Payload)
		}},
		{"zero attempts", func(r *ProviderInboxRecord) { r.Attempts = 0 }},
		{"unknown status", func(r *ProviderInboxRecord) { r.Status = "applied-with-side-effect" }},
		{"applied missing terminal", func(r *ProviderInboxRecord) { r.Status = ProviderInboxApplied }},
		{"updated before created", func(r *ProviderInboxRecord) { r.UpdatedAt = r.CreatedAt.Add(-time.Second) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := inboxFixture(t)
			tc.mutate(&r)
			if err := r.Validate(); !errors.Is(err, ErrInvalidProviderInboxRecord) {
				t.Fatalf("Validate()=%v", err)
			}
		})
	}
}

func TestApplyProviderInboxSameDeliveryNoOp(t *testing.T) {
	existing := inboxFixture(t)
	incoming := existing
	incoming.Attempts = 8
	incoming.UpdatedAt = incoming.UpdatedAt.Add(time.Minute)
	incoming.Payload = append([]byte(nil), existing.Payload...)
	got, result, err := ApplyProviderInbox(&existing, incoming)
	if err != nil || result != InboxApplyNoop || got.Status != ProviderInboxPending || got.Attempts != existing.Attempts || !bytes.Equal(got.Payload, existing.Payload) {
		t.Fatalf("got=%#v result=%s err=%v", got, result, err)
	}
}

func TestApplyProviderInboxDifferentPayloadQuarantines(t *testing.T) {
	existing := inboxFixture(t)
	incoming := existing
	incoming.Payload = []byte(`{"jobId":"job-1","status":"failed"}`)
	incoming.PayloadDigest = digestInbox(incoming.Payload)
	got, result, err := ApplyProviderInbox(&existing, incoming)
	if !errors.Is(err, ErrProviderInboxConflict) || result != InboxApplyQuarantined || got.Status != ProviderInboxQuarantined {
		t.Fatalf("got=%#v result=%s err=%v", got, result, err)
	}
}

func TestApplyProviderInboxReplayPreservesLocalTerminalDigest(t *testing.T) {
	existing := inboxFixture(t)
	existing.TerminalDigest = strings.Repeat("a", 64)
	incoming := existing
	incoming.TerminalDigest = strings.Repeat("b", 64)
	got, result, err := ApplyProviderInbox(&existing, incoming)
	if err != nil || result != InboxApplyNoop || got.TerminalDigest != existing.TerminalDigest {
		t.Fatalf("got=%#v result=%s err=%v", got, result, err)
	}
}

func TestApplyProviderInboxNewAndInvalidExisting(t *testing.T) {
	incoming := inboxFixture(t)
	got, result, err := ApplyProviderInbox(nil, incoming)
	if err != nil || result != InboxApplyInserted || got.Status != ProviderInboxPending {
		t.Fatalf("new got=%#v result=%s err=%v", got, result, err)
	}
	bad := inboxFixture(t)
	bad.Status = "unknown"
	if _, _, err := ApplyProviderInbox(&bad, incoming); !errors.Is(err, ErrInvalidProviderInboxRecord) {
		t.Fatalf("invalid existing accepted: %v", err)
	}
}

func TestApplyProviderInboxDoesNotMutateInputs(t *testing.T) {
	existing := inboxFixture(t)
	incoming := existing
	incoming.Payload = append([]byte(nil), existing.Payload...)
	before := append([]byte(nil), existing.Payload...)
	got, _, _ := ApplyProviderInbox(&existing, incoming)
	got.Payload[0] = '!'
	if !bytes.Equal(existing.Payload, before) || !bytes.Equal(incoming.Payload, before) {
		t.Fatal("apply aliased payload")
	}
}

func TestApplyProviderInboxRejectsIncomingLifecycleState(t *testing.T) {
	for _, status := range []ProviderInboxStatus{ProviderInboxApplied, ProviderInboxQuarantined} {
		t.Run(string(status), func(t *testing.T) {
			incoming := inboxFixture(t)
			incoming.Status = status
			incoming.TerminalDigest = strings.Repeat("a", 64)
			if _, _, err := ApplyProviderInbox(nil, incoming); !errors.Is(err, ErrInvalidProviderInboxRecord) {
				t.Fatalf("incoming lifecycle accepted: %v", err)
			}
		})
	}
}
