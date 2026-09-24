package adminsubscription

import (
	"context"
	"errors"
)

var ErrDependenciesUnavailable = errors.New("admin subscription dependencies unavailable")

type Operations struct{ repository Repository }

func NewOperations(repository Repository) *Operations { return &Operations{repository: repository} }
func (o *Operations) Overview(ctx context.Context, days int) (Overview, error) {
	if o == nil || o.repository == nil {
		return Overview{}, ErrDependenciesUnavailable
	}
	if days < 1 || days > 365 {
		return Overview{}, errors.New("invalid days")
	}
	return o.repository.SubscriptionOverview(ctx, days)
}
func (o *Operations) Trends(ctx context.Context, days int) ([]Trend, error) {
	if o == nil || o.repository == nil {
		return nil, ErrDependenciesUnavailable
	}
	if days < 1 || days > 365 {
		return nil, errors.New("invalid days")
	}
	return o.repository.SubscriptionTrends(ctx, days)
}
func (o *Operations) Breakdown(ctx context.Context) (Breakdown, error) {
	if o == nil || o.repository == nil {
		return Breakdown{}, ErrDependenciesUnavailable
	}
	return o.repository.SubscriptionBreakdown(ctx)
}
func (o *Operations) Subscribers(ctx context.Context, query Query) (SubscribersPage, error) {
	if o == nil || o.repository == nil {
		return SubscribersPage{}, ErrDependenciesUnavailable
	}
	if query.Page < 1 || query.PageSize < 1 || query.PageSize > 100 {
		return SubscribersPage{}, errors.New("invalid pagination")
	}
	return o.repository.SubscriptionSubscribers(ctx, query)
}
