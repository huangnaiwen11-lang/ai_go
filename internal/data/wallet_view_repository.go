package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ai-business-service/internal/biz/walletview"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	// 这两个值是创作预留写入的日免类别。钱包页只读取同一事实，不能引入第二套额度来源。
	walletViewImageQuotaKind = "vip_daily_image"
	walletViewVideoQuotaKind = "vip_daily_video"
)

type mongoWalletViewRepository struct {
	accounts      *mongo.Collection
	users         *mongo.Collection
	subscriptions *mongo.Collection
	dailyQuotas   *mongo.Collection
	ledgerEntries *mongo.Collection
	clock         func() time.Time
}

// NewWalletViewRepository 创建钱包页面的 MongoDB 只读投影仓储。
// 它只读取 Go 自有账户、订阅、日额度和账本集合，绝不读取旧 Node 钱包或支付订单。
func NewWalletViewRepository(data *Data) walletview.Repository {
	return NewWalletViewRepositoryWithClock(data, time.Now)
}

// NewWalletViewRepositoryWithClock 允许本地测试冻结读取时刻，生产代码应使用 NewWalletViewRepository。
func NewWalletViewRepositoryWithClock(data *Data, clock func() time.Time) walletview.Repository {
	if clock == nil {
		clock = time.Now
	}
	repository := &mongoWalletViewRepository{clock: clock}
	if data == nil || data.database == nil {
		return repository
	}
	repository.accounts = data.database.Collection(schema.CollectionAccounts)
	repository.users = data.database.Collection(schema.CollectionUsers)
	repository.subscriptions = data.database.Collection(schema.CollectionSubscriptions)
	repository.dailyQuotas = data.database.Collection(schema.CollectionDailyQuotas)
	repository.ledgerEntries = data.database.Collection(schema.CollectionLedgerEntries)
	return repository
}

// FindSnapshot 汇总当前用户的自有钱包读取事实。
// 此查询不能为了“补齐余额”而写入账户或日免：生成必须在后续事务中条件预扣，
// 把展示查询变成写入门禁会重新引入并发下的双免问题。
func (repository *mongoWalletViewRepository) FindSnapshot(ctx context.Context, userID string) (*walletview.Snapshot, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}

	snapshot := &walletview.Snapshot{UserID: userID}
	if err := repository.findDiamondBalance(ctx, userID, snapshot); err != nil {
		return nil, err
	}

	var user model.UserDocument
	err := repository.users.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&user)
	if errors.Is(err, mongo.ErrNoDocuments) {
		// 当前请求通常已由会话层确认用户存在。若历史投影缺失，也只返回已有余额事实，
		// 不创建默认用户或默认时区，避免把部署机器时区误当作用户时区。
		return snapshot, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find wallet view user %q: %w", userID, err)
	}
	snapshot.Timezone = user.Timezone

	if user.Timezone == "" || user.Timezone == "Local" {
		return nil, fmt.Errorf("wallet user %q has no valid stored timezone", userID)
	}
	location, err := time.LoadLocation(user.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load stored timezone for wallet user %q: %w", userID, err)
	}
	now := repository.clock().UTC()
	localDate := now.In(location).Format("2006-01-02")

	if err := repository.findVIP(ctx, userID, now, snapshot); err != nil {
		return nil, err
	}
	imageQuota, err := repository.findDailyQuota(ctx, userID, walletViewImageQuotaKind, localDate)
	if err != nil {
		return nil, err
	}
	videoQuota, err := repository.findDailyQuota(ctx, userID, walletViewVideoQuotaKind, localDate)
	if err != nil {
		return nil, err
	}
	snapshot.DailyImage = imageQuota
	snapshot.DailyVideo = videoQuota
	return snapshot, nil
}

