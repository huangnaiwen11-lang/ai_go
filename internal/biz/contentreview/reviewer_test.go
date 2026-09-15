package contentreview_test

import (
	"errors"
	"testing"

	"ai-business-service/internal/biz/contentreview"
)

// 审核请求只允许携带内容审核所需的最小文本事实，避免审核边界接触资金或身份数据。
func TestNewRequestAcceptsMinimalPromptFacts(t *testing.T) {
	request, err := contentreview.NewRequest("creation-1", "step-1", "安全的肖像", "不要血腥")
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if request.CreationID != "creation-1" || request.StepID != "step-1" {
		t.Fatalf("request identifiers = %#v", request)
	}
}

// 未知审核结论不能被工作者当作允许或拒绝处理，必须在领域边界失败关闭。
func TestDecisionRejectsUnknownOutcome(t *testing.T) {
	err := (contentreview.Decision{Outcome: contentreview.Outcome("unknown")}).Validate()
	if !errors.Is(err, contentreview.ErrInvalidDecision) {
		t.Fatalf("Decision.Validate() error = %v, want ErrInvalidDecision", err)
	}
}
