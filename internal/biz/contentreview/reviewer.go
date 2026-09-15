// Package contentreview 定义生成提交前的最小内容审核领域合同。
package contentreview

import (
	"context"
	"errors"
	"strings"
)

var (
	// ErrInvalidRequest 表示审核请求缺少可信的创作或步骤归属。
	ErrInvalidRequest = errors.New("content review: invalid request")
	// ErrInvalidDecision 表示审核端返回了领域无法处理的结论。
	ErrInvalidDecision = errors.New("content review: invalid decision")
	// ErrUnavailable 表示审核依赖无法给出可信结论，调用方必须按失败路径收敛。
	ErrUnavailable = errors.New("content review: unavailable")
)

// Outcome 表示内容审核的唯一有效结论。
type Outcome string

const (
	// OutcomeAllowed 表示内容可继续进入生成中台。
	OutcomeAllowed Outcome = "allowed"
	// OutcomeRejected 表示内容必须被没收，不能发送给生成中台。
	OutcomeRejected Outcome = "rejected"
)

// Request 是内容审核所需的最小文本事实。
// 它不承载用户身份、余额、权益、模板、素材地址、会话或支付信息。
type Request struct {
	CreationID     string
	StepID         string
	Prompt         string
	NegativePrompt string
}

// NewRequest 构造受控审核请求。创作和步骤标识不能为空，文本可以为空以兼容现网默认提示词。
func NewRequest(creationID, stepID, prompt, negativePrompt string) (Request, error) {
	request := Request{
		CreationID:     strings.TrimSpace(creationID),
		StepID:         strings.TrimSpace(stepID),
		Prompt:         prompt,
		NegativePrompt: negativePrompt,
	}
	if request.CreationID == "" || request.StepID == "" {
		return Request{}, ErrInvalidRequest
	}
	return request, nil
}

// Decision 是审核端可返回的稳定结论。ReasonCode 仅允许持久化安全分类，不保存模型原文理由。
type Decision struct {
	Outcome    Outcome
	ReasonCode string
}

// Validate 拒绝未知结论，避免审核异常被误判为允许。
func (decision Decision) Validate() error {
	if decision.Outcome != OutcomeAllowed && decision.Outcome != OutcomeRejected {
		return ErrInvalidDecision
	}
	return nil
}

// Reviewer 是审核服务的窄反转边界。实现可以使用 HTTP，但业务层不依赖具体供应商。
type Reviewer interface {
	Review(context.Context, Request) (Decision, error)
}
