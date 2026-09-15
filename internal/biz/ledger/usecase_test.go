package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/shared"
)

const (
	testUserID         = "user-1"
	testOtherUserID    = "user-2"
	testQuotaKind      = "vip_daily_image"
	testQuotaLocalDate = "2026-09-05"
)

func TestReserveInTx复用调用方事务且不创建嵌套事务(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	runner := &countingTxRunner{}
	usecase := ledger.NewUsecase(repository, runner)

	reservation, err := usecase.ReserveInTx(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-in-existing-transaction",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         disabledQuota(),
		BusinessAt:    testBusinessAt(),
	})

	if err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}
	if reservation.Status != ledger.ReservationStatusReserved {
		t.Fatalf("Reservation.Status = %q, want reserved", reservation.Status)
	}
	if got := repository.balance(testUserID); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
	if got := repository.reservationCount(); got != 1 {
		t.Fatalf("reservation count = %d, want 1", got)
	}
	if got := len(repository.ledgerEntries()); got != 1 {
		t.Fatalf("ledger entry count = %d, want 1", got)
	}
	if runner.calls != 0 {
		t.Fatalf("WithinTx calls = %d, want 0", runner.calls)
	}
}

func TestReverseInTx复用调用方事务且只冲正一次(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	runner := &countingTxRunner{}
	usecase := ledger.NewUsecase(repository, runner)
	creationID := "creation-reverse-in-existing-transaction"

	if _, err := usecase.ReserveInTx(context.Background(), ledger.ReserveRequest{
		CreationID:    creationID,
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         disabledQuota(),
		BusinessAt:    testBusinessAt(),
	}); err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}

	reversed, err := usecase.ReverseInTx(context.Background(), creationID, ledger.ReversalReasonSubmissionRejected, testBusinessAt().Add(time.Minute))
	if err != nil {
		t.Fatalf("ReverseInTx() error = %v", err)
	}
	if reversed.Status != ledger.ReservationStatusReversed {
		t.Fatalf("Reservation.Status = %q, want reversed", reversed.Status)
	}
	if runner.calls != 0 {
		t.Fatalf("WithinTx calls = %d, want 0", runner.calls)
	}
	if got := repository.balance(testUserID); got != 20 {
		t.Fatalf("balance = %d, want 20 after reversal", got)
	}
	if got := len(repository.ledgerEntries()); got != 2 {
		t.Fatalf("ledger entry count = %d, want 2", got)
	}

	again, err := usecase.ReverseInTx(context.Background(), creationID, ledger.ReversalReasonSubmissionRejected, testBusinessAt().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("second ReverseInTx() error = %v", err)
	}
	if again.Status != ledger.ReservationStatusReversed {
		t.Fatalf("second Reservation.Status = %q, want reversed", again.Status)
	}
	if got := repository.balance(testUserID); got != 20 {
		t.Fatalf("balance = %d, want 20 without double reversal", got)
	}
	if got := len(repository.ledgerEntries()); got != 2 {
		t.Fatalf("ledger entry count = %d, want 2 without duplicate reversal entry", got)
	}
}

// 审核没收只改变预留终态，既不能退款，也不能归还已占用的日免额度。
func TestConfiscateInTx不退款也不恢复日免(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 0)
	repository.setQuota(testUserID, testQuota(), 1)
	runner := &countingTxRunner{}
	usecase := ledger.NewUsecase(repository, runner)
	creationID := "creation-confiscate-in-transaction"

	if _, err := usecase.ReserveInTx(context.Background(), ledger.ReserveRequest{
		CreationID:    creationID,
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
		BusinessAt:    testBusinessAt(),
	}); err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}
	beforeBalance := repository.balance(testUserID)
	beforeQuota := repository.quota(testUserID, testQuota())
	beforeEntries := len(repository.ledgerEntries())

	reservation, err := usecase.ConfiscateInTx(context.Background(), creationID, "audit_rejected", testBusinessAt().Add(time.Minute))
	if err != nil {
		t.Fatalf("ConfiscateInTx() error = %v", err)
	}
	if reservation.Status != ledger.ReservationStatusConfiscated {
		t.Fatalf("Reservation.Status = %q, want confiscated", reservation.Status)
	}
	if got := repository.balance(testUserID); got != beforeBalance {
		t.Fatalf("balance = %d, want %d without refund", got, beforeBalance)
	}
	if got := repository.quota(testUserID, testQuota()); got != beforeQuota {
		t.Fatalf("quota = %d, want %d without restoration", got, beforeQuota)
	}
	if got := len(repository.ledgerEntries()); got != beforeEntries {
		t.Fatalf("ledger entry count = %d, want %d without confiscation entry", got, beforeEntries)
	}
	if runner.calls != 0 {
		t.Fatalf("WithinTx calls = %d, want 0", runner.calls)
	}
}

func TestReverse冻结业务时间并传入全部冲正写入(t *testing.T) {
	fixedAt := time.Date(2026, time.September, 7, 12, 34, 56, 0, time.UTC)
	delegate := newMemoryRepository()
	delegate.setBalance(testUserID, 20)
	repository := &timeRecordingRepository{memoryRepository: delegate}
	usecase := ledger.NewUsecaseWithClock(repository, newMemoryTxRunner(delegate), func() time.Time {
		return fixedAt
	})
	creationID := "creation-reverse-frozen-time"
	if _, err := usecase.ReserveInTx(context.Background(), ledger.ReserveRequest{
		CreationID:    creationID,
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         disabledQuota(),
		BusinessAt:    testBusinessAt(),
	}); err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}

	if _, err := usecase.Reverse(context.Background(), creationID, ledger.ReversalReasonSubmissionRejected); err != nil {
		t.Fatalf("Reverse() error = %v", err)
	}
	if !repository.transitionAt.Equal(fixedAt) || !repository.creditAt.Equal(fixedAt) {
		t.Fatalf("冲正写入业务时间 transition/credit = %s/%s, want both %s", repository.transitionAt, repository.creditAt, fixedAt)
	}
	entries := delegate.ledgerEntries()
	if got := entries[len(entries)-1].CreatedAt; !got.Equal(fixedAt) {
		t.Fatalf("冲正分录 CreatedAt = %s, want %s", got, fixedAt)
	}
}

