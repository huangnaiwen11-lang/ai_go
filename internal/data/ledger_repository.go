package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type mongoLedgerRepository struct {
	accounts      *mongo.Collection
	dailyQuotas   *mongo.Collection
	reservations  *mongo.Collection
	ledgerEntries *mongo.Collection
}

// NewLedgerRepository 返回账本领域仓储接口，不向业务层暴露 MongoDB 细节。
func NewLedgerRepository(data *Data) ledger.Repository {
	if data == nil || data.database == nil {
		return nil
	}
	return &mongoLedgerRepository{
		accounts:      data.database.Collection(schema.CollectionAccounts),
		dailyQuotas:   data.database.Collection(schema.CollectionDailyQuotas),
		reservations:  data.database.Collection(schema.CollectionReservations),
		ledgerEntries: data.database.Collection(schema.CollectionLedgerEntries),
	}
}

// FindReservation 按创作 ID 查找预留；缺失预留由领域层按命令语义处理。
func (repository *mongoLedgerRepository) FindReservation(ctx context.Context, creationID string) (*ledger.Reservation, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	var document model.ReservationDocument
	err := repository.reservations.FindOne(ctx, bson.D{{Key: "creation_id", Value: creationID}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find reservation for creation %q: %w", creationID, err)
	}
	return toBizReservation(document), nil
}

// FindDiamondBalance 返回当前事务可见的自有账户余额。
// 调用方只能在预留成功后用于响应投影，不能把它当作创建前的余额门禁。
func (repository *mongoLedgerRepository) FindDiamondBalance(ctx context.Context, userID string) (int64, error) {
	if err := repository.ready(); err != nil {
		return 0, err
	}
	var account model.AccountDocument
	err := repository.accounts.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account)
	if errors.Is(err, mongo.ErrNoDocuments) {
		// 日免创建可在尚未物化账户余额文档时合法完成；缺失账户只表示余额为零。
		// 它绝不能绕过 TryDebitDiamonds：后者仍要求账户存在且余额足够。
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find diamond balance for user %q: %w", userID, err)
	}
	return account.DiamondBalance, nil
}

// TryConsumeQuota 通过单条条件更新占用额度，避免先读再写造成并发超额。
func (repository *mongoLedgerRepository) TryConsumeQuota(ctx context.Context, userID string, quota ledger.QuotaReservation, businessAt time.Time) (bool, error) {
	if err := repository.ready(); err != nil {
		return false, err
	}
	if quota.Limit <= 0 || quota.Units <= 0 || quota.Units > quota.Limit {
		return false, fmt.Errorf("invalid quota reservation for user %q", userID)
	}
	if businessAt.IsZero() {
		return false, fmt.Errorf("business time is required to consume quota for user %q", userID)
	}
	result, err := repository.dailyQuotas.UpdateOne(
		ctx,
		bson.D{
			{Key: "user_id", Value: userID},
			{Key: "quota_kind", Value: quota.Kind},
			{Key: "local_date", Value: quota.LocalDate},
			{Key: "limit", Value: quota.Limit},
			{Key: "used_count", Value: bson.D{{Key: "$lte", Value: quota.Limit - quota.Units}}},
		},
		bson.D{
			{Key: "$inc", Value: bson.D{{Key: "used_count", Value: quota.Units}}},
			{Key: "$set", Value: bson.D{{Key: "updated_at", Value: businessAt.UTC()}}},
		},
	)
	if err != nil {
		return false, fmt.Errorf("conditionally consume quota for user %q: %w", userID, err)
	}
	return result.MatchedCount == 1, nil
}

