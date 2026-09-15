package data

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestMongo账本付费预留冲正恢复余额(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	creationID := uuid.NewString()
	dailyQuotaID := uuid.NewString()
	reservationID := "reservation:" + creationID
	reserveLedgerID := "reserve:" + creationID
	reverseLedgerID := "reverse:" + creationID
	accounts := database.Collection(schema.CollectionAccounts)
	dailyQuotas := database.Collection(schema.CollectionDailyQuotas)
	reservations := database.Collection(schema.CollectionReservations)
	ledgerEntries := database.Collection(schema.CollectionLedgerEntries)
	cleanupLedgerTestDocuments(t, database,
		[]string{userID},
		[]string{dailyQuotaID},
		[]string{reservationID},
		[]string{reserveLedgerID, reverseLedgerID},
	)

	if _, err := accounts.InsertOne(ctx, model.AccountDocument{
		ID:             userID,
		DiamondBalance: 20,
	}); err != nil {
		t.Fatalf("种入测试账户: %v", err)
	}

	repository := NewLedgerRepository(&Data{client: client, database: database})
	usecase := ledger.NewUsecase(repository, NewMongoTxRunner(client))
	disabledQuota := ledger.QuotaReservation{
		Kind:      "vip_daily_image",
		LocalDate: "2026-09-05",
		Limit:     0,
		Units:     0,
	}
	reserved, err := usecase.Reserve(ctx, ledger.ReserveRequest{
		CreationID:    creationID,
		UserID:        userID,
		PriceDiamonds: 20,
		Quota:         disabledQuota,
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if reserved.Status != ledger.ReservationStatusReserved {
		t.Fatalf("Reserve() status = %q, want reserved", reserved.Status)
	}
	assertLedgerTestAccountBalance(t, ctx, accounts, userID, 0)
	assertLedgerTestReservationStatus(t, ctx, reservations, reservationID, "reserved")
	assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reserveLedgerID, 1)
	assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reverseLedgerID, 0)
	assertLedgerTestIdempotencyKey(t, ctx, ledgerEntries, reserveLedgerID, "reserve:"+creationID)

	reversed, err := usecase.Reverse(ctx, creationID, "generation_failed")
	if err != nil {
		t.Fatalf("Reverse() error = %v", err)
	}
	if reversed.Status != ledger.ReservationStatusReversed {
		t.Fatalf("Reverse() status = %q, want reversed", reversed.Status)
	}
	assertLedgerTestAccountBalance(t, ctx, accounts, userID, 20)
	assertLedgerTestReservationStatus(t, ctx, reservations, reservationID, "reversed")
	assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reserveLedgerID, 1)
	assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reverseLedgerID, 1)
	assertLedgerTestIdempotencyKey(t, ctx, ledgerEntries, reserveLedgerID, "reserve:"+creationID)
	assertLedgerTestIdempotencyKey(t, ctx, ledgerEntries, reverseLedgerID, "reverse:"+creationID)
	assertLedgerTestEntryHasCreatedAt(t, ctx, ledgerEntries, reverseLedgerID)
	assertLedgerTestDocumentCount(t, ctx, dailyQuotas, dailyQuotaID, 0)
}