func TestReverse冻结业务时间并传入日免恢复(t *testing.T) {
	fixedAt := time.Date(2026, time.September, 7, 12, 34, 56, 0, time.UTC)
	delegate := newMemoryRepository()
	delegate.setQuota(testUserID, testQuota(), 1)
	repository := &timeRecordingRepository{memoryRepository: delegate}
	usecase := ledger.NewUsecaseWithClock(repository, newMemoryTxRunner(delegate), func() time.Time {
		return fixedAt
	})
	creationID := "creation-reverse-frozen-quota-time"
	if _, err := usecase.ReserveInTx(context.Background(), ledger.ReserveRequest{
		CreationID:    creationID,
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
		BusinessAt:    testBusinessAt(),
	}); err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}

	if _, err := usecase.Reverse(context.Background(), creationID, ledger.ReversalReasonSubmissionRejected); err != nil {
		t.Fatalf("Reverse() error = %v", err)
	}
	if !repository.transitionAt.Equal(fixedAt) || !repository.restoreAt.Equal(fixedAt) {
		t.Fatalf("冲正写入业务时间 transition/restore = %s/%s, want both %s", repository.transitionAt, repository.restoreAt, fixedAt)
	}
}

func TestReverseInTx拒绝外部原因且不持久化(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	usecase := ledger.NewUsecase(repository, &countingTxRunner{})
	creationID := "creation-reverse-untrusted-reason"
	if _, err := usecase.ReserveInTx(context.Background(), ledger.ReserveRequest{
		CreationID:    creationID,
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         disabledQuota(),
		BusinessAt:    testBusinessAt(),
	}); err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}
	externalReason := ledger.ReversalReason("provider error: prompt=secret&token=private")
	before := repository.snapshot()
	result, err := usecase.ReverseInTx(context.Background(), creationID, externalReason, testBusinessAt().Add(time.Minute))
	if !errors.Is(err, ledger.ErrInvalidReservationCommand) {
		t.Fatalf("ReverseInTx() reservation/error = %#v / %v, want ErrInvalidReservationCommand", result, err)
	}
	if err != nil && strings.Contains(err.Error(), string(externalReason)) {
		t.Fatalf("ReverseInTx() error leaked external reason: %q", err)
	}
	assertRepositorySnapshot(t, repository, before)
	result, err = usecase.Reverse(context.Background(), creationID, externalReason)
	if !errors.Is(err, ledger.ErrInvalidReservationCommand) {
		t.Fatalf("Reverse() reservation/error = %#v / %v, want ErrInvalidReservationCommand", result, err)
	}
	if err != nil && strings.Contains(err.Error(), string(externalReason)) {
		t.Fatalf("Reverse() error leaked external reason: %q", err)
	}
	assertRepositorySnapshot(t, repository, before)
}

func TestReserveInTx拒绝空创建ID且不访问仓储(t *testing.T) {
	repository := &rejectingRepository{err: errors.New("repository must not be called")}
	usecase := ledger.NewUsecase(repository, &countingTxRunner{})

	reservation, err := usecase.ReserveInTx(context.Background(), ledger.ReserveRequest{
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         disabledQuota(),
	})

	if !errors.Is(err, ledger.ErrInvalidReservationCommand) {
		t.Fatalf("ReserveInTx() reservation = %#v, error = %v, want ErrInvalidReservationCommand", reservation, err)
	}
	if repository.calls != 0 {
		t.Fatalf("repository calls = %d, want 0", repository.calls)
	}
}

func TestReserveInTx复用外层事务并透传事务上下文(t *testing.T) {
	delegate := newMemoryRepository()
	delegate.setBalance(testUserID, 20)
	marker := &transactionContextMarker{}
	repository := newContextAssertingRepository(delegate, marker)
	runner := &countingTxRunner{}
	usecase := ledger.NewUsecase(repository, runner)

	var reservation *ledger.Reservation
	err := runner.WithinTx(
		context.WithValue(context.Background(), transactionContextMarkerKey{}, marker),
		func(txCtx context.Context) error {
			var err error
			reservation, err = usecase.ReserveInTx(txCtx, ledger.ReserveRequest{
				CreationID:    "creation-context-forwarding",
				UserID:        testUserID,
				PriceDiamonds: 20,
				Quota:         disabledQuota(),
				BusinessAt:    testBusinessAt(),
			})
			return err
		},
	)

	if err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}
	if reservation == nil || reservation.Status != ledger.ReservationStatusReserved {
		t.Fatalf("Reservation = %#v, want reserved", reservation)
	}
	if runner.calls != 1 {
		t.Fatalf("WithinTx calls = %d, want 1", runner.calls)
	}
	repository.assertRequiredContextCalls(t)
}

func TestReserveUsesDailyQuotaWithoutDiamondDebit(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 0)
	repository.setQuota(testUserID, testQuota(), 1)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)

	reservation, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-daily",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if reservation.Source != "daily_quota" {
		t.Fatalf("Reservation.Source = %q, want daily_quota", reservation.Source)
	}
	if reservation.ChargedDiamonds != 0 {
		t.Fatalf("Reservation.ChargedDiamonds = %d, want 0", reservation.ChargedDiamonds)
	}
	if reservation.Status != ledger.ReservationStatusReserved {
		t.Fatalf("Reservation.Status = %q, want reserved", reservation.Status)
	}
	if got := repository.balance(testUserID); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
	if got := repository.quota(testUserID, testQuota()); got != 0 {
		t.Fatalf("quota = %d, want 0", got)
	}
	entries := repository.ledgerEntries()
	if len(entries) != 1 {
		t.Fatalf("ledger entry count = %d, want 1", len(entries))
	}
	if entries[0].DeltaDiamonds != 0 {
		t.Fatalf("LedgerEntry.DeltaDiamonds = %d, want 0", entries[0].DeltaDiamonds)
	}
	assertLedgerEntry(t, entries[0], "reserve:"+reservation.CreationID, reservation.CreationID, 0, "generation_reserved")
	assertTxCalls(t, runner, 1)
}

func TestReserveUsesDiamondsWhenDailyQuotaIsExplicitlyDisabled(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)
	disabledQuota := testQuota()
	disabledQuota.Limit = 0
	disabledQuota.Units = 0

	reservation, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-diamonds-with-disabled-quota",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         disabledQuota,
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if reservation.Source != "diamonds" {
		t.Fatalf("Reservation.Source = %q, want diamonds", reservation.Source)
	}
	if got := repository.balance(testUserID); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
	entries := repository.ledgerEntries()
	if len(entries) != 1 {
		t.Fatalf("ledger entry count = %d, want 1", len(entries))
	}
	assertLedgerEntry(t, entries[0], "reserve:"+reservation.CreationID, reservation.CreationID, -20, "generation_reserved")
	assertTxCalls(t, runner, 1)
}

