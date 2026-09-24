package adminview

import "context"

// Repository is the persistence boundary for management queries and audited operations.
type Repository interface {
	Overview(context.Context) (Overview, error)
	Trends(context.Context, string, Window) ([]Trend, error)
	RevenueBreakdowns(context.Context, Window) (RevenueBreakdown, error)
	Users(context.Context, Query) (UserPage, error)
	User(context.Context, string) (User, error)
	UserKPIs(context.Context, Query) (UserKPIs, error)
	ChangeUser(context.Context, Actor, UserChange) (User, error)
	Ledger(context.Context, Query) (LedgerPage, error)
	Purchases(context.Context, Query) (PurchasePage, error)
	PurchaseCountries(context.Context) ([]string, error)
	WalletStats(context.Context, Query) (WalletStats, error)
	Wallet(context.Context, string) (Wallet, error)
	Adjust(context.Context, Actor, Adjustment) (AdjustmentResult, error)
	Feedbacks(context.Context, Query) (FeedbackPage, error)
	FeedbackStats(context.Context) (FeedbackStats, error)
	Respond(context.Context, Actor, Reply) error
	GrantSubscription(context.Context, Actor, SubscriptionGrant) (SubscriptionGrantResult, error)
}