func TestMongo账本事务内冲正使用冻结业务时间(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	creationID := uuid.NewString()
	reservationID := "reservation:" + creationID
	reserveLedgerID := "reserve:" + creationID
	reverseLedgerID := "reverse:" + creationID
	accounts := database.Collection(schema.CollectionAccounts)
	reservations := database.Collection(schema.CollectionReservations)
	ledgerEntries := database.Collection(schema.CollectionLedgerEntries)
	cleanupLedgerTestDocuments(t, database, []string{userID}, nil, []string{reservationID}, []string{reserveLedgerID, reverseLedgerID})
	reserveAt := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	reverseAt := reserveAt.Add(5 * time.Minute)
	if _, err := accounts.InsertOne(ctx, model.AccountDocument{
		ID:             userID,
		DiamondBalance: 20,
		CreatedAt:      reserveAt,
		UpdatedAt:      reserveAt,
	}); err != nil {
		t.Fatalf("种入测试账户: %v", err)
	}

	repository := NewLedgerRepository(&Data{client: client, database: database})
	runner := NewMongoTxRunner(client)
	usecase := ledger.NewUsecase(repository, runner)
	if err := runner.WithinTx(ctx, func(txCtx context.Context) error {
		_, err := usecase.ReserveInTx(txCtx, ledger.ReserveRequest{
			CreationID:    creationID,
			UserID:        userID,
			PriceDiamonds: 20,
			Quota: ledger.QuotaReservation{
				Kind:      "vip_daily_image",
				LocalDate: "2026-09-07",
			},
			BusinessAt: reserveAt,
		})
		return err
	}); err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}
	if err := runner.WithinTx(ctx, func(txCtx context.Context) error {
		_, err := usecase.ReverseInTx(txCtx, creationID, ledger.ReversalReasonSubmissionRejected, reverseAt)
		return err
	}); err != nil {
		t.Fatalf("ReverseInTx() error = %v", err)
	}

	var account model.AccountDocument
	if err := accounts.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account); err != nil {
		t.Fatalf("读取测试账户: %v", err)
	}
	var reservation model.ReservationDocument
	if err := reservations.FindOne(ctx, bson.D{{Key: "_id", Value: reservationID}}).Decode(&reservation); err != nil {
		t.Fatalf("读取测试预留: %v", err)
	}
	var entry model.LedgerEntryDocument
	if err := ledgerEntries.FindOne(ctx, bson.D{{Key: "_id", Value: reverseLedgerID}}).Decode(&entry); err != nil {
		t.Fatalf("读取冲正账本分录: %v", err)
	}
	if !account.UpdatedAt.Equal(reverseAt) || !reservation.UpdatedAt.Equal(reverseAt) || !entry.CreatedAt.Equal(reverseAt) {
		t.Fatalf("冲正业务时间 account/reservation/entry = %s/%s/%s, want all %s", account.UpdatedAt, reservation.UpdatedAt, entry.CreatedAt, reverseAt)
	}
	if entry.Reason != string(ledger.ReversalReasonSubmissionRejected) {
		t.Fatalf("冲正分录原因 = %q, want %q", entry.Reason, ledger.ReversalReasonSubmissionRejected)
	}
}

