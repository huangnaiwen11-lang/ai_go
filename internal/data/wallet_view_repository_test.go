package data

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"ai-business-service/internal/biz/walletview"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestMongo钱包只读快照按用户时区读取当日额度且不写缺失余额(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	// 当前 UTC 时刻在上海已跨日，而洛杉矶仍是前一天；查询必须只命中用户固化时区的本地日期。
	now := time.Date(2026, time.September, 11, 0, 30, 0, 0, time.UTC)
	userID := uuid.NewString()
	otherUserID := uuid.NewString()
	localDate := "2026-09-11"
	previousDate := "2026-09-10"
	insertWalletViewDocument(t, ctx, database, schema.CollectionUsers, model.UserDocument{ID: userID, Timezone: "Asia/Shanghai"})
	insertWalletViewDocument(t, ctx, database, schema.CollectionUsers, model.UserDocument{ID: otherUserID, Timezone: "Asia/Shanghai"})
	insertWalletViewDocument(t, ctx, database, schema.CollectionAccounts, model.AccountDocument{ID: otherUserID, DiamondBalance: 999})
	insertWalletViewDocument(t, ctx, database, schema.CollectionSubscriptions, model.SubscriptionDocument{UserID: userID, Status: "active", ExpiresAt: now.Add(time.Hour)})
	insertWalletViewDocument(t, ctx, database, schema.CollectionDailyQuotas, model.DailyQuotaDocument{ID: uuid.NewString(), UserID: userID, QuotaKind: walletViewImageQuotaKind, LocalDate: localDate, Limit: 10, UsedCount: 4})
	insertWalletViewDocument(t, ctx, database, schema.CollectionDailyQuotas, model.DailyQuotaDocument{ID: uuid.NewString(), UserID: userID, QuotaKind: walletViewVideoQuotaKind, LocalDate: localDate, Limit: 3, UsedCount: 8})
	insertWalletViewDocument(t, ctx, database, schema.CollectionDailyQuotas, model.DailyQuotaDocument{ID: uuid.NewString(), UserID: userID, QuotaKind: walletViewImageQuotaKind, LocalDate: previousDate, Limit: 10, UsedCount: 1})
	insertWalletViewDocument(t, ctx, database, schema.CollectionDailyQuotas, model.DailyQuotaDocument{ID: uuid.NewString(), UserID: otherUserID, QuotaKind: walletViewImageQuotaKind, LocalDate: localDate, Limit: 99, UsedCount: 1})

	repository := NewWalletViewRepositoryWithClock(&Data{database: database}, func() time.Time { return now })
	snapshot, err := repository.FindSnapshot(ctx, userID)
	if err != nil {
		t.Fatalf("FindSnapshot() error = %v", err)
	}
	if snapshot.DiamondBalance != 0 {
		t.Fatalf("缺失账户余额 = %d，want 0", snapshot.DiamondBalance)
	}
	if !snapshot.VIP.Active || !snapshot.VIP.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("VIP = %#v，want active until %s", snapshot.VIP, now.Add(time.Hour))
	}
	if snapshot.Timezone != "Asia/Shanghai" {
		t.Fatalf("Timezone = %q，want Asia/Shanghai", snapshot.Timezone)
	}
	if snapshot.DailyImage != (walletview.DailyQuota{Limit: 10, Used: 4, Remaining: 6, LocalDate: localDate}) {
		t.Fatalf("DailyImage = %#v，want 当日上海图片额度", snapshot.DailyImage)
	}
	if snapshot.DailyVideo != (walletview.DailyQuota{Limit: 3, Used: 8, Remaining: 0, LocalDate: localDate}) {
		t.Fatalf("DailyVideo = %#v，want remaining 截断到 0", snapshot.DailyVideo)
	}
	assertWalletViewDocumentCount(t, ctx, database, schema.CollectionAccounts, bson.D{{Key: "_id", Value: userID}}, 0)
	assertWalletViewDocumentCount(t, ctx, database, schema.CollectionDailyQuotas, bson.D{{Key: "user_id", Value: userID}}, 3)
}

