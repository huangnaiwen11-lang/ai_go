package walletview_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/walletview"
)

func TestGetSnapshot缺失钱包投影返回零钻石余额(t *testing.T) {
	repository := &fakeRepository{}
	usecase := walletview.NewUsecase(repository)

	snapshot, err := usecase.GetSnapshot(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GetSnapshot() error = %v", err)
	}
	if snapshot.UserID != "user-1" {
		t.Fatalf("snapshot.UserID = %q, want user-1", snapshot.UserID)
	}
	if snapshot.DiamondBalance != 0 {
		t.Fatalf("snapshot.DiamondBalance = %d, want 0", snapshot.DiamondBalance)
	}
}

func TestGetSnapshot完整投影不丢失权益与额度字段(t *testing.T) {
	vipExpiresAt := time.Date(2026, time.October, 11, 8, 0, 0, 0, time.UTC)
	repository := &fakeRepository{snapshot: &walletview.Snapshot{
		UserID:         "user-1",
		DiamondBalance: 80,
		VIP: walletview.VIP{
			Active:    true,
			ExpiresAt: vipExpiresAt,
		},
		Timezone: "Asia/Shanghai",
		DailyImage: walletview.DailyQuota{
			Limit:     10,
			Used:      3,
			Remaining: 7,
			LocalDate: "2026-09-11",
		},
		DailyVideo: walletview.DailyQuota{
			Limit:     3,
			Used:      1,
			Remaining: 2,
			LocalDate: "2026-09-11",
		},
	}}
	usecase := walletview.NewUsecase(repository)

	snapshot, err := usecase.GetSnapshot(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GetSnapshot() error = %v", err)
	}
	if snapshot.UserID != "user-1" || snapshot.DiamondBalance != 80 {
		t.Fatalf("基础快照 = %#v，want user-1 / 80 diamonds", snapshot)
	}
	if !snapshot.VIP.Active || !snapshot.VIP.ExpiresAt.Equal(vipExpiresAt) {
		t.Fatalf("VIP = %#v，want active until %s", snapshot.VIP, vipExpiresAt)
	}
	if snapshot.Timezone != "Asia/Shanghai" {
		t.Fatalf("Timezone = %q，want Asia/Shanghai", snapshot.Timezone)
	}
	if snapshot.DailyImage != (walletview.DailyQuota{Limit: 10, Used: 3, Remaining: 7, LocalDate: "2026-09-11"}) {
		t.Fatalf("DailyImage = %#v，want complete image quota", snapshot.DailyImage)
	}
	if snapshot.DailyVideo != (walletview.DailyQuota{Limit: 3, Used: 1, Remaining: 2, LocalDate: "2026-09-11"}) {
		t.Fatalf("DailyVideo = %#v，want complete video quota", snapshot.DailyVideo)
	}
}

func TestListLedgerEntries有Cursor时忽略Skip(t *testing.T) {
	repository := &fakeRepository{
		ledgerPage: &walletview.LedgerPage{Entries: []walletview.LedgerEntry{{
			ID:            "ledger-1",
			CreationID:    "creation-1",
			DeltaDiamonds: -20,
			Reason:        "generation_reserved",
			CreatedAt:     time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC),
		}}},
	}
	usecase := walletview.NewUsecase(repository)

	_, err := usecase.ListLedgerEntries(context.Background(), walletview.LedgerPageQuery{
		UserID: "user-1",
		Limit:  20,
		Skip:   40,
		Cursor: walletViewTestLedgerCursor(t, time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC), "ledger-previous-page"),
	})
	if err != nil {
		t.Fatalf("ListLedgerEntries() error = %v", err)
	}
	if repository.receivedLedgerQuery.Cursor == "" {
		t.Fatal("repository cursor 为空，want 保留有效 cursor")
	}
	if repository.receivedLedgerQuery.Skip != 0 {
		t.Fatalf("repository skip = %d, want 0 when cursor is present", repository.receivedLedgerQuery.Skip)
	}
}

