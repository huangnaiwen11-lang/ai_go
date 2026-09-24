package adminsubscription

import "context"

// Repository is the read-only boundary used by admin subscription analytics.
// It intentionally exposes no entitlement, payment, refund, or grant writes.
type Repository interface {
	SubscriptionOverview(context.Context, int) (Overview, error)
	SubscriptionTrends(context.Context, int) ([]Trend, error)
	SubscriptionBreakdown(context.Context) (Breakdown, error)
	SubscriptionSubscribers(context.Context, Query) (SubscribersPage, error)
}

type Query struct {
	Page, PageSize int
	Status         string
}

type Overview struct {
	ActiveSubscribers, NewSubscribers, CancelledSubscribers, ExpiredSubscribers int64
	ChurnRate, MRR, ARR, AvgSubscriptionDays                                    float64
	Expiring                                                                    Expiring
	RenewalForecast                                                             []RenewalForecast
}
type Expiring struct{ D7, D30, D90 int64 }
type RenewalForecast struct {
	Month               string
	Count               int64
	Revenue, RevenueNet float64
}
type Trend struct {
	Date                                                     string
	NewSubs, Cancelled, Expired, NetChange, ActiveCumulative int64
}
type Breakdown struct {
	ByTier, ByProvider, ByPeriod, ByChannel []BreakdownRow
	ByCountry                               []CountryRow
	RecentCancellations                     []Cancellation
}
type BreakdownRow struct {
	Key                 string
	Count               int64
	Revenue, RevenueNet float64
}
type CountryRow struct {
	Country             string
	CountryName         *string
	Count               int64
	Revenue, RevenueNet float64
}
type Cancellation struct {
	UserID                                            string
	Username, Country                                 *string
	Tier, BillingPeriod, PaymentProvider, CancelledAt string
	SubscribedDays                                    *int64
}
type SubscribersPage struct {
	Subscribers []Subscriber
	Total       int64
}
type Subscriber struct {
	SubscriptionID, UserID                                                                 string
	Username, Email, Country, CountryName                                                  *string
	Channel                                                                                string
	AuthProvider, UserRegisteredAt, LastSeenAt                                             *string
	Tier, BillingPeriod, Status, PaymentProvider                                           string
	PricePaid                                                                              *float64
	AutoRenew                                                                              bool
	StartDate, EndDate, CancelledAt                                                        *string
	CreatedAt                                                                              string
	SubscribedDays, DiamondClaimDays, WalletBalance                                        int64
	ImageCreditsRemaining, VideoCreditsRemaining, ImageCreditsGranted, VideoCreditsGranted int64
}
