package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	a "ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type adminLedgerRow struct {
	model.LedgerEntryDocument `bson:",inline"`
	User                      model.UserDocument `bson:"admin_user"`
	Email                     string             `bson:"admin_email"`
	BalanceAfter              *int64             `bson:"balance_after"`
}

func (r *mongoAdminViewRepository) PurchaseCountries(ctx context.Context) ([]string, error) {
	p := adminOwnerLookup("user_id")
	p = append(p, bson.M{"$match": bson.M{"admin_user.geo.country": bson.M{"$type": "string", "$ne": ""}}}, bson.M{"$group": bson.M{"_id": "$admin_user.geo.country"}}, bson.M{"$sort": bson.M{"_id": 1}})
	cur, err := r.data.database.Collection(schema.CollectionPaymentOrders).Aggregate(ctx, p)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	result := []string{}
	for cur.Next(ctx) {
		var row struct {
			Country string `bson:"_id"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		result = append(result, row.Country)
	}
	return result, cur.Err()
}

func ledgerDO(d adminLedgerRow) a.LedgerEntry {
	return a.LedgerEntry{ID: d.ID, UserID: d.AccountID, UserName: d.User.DisplayName, UserEmail: d.Email, Reason: d.Reason, RefID: d.CreationID, Delta: d.DeltaDiamonds, BalanceAfter: d.BalanceAfter, CreatedAt: d.CreatedAt}
}
func adminPageFacet(q a.Query) bson.M {
	return bson.M{"$facet": bson.M{"rows": bson.A{bson.M{"$sort": bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}}, bson.M{"$skip": (q.Page - 1) * q.Limit}, bson.M{"$limit": q.Limit}}, "total": bson.A{bson.M{"$count": "n"}}}}
}
func (r *mongoAdminViewRepository) Ledger(ctx context.Context, q a.Query) (a.LedgerPage, error) {
	f := bson.M{}
	if q.UserID != "" {
		f["account_id"] = q.UserID
	}
	if q.Type == "credit" {
		f["delta_diamonds"] = bson.M{"$gt": 0}
	} else if q.Type == "debit" {
		f["delta_diamonds"] = bson.M{"$lt": 0}
	}
	p := bson.A{bson.M{"$match": f}}
	p = append(p, adminOwnerLookup("account_id")...)
	p = append(p, bson.M{"$match": adminOwnerFilter(q)}, adminPageFacet(q))
	cur, err := r.data.database.Collection(schema.CollectionLedgerEntries).Aggregate(ctx, p)
	if err != nil {
		return a.LedgerPage{}, err
	}
	defer cur.Close(ctx)
	var rows struct {
		Rows  []adminLedgerRow
		Total []struct{ N int64 }
	}
	if cur.Next(ctx) {
		if err := cur.Decode(&rows); err != nil {
			return a.LedgerPage{}, err
		}
	}
	result := a.LedgerPage{Entries: make([]a.LedgerEntry, 0, len(rows.Rows))}
	for _, row := range rows.Rows {
		result.Entries = append(result.Entries, ledgerDO(row))
	}
	if len(rows.Total) > 0 {
		result.Total = rows.Total[0].N
	}
	return result, cur.Err()
}
func adminPurchaseFilter(q a.Query) bson.M {
	f := bson.M{}
	if q.UserID != "" {
		f["user_id"] = q.UserID
	}
	if q.Provider != "" {
		f["provider"] = q.Provider
	}
	if q.Status != "" {
		status := q.Status
		if status == "completed" {
			status = "paid"
		}
		f["status"] = status
	}
	return f
}
func (r *mongoAdminViewRepository) Purchases(ctx context.Context, q a.Query) (a.PurchasePage, error) {
	p := bson.A{bson.M{"$match": adminPurchaseFilter(q)}}
	p = append(p, adminOwnerLookup("user_id")...)
	p = append(p, bson.M{"$match": adminOwnerFilter(q)}, adminPageFacet(q))
	cur, err := r.data.database.Collection(schema.CollectionPaymentOrders).Aggregate(ctx, p)
	if err != nil {
		return a.PurchasePage{}, err
	}
	defer cur.Close(ctx)
	var rows struct {
		Rows []struct {
			model.PaymentOrderDocument `bson:",inline"`
			User                       model.UserDocument `bson:"admin_user"`
			Email                      string             `bson:"admin_email"`
		}
		Total []struct{ N int64 }
	}
	if cur.Next(ctx) {
		if err := cur.Decode(&rows); err != nil {
			return a.PurchasePage{}, err
		}
	}
	result := a.PurchasePage{Purchases: make([]a.Purchase, 0, len(rows.Rows))}
	for _, d := range rows.Rows {
		status := d.Status
		if status == "paid" {
			status = "completed"
		}
		result.Purchases = append(result.Purchases, a.Purchase{ID: d.ID, UserID: d.UserID, UserName: d.User.DisplayName, UserEmail: d.Email, Provider: d.Provider, ProviderTxnID: d.ProviderOrderID, ProductID: d.ProductID, Currency: d.Currency, Status: status, Coins: d.DiamondAmount, Cents: d.AmountCents, CreatedAt: d.CreatedAt})
	}
	if len(rows.Total) > 0 {
		result.Total = rows.Total[0].N
	}
	return result, cur.Err()
}
func (r *mongoAdminViewRepository) WalletStats(ctx context.Context, q a.Query) (a.WalletStats, error) {
	result := a.WalletStats{ByProvider: map[string]a.ProviderTotal{}}
	today := a.DayStart(time.Now())
	f := adminPurchaseFilter(q)
	f["status"] = "paid"
	f["currency"] = "USD"
	p := bson.A{bson.M{"$match": f}}
	p = append(p, adminOwnerLookup("user_id")...)
	p = append(p, bson.M{"$match": adminOwnerFilter(q)}, bson.M{"$group": bson.M{"_id": "$provider", "count": bson.M{"$sum": 1}, "cents": bson.M{"$sum": "$amount_cents"}, "coins": bson.M{"$sum": "$diamond_amount"}, "todayCents": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$gte": bson.A{"$updated_at", today}}, "$amount_cents", 0}}}, "todayCoins": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$gte": bson.A{"$updated_at", today}}, "$diamond_amount", 0}}}}})
	cur, err := r.data.database.Collection(schema.CollectionPaymentOrders).Aggregate(ctx, p)
	if err != nil {
		return result, err
	}
	for cur.Next(ctx) {
		var d struct {
			Provider            string `bson:"_id"`
			Count, Cents, Coins int64
			TodayCents          int64 `bson:"todayCents"`
			TodayCoins          int64 `bson:"todayCoins"`
		}
		if err := cur.Decode(&d); err != nil {
			cur.Close(ctx)
			return result, err
		}
		result.ByProvider[d.Provider] = a.ProviderTotal{Count: d.Count, Cents: d.Cents}
		result.TotalRevenue += d.Cents
		result.TotalCoins += d.Coins
		result.TodayRevenue += d.TodayCents
		result.TodayCoins += d.TodayCoins
	}
	err = cur.Err()
	cur.Close(ctx)
	if err != nil {
		return result, err
	}
	// Consumption excludes admin debits; it is gross generation reservations, not net refunds.
	p = adminOwnerLookup("account_id")
	p = append(p, bson.M{"$match": adminOwnerFilter(q)}, bson.M{"$group": bson.M{"_id": nil,
		"spent":   bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$and": bson.A{bson.M{"$lt": bson.A{"$delta_diamonds", 0}}, bson.M{"$eq": bson.A{"$reason", "generation_reserved"}}}}, bson.M{"$multiply": bson.A{"$delta_diamonds", -1}}, 0}}},
		"today":   bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$and": bson.A{bson.M{"$lt": bson.A{"$delta_diamonds", 0}}, bson.M{"$eq": bson.A{"$reason", "generation_reserved"}}, bson.M{"$gte": bson.A{"$created_at", today}}}}, bson.M{"$multiply": bson.A{"$delta_diamonds", -1}}, 0}}},
		"granted": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$and": bson.A{bson.M{"$gt": bson.A{"$delta_diamonds", 0}}, bson.M{"$eq": bson.A{"$reason", "admin_adjustment"}}}}, "$delta_diamonds", 0}}},
	}})
	cur, err = r.data.database.Collection(schema.CollectionLedgerEntries).Aggregate(ctx, p)
	if err != nil {
		return result, err
	}
	defer cur.Close(ctx)
	if cur.Next(ctx) {
		var d struct{ Spent, Today, Granted int64 }
		if err := cur.Decode(&d); err != nil {
			return result, err
		}
		result.TotalSpent = d.Spent
		result.TodaySpent = d.Today
		result.TotalGranted = d.Granted
	}
	return result, cur.Err()
}
func (r *mongoAdminViewRepository) Wallet(ctx context.Context, id string) (a.Wallet, error) {
	user, err := r.User(ctx, id)
	if err != nil {
		return a.Wallet{}, err
	}
	result := a.Wallet{User: user}
	var account model.AccountDocument
	err = r.data.database.Collection(schema.CollectionAccounts).FindOne(ctx, bson.M{"_id": id}).Decode(&account)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return result, err
	}
	result.Balance = account.DiamondBalance
	page, err := r.Ledger(ctx, a.Query{UserID: id, Page: 1, Limit: 20})
	if err != nil {
		return result, err
	}
	result.Entries = page.Entries
	cur, err := r.data.database.Collection(schema.CollectionLedgerEntries).Aggregate(ctx, bson.A{bson.M{"$match": bson.M{"account_id": id}}, bson.M{"$group": bson.M{"_id": nil, "earned": bson.M{"$sum": bson.M{"$max": bson.A{0, "$delta_diamonds"}}}, "spent": bson.M{"$sum": bson.M{"$max": bson.A{0, bson.M{"$multiply": bson.A{-1, "$delta_diamonds"}}}}}}}})
	if err != nil {
		return result, err
	}
	defer cur.Close(ctx)
	if cur.Next(ctx) {
		var row struct{ Earned, Spent int64 }
		if err := cur.Decode(&row); err != nil {
			return result, err
		}
		result.TotalEarned = row.Earned
		result.TotalSpent = row.Spent
	}
	return result, cur.Err()
}

type adminAdjustmentDocument struct {
	ID        string    `bson:"_id"`
	ActorID   string    `bson:"actor_id"`
	UserID    string    `bson:"target_id"`
	Delta     int64     `bson:"delta"`
	Reason    string    `bson:"reason"`
	Balance   int64     `bson:"balance"`
	UserName  string    `bson:"user_name"`
	CreatedAt time.Time `bson:"created_at"`
}

func (r *mongoAdminViewRepository) Adjust(ctx context.Context, actor a.Actor, in a.Adjustment) (a.AdjustmentResult, error) {
	digest := sha256.Sum256([]byte(actor.ID + "\x00" + in.Key))
	id := "adjust-" + hex.EncodeToString(digest[:])
	var result a.AdjustmentResult
	replay := func(tx context.Context) (bool, error) {
		var previous adminAdjustmentDocument
		err := r.data.database.Collection(adminAuditCollection).FindOne(tx, bson.M{"_id": id}).Decode(&previous)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if previous.UserID != in.UserID || previous.Delta != in.Delta || previous.Reason != in.Reason {
			return true, a.ErrConflict
		}
		result = a.AdjustmentResult{Balance: previous.Balance, UserName: previous.UserName}
		return true, nil
	}
	err := NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		if found, err := replay(tx); found || err != nil {
			return err
		}
		user, err := r.User(tx, in.UserID)
		if err != nil {
			return err
		}
		if user.Status != "active" {
			return a.ErrConflict
		}
		now := time.Now().UTC()
		filter := bson.M{"_id": in.UserID, "diamond_balance": bson.M{"$gte": int64(0), "$lte": int64(9007199254740991) - in.Delta}}
		if in.Delta < 0 {
			filter["diamond_balance"] = bson.M{"$gte": -in.Delta}
		}
		var account model.AccountDocument
		err = r.data.database.Collection(schema.CollectionAccounts).FindOneAndUpdate(tx, filter, bson.M{"$inc": bson.M{"diamond_balance": in.Delta}, "$set": bson.M{"updated_at": now}}, adminAfter).Decode(&account)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return a.ErrInsufficientBalance
		}
		if err != nil {
			return err
		}
		_, err = r.data.database.Collection(schema.CollectionLedgerEntries).InsertOne(tx, bson.M{"_id": id, "idempotency_key": id, "account_id": in.UserID, "delta_diamonds": in.Delta, "balance_after": account.DiamondBalance, "reason": "admin_adjustment", "admin_reason": in.Reason, "actor_id": actor.ID, "created_at": now})
		if err != nil {
			return err
		}
		_, err = r.data.database.Collection(adminAuditCollection).InsertOne(tx, adminAdjustmentDocument{ID: id, ActorID: actor.ID, UserID: in.UserID, Delta: in.Delta, Reason: in.Reason, Balance: account.DiamondBalance, UserName: user.Name, CreatedAt: now})
		if err != nil {
			return err
		}
		result = a.AdjustmentResult{Balance: account.DiamondBalance, UserName: user.Name}
		return nil
	})
	if mongo.IsDuplicateKeyError(err) {
		var found bool
		found, err = replay(ctx)
		if !found && err == nil {
			err = a.ErrConflict
		}
	}
	return result, err
}