func TestMongo钱包只读快照只将有效Active订阅投影为VIP(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	now := time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC)
	testCases := []struct {
		name         string
		status       string
		expiresAt    time.Time
		wantVIPAlive bool
	}{
		{name: "有效 active", status: "active", expiresAt: now.Add(time.Hour), wantVIPAlive: true},
		{name: "已过期 active", status: "active", expiresAt: now.Add(-time.Nanosecond), wantVIPAlive: false},
		{name: "非 active", status: "cancelled", expiresAt: now.Add(time.Hour), wantVIPAlive: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			userID := uuid.NewString()
			insertWalletViewDocument(t, ctx, database, schema.CollectionUsers, model.UserDocument{ID: userID, Timezone: "UTC"})
			insertWalletViewDocument(t, ctx, database, schema.CollectionSubscriptions, model.SubscriptionDocument{UserID: userID, Status: testCase.status, ExpiresAt: testCase.expiresAt})

			repository := NewWalletViewRepositoryWithClock(&Data{database: database}, func() time.Time { return now })
			snapshot, err := repository.FindSnapshot(ctx, userID)
			if err != nil {
				t.Fatalf("FindSnapshot() error = %v", err)
			}
			if snapshot.VIP.Active != testCase.wantVIPAlive {
				t.Fatalf("VIP.Active = %t，want %t", snapshot.VIP.Active, testCase.wantVIPAlive)
			}
		})
	}
}

func TestMongo钱包账本只返回本人并保持Cursor与Skip分页语义(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	otherUserID := uuid.NewString()
	base := time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC)
	entries := []model.LedgerEntryDocument{
		{ID: "wallet-view-" + uuid.NewString(), AccountID: userID, CreationID: "creation-1", DeltaDiamonds: -20, Reason: "generation_reserved", CreatedAt: base.Add(3 * time.Minute)},
		{ID: "wallet-view-" + uuid.NewString(), AccountID: userID, CreationID: "creation-2", DeltaDiamonds: 20, Reason: "generation_reversed", CreatedAt: base.Add(2 * time.Minute)},
		{ID: "wallet-view-" + uuid.NewString(), AccountID: userID, CreationID: "creation-3", DeltaDiamonds: 50, Reason: "payment_credited", CreatedAt: base.Add(time.Minute)},
		{ID: "wallet-view-" + uuid.NewString(), AccountID: otherUserID, CreationID: "other-creation", DeltaDiamonds: 999, Reason: "other_user", CreatedAt: base.Add(4 * time.Minute)},
	}
	for index := range entries {
		entries[index].IdempotencyKey = entries[index].ID
		insertWalletViewDocument(t, ctx, database, schema.CollectionLedgerEntries, entries[index])
	}

	repository := NewWalletViewRepositoryWithClock(&Data{database: database}, func() time.Time { return base })
	firstPage, err := repository.ListLedgerEntries(ctx, walletview.LedgerPageQuery{UserID: userID, Limit: 2})
	if err != nil {
		t.Fatalf("ListLedgerEntries(first page) error = %v", err)
	}
	if got, want := walletViewLedgerIDs(firstPage), []string{entries[0].ID, entries[1].ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("第一页账本 ID = %v，want %v", got, want)
	}
	if firstPage.NextCursor == "" {
		t.Fatal("NextCursor 为空，want 满页时返回分页游标")
	}

	cursorPage, err := repository.ListLedgerEntries(ctx, walletview.LedgerPageQuery{UserID: userID, Limit: 2, Skip: 999, Cursor: firstPage.NextCursor})
	if err != nil {
		t.Fatalf("ListLedgerEntries(cursor page) error = %v", err)
	}
	if got, want := walletViewLedgerIDs(cursorPage), []string{entries[2].ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cursor 页账本 ID = %v，want %v", got, want)
	}
	if cursorPage.NextCursor != "" {
		t.Fatalf("cursor 页 NextCursor = %q，want empty", cursorPage.NextCursor)
	}

	skipPage, err := repository.ListLedgerEntries(ctx, walletview.LedgerPageQuery{UserID: userID, Limit: 1, Skip: 1})
	if err != nil {
		t.Fatalf("ListLedgerEntries(skip page) error = %v", err)
	}
	if got, want := walletViewLedgerIDs(skipPage), []string{entries[1].ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skip 页账本 ID = %v，want %v", got, want)
	}
}