func TestMongo账本事务内日免冲正使用冻结业务时间(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	creationID := uuid.NewString()
	dailyQuotaID := uuid.NewString()
	reservationID := "reservation:" + creationID
	reserveLedgerID := "reserve:" + creationID
	reverseLedgerID := "reverse:" + creationID
	accounts := database.Collection(schema.CollectionAccounts)
	dailyQuotas := database.Collection(schema.CollectionDailyQuotas)
	reservations := database.Collection(schema.CollectionReservations)
	ledgerEntries := database.Collection(schema.CollectionLedgerEntries)
	cleanupLedgerTestDocuments(t, database, []string{userID}, []string{dailyQuotaID}, []string{reservationID}, []string{reserveLedgerID, reverseLedgerID})
	reserveAt := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	reverseAt := reserveAt.Add(5 * time.Minute)
	quota := ledger.QuotaReservation{Kind: "vip_daily_image", LocalDate: "2026-09-07", Limit: 1, Units: 1}
	if _, err := accounts.InsertOne(ctx, model.AccountDocument{ID: userID, DiamondBalance: 0, CreatedAt: reserveAt, UpdatedAt: reserveAt}); err != nil {
		t.Fatalf("种入测试账户: %v", err)
	}
	if _, err := dailyQuotas.InsertOne(ctx, model.DailyQuotaDocument{ID: dailyQuotaID, UserID: userID, QuotaKind: quota.Kind, LocalDate: quota.LocalDate, Limit: quota.Limit, UsedCount: 0, UpdatedAt: reserveAt}); err != nil {
		t.Fatalf("种入测试日免: %v", err)
	}

	repository := NewLedgerRepository(&Data{client: client, database: database})
	runner := NewMongoTxRunner(client)
	usecase := ledger.NewUsecase(repository, runner)
	if err := runner.WithinTx(ctx, func(txCtx context.Context) error {
		_, err := usecase.ReserveInTx(txCtx, ledger.ReserveRequest{CreationID: creationID, UserID: userID, PriceDiamonds: 20, Quota: quota, BusinessAt: reserveAt})
		return err
	}); err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}
	if err := runner.WithinTx(ctx, func(txCtx context.Context) error {
		_, err := usecase.ReverseInTx(txCtx, creationID, ledger.ReversalReasonSubmissionRejected, reverseAt)
		return err
	}); err != nil {
		t.Fatalf("ReverseInTx() error = %v", err)
	}

	var dailyQuota model.DailyQuotaDocument
	if err := dailyQuotas.FindOne(ctx, bson.D{{Key: "_id", Value: dailyQuotaID}}).Decode(&dailyQuota); err != nil {
		t.Fatalf("读取测试日免: %v", err)
	}
	var reservation model.ReservationDocument
	if err := reservations.FindOne(ctx, bson.D{{Key: "_id", Value: reservationID}}).Decode(&reservation); err != nil {
		t.Fatalf("读取测试预留: %v", err)
	}
	var entry model.LedgerEntryDocument
	if err := ledgerEntries.FindOne(ctx, bson.D{{Key: "_id", Value: reverseLedgerID}}).Decode(&entry); err != nil {
		t.Fatalf("读取冲正账本分录: %v", err)
	}
	if !dailyQuota.UpdatedAt.Equal(reverseAt) || !reservation.UpdatedAt.Equal(reverseAt) || !entry.CreatedAt.Equal(reverseAt) {
		t.Fatalf("日免冲正业务时间 quota/reservation/entry = %s/%s/%s, want all %s", dailyQuota.UpdatedAt, reservation.UpdatedAt, entry.CreatedAt, reverseAt)
	}
}

// TestMongo账本预留重复键同时保留领域和驱动错误确保上层可按领域语义处理，
// 同时保留 MongoDB 错误链供日志、指标与竞争诊断使用。
func TestMongo账本预留重复键同时保留领域和驱动错误(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	creationID := uuid.NewString()
	reservationID := "reservation:" + creationID
	cleanupLedgerTestDocuments(t, database, nil, nil, []string{reservationID}, nil)
	repository := NewLedgerRepository(&Data{client: client, database: database})
	reservation := &ledger.Reservation{
		ID:            reservationID,
		CreationID:    creationID,
		UserID:        uuid.NewString(),
		PriceDiamonds: 20,
		Source:        ledger.BenefitSourceDiamonds,
		Quota: ledger.QuotaReservation{
			Kind:      "vip_daily_image",
			LocalDate: "2026-09-05",
		},
		Status: ledger.ReservationStatusReserved,
	}
	if err := repository.CreateReservation(ctx, reservation); err != nil {
		t.Fatalf("首次 CreateReservation() error = %v", err)
	}

	err := repository.CreateReservation(ctx, reservation)
	if !errors.Is(err, ledger.ErrReservationCommandConflict) {
		t.Fatalf("CreateReservation() error = %v，要求包含 ErrReservationCommandConflict", err)
	}
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("CreateReservation() error = %v，要求保留 Mongo duplicate key 错误链", err)
	}
}

