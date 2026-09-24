package generation

import (
	"context"
	"errors"
	"strings"
	"time"

	"ai-business-service/internal/biz/creations"
)

const maxSubmissionIdentifierLength = 512

var (
	// ErrInvalidSubmissionCommand 表示提交状态命令缺少安全且完整的领域字段。
	ErrInvalidSubmissionCommand = errors.New("generation: invalid submission command")
	// ErrSubmissionConflict 表示事件状态、租约或唯一任务标识的条件更新未命中。
	ErrSubmissionConflict = errors.New("generation: submission state conflict")
)

// ProviderRejectionCause 是提交被供应商确定拒绝时可持久化的内部原因。
// 它不是账本 ReversalReason，也不是用户钱包展示文案；未知拒绝必须保持空值。
type ProviderRejectionCause string

const (
	ProviderRejectionCausePaymentRequired ProviderRejectionCause = "provider_payment_required"
)

// SubmissionRecord 是已领取生成提交的最小技术与审核归属事实。
// 它不携带余额、权益、账本或支付相关字段。
type SubmissionRecord struct {
	Route          creations.ExecutionRoute
	EventID        string
	CreationID     string
	StepID         string
	CreationStatus creations.CreationStatus
	// UserID 仅用于读取用户已经固化的内容访问级别，绝不向生成中台透传。
	UserID        string
	ContentAccess string
	LeaseToken    string
	// LeaseOwner 与 Fence 是 B2B 绑定 CAS 的租约护栏；本地旧提交路径可不使用。
	LeaseOwner       string
	Fence            int32
	LeaseUntil       time.Time
	ExecutionPayload []byte
}

// SubmittedCommand 表示生成中台已经返回稳定任务标识后的结案命令。
type SubmittedCommand struct {
	EventID    string
	LeaseToken string
	JobID      string
	At         time.Time
}

// ReconcilingCommand 表示本次提交结果未知，需要重新核对。
//
// NextAttemptAt 必须由调用方显式给出，且不得早于 At：把「结果未知」退回成
// 「立即可再次领取」会让 429/503 退化成忙轮询——平台刚刚要求我们等待，
// 我们却在下一次轮询就再打一次。零值不是「尽快」，而是缺少调度决策。
type ReconcilingCommand struct {
	EventID       string
	LeaseToken    string
	At            time.Time
	NextAttemptAt time.Time
}

// RejectedCommand 表示本次提交已确定失败，需要由外层事务继续编排冲正和发件箱结案。
type RejectedCommand struct {
	EventID                string
	LeaseToken             string
	At                     time.Time
	ProviderRejectionCause ProviderRejectionCause
}

// ConfiscatedCommand 表示审核明确拒绝，需要由外层事务继续编排预留没收和发件箱结案。
type ConfiscatedCommand struct {
	EventID    string
	LeaseToken string
	At         time.Time
}

// SubmissionStore 定义提交器对已领取事件的读取和条件状态写入边界。
// 所有方法复用调用方提供的事务上下文，不得自行创建事务。
type SubmissionStore interface {
	ClaimedSubmission(context.Context, string) (*SubmissionRecord, error)
	MarkSubmitted(context.Context, SubmittedCommand) error
	MarkReconciling(context.Context, ReconcilingCommand) error
	MarkRejected(context.Context, RejectedCommand) error
	MarkConfiscated(context.Context, ConfiscatedCommand) error
}

// Validate 校验已提交结案命令。
func (command SubmittedCommand) Validate() error {
	_, err := command.Normalize()
	return err
}

// Normalize 校验并统一已提交结案命令的业务时间。
func (command SubmittedCommand) Normalize() (SubmittedCommand, error) {
	if !isSubmissionIdentifier(command.EventID) || !isSubmissionIdentifier(command.LeaseToken) || !isSubmissionIdentifier(command.JobID) || command.At.IsZero() {
		return SubmittedCommand{}, ErrInvalidSubmissionCommand
	}
	command.At = command.At.UTC()
	return command, nil
}

// Validate 校验重新核对命令。
func (command ReconcilingCommand) Validate() error {
	_, err := command.Normalize()
	return err
}

// Normalize 校验并统一重新核对命令的业务时间。
func (command ReconcilingCommand) Normalize() (ReconcilingCommand, error) {
	if !isSubmissionIdentifier(command.EventID) || !isSubmissionIdentifier(command.LeaseToken) ||
		command.At.IsZero() || command.NextAttemptAt.IsZero() || command.NextAttemptAt.Before(command.At) {
		return ReconcilingCommand{}, ErrInvalidSubmissionCommand
	}
	command.At = command.At.UTC()
	command.NextAttemptAt = command.NextAttemptAt.UTC()
	return command, nil
}

// Validate 校验提交拒绝命令。
func (command RejectedCommand) Validate() error {
	_, err := command.Normalize()
	return err
}

// Normalize 校验并统一提交拒绝命令的业务时间。
func (command RejectedCommand) Normalize() (RejectedCommand, error) {
	if !isSubmissionIdentifier(command.EventID) || !isSubmissionIdentifier(command.LeaseToken) || command.At.IsZero() {
		return RejectedCommand{}, ErrInvalidSubmissionCommand
	}
	if command.ProviderRejectionCause != "" && command.ProviderRejectionCause != ProviderRejectionCausePaymentRequired {
		return RejectedCommand{}, ErrInvalidSubmissionCommand
	}
	command.At = command.At.UTC()
	return command, nil
}

// Validate 校验审核没收命令。
func (command ConfiscatedCommand) Validate() error {
	_, err := command.Normalize()
	return err
}

// Normalize 校验并统一审核没收命令的业务时间。
func (command ConfiscatedCommand) Normalize() (ConfiscatedCommand, error) {
	if !isSubmissionIdentifier(command.EventID) || !isSubmissionIdentifier(command.LeaseToken) || command.At.IsZero() {
		return ConfiscatedCommand{}, ErrInvalidSubmissionCommand
	}
	command.At = command.At.UTC()
	return command, nil
}

func isSubmissionIdentifier(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, " \t\r\n") && len(value) <= maxSubmissionIdentifierLength
}