// ListLedgerEntries 只读取当前账户的账本，绝不连带支付订单、回执或用户资料。
func (repository *mongoWalletViewRepository) ListLedgerEntries(ctx context.Context, query walletview.LedgerPageQuery) (*walletview.LedgerPage, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}

	filter := bson.D{{Key: "account_id", Value: query.UserID}}
	findOptions := options.Find().SetSort(bson.D{
		{Key: "created_at", Value: -1},
		{Key: "_id", Value: -1},
	}).SetLimit(int64(query.Limit))
	if query.Cursor != "" {
		cursorPosition, err := walletview.ParseLedgerCursor(query.Cursor)
		if err != nil {
			return nil, err
		}
		// cursor 是唯一分页基准；即使低层调用方误传 Skip，也绝不能把两个范围叠加。
		filter = append(filter, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "created_at", Value: bson.D{{Key: "$lt", Value: cursorPosition.CreatedAt}}}},
			bson.D{{Key: "created_at", Value: cursorPosition.CreatedAt}, {Key: "_id", Value: bson.D{{Key: "$lt", Value: cursorPosition.EntryID}}}},
		}})
	} else if query.Skip > 0 {
		findOptions.SetSkip(int64(query.Skip))
	}

	cursor, err := repository.ledgerEntries.Find(ctx, filter, findOptions)
	if err != nil {
		return nil, fmt.Errorf("find wallet ledger entries for user %q: %w", query.UserID, err)
	}
	defer cursor.Close(ctx)

	page := &walletview.LedgerPage{Entries: make([]walletview.LedgerEntry, 0, query.Limit)}
	for cursor.Next(ctx) {
		var document model.LedgerEntryDocument
		if err := cursor.Decode(&document); err != nil {
			return nil, fmt.Errorf("decode wallet ledger entry for user %q: %w", query.UserID, err)
		}
		page.Entries = append(page.Entries, walletview.LedgerEntry{
			ID:            document.ID,
			CreationID:    document.CreationID,
			DeltaDiamonds: document.DeltaDiamonds,
			Reason:        document.Reason,
			CreatedAt:     document.CreatedAt,
		})
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate wallet ledger entries for user %q: %w", query.UserID, err)
	}
	if len(page.Entries) == query.Limit {
		nextCursor, err := walletview.EncodeLedgerCursor(page.Entries[len(page.Entries)-1].CreatedAt, page.Entries[len(page.Entries)-1].ID)
		if err != nil {
			return nil, fmt.Errorf("encode wallet ledger next cursor for user %q: %w", query.UserID, err)
		}
		page.NextCursor = nextCursor
	}
	return page, nil
}

func (repository *mongoWalletViewRepository) findDiamondBalance(ctx context.Context, userID string, snapshot *walletview.Snapshot) error {
	var account model.AccountDocument
	err := repository.accounts.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find wallet balance for user %q: %w", userID, err)
	}
	snapshot.DiamondBalance = account.DiamondBalance
	return nil
}

func (repository *mongoWalletViewRepository) findVIP(ctx context.Context, userID string, now time.Time, snapshot *walletview.Snapshot) error {
	var subscription model.SubscriptionDocument
	err := repository.subscriptions.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&subscription)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find wallet subscription for user %q: %w", userID, err)
	}
	if subscription.Status == "active" && subscription.ExpiresAt.After(now) {
		snapshot.VIP = walletview.VIP{Active: true, ExpiresAt: subscription.ExpiresAt}
	}
	return nil
}

func (repository *mongoWalletViewRepository) findDailyQuota(ctx context.Context, userID, kind, localDate string) (walletview.DailyQuota, error) {
	quota := walletview.DailyQuota{LocalDate: localDate}
	var document model.DailyQuotaDocument
	err := repository.dailyQuotas.FindOne(ctx, bson.D{
		{Key: "user_id", Value: userID},
		{Key: "quota_kind", Value: kind},
		{Key: "local_date", Value: localDate},
	}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return quota, nil
	}
	if err != nil {
		return walletview.DailyQuota{}, fmt.Errorf("find wallet daily quota for user %q, kind %q, date %q: %w", userID, kind, localDate, err)
	}
	quota.Limit = document.Limit
	quota.Used = document.UsedCount
	quota.Remaining = document.Limit - document.UsedCount
	if quota.Remaining < 0 {
		quota.Remaining = 0
	}
	return quota, nil
}

func (repository *mongoWalletViewRepository) ready() error {
	if repository == nil || repository.accounts == nil || repository.users == nil || repository.subscriptions == nil || repository.dailyQuotas == nil || repository.ledgerEntries == nil || repository.clock == nil {
		return errors.New("wallet view repository is not configured")
	}
	return nil
}

var _ walletview.Repository = (*mongoWalletViewRepository)(nil)
