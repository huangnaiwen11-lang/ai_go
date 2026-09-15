package contentreview

import (
	"context"
	"errors"
	"testing"

	bizreview "ai-business-service/internal/biz/contentreview"
	"ai-business-service/internal/conf"
)

// 审核配置缺失不能阻断普通服务装配；真正遇到受限任务时必须以不可用错误安全失败。
func TestNewConfiguredReviewer配置缺失返回不可用审核器(t *testing.T) {
	reviewer := NewConfiguredReviewer(&conf.Integrations{})
	if reviewer == nil {
		t.Fatal("NewConfiguredReviewer() = nil")
	}
	request, err := bizreview.NewRequest("creation-1", "step-1", "safe portrait", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = reviewer.Review(context.Background(), request)
	if !errors.Is(err, bizreview.ErrUnavailable) {
		t.Fatalf("Review() error = %v, want ErrUnavailable", err)
	}
}