func TestMongo账本并发日免预留只允许一个成功(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	dailyQuotaID := uuid.NewString()
	creationIDs := []string{uuid.NewString(), uuid.NewString()}
	reservationIDs := []string{"reservation:" + creationIDs[0], "reservation:" + creationIDs[1]}
	ledgerEntryIDs := []string{"reserve:" + creationIDs[0], "reserve:" + creationIDs[1]}
	accounts := database.Collection(schema.CollectionAccounts)
	dailyQuotas := database.Collection(schema.CollectionDailyQuotas)
	reservations := database.Collection(schema.CollectionReservations)
	ledgerEntries := database.Collection(schema.CollectionLedgerEntries)
	cleanupLedgerTestDocuments(t, database, []string{userID}, []string{dailyQuotaID}, reservationIDs, ledgerEntryIDs)

	quota := ledger.QuotaReservation{
		Kind:      "vip_daily_image",
		LocalDate: "2026-09-05",
		Limit:     1,
		Units:     1,
	}
	if _, err := accounts.InsertOne(ctx, model.AccountDocument{
		ID:             userID,
		DiamondBalance: 0,
	}); err != nil {
		t.Fatalf("种入零余额测试账户: %v", err)
	}
	if _, err := dailyQuotas.InsertOne(ctx, model.DailyQuotaDocument{
		ID:        dailyQuotaID,
		UserID:    userID,
		QuotaKind: quota.Kind,
		LocalDate: quota.LocalDate,
		UsedCount: 0,
		Limit:     quota.Limit,
	}); err != nil {
		t.Fatalf("种入日免额度: %v", err)
	}

	repository := NewLedgerRepository(&Data{client: client, database: database})
	usecase := ledger.NewUsecase(repository, NewMongoTxRunner(client))
	start := make(chan struct{})
	results := make(chan ledgerReserveResult, len(creationIDs))
	var waitGroup sync.WaitGroup
	for _, creationID := range creationIDs {
		waitGroup.Add(1)
		go func(creationID string) {
			defer waitGroup.Done()
			<-start
			reservation, err := usecase.Reserve(ctx, ledger.ReserveRequest{
				CreationID:    creationID,
				UserID:        userID,
				PriceDiamonds: 20,
				Quota:         quota,
			})
			results <- ledgerReserveResult{creationID: creationID, reservation: reservation, err: err}
		}(creationID)
	}
	close(start)
	waitGroup.Wait()
	close(results)

	successes := 0
	insufficientFunds := 0
	successfulCreationID := ""
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			successfulCreationID = result.creationID
			if result.reservation.Status != ledger.ReservationStatusReserved {
				t.Fatalf("成功预留状态 = %q, want reserved", result.reservation.Status)
			}
		case errors.Is(result.err, shared.ErrInsufficientFunds):
			insufficientFunds++
		default:
			t.Fatalf("并发 Reserve(%q) error = %v", result.creationID, result.err)
		}
	}
	if successes != 1 || insufficientFunds != 1 {
		t.Fatalf("并发结果：成功 = %d、余额不足 = %d，want 1 和 1", successes, insufficientFunds)
	}

	var storedQuota model.DailyQuotaDocument
	if err := dailyQuotas.FindOne(ctx, bson.D{{Key: "_id", Value: dailyQuotaID}}).Decode(&storedQuota); err != nil {
		t.Fatalf("读取日免额度: %v", err)
	}
	if storedQuota.UsedCount != 1 {
		t.Fatalf("日免 UsedCount = %d, want 1", storedQuota.UsedCount)
	}
	for _, creationID := range creationIDs {
		want := int64(0)
		if creationID == successfulCreationID {
			want = 1
		}
		assertLedgerTestDocumentCount(t, ctx, reservations, "reservation:"+creationID, want)
		assertLedgerTestDocumentCount(t, ctx, ledgerEntries, "reserve:"+creationID, want)
	}
	assertLedgerTestIdempotencyKey(t, ctx, ledgerEntries, "reserve:"+successfulCreationID, "reserve:"+successfulCreationID)
	assertLedgerTestAccountBalance(t, ctx, accounts, userID, 0)
}