func TestReserveLeavesAllStateUntouchedWhenQuotaIsExhaustedAndDiamondsAreInsufficient(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 19)
	repository.setQuota(testUserID, testQuota(), 0)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)

	reservation, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-insufficient",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if !errors.Is(err, shared.ErrInsufficientFunds) {
		t.Fatalf("Reserve() error = %v, want ErrInsufficientFunds", err)
	}
	if reservation != nil {
		t.Fatalf("Reserve() reservation = %#v, want nil", reservation)
	}
	if got := repository.balance(testUserID); got != 19 {
		t.Fatalf("balance = %d, want 19", got)
	}
	if got := repository.quota(testUserID, testQuota()); got != 0 {
		t.Fatalf("quota = %d, want 0", got)
	}
	if got := repository.reservationCount(); got != 0 {
		t.Fatalf("reservation count = %d, want 0", got)
	}
	if got := len(repository.ledgerEntries()); got != 0 {
		t.Fatalf("ledger entry count = %d, want 0", got)
	}
	assertTxCalls(t, runner, 1)
}

func TestReserveIsIdempotentForSameCreationID(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	usecase := ledger.NewUsecase(repository, newMemoryTxRunner(repository))
	request := ledger.ReserveRequest{
		CreationID:    "creation-idempotent",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	}

	first, err := usecase.Reserve(context.Background(), request)
	if err != nil {
		t.Fatalf("first Reserve() error = %v", err)
	}
	second, err := usecase.Reserve(context.Background(), request)
	if err != nil {
		t.Fatalf("second Reserve() error = %v", err)
	}
	if second.CreationID != first.CreationID {
		t.Fatalf("second Reservation.CreationID = %q, want %q", second.CreationID, first.CreationID)
	}
	if second.Status != first.Status || second.Source != first.Source || second.ChargedDiamonds != first.ChargedDiamonds {
		t.Fatalf("second reservation = %#v, want same reservation as %#v", second, first)
	}
	if got := repository.balance(testUserID); got != 0 {
		t.Fatalf("balance = %d, want 0 after idempotent Reserve", got)
	}
	if got := repository.reservationCount(); got != 1 {
		t.Fatalf("reservation count = %d, want 1", got)
	}
	if got := len(repository.ledgerEntries()); got != 1 {
		t.Fatalf("ledger entry count = %d, want 1", got)
	}
}

func TestReserveRejectsSameCreationIDWithDifferentCommandContext(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*ledger.ReserveRequest)
		mutate  func(*ledger.ReserveRequest)
	}{
		{
			name: "价格不同",
			mutate: func(request *ledger.ReserveRequest) {
				request.PriceDiamonds = 50
			},
		},
		{
			name: "用户不同",
			mutate: func(request *ledger.ReserveRequest) {
				request.UserID = testOtherUserID
			},
		},
		{
			name: "额度种类不同",
			mutate: func(request *ledger.ReserveRequest) {
				request.Quota.Kind = "vip_daily_video"
			},
		},
		{
			name: "额度本地日期不同",
			mutate: func(request *ledger.ReserveRequest) {
				request.Quota.LocalDate = "2026-09-06"
			},
		},
		{
			name: "额度单位不同",
			prepare: func(request *ledger.ReserveRequest) {
				request.Quota.Limit = 2
			},
			mutate: func(request *ledger.ReserveRequest) {
				request.Quota.Units = 2
			},
		},
		{
			name: "额度上限不同",
			mutate: func(request *ledger.ReserveRequest) {
				request.Quota.Limit = 2
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repository := newMemoryRepository()
			repository.setBalance(testUserID, 20)
			repository.setBalance(testOtherUserID, 100)
			repository.setQuota(testUserID, testQuota(), 0)
			runner := newMemoryTxRunner(repository)
			usecase := ledger.NewUsecase(repository, runner)
			firstRequest := ledger.ReserveRequest{
				CreationID:    "creation-conflicting-command",
				UserID:        testUserID,
				PriceDiamonds: 20,
				Quota:         testQuota(),
			}
			if testCase.prepare != nil {
				testCase.prepare(&firstRequest)
			}

			first, err := usecase.Reserve(context.Background(), firstRequest)
			if err != nil {
				t.Fatalf("first Reserve() error = %v", err)
			}
			conflictingRequest := firstRequest
			testCase.mutate(&conflictingRequest)
			repository.setQuota(testOtherUserID, testQuota(), 1)
			repository.setQuota(conflictingRequest.UserID, conflictingRequest.Quota, conflictingRequest.Quota.Units)
			before := repository.snapshot()

			second, err := usecase.Reserve(context.Background(), conflictingRequest)
			if !errors.Is(err, ledger.ErrReservationCommandConflict) {
				t.Fatalf("second Reserve() reservation = %#v, error = %v, want ErrReservationCommandConflict", second, err)
			}
			assertRepositorySnapshot(t, repository, before)
			stored := repository.reservation(first.CreationID)
			if stored == nil {
				t.Fatal("stored reservation = nil, want first reservation")
			}
			if stored.CreationID != first.CreationID || stored.Source != "diamonds" || stored.ChargedDiamonds != 20 {
				t.Fatalf("stored reservation = %#v, want unchanged first paid reservation", stored)
			}
			if got := repository.balance(testUserID); got != 0 {
				t.Fatalf("first user balance after conflicting Reserve = %d, want 0", got)
			}
			if got := repository.balance(testOtherUserID); got != 100 {
				t.Fatalf("second user balance after conflicting Reserve = %d, want 100", got)
			}
			if got := repository.reservationCount(); got != 1 {
				t.Fatalf("reservation count = %d, want 1", got)
			}
			if got := len(repository.ledgerEntries()); got != 1 {
				t.Fatalf("ledger entry count = %d, want 1", got)
			}
		})
	}
}

