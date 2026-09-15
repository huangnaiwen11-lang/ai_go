package ledger

import (
	"context"
	"time"
)

// Repository 定义账本领域所需的原子持久化操作。
// 余额和额度只通过条件占用方法变更，领域层不读取它们后再自行判断。
type Repository interface {
	FindReservation(context.Context, string) (*Reservation, error)
	FindDiamondBalance(context.Context, string) (int64, error)
	TryConsumeQuota(context.Context, string, QuotaReservation, time.Time) (bool, error)
	RestoreQuota(context.Context, string, QuotaReservation, time.Time) error
	TryDebitDiamonds(context.Context, string, int64, time.Time) (bool, error)
	CreditDiamonds(context.Context, string, int64, time.Time) error
	CreateReservation(context.Context, *Reservation) error
	TransitionReservation(context.Context, string, ReservationStatus, ReservationStatus, time.Time) (bool, error)
	AppendLedgerEntry(context.Context, *LedgerEntry) error
}
