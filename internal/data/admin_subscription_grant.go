package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	a "ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// GrantSubscription 手工发放或续期订阅权益。
//
// subscriptions 以 user_id 作为 _id，同一用户只有一条权益快照，因此：
//   - 既有快照仍在有效期内 → 从既有 expires_at 顺延，action = extended
//   - 否则从当前时刻起算，action = created
//
// 这是赠予而不是支付事实：只改权益快照 + 写 admin_audit，不写支付订单、不动钻石余额，
// 所以不会污染收入统计。两者同事务落库，避免「发了订阅但审计缺失」。
func (r *mongoAdminViewRepository) GrantSubscription(ctx context.Context, actor a.Actor, in a.SubscriptionGrant) (a.SubscriptionGrantResult, error) {
	var result a.SubscriptionGrantResult
	err := NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		user, err := r.User(tx, in.UserID)
		if err != nil {
			return err
		}
		// 已删除账号不接受发放：权益挂在一个不可登录的账号上没有意义。
		if user.Status == "deleted" {
			return a.ErrConflict
		}

		now := time.Now().UTC()
		collection := r.data.database.Collection(schema.CollectionSubscriptions)

		var existing model.SubscriptionDocument
		found := true
		if err := collection.FindOne(tx, bson.M{"_id": in.UserID}).Decode(&existing); err != nil {
			if !errors.Is(err, mongo.ErrNoDocuments) {
				return fmt.Errorf("read subscription snapshot: %w", err)
			}
			found = false
		}

		action, startsAt, base, createdAt := "created", now, now, now
		if found {
			createdAt = existing.CreatedAt
			startsAt = now
			if existing.ExpiresAt.After(now) {
				action = "extended"
				startsAt = existing.StartsAt
				base = existing.ExpiresAt
			}
		}

		document := model.SubscriptionDocument{
			UserID:        in.UserID,
			Status:        string(entitlement.SubscriptionStatusActive),
			BillingPeriod: string(entitlement.SubscriptionBillingPeriodMonthly),
			StartsAt:      startsAt,
			ExpiresAt:     base.AddDate(0, 0, in.Days),
			CreatedAt:     createdAt,
			UpdatedAt:     now,
		}
		if _, err := collection.ReplaceOne(tx, bson.M{"_id": in.UserID}, document, options.Replace().SetUpsert(true)); err != nil {
			return fmt.Errorf("grant subscription: %w", err)
		}

		audit := bson.M{
			"_id":        uuid.NewString(),
			"actor_id":   actor.ID,
			"target_id":  in.UserID,
			"action":     "wallet_grant_subscription",
			"tier":       in.Tier,
			"days":       in.Days,
			"reason":     in.Reason,
			"end_date":   document.ExpiresAt,
			"created_at": now,
		}
		if _, err := r.data.database.Collection(adminAuditCollection).InsertOne(tx, audit); err != nil {
			return fmt.Errorf("write grant audit: %w", err)
		}

		result = a.SubscriptionGrantResult{Action: action, Tier: in.Tier, EndDate: document.ExpiresAt, UserName: user.Name}
		return nil
	})
	if err != nil {
		return a.SubscriptionGrantResult{}, err
	}
	return result, nil
}