func TestReserveRejectsExistingCreationIDWithInvalidChangedCommand(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	repository.setQuota(testUserID, testQuota(), 0)
	usecase := ledger.NewUsecase(repository, newMemoryTxRunner(repository))
	firstRequest := ledger.ReserveRequest{
		CreationID:    "creation-conflicting-invalid-price",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	}

	first, err := usecase.Reserve(context.Background(), firstRequest)
	if err != nil {
		t.Fatalf("first Reserve() error = %v", err)
	}
	before := repository.snapshot()
	conflictingRequest := firstRequest
	conflictingRequest.CreationID = first.CreationID
	conflictingRequest.PriceDiamonds = 0

	second, err := usecase.Reserve(context.Background(), conflictingRequest)
	if !errors.Is(err, ledger.ErrReservationCommandConflict) {
		t.Fatalf("second Reserve() reservation = %#v, error = %v, want ErrReservationCommandConflict", second, err)
	}
	assertRepositorySnapshot(t, repository, before)
}

func TestReversePaidReservationRefundsExactlyOnce(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)

	reserved, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-paid-reverse",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if reserved.Source != "diamonds" {
		t.Fatalf("Reservation.Source = %q, want diamonds", reserved.Source)
	}
	entries := repository.ledgerEntries()
	if len(entries) != 1 {
		t.Fatalf("ledger entry count after Reserve = %d, want 1", len(entries))
	}
	assertLedgerEntry(t, entries[0], "reserve:"+reserved.CreationID, reserved.CreationID, -20, "generation_reserved")
	assertTxCalls(t, runner, 1)

	reversed, err := usecase.Reverse(context.Background(), reserved.CreationID, "generation_failed")
	if err != nil {
		t.Fatalf("first Reverse() error = %v", err)
	}
	if reversed.Status != ledger.ReservationStatusReversed {
		t.Fatalf("Reservation.Status = %q, want reversed", reversed.Status)
	}
	if got := repository.balance(testUserID); got != 20 {
		t.Fatalf("balance after first Reverse = %d, want 20", got)
	}
	if got := len(repository.ledgerEntries()); got != 2 {
		t.Fatalf("ledger entry count after first Reverse = %d, want 2", got)
	}
	entries = repository.ledgerEntries()
	assertLedgerEntry(t, entries[1], "reverse:"+reserved.CreationID, reserved.CreationID, 20, "generation_failed")
	assertTxCalls(t, runner, 2)

	second, err := usecase.Reverse(context.Background(), reserved.CreationID, "generation_failed")
	if err != nil {
		t.Fatalf("second Reverse() error = %v", err)
	}
	if second.Status != ledger.ReservationStatusReversed {
		t.Fatalf("second Reservation.Status = %q, want reversed", second.Status)
	}
	if got := repository.balance(testUserID); got != 20 {
		t.Fatalf("balance after second Reverse = %d, want 20", got)
	}
	if got := len(repository.ledgerEntries()); got != 2 {
		t.Fatalf("ledger entry count after second Reverse = %d, want 2", got)
	}
	assertTxCalls(t, runner, 3)
}

func TestReverseRestoresDailyQuotaAndConfiscateOnlyChangesStatus(t *testing.T) {
	repository := newMemoryRepository()
	reversibleQuota := testQuota()
	reversibleQuota.Limit = 3
	reversibleQuota.Units = 2
	differentDateQuota := reversibleQuota
	differentDateQuota.LocalDate = "2026-09-06"
	differentKindQuota := reversibleQuota
	differentKindQuota.Kind = "vip_daily_video"
	repository.setBalance(testUserID, 0)
	repository.setQuota(testUserID, reversibleQuota, 3)
	repository.setQuota(testUserID, differentDateQuota, 3)
	repository.setQuota(testUserID, differentKindQuota, 3)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)

	first, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-daily-reverse",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         reversibleQuota,
	})
	if err != nil {
		t.Fatalf("first Reserve() error = %v", err)
	}
	if got := repository.quota(testUserID, reversibleQuota); got != 1 {
		t.Fatalf("quota after Reserve = %d, want 1", got)
	}
	if got := repository.quota(testUserID, differentDateQuota); got != 3 {
		t.Fatalf("quota for different LocalDate after Reserve = %d, want 3", got)
	}
	if got := repository.quota(testUserID, differentKindQuota); got != 3 {
		t.Fatalf("quota for different Kind after Reserve = %d, want 3", got)
	}
	entries := repository.ledgerEntries()
	if len(entries) != 1 {
		t.Fatalf("ledger entry count after daily Reserve = %d, want 1", len(entries))
	}
	assertLedgerEntry(t, entries[0], "reserve:"+first.CreationID, first.CreationID, 0, "generation_reserved")
	assertTxCalls(t, runner, 1)

	if _, err := usecase.Reverse(context.Background(), first.CreationID, "generation_failed"); err != nil {
		t.Fatalf("Reverse() error = %v", err)
	}
	if got := repository.quota(testUserID, reversibleQuota); got != 3 {
		t.Fatalf("quota after Reverse = %d, want 3 for original LocalDate and Units", got)
	}
	if got := repository.quota(testUserID, differentDateQuota); got != 3 {
		t.Fatalf("quota for different LocalDate after Reverse = %d, want 3", got)
	}
	if got := repository.quota(testUserID, differentKindQuota); got != 3 {
		t.Fatalf("quota for different Kind after Reverse = %d, want 3", got)
	}
	entries = repository.ledgerEntries()
	if len(entries) != 2 {
		t.Fatalf("ledger entry count after daily Reverse = %d, want 2", len(entries))
	}
	assertLedgerEntry(t, entries[1], "reverse:"+first.CreationID, first.CreationID, 0, "generation_failed")
	assertTxCalls(t, runner, 2)

	reversedAgain, err := usecase.Reverse(context.Background(), first.CreationID, "generation_failed")
	if err != nil {
		t.Fatalf("second Reverse() error = %v", err)
	}
	if reversedAgain.Status != ledger.ReservationStatusReversed {
		t.Fatalf("second Reverse() status = %q, want reversed", reversedAgain.Status)
	}
	if got := repository.quota(testUserID, reversibleQuota); got != 3 {
		t.Fatalf("quota after second Reverse = %d, want 3", got)
	}
	if got := len(repository.ledgerEntries()); got != 2 {
		t.Fatalf("ledger entry count after second daily Reverse = %d, want 2", got)
	}
	assertTxCalls(t, runner, 3)

	second, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-daily-confiscate",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         reversibleQuota,
	})
	if err != nil {
		t.Fatalf("second Reserve() error = %v", err)
	}
	assertTxCalls(t, runner, 4)
	beforeBalance := repository.balance(testUserID)
	beforeQuota := repository.quota(testUserID, reversibleQuota)
	beforeLedgerCount := len(repository.ledgerEntries())

	confiscated, err := usecase.Confiscate(context.Background(), second.CreationID, "review_rejected")
	if err != nil {
		t.Fatalf("Confiscate() error = %v", err)
	}
	if confiscated.Status != ledger.ReservationStatusConfiscated {
		t.Fatalf("Reservation.Status = %q, want confiscated", confiscated.Status)
	}
	if got := repository.balance(testUserID); got != beforeBalance {
		t.Fatalf("balance after Confiscate = %d, want %d", got, beforeBalance)
	}
	if got := repository.quota(testUserID, reversibleQuota); got != beforeQuota {
		t.Fatalf("quota after Confiscate = %d, want %d", got, beforeQuota)
	}
	if got := len(repository.ledgerEntries()); got != beforeLedgerCount {
		t.Fatalf("ledger entry count after Confiscate = %d, want %d", got, beforeLedgerCount)
	}
	assertTxCalls(t, runner, 5)
}

