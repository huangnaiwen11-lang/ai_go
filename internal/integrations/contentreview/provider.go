package contentreview

import (
	"context"

	bizreview "ai-business-service/internal/biz/contentreview"
	"ai-business-service/internal/conf"

	"github.com/google/wire"
)

// unavailableReviewer 在审核配置缺失或非法时保持普通服务可启动。
// 受限用户真正提交时，它会返回固定不可用错误，由工作者立即冲正而不是放行或没收。
type unavailableReviewer struct{}

// NewConfiguredReviewer 按本地主站配置装配审核器，构造过程不会发送任何网络请求。
func NewConfiguredReviewer(integrations *conf.Integrations) bizreview.Reviewer {
	if integrations == nil || integrations.GetContentReview() == nil {
		return unavailableReviewer{}
	}
	configuration := integrations.GetContentReview()
	client, err := NewClient(
		configuration.GetBaseUrl(),
		configuration.GetApiKey(),
		configuration.GetModel(),
		configuration.GetTimeout().AsDuration(),
		newConfiguredHTTPClient(),
	)
	if err != nil {
		return unavailableReviewer{}
	}
	return client
}

func (unavailableReviewer) Review(context.Context, bizreview.Request) (bizreview.Decision, error) {
	return bizreview.Decision{}, bizreview.ErrUnavailable
}

// ProviderSet 只装配审核器；任何 HTTP 调用只能由显式提交器执行。
var ProviderSet = wire.NewSet(NewConfiguredReviewer)

var _ bizreview.Reviewer = unavailableReviewer{}