func TestMongo账本日免预留按原快照精确冲正(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	creationID := uuid.NewString()
	dailyQuotaID := uuid.NewString()
	reservationID := "reservation:" + creationID
	reserveLedgerID := "reserve:" + creationID
	reverseLedgerID := "reverse:" + creationID
	accounts := database.Collection(schema.CollectionAccounts)
	dailyQuotas := database.Collection(schema.CollectionDailyQuotas)
	reservations := database.Collection(schema.CollectionReservations)
	ledgerEntries := database.Collection(schema.CollectionLedgerEntries)
	cleanupLedgerTestDocuments(t, database,
		[]string{userID},
		[]string{dailyQuotaID},
		[]string{reservationID},
		[]string{reserveLedgerID, reverseLedgerID},
	)

	quota := ledger.QuotaReservation{
		Kind:      "special_video_daily",
		LocalDate: "2026-10-19",
		Limit:     3,
		Units:     2,
	}
	if _, err := accounts.InsertOne(ctx, model.AccountDocument{
		ID:             userID,
		DiamondBalance: 0,
	}); err != nil {
		t.Fatalf("种入零余额测试账户: %v", err)
	}
	if _, err := dailyQuotas.InsertOne(ctx, model.DailyQuotaDocument{
		ID:        dailyQuotaID,
		UserID:    userID,
		QuotaKind: quota.Kind,
		LocalDate: quota.LocalDate,
		UsedCount: 0,
		Limit:     quota.Limit,
	}); err != nil {
		t.Fatalf("种入日免额度: %v", err)
	}

	repository := NewLedgerRepository(&Data{client: client, database: database})
	usecase := ledger.NewUsecase(repository, NewMongoTxRunner(client))
	reserved, err := usecase.Reserve(ctx, ledger.ReserveRequest{
		CreationID:    creationID,
		UserID:        userID,
		PriceDiamonds: 20,
		Quota:         quota,
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if reserved.Status != ledger.ReservationStatusReserved {
		t.Fatalf("Reserve() status = %q, want reserved", reserved.Status)
	}
	assertLedgerTestDailyQuotaUsedCount(t, ctx, dailyQuotas, dailyQuotaID, 2)
	assertLedgerTestReservationQuotaSnapshot(t, ctx, reservations, reservationID, quota)
	assertLedgerTestLedgerDelta(t, ctx, ledgerEntries, reserveLedgerID, 0)

	reversed, err := usecase.Reverse(ctx, creationID, "generation_failed")
	if err != nil {
		t.Fatalf("Reverse() error = %v", err)
	}
	if reversed.Status != ledger.ReservationStatusReversed {
		t.Fatalf("Reverse() status = %q, want reversed", reversed.Status)
	}
	assertLedgerTestDailyQuotaUsedCount(t, ctx, dailyQuotas, dailyQuotaID, 0)
	assertLedgerTestReservationStatus(t, ctx, reservations, reservationID, "reversed")
	assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reserveLedgerID, 1)
	assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reverseLedgerID, 1)
	assertLedgerTestCreationLedgerCount(t, ctx, ledgerEntries, creationID, 2)
	assertLedgerTestLedgerDelta(t, ctx, ledgerEntries, reserveLedgerID, 0)
	assertLedgerTestLedgerDelta(t, ctx, ledgerEntries, reverseLedgerID, 0)
}