func TestTerminalReservationStatesRejectFurtherMigration(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	repository.setQuota(testUserID, testQuota(), 0)
	usecase := ledger.NewUsecase(repository, newMemoryTxRunner(repository))

	reversed, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-reversed-terminal",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if err != nil {
		t.Fatalf("first Reserve() error = %v", err)
	}
	if _, err := usecase.Reverse(context.Background(), reversed.CreationID, "generation_failed"); err != nil {
		t.Fatalf("Reverse() error = %v", err)
	}
	beforeReversedBalance := repository.balance(testUserID)
	beforeReversedQuota := repository.quota(testUserID, testQuota())
	beforeReversedLedgerCount := len(repository.ledgerEntries())

	if _, err := usecase.Confiscate(context.Background(), reversed.CreationID, "review_rejected"); err == nil {
		t.Fatal("Confiscate() error = nil, want error for reversed reservation")
	}
	if got := repository.reservation(reversed.CreationID).Status; got != ledger.ReservationStatusReversed {
		t.Fatalf("Reservation.Status after invalid Confiscate = %q, want reversed", got)
	}
	if got := repository.balance(testUserID); got != beforeReversedBalance {
		t.Fatalf("balance after invalid Confiscate = %d, want %d", got, beforeReversedBalance)
	}
	if got := repository.quota(testUserID, testQuota()); got != beforeReversedQuota {
		t.Fatalf("quota after invalid Confiscate = %d, want %d", got, beforeReversedQuota)
	}
	if got := len(repository.ledgerEntries()); got != beforeReversedLedgerCount {
		t.Fatalf("ledger entry count after invalid Confiscate = %d, want %d", got, beforeReversedLedgerCount)
	}

	confiscated, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-confiscated-terminal",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if err != nil {
		t.Fatalf("second Reserve() error = %v", err)
	}
	if _, err := usecase.Confiscate(context.Background(), confiscated.CreationID, "review_rejected"); err != nil {
		t.Fatalf("Confiscate() error = %v", err)
	}
	beforeConfiscatedBalance := repository.balance(testUserID)
	beforeConfiscatedQuota := repository.quota(testUserID, testQuota())
	beforeConfiscatedLedgerCount := len(repository.ledgerEntries())

	if _, err := usecase.Reverse(context.Background(), confiscated.CreationID, "generation_failed"); err == nil {
		t.Fatal("Reverse() error = nil, want error for confiscated reservation")
	}
	if got := repository.reservation(confiscated.CreationID).Status; got != ledger.ReservationStatusConfiscated {
		t.Fatalf("Reservation.Status after invalid Reverse = %q, want confiscated", got)
	}
	if got := repository.balance(testUserID); got != beforeConfiscatedBalance {
		t.Fatalf("balance after invalid Reverse = %d, want %d", got, beforeConfiscatedBalance)
	}
	if got := repository.quota(testUserID, testQuota()); got != beforeConfiscatedQuota {
		t.Fatalf("quota after invalid Reverse = %d, want %d", got, beforeConfiscatedQuota)
	}
	if got := len(repository.ledgerEntries()); got != beforeConfiscatedLedgerCount {
		t.Fatalf("ledger entry count after invalid Reverse = %d, want %d", got, beforeConfiscatedLedgerCount)
	}
}

func TestReversePaidReservationRollsBackWhenStateTransitionCASFails(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	repository.setQuota(testUserID, testQuota(), 0)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)

	reserved, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-paid-reverse-cas",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	before := repository.snapshot()
	repository.failNextTransition()

	result, err := usecase.Reverse(context.Background(), reserved.CreationID, "generation_failed")
	if !errors.Is(err, ledger.ErrReservationStateConflict) {
		t.Fatalf("Reverse() reservation = %#v, error = %v, want ErrReservationStateConflict", result, err)
	}
	assertRepositorySnapshot(t, repository, before)
	assertReservationStatus(t, repository, reserved.CreationID, ledger.ReservationStatusReserved)
	assertTxCalls(t, runner, 2)
}

func TestReverseDailyReservationRollsBackWhenStateTransitionCASFails(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 0)
	repository.setQuota(testUserID, testQuota(), 1)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)

	reserved, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-daily-reverse-cas",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	before := repository.snapshot()
	repository.failNextTransition()

	result, err := usecase.Reverse(context.Background(), reserved.CreationID, "generation_failed")
	if !errors.Is(err, ledger.ErrReservationStateConflict) {
		t.Fatalf("Reverse() reservation = %#v, error = %v, want ErrReservationStateConflict", result, err)
	}
	assertRepositorySnapshot(t, repository, before)
	assertReservationStatus(t, repository, reserved.CreationID, ledger.ReservationStatusReserved)
	assertTxCalls(t, runner, 2)
}

func TestConfiscateRollsBackWhenStateTransitionCASFails(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	repository.setQuota(testUserID, testQuota(), 0)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)

	reserved, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-confiscate-cas",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	before := repository.snapshot()
	repository.failNextTransition()

	result, err := usecase.Confiscate(context.Background(), reserved.CreationID, "review_rejected")
	if !errors.Is(err, ledger.ErrReservationStateConflict) {
		t.Fatalf("Confiscate() reservation = %#v, error = %v, want ErrReservationStateConflict", result, err)
	}
	assertRepositorySnapshot(t, repository, before)
	assertReservationStatus(t, repository, reserved.CreationID, ledger.ReservationStatusReserved)
	assertTxCalls(t, runner, 2)
}