func TestListLedgerEntries拒绝非法复合Cursor且不访问仓储(t *testing.T) {
	repository := &fakeRepository{}
	usecase := walletview.NewUsecase(repository)

	_, err := usecase.ListLedgerEntries(context.Background(), walletview.LedgerPageQuery{
		UserID: "user-1",
		Limit:  20,
		Cursor: "not-an-opaque-ledger-cursor",
	})
	if !errors.Is(err, walletview.ErrInvalidWalletViewQuery) {
		t.Fatalf("ListLedgerEntries() error = %v，want ErrInvalidWalletViewQuery", err)
	}
	if repository.ledgerCalls != 0 {
		t.Fatalf("非法 cursor 的仓储调用次数 = %d，want 0", repository.ledgerCalls)
	}
}

func Test只读查询拒绝非法参数且不访问仓储(t *testing.T) {
	type queryKind string
	const (
		snapshotQuery queryKind = "snapshot"
		ledgerQuery   queryKind = "ledger"
	)

	testCases := []struct {
		name         string
		kind         queryKind
		snapshotUser string
		ledgerQuery  walletview.LedgerPageQuery
	}{
		{name: "快照空用户 ID", kind: snapshotQuery, snapshotUser: ""},
		{name: "快照空白用户 ID", kind: snapshotQuery, snapshotUser: " \t "},
		{name: "账本空用户 ID", kind: ledgerQuery, ledgerQuery: walletview.LedgerPageQuery{Limit: 20}},
		{name: "账本空白用户 ID", kind: ledgerQuery, ledgerQuery: walletview.LedgerPageQuery{UserID: "\n", Limit: 20}},
		{name: "账本 Limit 为负数", kind: ledgerQuery, ledgerQuery: walletview.LedgerPageQuery{UserID: "user-1", Limit: -1}},
		{name: "账本 Limit 为零", kind: ledgerQuery, ledgerQuery: walletview.LedgerPageQuery{UserID: "user-1", Limit: 0}},
		{name: "账本 Skip 为负数", kind: ledgerQuery, ledgerQuery: walletview.LedgerPageQuery{UserID: "user-1", Limit: 20, Skip: -1}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			repository := &fakeRepository{}
			usecase := walletview.NewUsecase(repository)

			var err error
			if testCase.kind == snapshotQuery {
				_, err = usecase.GetSnapshot(context.Background(), testCase.snapshotUser)
			} else {
				_, err = usecase.ListLedgerEntries(context.Background(), testCase.ledgerQuery)
			}

			if !errors.Is(err, walletview.ErrInvalidWalletViewQuery) {
				t.Fatalf("查询 error = %v，want ErrInvalidWalletViewQuery", err)
			}
			if repository.snapshotCalls != 0 || repository.ledgerCalls != 0 {
				t.Fatalf("仓储调用次数 snapshot/ledger = %d/%d，want 0/0", repository.snapshotCalls, repository.ledgerCalls)
			}
		})
	}
}

type fakeRepository struct {
	snapshot            *walletview.Snapshot
	ledgerPage          *walletview.LedgerPage
	receivedLedgerQuery walletview.LedgerPageQuery
	snapshotCalls       int
	ledgerCalls         int
}

// walletViewTestLedgerCursor 固定测试期望的 JSON + URL-safe Base64 游标格式。
// 载荷不是展示字段；时间与分录 ID 共同构成稳定分页位置，避免同一时间戳的账本分录遗漏。
func walletViewTestLedgerCursor(t *testing.T, createdAt time.Time, entryID string) string {
	t.Helper()
	payload, err := json.Marshal(struct {
		CreatedAt string `json:"created_at"`
		EntryID   string `json:"entry_id"`
	}{
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
		EntryID:   entryID,
	})
	if err != nil {
		t.Fatalf("编码账本测试游标: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func (repository *fakeRepository) FindSnapshot(context.Context, string) (*walletview.Snapshot, error) {
	repository.snapshotCalls++
	return repository.snapshot, nil
}

func (repository *fakeRepository) ListLedgerEntries(_ context.Context, query walletview.LedgerPageQuery) (*walletview.LedgerPage, error) {
	repository.ledgerCalls++
	repository.receivedLedgerQuery = query
	return repository.ledgerPage, nil
}