func TestMongo账本冲正缺失事实回滚整笔事务(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewLedgerRepository(&Data{client: client, database: database})
	usecase := ledger.NewUsecase(repository, NewMongoTxRunner(client))

	t.Run("付费预扣的账户事实缺失", func(t *testing.T) {
		userID := uuid.NewString()
		creationID := uuid.NewString()
		reservationID := "reservation:" + creationID
		reserveLedgerID := "reserve:" + creationID
		reverseLedgerID := "reverse:" + creationID
		accounts := database.Collection(schema.CollectionAccounts)
		reservations := database.Collection(schema.CollectionReservations)
		ledgerEntries := database.Collection(schema.CollectionLedgerEntries)
		cleanupLedgerTestDocuments(t, database,
			[]string{userID},
			nil,
			[]string{reservationID},
			[]string{reserveLedgerID, reverseLedgerID},
		)

		if _, err := accounts.InsertOne(ctx, model.AccountDocument{
			ID:             userID,
			DiamondBalance: 20,
		}); err != nil {
			t.Fatalf("种入付费测试账户: %v", err)
		}
		if _, err := usecase.Reserve(ctx, ledger.ReserveRequest{
			CreationID:    creationID,
			UserID:        userID,
			PriceDiamonds: 20,
			Quota: ledger.QuotaReservation{
				Kind:      "paid_video",
				LocalDate: "2026-10-19",
				Limit:     0,
				Units:     0,
			},
		}); err != nil {
			t.Fatalf("Reserve() error = %v", err)
		}

		if result, err := accounts.DeleteOne(ctx, bson.D{{Key: "_id", Value: userID}}); err != nil {
			t.Fatalf("删除测试账户事实: %v", err)
		} else if result.DeletedCount != 1 {
			t.Fatalf("删除测试账户事实数量 = %d, want 1", result.DeletedCount)
		}
		// 验证 MongoDB 事务回滚：缺失账户事实不能降级为仅迁移预留状态。
		if _, err := usecase.Reverse(ctx, creationID, "generation_failed"); err == nil {
			t.Fatal("Reverse() error = nil, want missing account fact error")
		}
		assertLedgerTestReservationStatus(t, ctx, reservations, reservationID, "reserved")
		assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reserveLedgerID, 1)
		assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reverseLedgerID, 0)
	})

	t.Run("日免预留的额度事实缺失", func(t *testing.T) {
		userID := uuid.NewString()
		creationID := uuid.NewString()
		dailyQuotaID := uuid.NewString()
		reservationID := "reservation:" + creationID
		reserveLedgerID := "reserve:" + creationID
		reverseLedgerID := "reverse:" + creationID
		accounts := database.Collection(schema.CollectionAccounts)
		dailyQuotas := database.Collection(schema.CollectionDailyQuotas)
		reservations := database.Collection(schema.CollectionReservations)
		ledgerEntries := database.Collection(schema.CollectionLedgerEntries)
		cleanupLedgerTestDocuments(t, database,
			[]string{userID},
			[]string{dailyQuotaID},
			[]string{reservationID},
			[]string{reserveLedgerID, reverseLedgerID},
		)

		quota := ledger.QuotaReservation{
			Kind:      "special_video_daily",
			LocalDate: "2026-10-20",
			Limit:     3,
			Units:     2,
		}
		if _, err := accounts.InsertOne(ctx, model.AccountDocument{
			ID:             userID,
			DiamondBalance: 0,
		}); err != nil {
			t.Fatalf("种入零余额测试账户: %v", err)
		}
		if _, err := dailyQuotas.InsertOne(ctx, model.DailyQuotaDocument{
			ID:        dailyQuotaID,
			UserID:    userID,
			QuotaKind: quota.Kind,
			LocalDate: quota.LocalDate,
			UsedCount: 0,
			Limit:     quota.Limit,
		}); err != nil {
			t.Fatalf("种入日免额度: %v", err)
		}
		if _, err := usecase.Reserve(ctx, ledger.ReserveRequest{
			CreationID:    creationID,
			UserID:        userID,
			PriceDiamonds: 20,
			Quota:         quota,
		}); err != nil {
			t.Fatalf("Reserve() error = %v", err)
		}

		if result, err := dailyQuotas.DeleteOne(ctx, bson.D{{Key: "_id", Value: dailyQuotaID}}); err != nil {
			t.Fatalf("删除测试日免事实: %v", err)
		} else if result.DeletedCount != 1 {
			t.Fatalf("删除测试日免事实数量 = %d, want 1", result.DeletedCount)
		}
		// 验证 MongoDB 事务回滚：缺失日免事实不能降级为仅迁移预留状态。
		if _, err := usecase.Reverse(ctx, creationID, "generation_failed"); err == nil {
			t.Fatal("Reverse() error = nil, want missing daily quota fact error")
		}
		assertLedgerTestReservationStatus(t, ctx, reservations, reservationID, "reserved")
		assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reserveLedgerID, 1)
		assertLedgerTestDocumentCount(t, ctx, ledgerEntries, reverseLedgerID, 0)
	})
}