func TestReserveRollsBackWhenLedgerEntryWriteFails(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	repository.setQuota(testUserID, testQuota(), 0)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)
	writeError := errors.New("injected ledger write failure")
	repository.failNextLedgerEntryAppend(writeError)
	before := repository.snapshot()

	reservation, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-ledger-write-failure",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if !errors.Is(err, writeError) {
		t.Fatalf("Reserve() reservation = %#v, error = %v, want injected ledger write error", reservation, err)
	}
	assertRepositorySnapshot(t, repository, before)
	assertTxCalls(t, runner, 1)
}

func TestReversePaidReservationRollsBackWhenLedgerEntryWriteFails(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 20)
	repository.setQuota(testUserID, testQuota(), 0)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)

	reserved, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-paid-reverse-ledger-write-failure",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	writeError := errors.New("injected paid reverse ledger write failure")
	repository.failNextLedgerEntryAppend(writeError)
	before := repository.snapshot()

	result, err := usecase.Reverse(context.Background(), reserved.CreationID, "generation_failed")
	if !errors.Is(err, writeError) {
		t.Fatalf("Reverse() reservation = %#v, error = %v, want injected ledger write error", result, err)
	}
	assertRepositorySnapshot(t, repository, before)
	assertReservationStatus(t, repository, reserved.CreationID, ledger.ReservationStatusReserved)
	if got := repository.balance(testUserID); got != 0 {
		t.Fatalf("balance after failed Reverse = %d, want 0", got)
	}
	if got := len(repository.ledgerEntries()); got != 1 {
		t.Fatalf("ledger entry count after failed Reverse = %d, want 1", got)
	}
	assertTxCalls(t, runner, 2)
}