func TestMongo钱包账本复合游标不会遗漏同一时间戳分录(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	otherUserID := uuid.NewString()
	prefix := "wallet-view-same-time-" + uuid.NewString() + "-"
	sameTime := time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC)
	entries := []model.LedgerEntryDocument{
		{ID: prefix + "c", AccountID: userID, CreationID: "creation-c", DeltaDiamonds: -20, Reason: "generation_reserved", CreatedAt: sameTime},
		{ID: prefix + "b", AccountID: userID, CreationID: "creation-b", DeltaDiamonds: -20, Reason: "generation_reserved", CreatedAt: sameTime},
		{ID: prefix + "a", AccountID: userID, CreationID: "creation-a", DeltaDiamonds: -20, Reason: "generation_reserved", CreatedAt: sameTime},
		{ID: prefix + "older", AccountID: userID, CreationID: "creation-older", DeltaDiamonds: 20, Reason: "generation_reversed", CreatedAt: sameTime.Add(-time.Minute)},
		{ID: prefix + "other", AccountID: otherUserID, CreationID: "creation-other", DeltaDiamonds: 999, Reason: "other_user", CreatedAt: sameTime},
	}
	for index := range entries {
		entries[index].IdempotencyKey = entries[index].ID
		insertWalletViewDocument(t, ctx, database, schema.CollectionLedgerEntries, entries[index])
	}

	repository := NewWalletViewRepositoryWithClock(&Data{database: database}, func() time.Time { return sameTime })
	wantIDs := []string{entries[0].ID, entries[1].ID, entries[2].ID, entries[3].ID}
	for _, limit := range []int{1, 2} {
		t.Run("limit="+strconv.Itoa(limit), func(t *testing.T) {
			gotIDs := collectWalletViewLedgerIDs(t, ctx, repository, userID, limit)
			if !reflect.DeepEqual(gotIDs, wantIDs) {
				t.Fatalf("limit %d 账本 ID = %v，want %v；同一时间戳分录不得遗漏或重复", limit, gotIDs, wantIDs)
			}
			if len(uniqueWalletViewLedgerIDs(gotIDs)) != len(wantIDs) {
				t.Fatalf("limit %d 账本出现重复 ID: %v", limit, gotIDs)
			}
		})
	}
}

func insertWalletViewDocument(t *testing.T, ctx context.Context, database *mongo.Database, collectionName string, document interface{}) {
	t.Helper()
	collection := database.Collection(collectionName)
	if _, err := collection.InsertOne(ctx, document); err != nil {
		t.Fatalf("写入钱包只读测试数据到 %s: %v", collectionName, err)
	}

	var id string
	switch typed := document.(type) {
	case model.UserDocument:
		id = typed.ID
	case model.AccountDocument:
		id = typed.ID
	case model.SubscriptionDocument:
		id = typed.UserID
	case model.DailyQuotaDocument:
		id = typed.ID
	case model.LedgerEntryDocument:
		id = typed.ID
	default:
		t.Fatalf("未识别的钱包只读测试文档类型 %T", document)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := newMongoTestContext()
		defer cancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: id}}); err != nil {
			t.Errorf("清理钱包只读测试数据 %s/%s: %v", collectionName, id, err)
		}
	})
}

func assertWalletViewDocumentCount(t *testing.T, ctx context.Context, database *mongo.Database, collectionName string, filter bson.D, want int64) {
	t.Helper()
	got, err := database.Collection(collectionName).CountDocuments(ctx, filter)
	if err != nil {
		t.Fatalf("统计钱包只读测试数据 %s: %v", collectionName, err)
	}
	if got != want {
		t.Fatalf("%s 文档数量 = %d，want %d", collectionName, got, want)
	}
}

func walletViewLedgerIDs(page *walletview.LedgerPage) []string {
	ids := make([]string, 0, len(page.Entries))
	for _, entry := range page.Entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

func collectWalletViewLedgerIDs(t *testing.T, ctx context.Context, repository walletview.Repository, userID string, limit int) []string {
	t.Helper()
	query := walletview.LedgerPageQuery{UserID: userID, Limit: limit}
	result := make([]string, 0)
	for {
		page, err := repository.ListLedgerEntries(ctx, query)
		if err != nil {
			t.Fatalf("ListLedgerEntries(limit=%d) error = %v", limit, err)
		}
		result = append(result, walletViewLedgerIDs(page)...)
		if page.NextCursor == "" {
			return result
		}
		query.Cursor = page.NextCursor
		query.Skip = 999
	}
}

func uniqueWalletViewLedgerIDs(ids []string) map[string]struct{} {
	unique := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		unique[id] = struct{}{}
	}
	return unique
}