func Test账户文档仅以ID表达用户归属(t *testing.T) {
	accountType := reflect.TypeOf(model.AccountDocument{})
	if _, exists := accountType.FieldByName("UserID"); exists {
		t.Fatal("AccountDocument 不应定义 UserID 字段")
	}

	// 身份归属由 _id 唯一表达；重复 user_id 会让条件扣钻存在两套定位事实。
	encoded, err := bson.Marshal(model.AccountDocument{ID: "account-user", DiamondBalance: 20})
	if err != nil {
		t.Fatalf("序列化账户文档: %v", err)
	}
	var document bson.M
	if err := bson.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("反序列化账户文档: %v", err)
	}
	if _, exists := document["_id"]; !exists {
		t.Fatalf("账户 BSON 缺少 _id: %#v", document)
	}
	if _, exists := document["diamond_balance"]; !exists {
		t.Fatalf("账户 BSON 缺少 diamond_balance: %#v", document)
	}
	if _, exists := document["user_id"]; exists {
		t.Fatalf("账户 BSON 不应包含 user_id: %#v", document)
	}
}

type ledgerReserveResult struct {
	creationID  string
	reservation *ledger.Reservation
	err         error
}

func cleanupLedgerTestDocuments(t *testing.T, database *mongo.Database, accountIDs, dailyQuotaIDs, reservationIDs, ledgerEntryIDs []string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		deleteLedgerTestDocuments(t, ctx, database.Collection(schema.CollectionAccounts), accountIDs)
		deleteLedgerTestDocuments(t, ctx, database.Collection(schema.CollectionDailyQuotas), dailyQuotaIDs)
		deleteLedgerTestDocuments(t, ctx, database.Collection(schema.CollectionReservations), reservationIDs)
		deleteLedgerTestDocuments(t, ctx, database.Collection(schema.CollectionLedgerEntries), ledgerEntryIDs)
	})
}

func deleteLedgerTestDocuments(t *testing.T, ctx context.Context, collection *mongo.Collection, ids []string) {
	t.Helper()
	for _, id := range ids {
		if _, err := collection.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
			t.Errorf("删除测试集合 %q 中 _id=%q 的文档: %v", collection.Name(), id, err)
		}
	}
}

func assertLedgerTestAccountBalance(t *testing.T, ctx context.Context, accounts *mongo.Collection, accountID string, want int64) {
	t.Helper()
	var account model.AccountDocument
	if err := accounts.FindOne(ctx, bson.D{{Key: "_id", Value: accountID}}).Decode(&account); err != nil {
		t.Fatalf("读取测试账户: %v", err)
	}
	if account.DiamondBalance != want {
		t.Fatalf("账户钻石余额 = %d, want %d", account.DiamondBalance, want)
	}
}

func assertLedgerTestReservationStatus(t *testing.T, ctx context.Context, reservations *mongo.Collection, reservationID, want string) {
	t.Helper()
	var reservation bson.M
	if err := reservations.FindOne(ctx, bson.D{{Key: "_id", Value: reservationID}}).Decode(&reservation); err != nil {
		t.Fatalf("读取预留: %v", err)
	}
	if got, _ := reservation["status"].(string); got != want {
		t.Fatalf("预留状态 = %q, want %q", got, want)
	}
}

