package generation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/outbox"
)

// FrozenProviderRequest is the already-mapped wire request, never credentials.
// Recovery must validate it through the protocol adapter before any network I/O.
type FrozenProviderRequest struct {
	Route                                      creations.ExecutionRoute
	StepID, Capability, IdempotencyKey, Digest string
	Payload                                    []byte
}

// PrepareProviderSubmissionCommand consumes an active submission lease once.
// Fence is the monotonically increasing Outbox attempt_count; At is UTC business time.
type PrepareProviderSubmissionCommand struct {
	EventID, CreationID, LeaseToken, LeaseOwner string
	Fence                                       int32
	At                                          time.Time
	Request                                     FrozenProviderRequest
}

type ProviderSubmissionIntent struct {
	Request    FrozenProviderRequest
	PreparedAt time.Time
	Fence      int32
	// ExternalExecutionID 是供应商返回并被绑定到步骤的外部任务号。
	//
	// 空值意味着「授权已发出但结果未知」：外部任务可能已被受理，只是还没绑定。
	// 对账路径必须据此区分「可以按外部任务号核对」与「只能等提交路径收敛」，
	// 而不是把两种情况都当成同一件事。
	ExternalExecutionID string
}

// ProviderSubmissionStore requires an outer transaction for preparation.
// A successful preparation is not idempotent permission to POST again: any
// already-prepared step returns ErrSubmissionConflict and must use recovery.
//
// 唯一的例外是 ProviderReauthorizationStore：对账**证明**平台从未受理这笔任务时
// （按确定派生的幂等键查询得到明确 404），可以消费一次有审计的重新授权。
// 除此之外任何路径都不得重发。
type ProviderSubmissionStore interface {
	PrepareProviderSubmission(context.Context, PrepareProviderSubmissionCommand) error
	ReadProviderSubmission(context.Context, string) (*ProviderSubmissionIntent, error)
}

// ErrReauthorizationExhausted 表示该步骤的重新授权额度已经用完。
//
// 它不是错误路径，而是「重发已经做过了」的正常事实：调用方必须回到对账，
// 绝不能据此再造一次授权，也不能据此判定任务失败。
var ErrReauthorizationExhausted = errors.New("provider submission: reauthorization exhausted")

// 重新授权的审计原因。取值受限于这里，避免把供应商响应写进本地事实。
const (
	// ReauthorizationReasonLookupNotFound 表示平台按幂等键明确返回 404。
	ReauthorizationReasonLookupNotFound = "lookup_not_found"
)

// ReauthorizeProviderSubmissionCommand 请求消费唯一一次「重新授权提交」。
//
// 它只在对账已经**证明**平台从未受理该步骤时使用：按确定派生的幂等键查询得到
// 明确的 404。平台对同一幂等键的重放是幂等的，因此重发至多产生一笔任务。
//
// 与 PrepareProviderSubmissionCommand 一样携带租约与围栏：过期的对账工作者
// 不得消费额度，否则「唯一一次」会被并行重试悄悄消耗掉。
type ReauthorizeProviderSubmissionCommand struct {
	EventID, CreationID, StepID, LeaseToken, LeaseOwner string
	Fence                                               int32
	At                                                  time.Time
	// Reason 是有限枚举的审计原因，不是自由文本。
	Reason string
}

func (c ReauthorizeProviderSubmissionCommand) Validate() error {
	if !isSubmissionIdentifier(c.CreationID) || !isSubmissionIdentifier(c.LeaseToken) || !isSubmissionIdentifier(c.LeaseOwner) ||
		c.At.IsZero() || c.Fence <= 0 || outbox.SubmissionEventID(c.StepID) == "" ||
		c.EventID != outbox.SubmissionEventID(c.StepID) ||
		c.Reason != ReauthorizationReasonLookupNotFound {
		return ErrInvalidSubmissionCommand
	}
	return nil
}

// ProviderReauthorizationStore 是「重新授权一次提交」的可选能力边界。
//
// 它刻意与 ProviderSubmissionStore 分开：只读回冻结意图的实现不需要、也不应该
// 拿到重发能力。缺失时调用方必须保持既有语义（只对账，不重发），
// 而不是降级到别的路径。
type ProviderReauthorizationStore interface {
	// ReauthorizeProviderSubmission 原子地消费唯一一次重新授权并写下审计事实。
	//
	// 成功返回 nil，此后调用方可以用同一份冻结字节重发。
	// 额度已用完返回 ErrReauthorizationExhausted。
	ReauthorizeProviderSubmission(context.Context, ReauthorizeProviderSubmissionCommand) error
}

func (c PrepareProviderSubmissionCommand) Validate() error {
	if !isSubmissionIdentifier(c.CreationID) || !isSubmissionIdentifier(c.LeaseToken) || !isSubmissionIdentifier(c.LeaseOwner) ||
		c.At.IsZero() || c.Fence <= 0 || c.EventID != outbox.SubmissionEventID(c.Request.StepID) {
		return ErrInvalidSubmissionCommand
	}
	return c.Request.Validate()
}

// Validate checks durable identity and integrity. The protocol adapter owns the
// stricter wire allowlist; callers must restore through it before transmission.
func (r FrozenProviderRequest) Validate() error {
	route, err := creations.NormalizeExecutionRoute(r.Route)
	if err != nil || route.Provider != creations.PolarStarB2BProvider ||
		len(r.StepID) > 189 || outbox.SubmissionEventID(r.StepID) == "" ||
		r.IdempotencyKey != "cling-step:"+r.StepID || len(r.Payload) == 0 || len(r.Payload) > 128<<10 || !json.Valid(r.Payload) {
		return ErrInvalidSubmissionCommand
	}
	if r.Capability != "text_to_image" && r.Capability != "image_edit" && r.Capability != "image_to_video" {
		return ErrInvalidSubmissionCommand
	}
	sum := sha256.Sum256(r.Payload)
	if r.Digest != hex.EncodeToString(sum[:]) {
		return ErrInvalidSubmissionCommand
	}
	var identity struct {
		ExternalID string `json:"externalId"`
		Key        string `json:"idempotencyKey"`
		Capability string `json:"capability"`
	}
	if json.Unmarshal(r.Payload, &identity) != nil || identity.ExternalID != r.StepID || identity.Key != r.IdempotencyKey || identity.Capability != r.Capability {
		return ErrInvalidSubmissionCommand
	}
	return nil
}
