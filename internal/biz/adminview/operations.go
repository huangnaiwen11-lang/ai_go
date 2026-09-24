package adminview

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalid             = errors.New("admin: invalid input")
	ErrForbidden           = errors.New("admin: forbidden")
	ErrNotFound            = errors.New("admin: not found")
	ErrConflict            = errors.New("admin: conflicting operation")
	ErrInsufficientBalance = errors.New("admin: insufficient balance")
)

type Query struct {
	Page, Limit                                                                                                   int
	Status, Role, Search, ExactEmail, LoginType, Platform, Client, Source, UserID, Email, Type, Provider, Country string
	Created, Seen                                                                                                 Window
}
type User struct {
	ID, Name, Email, Status, Role, Platform, Binding string
	CreatedAt, UpdatedAt                             time.Time
}
type UserPage struct {
	Users []User
	Total int64
}
type UserKPIs struct{ Total, Registered, Active, PWAActive int64 }
type LedgerEntry struct {
	ID, UserID, UserName, UserEmail, Reason, RefID string
	Delta                                          int64
	BalanceAfter                                   *int64
	CreatedAt                                      time.Time
}
type LedgerPage struct {
	Entries []LedgerEntry
	Total   int64
}
type Purchase struct {
	ID, UserID, UserName, UserEmail, Provider, ProviderTxnID, ProductID, Currency, Status string
	Coins, Cents                                                                          int64
	CreatedAt                                                                             time.Time
}
type PurchasePage struct {
	Purchases []Purchase
	Total     int64
}
type ProviderTotal struct{ Count, Cents int64 }
type WalletStats struct {
	TotalRevenue, TodayRevenue, TotalCoins, TodayCoins, TotalSpent, TodaySpent, TotalGranted int64
	ByProvider                                                                               map[string]ProviderTotal
}
type Wallet struct {
	User                             User
	Balance, TotalEarned, TotalSpent int64
	Entries                          []LedgerEntry
}
type Feedback struct {
	ID, UserID, Type, Message, Email, Status, Response, Note string
	User                                                     *User
	Attachments                                              []Attachment
	CreatedAt                                                time.Time
	ReviewedAt                                               *time.Time
}
type Attachment struct{ ID, URL string }
type FeedbackPage struct {
	Feedbacks []Feedback
	Total     int64
}
type FeedbackStats struct {
	Total, Pending, Resolved int64
	ByType                   map[string]int64
}
type Actor struct{ ID, Role string }
type UserChange struct{ UserID, Status, Role string }
type Adjustment struct {
	UserID, Key, Reason string
	Delta               int64
}
type AdjustmentResult struct {
	Balance  int64
	UserName string
}
type Reply struct{ ID, Response, Note string }

// Operations keeps domain validation separate from HTTP parsing and Mongo transactions.
type Operations struct{ repository Repository }

func NewOperations(r Repository) *Operations { return &Operations{repository: r} }
func isAdmin(a Actor) bool                   { return a.ID != "" && (a.Role == "admin" || a.Role == "super_admin") }
func (u *Operations) ChangeUser(ctx context.Context, a Actor, in UserChange) (User, error) {
	if !isAdmin(a) || a.ID == in.UserID {
		return User{}, ErrForbidden
	}
	if in.UserID == "" {
		return User{}, ErrInvalid
	}
	if (in.Role == "") == (in.Status == "") {
		return User{}, ErrInvalid
	}
	if in.Status != "" && in.Status != "active" && in.Status != "suspended" && in.Status != "deleted" {
		return User{}, ErrInvalid
	}
	if in.Role != "" && (a.Role != "super_admin" || (in.Role != "user" && in.Role != "admin" && in.Role != "editor")) {
		return User{}, ErrForbidden
	}
	return u.repository.ChangeUser(ctx, a, in)
}
func (u *Operations) Adjust(ctx context.Context, a Actor, in Adjustment) (AdjustmentResult, error) {
	if !isAdmin(a) {
		return AdjustmentResult{}, ErrForbidden
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.UserID == "" || in.Delta == 0 || in.Delta > 1000000 || in.Delta < -1000000 || len(in.Reason) < 3 || len(in.Reason) > 500 || len(in.Key) < 8 || len(in.Key) > 128 {
		return AdjustmentResult{}, ErrInvalid
	}
	return u.repository.Adjust(ctx, a, in)
}
func (u *Operations) Respond(ctx context.Context, a Actor, in Reply) error {
	if !isAdmin(a) {
		return ErrForbidden
	}
	in.Response = strings.TrimSpace(in.Response)
	in.Note = strings.TrimSpace(in.Note)
	if in.ID == "" || len([]rune(in.Response)) < 1 || len([]rune(in.Response)) > 2000 || len([]rune(in.Note)) > 2000 {
		return ErrInvalid
	}
	return u.repository.Respond(ctx, a, in)
}

// GrantSubscription 手工发放或续期订阅权益。
// 这是赠予，不是支付事实：只改 subscriptions 权益快照与审计，不写支付订单、不动钻石余额，
// 因此不会污染收入统计。
func (u *Operations) GrantSubscription(ctx context.Context, actor Actor, in SubscriptionGrant) (SubscriptionGrantResult, error) {
	if !isAdmin(actor) {
		return SubscriptionGrantResult{}, ErrForbidden
	}
	normalized, err := NormalizeSubscriptionGrant(in)
	if err != nil {
		return SubscriptionGrantResult{}, err
	}
	return u.repository.GrantSubscription(ctx, actor, normalized)
}