// RestoreQuota 使用预留快照中的用户、种类和本地日期精确释放已占用单位。
func (repository *mongoLedgerRepository) RestoreQuota(ctx context.Context, userID string, quota ledger.QuotaReservation, businessAt time.Time) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if quota.Units <= 0 {
		return fmt.Errorf("invalid quota units for user %q", userID)
	}
	if businessAt.IsZero() {
		return fmt.Errorf("business time is required to restore quota for user %q", userID)
	}
	result, err := repository.dailyQuotas.UpdateOne(
		ctx,
		bson.D{
			{Key: "user_id", Value: userID},
			{Key: "quota_kind", Value: quota.Kind},
			{Key: "local_date", Value: quota.LocalDate},
			{Key: "used_count", Value: bson.D{{Key: "$gte", Value: quota.Units}}},
		},
		bson.D{
			{Key: "$inc", Value: bson.D{{Key: "used_count", Value: -quota.Units}}},
			{Key: "$set", Value: bson.D{{Key: "updated_at", Value: businessAt.UTC()}}},
		},
	)
	if err != nil {
		return fmt.Errorf("restore quota for user %q: %w", userID, err)
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("quota fact missing or insufficient for user %q, kind %q, date %q", userID, quota.Kind, quota.LocalDate)
	}
	return nil
}

// TryDebitDiamonds 以账户 _id 为用户 ID 的条件更新预扣钻石，不预先读取余额。
func (repository *mongoLedgerRepository) TryDebitDiamonds(ctx context.Context, userID string, diamonds int64, businessAt time.Time) (bool, error) {
	if err := repository.ready(); err != nil {
		return false, err
	}
	if diamonds <= 0 {
		return false, fmt.Errorf("invalid debit amount for user %q", userID)
	}
	if businessAt.IsZero() {
		return false, fmt.Errorf("business time is required to debit diamonds for user %q", userID)
	}
	result, err := repository.accounts.UpdateOne(
		ctx,
		bson.D{
			{Key: "_id", Value: userID},
			{Key: "diamond_balance", Value: bson.D{{Key: "$gte", Value: diamonds}}},
		},
		bson.D{
			{Key: "$inc", Value: bson.D{{Key: "diamond_balance", Value: -diamonds}}},
			{Key: "$set", Value: bson.D{{Key: "updated_at", Value: businessAt.UTC()}}},
		},
	)
	if err != nil {
		return false, fmt.Errorf("conditionally debit diamonds for user %q: %w", userID, err)
	}
	return result.MatchedCount == 1, nil
}

// CreditDiamonds 向指定账户补回钻石；账户事实缺失必须显式失败。
func (repository *mongoLedgerRepository) CreditDiamonds(ctx context.Context, userID string, diamonds int64, businessAt time.Time) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if diamonds <= 0 {
		return fmt.Errorf("invalid credit amount for user %q", userID)
	}
	if businessAt.IsZero() {
		return fmt.Errorf("business time is required to credit diamonds for user %q", userID)
	}
	result, err := repository.accounts.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: userID}},
		bson.D{
			{Key: "$inc", Value: bson.D{{Key: "diamond_balance", Value: diamonds}}},
			{Key: "$set", Value: bson.D{{Key: "updated_at", Value: businessAt.UTC()}}},
		},
	)
	if err != nil {
		return fmt.Errorf("credit diamonds for user %q: %w", userID, err)
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("account fact missing for user %q", userID)
	}
	return nil
}

// CreateReservation 插入唯一 creation_id 预留事实，冲突时映射为领域命令冲突。
func (repository *mongoLedgerRepository) CreateReservation(ctx context.Context, reservation *ledger.Reservation) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if reservation == nil {
		return errors.New("reservation is required")
	}
	if _, err := repository.reservations.InsertOne(ctx, newReservationDocument(reservation)); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("create reservation for creation %q: %w; %w", reservation.CreationID, ledger.ErrReservationCommandConflict, err)
		}
		return fmt.Errorf("create reservation for creation %q: %w", reservation.CreationID, err)
	}
	return nil
}

