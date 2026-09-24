package adminsubscription

import (
	"context"
	"testing"
)

type fakeRepository struct{}

func (fakeRepository) SubscriptionOverview(context.Context, int) (Overview, error) {
	return Overview{ActiveSubscribers: 2}, nil
}
func (fakeRepository) SubscriptionTrends(context.Context, int) ([]Trend, error) {
	return []Trend{{Date: "2026-09-18"}}, nil
}
func (fakeRepository) SubscriptionBreakdown(context.Context) (Breakdown, error) {
	return Breakdown{}, nil
}
func (fakeRepository) SubscriptionSubscribers(context.Context, Query) (SubscribersPage, error) {
	return SubscribersPage{Total: 1}, nil
}
func TestOperationsRejectInvalidDaysBeforeRepositoryCall(t *testing.T) {
	if _, err := NewOperations(fakeRepository{}).Overview(context.Background(), 0); err == nil {
		t.Fatal("Overview() error = nil, want invalid days")
	}
}
func TestOperationsDelegateReadOnlyQueries(t *testing.T) {
	got, err := NewOperations(fakeRepository{}).Overview(context.Background(), 30)
	if err != nil || got.ActiveSubscribers != 2 {
		t.Fatalf("Overview() = %#v, %v", got, err)
	}
	page, err := NewOperations(fakeRepository{}).Subscribers(context.Background(), Query{Page: 1, PageSize: 15, Status: "active"})
	if err != nil || page.Total != 1 {
		t.Fatalf("Subscribers() = %#v, %v", page, err)
	}
}
