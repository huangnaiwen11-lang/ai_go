package generation

import (
	"context"
	"errors"
	"strings"
	"time"
)

const maxSubmissionIdentifierLength = 512

var (
	// ErrInvalidSubmissionCommand 表示提交状态命令缺少安全且完整的领域字段。
	ErrInvalidSubmissionCommand = errors.New("generation: invalid submission command")
	// ErrSubmissionConflict 表示事件状态、租约或唯一任务标识的条件更新未命中。
	ErrSubmissionConflict = errors.New("generation: submission state conflict")
)

// SubmissionRecord 是已领取生成提交的最小技术与审核归属事实。
// 它不携带余额、权益、账本或支付相关字段。
type SubmissionRecord struct {
	EventID    string
	CreationID string
	StepID     string
	// UserID 仅用于读取用户已经固化的内容访问级别，绝不向生成中台透传。
	UserID           string
	ContentAccess    string
	LeaseToken       string
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
type ReconcilingCommand struct {
	EventID    string
	LeaseToken string
	At         time.Time
}

// RejectedCommand 表示本次提交已确定失败，需要由外层事务继续编排冲正和发件箱结案。
type RejectedCommand struct {
	EventID    string
	LeaseToken string
	At         time.Time
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
	if !isSubmissionIdentifier(command.EventID) || !isSubmissionIdentifier(command.LeaseToken) || command.At.IsZero() {
		return ReconcilingCommand{}, ErrInvalidSubmissionCommand
	}
	command.At = command.At.UTC()
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
