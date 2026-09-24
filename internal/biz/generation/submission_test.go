package generation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
)

type submissionStoreContract struct{}

func (submissionStoreContract) ClaimedSubmission(context.Context, string) (*SubmissionRecord, error) {
	return nil, nil
}

func (submissionStoreContract) MarkSubmitted(context.Context, SubmittedCommand) error { return nil }

func (submissionStoreContract) MarkReconciling(context.Context, ReconcilingCommand) error { return nil }

func (submissionStoreContract) MarkRejected(context.Context, RejectedCommand) error { return nil }

func (submissionStoreContract) MarkConfiscated(context.Context, ConfiscatedCommand) error { return nil }

var _ SubmissionStore = submissionStoreContract{}

func Test提交状态命令校验并规范化业务时间(t *testing.T) {
	at := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	command := SubmittedCommand{
		EventID:    "generation.submission:step-1",
		LeaseToken: "lease-1",
		JobID:      "job-1",
		At:         at,
	}

	normalized, err := command.Normalize()
	if err != nil {
		t.Fatalf("SubmittedCommand.Normalize() error = %v", err)
	}
	if normalized.At.Location() != time.UTC || !normalized.At.Equal(at.UTC()) {
		t.Fatalf("normalized.At = %s, want %s UTC", normalized.At, at.UTC())
	}
	if err := (ReconcilingCommand{EventID: command.EventID, LeaseToken: command.LeaseToken, At: at, NextAttemptAt: at.Add(time.Minute)}).Validate(); err != nil {
		t.Fatalf("ReconcilingCommand.Validate() error = %v", err)
	}
	if err := (RejectedCommand{EventID: command.EventID, LeaseToken: command.LeaseToken, At: at}).Validate(); err != nil {
		t.Fatalf("RejectedCommand.Validate() error = %v", err)
	}
	if err := (ConfiscatedCommand{EventID: command.EventID, LeaseToken: command.LeaseToken, At: at}).Validate(); err != nil {
		t.Fatalf("ConfiscatedCommand.Validate() error = %v", err)
	}
}

func Test提交状态命令拒绝不安全标识且不回显输入(t *testing.T) {
	cases := []struct {
		name string
		call func() error
	}{
		{"空事件", func() error {
			return (SubmittedCommand{LeaseToken: "lease-1", JobID: "job-1", At: time.Now()}).Validate()
		}},
		{"租约空白", func() error {
			return (SubmittedCommand{EventID: "generation.submission:step-1", LeaseToken: " lease-1", JobID: "job-1", At: time.Now()}).Validate()
		}},
		{"任务空白", func() error {
			return (SubmittedCommand{EventID: "generation.submission:step-1", LeaseToken: "lease-1", JobID: "job 1", At: time.Now()}).Validate()
		}},
		{"零时间", func() error {
			return (RejectedCommand{EventID: "generation.submission:step-1", LeaseToken: "lease-1"}).Validate()
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.call()
			if !errors.Is(err, ErrInvalidSubmissionCommand) {
				t.Fatalf("validation error = %v, want ErrInvalidSubmissionCommand", err)
			}
			if err != nil && (strings.Contains(err.Error(), "lease-1") || strings.Contains(err.Error(), "job 1")) {
				t.Fatalf("validation error leaked command input: %q", err)
			}
		})
	}
}

func TestRejectedCommandCause仅允许已知供应商原因(t *testing.T) {
	base := RejectedCommand{EventID: "generation.submission:step-1", LeaseToken: "lease-1", At: time.Now()}
	if _, err := base.Normalize(); err != nil {
		t.Fatalf("空 cause 应合法: %v", err)
	}
	base.ProviderRejectionCause = ProviderRejectionCausePaymentRequired
	if _, err := base.Normalize(); err != nil {
		t.Fatalf("provider_payment_required 应合法: %v", err)
	}
	base.ProviderRejectionCause = ProviderRejectionCause("secret-or-provider-message")
	if _, err := base.Normalize(); !errors.Is(err, ErrInvalidSubmissionCommand) {
		t.Fatalf("未知 cause = %v, want ErrInvalidSubmissionCommand", err)
	}
}

func TestProviderJobBindingNormalizeRequiresFrozenB2BIdentity(t *testing.T) {
	base := ProviderJobBinding{
		EventID: "generation.submission:step-1", CreationID: "creation-1", StepID: "step-1",
		LeaseToken: "lease-1", LeaseOwner: "worker-1", Fence: 1,
		Route:      creations.ExecutionRoute{Provider: creations.PolarStarB2BProvider, AccountRef: "account-a", ContractVersion: creations.B2BContractVersion, MappingVersion: "mapping-1"},
		ExternalID: "job-1", Capability: "text_to_image", At: time.Now(),
	}
	if _, err := base.Normalize(); err != nil {
		t.Fatalf("valid B2B binding rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ProviderJobBinding){
		"local provider":      func(v *ProviderJobBinding) { v.Route = creations.LocalExecutionRoute() },
		"wrong event step":    func(v *ProviderJobBinding) { v.EventID = "generation.submission:step-2" },
		"missing lease owner": func(v *ProviderJobBinding) { v.LeaseOwner = "" },
		"missing fence":       func(v *ProviderJobBinding) { v.Fence = 0 },
		"unknown capability":  func(v *ProviderJobBinding) { v.Capability = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			if !errors.Is(candidate.Validate(), ErrInvalidProviderJobBinding) {
				t.Fatalf("Validate() = %v, want ErrInvalidProviderJobBinding", candidate.Validate())
			}
		})
	}
}