// TransitionReservation 仅在当前状态等于 from 时更新，返回值可作为 CAS 是否命中的领域信号。
func (repository *mongoLedgerRepository) TransitionReservation(ctx context.Context, creationID string, from, to ledger.ReservationStatus, businessAt time.Time) (bool, error) {
	if err := repository.ready(); err != nil {
		return false, err
	}
	if businessAt.IsZero() {
		return false, fmt.Errorf("business time is required to transition reservation for creation %q", creationID)
	}
	result, err := repository.reservations.UpdateOne(
		ctx,
		bson.D{
			{Key: "creation_id", Value: creationID},
			{Key: "status", Value: string(from)},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: string(to)},
			{Key: "updated_at", Value: businessAt.UTC()},
		}}},
	)
	if err != nil {
		return false, fmt.Errorf("transition reservation for creation %q: %w", creationID, err)
	}
	return result.MatchedCount == 1, nil
}

// AppendLedgerEntry 在同一事务中读取预留以固化账户归属，再插入稳定幂等键分录。
func (repository *mongoLedgerRepository) AppendLedgerEntry(ctx context.Context, entry *ledger.LedgerEntry) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if entry == nil {
		return errors.New("ledger entry is required")
	}
	var reservation model.ReservationDocument
	err := repository.reservations.FindOne(ctx, bson.D{{Key: "creation_id", Value: entry.CreationID}}).Decode(&reservation)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return fmt.Errorf("reservation fact missing for ledger creation %q", entry.CreationID)
	}
	if err != nil {
		return fmt.Errorf("find reservation for ledger creation %q: %w", entry.CreationID, err)
	}
	document := model.LedgerEntryDocument{
		ID:             entry.IdempotencyKey,
		IdempotencyKey: entry.IdempotencyKey,
		AccountID:      reservation.UserID,
		CreationID:     entry.CreationID,
		DeltaDiamonds:  entry.DeltaDiamonds,
		Reason:         entry.Reason,
		ReservationID:  reservation.ID,
		CreatedAt:      entry.CreatedAt,
	}
	if _, err := repository.ledgerEntries.InsertOne(ctx, document); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("ledger idempotency key %q already exists: %w", entry.IdempotencyKey, err)
		}
		return fmt.Errorf("append ledger entry %q: %w", entry.IdempotencyKey, err)
	}
	return nil
}

func (repository *mongoLedgerRepository) ready() error {
	if repository == nil || repository.accounts == nil || repository.dailyQuotas == nil || repository.reservations == nil || repository.ledgerEntries == nil {
		return errors.New("ledger repository is not configured")
	}
	return nil
}

func newReservationDocument(reservation *ledger.Reservation) model.ReservationDocument {
	return model.ReservationDocument{
		ID:               reservation.ID,
		CreationID:       reservation.CreationID,
		UserID:           reservation.UserID,
		PriceDiamonds:    reservation.PriceDiamonds,
		ReservedDiamonds: reservation.ChargedDiamonds,
		BenefitSource:    string(reservation.Source),
		QuotaKind:        reservation.Quota.Kind,
		LocalDate:        reservation.Quota.LocalDate,
		QuotaLimit:       reservation.Quota.Limit,
		QuotaUnits:       reservation.Quota.Units,
		Status:           string(reservation.Status),
		CreatedAt:        reservation.CreatedAt,
		UpdatedAt:        reservation.UpdatedAt,
	}
}

func toBizReservation(document model.ReservationDocument) *ledger.Reservation {
	return &ledger.Reservation{
		ID:              document.ID,
		CreationID:      document.CreationID,
		UserID:          document.UserID,
		PriceDiamonds:   document.PriceDiamonds,
		Source:          ledger.BenefitSource(document.BenefitSource),
		ChargedDiamonds: document.ReservedDiamonds,
		Quota: ledger.QuotaReservation{
			Kind:      document.QuotaKind,
			LocalDate: document.LocalDate,
			Limit:     document.QuotaLimit,
			Units:     document.QuotaUnits,
		},
		Status:    ledger.ReservationStatus(document.Status),
		CreatedAt: document.CreatedAt,
		UpdatedAt: document.UpdatedAt,
	}
}

var _ ledger.Repository = (*mongoLedgerRepository)(nil)
