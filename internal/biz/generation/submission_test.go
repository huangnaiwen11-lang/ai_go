package generation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
	if err := (ReconcilingCommand{EventID: command.EventID, LeaseToken: command.LeaseToken, At: at}).Validate(); err != nil {
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