func assertLedgerTestReservationQuotaSnapshot(t *testing.T, ctx context.Context, reservations *mongo.Collection, reservationID string, want ledger.QuotaReservation) {
	t.Helper()
	var reservation model.ReservationDocument
	if err := reservations.FindOne(ctx, bson.D{{Key: "_id", Value: reservationID}}).Decode(&reservation); err != nil {
		t.Fatalf("读取预留快照: %v", err)
	}
	if reservation.QuotaKind != want.Kind ||
		reservation.LocalDate != want.LocalDate ||
		reservation.QuotaLimit != want.Limit ||
		reservation.QuotaUnits != want.Units {
		t.Fatalf("预留额度快照 = kind=%q date=%q limit=%d units=%d, want kind=%q date=%q limit=%d units=%d",
			reservation.QuotaKind,
			reservation.LocalDate,
			reservation.QuotaLimit,
			reservation.QuotaUnits,
			want.Kind,
			want.LocalDate,
			want.Limit,
			want.Units,
		)
	}
}

func assertLedgerTestDailyQuotaUsedCount(t *testing.T, ctx context.Context, dailyQuotas *mongo.Collection, dailyQuotaID string, want int32) {
	t.Helper()
	var quota model.DailyQuotaDocument
	if err := dailyQuotas.FindOne(ctx, bson.D{{Key: "_id", Value: dailyQuotaID}}).Decode(&quota); err != nil {
		t.Fatalf("读取日免额度: %v", err)
	}
	if quota.UsedCount != want {
		t.Fatalf("日免 UsedCount = %d, want %d", quota.UsedCount, want)
	}
}

func assertLedgerTestIdempotencyKey(t *testing.T, ctx context.Context, entries *mongo.Collection, entryID, want string) {
	t.Helper()
	var entry bson.M
	if err := entries.FindOne(ctx, bson.D{{Key: "_id", Value: entryID}}).Decode(&entry); err != nil {
		t.Fatalf("读取账本分录: %v", err)
	}
	if got, _ := entry["idempotency_key"].(string); got != want {
		t.Fatalf("账本幂等键 = %q, want %q", got, want)
	}
}

func assertLedgerTestLedgerDelta(t *testing.T, ctx context.Context, entries *mongo.Collection, entryID string, want int64) {
	t.Helper()
	var entry model.LedgerEntryDocument
	if err := entries.FindOne(ctx, bson.D{{Key: "_id", Value: entryID}}).Decode(&entry); err != nil {
		t.Fatalf("读取账本分录: %v", err)
	}
	if entry.DeltaDiamonds != want {
		t.Fatalf("账本分录 _id=%q 的钻石变化 = %d, want %d", entryID, entry.DeltaDiamonds, want)
	}
}

func assertLedgerTestEntryHasCreatedAt(t *testing.T, ctx context.Context, entries *mongo.Collection, entryID string) {
	t.Helper()
	var entry model.LedgerEntryDocument
	if err := entries.FindOne(ctx, bson.D{{Key: "_id", Value: entryID}}).Decode(&entry); err != nil {
		t.Fatalf("读取账本分录: %v", err)
	}
	if entry.CreatedAt.IsZero() {
		t.Fatalf("账本分录 _id=%q 的 created_at 不能为空", entryID)
	}
}

func assertLedgerTestCreationLedgerCount(t *testing.T, ctx context.Context, entries *mongo.Collection, creationID string, want int64) {
	t.Helper()
	count, err := entries.CountDocuments(ctx, bson.D{{Key: "creation_id", Value: creationID}})
	if err != nil {
		t.Fatalf("统计创作 %q 的账本分录: %v", creationID, err)
	}
	if count != want {
		t.Fatalf("创作 %q 的账本分录数 = %d, want %d", creationID, count, want)
	}
}

func assertLedgerTestDocumentCount(t *testing.T, ctx context.Context, collection *mongo.Collection, id string, want int64) {
	t.Helper()
	count, err := collection.CountDocuments(ctx, bson.D{{Key: "_id", Value: id}})
	if err != nil {
		t.Fatalf("统计集合 %q 中 _id=%q 的文档: %v", collection.Name(), id, err)
	}
	if count != want {
		t.Fatalf("集合 %q 中 _id=%q 的文档数 = %d, want %d", collection.Name(), id, count, want)
	}
}