func TestReverseDailyReservationRollsBackWhenLedgerEntryWriteFails(t *testing.T) {
	repository := newMemoryRepository()
	repository.setBalance(testUserID, 0)
	repository.setQuota(testUserID, testQuota(), 1)
	runner := newMemoryTxRunner(repository)
	usecase := ledger.NewUsecase(repository, runner)

	reserved, err := usecase.Reserve(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-daily-reverse-ledger-write-failure",
		UserID:        testUserID,
		PriceDiamonds: 20,
		Quota:         testQuota(),
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	writeError := errors.New("injected daily reverse ledger write failure")
	repository.failNextLedgerEntryAppend(writeError)
	before := repository.snapshot()

	result, err := usecase.Reverse(context.Background(), reserved.CreationID, "generation_failed")
	if !errors.Is(err, writeError) {
		t.Fatalf("Reverse() reservation = %#v, error = %v, want injected ledger write error", result, err)
	}
	assertRepositorySnapshot(t, repository, before)
	assertReservationStatus(t, repository, reserved.CreationID, ledger.ReservationStatusReserved)
	if got := repository.quota(testUserID, testQuota()); got != 0 {
		t.Fatalf("quota after failed Reverse = %d, want 0", got)
	}
	if got := len(repository.ledgerEntries()); got != 1 {
		t.Fatalf("ledger entry count after failed Reverse = %d, want 1", got)
	}
	assertTxCalls(t, runner, 2)
}

func testQuota() ledger.QuotaReservation {
	return ledger.QuotaReservation{
		Kind:      testQuotaKind,
		LocalDate: testQuotaLocalDate,
		Limit:     1,
		Units:     1,
	}
}

func disabledQuota() ledger.QuotaReservation {
	quota := testQuota()
	quota.Limit = 0
	quota.Units = 0
	return quota
}

// testBusinessAt 为直接调用 ReserveInTx 的测试提供事务外冻结的业务时刻。
func testBusinessAt() time.Time {
	return time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
}

type countingTxRunner struct {
	calls int
}

func (runner *countingTxRunner) WithinTx(ctx context.Context, callback func(context.Context) error) error {
	runner.calls++
	return callback(ctx)
}

type transactionContextMarkerKey struct{}

type transactionContextMarker struct{}

type rejectingRepository struct {
	calls int
	err   error
}

func (repository *rejectingRepository) FindReservation(context.Context, string) (*ledger.Reservation, error) {
	repository.calls++
	return nil, repository.err
}

func (repository *rejectingRepository) FindDiamondBalance(context.Context, string) (int64, error) {
	repository.calls++
	return 0, repository.err
}

func (repository *rejectingRepository) TryConsumeQuota(context.Context, string, ledger.QuotaReservation, time.Time) (bool, error) {
	repository.calls++
	return false, repository.err
}

func (repository *rejectingRepository) RestoreQuota(context.Context, string, ledger.QuotaReservation, time.Time) error {
	repository.calls++
	return repository.err
}

func (repository *rejectingRepository) TryDebitDiamonds(context.Context, string, int64, time.Time) (bool, error) {
	repository.calls++
	return false, repository.err
}

func (repository *rejectingRepository) CreditDiamonds(context.Context, string, int64, time.Time) error {
	repository.calls++
	return repository.err
}

func (repository *rejectingRepository) CreateReservation(context.Context, *ledger.Reservation) error {
	repository.calls++
	return repository.err
}

func (repository *rejectingRepository) TransitionReservation(context.Context, string, ledger.ReservationStatus, ledger.ReservationStatus, time.Time) (bool, error) {
	repository.calls++
	return false, repository.err
}

func (repository *rejectingRepository) AppendLedgerEntry(context.Context, *ledger.LedgerEntry) error {
	repository.calls++
	return repository.err
}

type contextAssertingRepository struct {
	delegate *memoryRepository
	marker   *transactionContextMarker
	calls    map[string]int
}

func newContextAssertingRepository(delegate *memoryRepository, marker *transactionContextMarker) *contextAssertingRepository {
	return &contextAssertingRepository{
		delegate: delegate,
		marker:   marker,
		calls:    make(map[string]int),
	}
}

func (repository *contextAssertingRepository) FindReservation(ctx context.Context, creationID string) (*ledger.Reservation, error) {
	if err := repository.assertContext(ctx, "FindReservation"); err != nil {
		return nil, err
	}
	return repository.delegate.FindReservation(ctx, creationID)
}

func (repository *contextAssertingRepository) FindDiamondBalance(ctx context.Context, userID string) (int64, error) {
	return repository.delegate.FindDiamondBalance(ctx, userID)
}

func (repository *contextAssertingRepository) TryConsumeQuota(ctx context.Context, userID string, quota ledger.QuotaReservation, businessAt time.Time) (bool, error) {
	return repository.delegate.TryConsumeQuota(ctx, userID, quota, businessAt)
}

func (repository *contextAssertingRepository) RestoreQuota(ctx context.Context, userID string, quota ledger.QuotaReservation, businessAt time.Time) error {
	return repository.delegate.RestoreQuota(ctx, userID, quota, businessAt)
}

func (repository *contextAssertingRepository) TryDebitDiamonds(ctx context.Context, userID string, diamonds int64, businessAt time.Time) (bool, error) {
	if err := repository.assertContext(ctx, "TryDebitDiamonds"); err != nil {
		return false, err
	}
	return repository.delegate.TryDebitDiamonds(ctx, userID, diamonds, businessAt)
}

func (repository *contextAssertingRepository) CreditDiamonds(ctx context.Context, userID string, diamonds int64, businessAt time.Time) error {
	return repository.delegate.CreditDiamonds(ctx, userID, diamonds, businessAt)
}

func (repository *contextAssertingRepository) CreateReservation(ctx context.Context, reservation *ledger.Reservation) error {
	if err := repository.assertContext(ctx, "CreateReservation"); err != nil {
		return err
	}
	return repository.delegate.CreateReservation(ctx, reservation)
}

func (repository *contextAssertingRepository) TransitionReservation(ctx context.Context, creationID string, from, to ledger.ReservationStatus, businessAt time.Time) (bool, error) {
	return repository.delegate.TransitionReservation(ctx, creationID, from, to, businessAt)
}

func (repository *contextAssertingRepository) AppendLedgerEntry(ctx context.Context, entry *ledger.LedgerEntry) error {
	if err := repository.assertContext(ctx, "AppendLedgerEntry"); err != nil {
		return err
	}
	return repository.delegate.AppendLedgerEntry(ctx, entry)
}

func (repository *contextAssertingRepository) assertContext(ctx context.Context, operation string) error {
	if got := ctx.Value(transactionContextMarkerKey{}); got != repository.marker {
		return fmt.Errorf("%s did not receive caller transaction context", operation)
	}
	repository.calls[operation]++
	return nil
}

func (repository *contextAssertingRepository) assertRequiredContextCalls(t *testing.T) {
	t.Helper()
	for _, operation := range []string{"FindReservation", "TryDebitDiamonds", "CreateReservation", "AppendLedgerEntry"} {
		if got := repository.calls[operation]; got != 1 {
			t.Fatalf("%s context calls = %d, want 1", operation, got)
		}
	}
}

type memoryTxRunner struct {
	repository *memoryRepository
	calls      int
}

func newMemoryTxRunner(repository *memoryRepository) *memoryTxRunner {
	return &memoryTxRunner{repository: repository}
}

func (runner *memoryTxRunner) WithinTx(ctx context.Context, callback func(context.Context) error) error {
	runner.calls++
	// 使用仓储快照模拟事务，回调报错时恢复所有内存事实。
	snapshot := runner.repository.snapshot()
	err := callback(ctx)
	if err != nil {
		runner.repository.restore(snapshot)
	}
	return err
}

type quotaKey struct {
	userID    string
	kind      string
	localDate string
}

type memoryRepository struct {
	balances                 map[string]int64
	quotas                   map[quotaKey]int32
	reservationsByCreationID map[string]*ledger.Reservation
	entries                  []*ledger.LedgerEntry
	transitionFailurePending bool
	failNextLedgerEntryError error
}

type timeRecordingRepository struct {
	*memoryRepository
	transitionAt time.Time
	creditAt     time.Time
	restoreAt    time.Time
}

func (repository *timeRecordingRepository) TransitionReservation(ctx context.Context, creationID string, from, to ledger.ReservationStatus, businessAt time.Time) (bool, error) {
	repository.transitionAt = businessAt
	return repository.memoryRepository.TransitionReservation(ctx, creationID, from, to, businessAt)
}

func (repository *timeRecordingRepository) CreditDiamonds(ctx context.Context, userID string, diamonds int64, businessAt time.Time) error {
	repository.creditAt = businessAt
	return repository.memoryRepository.CreditDiamonds(ctx, userID, diamonds, businessAt)
}

func (repository *timeRecordingRepository) RestoreQuota(ctx context.Context, userID string, quota ledger.QuotaReservation, businessAt time.Time) error {
	repository.restoreAt = businessAt
	return repository.memoryRepository.RestoreQuota(ctx, userID, quota, businessAt)
}

type memoryRepositorySnapshot struct {
	balances                 map[string]int64
	quotas                   map[quotaKey]int32
	reservationsByCreationID map[string]*ledger.Reservation
	entries                  []*ledger.LedgerEntry
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{
		balances:                 make(map[string]int64),
		quotas:                   make(map[quotaKey]int32),
		reservationsByCreationID: make(map[string]*ledger.Reservation),
	}
}

func (repository *memoryRepository) FindReservation(ctx context.Context, creationID string) (*ledger.Reservation, error) {
	return cloneReservation(repository.reservationsByCreationID[creationID]), nil
}

func (repository *memoryRepository) FindDiamondBalance(_ context.Context, userID string) (int64, error) {
	return repository.balances[userID], nil
}

func (repository *memoryRepository) TryConsumeQuota(ctx context.Context, userID string, quota ledger.QuotaReservation, _ time.Time) (bool, error) {
	key := quotaKeyFor(userID, quota)
	if repository.quotas[key] < quota.Units {
		return false, nil
	}
	repository.quotas[key] -= quota.Units
	return true, nil
}

func (repository *memoryRepository) RestoreQuota(ctx context.Context, userID string, quota ledger.QuotaReservation, _ time.Time) error {
	repository.quotas[quotaKeyFor(userID, quota)] += quota.Units
	return nil
}

func (repository *memoryRepository) TryDebitDiamonds(ctx context.Context, userID string, diamonds int64, _ time.Time) (bool, error) {
	if repository.balances[userID] < diamonds {
		return false, nil
	}
	repository.balances[userID] -= diamonds
	return true, nil
}

func (repository *memoryRepository) CreditDiamonds(ctx context.Context, userID string, diamonds int64, _ time.Time) error {
	repository.balances[userID] += diamonds
	return nil
}

func (repository *memoryRepository) CreateReservation(ctx context.Context, reservation *ledger.Reservation) error {
	if _, exists := repository.reservationsByCreationID[reservation.CreationID]; exists {
		return fmt.Errorf("creation %q already has a reservation", reservation.CreationID)
	}
	stored := cloneReservation(reservation)
	repository.reservationsByCreationID[stored.CreationID] = stored
	return nil
}

func (repository *memoryRepository) TransitionReservation(ctx context.Context, creationID string, from, to ledger.ReservationStatus, _ time.Time) (bool, error) {
	reservation, exists := repository.reservationsByCreationID[creationID]
	if !exists {
		return false, fmt.Errorf("reservation for creation %q does not exist", creationID)
	}
	if repository.transitionFailurePending {
		repository.transitionFailurePending = false
		return false, nil
	}
	if reservation.Status != from {
		return false, nil
	}
	reservation.Status = to
	return true, nil
}

func (repository *memoryRepository) AppendLedgerEntry(ctx context.Context, entry *ledger.LedgerEntry) error {
	if repository.failNextLedgerEntryError != nil {
		err := repository.failNextLedgerEntryError
		repository.failNextLedgerEntryError = nil
		return err
	}
	repository.entries = append(repository.entries, cloneLedgerEntry(entry))
	return nil
}

func (repository *memoryRepository) setBalance(userID string, diamonds int64) {
	repository.balances[userID] = diamonds
}

func (repository *memoryRepository) balance(userID string) int64 {
	return repository.balances[userID]
}

func (repository *memoryRepository) setQuota(userID string, quota ledger.QuotaReservation, units int32) {
	repository.quotas[quotaKeyFor(userID, quota)] = units
}

func (repository *memoryRepository) quota(userID string, quota ledger.QuotaReservation) int32 {
	return repository.quotas[quotaKeyFor(userID, quota)]
}

func (repository *memoryRepository) reservationCount() int {
	return len(repository.reservationsByCreationID)
}

func (repository *memoryRepository) reservation(creationID string) *ledger.Reservation {
	return cloneReservation(repository.reservationsByCreationID[creationID])
}

func (repository *memoryRepository) failNextTransition() {
	repository.transitionFailurePending = true
}

func (repository *memoryRepository) failNextLedgerEntryAppend(err error) {
	repository.failNextLedgerEntryError = err
}

func (repository *memoryRepository) ledgerEntries() []*ledger.LedgerEntry {
	entries := make([]*ledger.LedgerEntry, 0, len(repository.entries))
	for _, entry := range repository.entries {
		entries = append(entries, cloneLedgerEntry(entry))
	}
	return entries
}

func (repository *memoryRepository) snapshot() memoryRepositorySnapshot {
	snapshot := memoryRepositorySnapshot{
		balances:                 make(map[string]int64, len(repository.balances)),
		quotas:                   make(map[quotaKey]int32, len(repository.quotas)),
		reservationsByCreationID: make(map[string]*ledger.Reservation, len(repository.reservationsByCreationID)),
		entries:                  make([]*ledger.LedgerEntry, 0, len(repository.entries)),
	}
	for userID, balance := range repository.balances {
		snapshot.balances[userID] = balance
	}
	for key, quota := range repository.quotas {
		snapshot.quotas[key] = quota
	}
	for creationID, reservation := range repository.reservationsByCreationID {
		snapshot.reservationsByCreationID[creationID] = cloneReservation(reservation)
	}
	for _, entry := range repository.entries {
		snapshot.entries = append(snapshot.entries, cloneLedgerEntry(entry))
	}
	return snapshot
}

func (repository *memoryRepository) restore(snapshot memoryRepositorySnapshot) {
	repository.balances = snapshot.balances
	repository.quotas = snapshot.quotas
	repository.reservationsByCreationID = snapshot.reservationsByCreationID
	repository.entries = snapshot.entries
}

func quotaKeyFor(userID string, quota ledger.QuotaReservation) quotaKey {
	return quotaKey{
		userID:    userID,
		kind:      quota.Kind,
		localDate: quota.LocalDate,
	}
}

func cloneReservation(reservation *ledger.Reservation) *ledger.Reservation {
	if reservation == nil {
		return nil
	}
	clone := *reservation
	return &clone
}

func cloneLedgerEntry(entry *ledger.LedgerEntry) *ledger.LedgerEntry {
	if entry == nil {
		return nil
	}
	clone := *entry
	return &clone
}

func assertLedgerEntry(t *testing.T, entry *ledger.LedgerEntry, idempotencyKey, creationID string, deltaDiamonds int64, reason string) {
	t.Helper()
	if entry.IdempotencyKey != idempotencyKey {
		t.Fatalf("LedgerEntry.IdempotencyKey = %q, want %q", entry.IdempotencyKey, idempotencyKey)
	}
	if entry.DeltaDiamonds != deltaDiamonds {
		t.Fatalf("LedgerEntry.DeltaDiamonds = %d, want %d", entry.DeltaDiamonds, deltaDiamonds)
	}
	if entry.CreationID != creationID {
		t.Fatalf("LedgerEntry.CreationID = %q, want %q", entry.CreationID, creationID)
	}
	if entry.Reason != reason {
		t.Fatalf("LedgerEntry.Reason = %q, want %q", entry.Reason, reason)
	}
}

func assertTxCalls(t *testing.T, runner *memoryTxRunner, want int) {
	t.Helper()
	if runner.calls != want {
		t.Fatalf("WithinTx calls = %d, want %d", runner.calls, want)
	}
}

func assertRepositorySnapshot(t *testing.T, repository *memoryRepository, want memoryRepositorySnapshot) {
	t.Helper()
	if got := repository.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("repository facts = %#v, want %#v", got, want)
	}
}

func assertReservationStatus(t *testing.T, repository *memoryRepository, creationID string, want ledger.ReservationStatus) {
	t.Helper()
	reservation := repository.reservation(creationID)
	if reservation == nil {
		t.Fatalf("reservation for creation %q = nil", creationID)
	}
	if reservation.Status != want {
		t.Fatalf("Reservation.Status = %q, want %q", reservation.Status, want)
	}
}

var _ shared.TxRunner = (*memoryTxRunner)(nil)
var _ ledger.Repository = (*memoryRepository)(nil)
