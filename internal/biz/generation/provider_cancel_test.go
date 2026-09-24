package generation

import (
	"errors"
	"testing"
)

func TestProviderCancelEventPayload冻结已绑定B2B任务身份(t *testing.T) {
	payload := ProviderCancelEventPayload{
		CreationID: "creation-1",
		StepID:     "step-1",
		Provider:   "polarstar_b2b_v2",
		AccountRef: "account-1",
		JobID:      "job-1",
		Capability: "text_to_image",
	}

	raw, err := MarshalProviderCancelEventPayload(payload)
	if err != nil {
		t.Fatalf("MarshalProviderCancelEventPayload() error = %v", err)
	}
	if got, want := string(raw), `{"accountRef":"account-1","capability":"text_to_image","creationId":"creation-1","jobId":"job-1","provider":"polarstar_b2b_v2","stepId":"step-1"}`; got != want {
		t.Fatalf("MarshalProviderCancelEventPayload() = %s, want %s", got, want)
	}
	parsed, err := ParseProviderCancelEventPayload(raw)
	if err != nil {
		t.Fatalf("ParseProviderCancelEventPayload() error = %v", err)
	}
	if parsed != payload {
		t.Fatalf("ParseProviderCancelEventPayload() = %#v, want %#v", parsed, payload)
	}
	if got, want := ProviderCancelEventID(payload.StepID), "generation.provider-cancel:step-1"; got != want {
		t.Fatalf("ProviderCancelEventID() = %q, want %q", got, want)
	}
}

func TestParseProviderCancelEventPayload拒绝非规范或不完整载荷(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"creationId":"creation-1","stepId":"step-1","provider":"polarstar_b2b_v2","accountRef":"account-1","jobId":"job-1","capability":"text_to_image"}`),
		[]byte(`{"accountRef":"account-1","capability":"text_to_image","creationId":"creation-1","jobId":"","provider":"polarstar_b2b_v2","stepId":"step-1"}`),
		[]byte(`{"accountRef":"account-1","capability":"text_to_image","creationId":"creation-1","extra":true,"jobId":"job-1","provider":"polarstar_b2b_v2","stepId":"step-1"}`),
		[]byte(`{"accountRef":"account-1","capability":"text_to_image","creationId":"creation-1","jobId":"job-1","provider":"polarstar_b2b_v2","stepId":"step-1"}{}`),
	} {
		if _, err := ParseProviderCancelEventPayload(raw); !errors.Is(err, ErrInvalidProviderCancel) {
			t.Fatalf("ParseProviderCancelEventPayload(%s) error = %v, want ErrInvalidProviderCancel", raw, err)
		}
	}
}
