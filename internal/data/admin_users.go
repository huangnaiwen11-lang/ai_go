package data

import (
	"context"
	"errors"
	"regexp"
	"time"

	a "ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const adminAuditCollection = schema.CollectionAdminAudit

type adminUserRow struct {
	model.UserDocument `bson:",inline"`
	Email              string `bson:"admin_email"`
}

func toAdminUser(d adminUserRow) a.User {
	role := d.Role
	if role == "" {
		role = "user"
	}
	status := map[string]string{"normal": "active", "banned": "suspended", "deleted": "deleted"}[d.AccountStatus]
	return a.User{ID: d.ID, Name: d.DisplayName, Email: d.Email, Role: role, Status: status, Platform: d.GuestPlatform, Binding: d.BindingState, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
}
func adminWindow(w a.Window) bson.M {
	m := bson.M{}
	if !w.From.IsZero() {
		m["$gte"] = w.From
	}
	if !w.Until.IsZero() {
		m["$lt"] = w.Until
	}
	return m
}
func adminEmailLookup(local string) bson.A {
	return bson.A{
		bson.M{"$lookup": bson.M{"from": schema.CollectionCredentials, "let": bson.M{"uid": "$" + local}, "pipeline": bson.A{bson.M{"$match": bson.M{"$expr": bson.M{"$eq": bson.A{"$user_id", "$$uid"}}, "active": true}}, bson.M{"$project": bson.M{"_id": 0, "email_normalized": 1}}, bson.M{"$limit": 1}}, "as": "admin_credentials"}},
		bson.M{"$set": bson.M{"admin_email": bson.M{"$ifNull": bson.A{bson.M{"$arrayElemAt": bson.A{"$admin_credentials.email_normalized", 0}}, ""}}}}, bson.M{"$unset": "admin_credentials"},
	}
}
func adminUserFilter(q a.Query) bson.M {
	f := bson.M{}
	if q.Status != "" {
		f["account_status"] = map[string]string{"active": "normal", "suspended": "banned", "deleted": "deleted"}[q.Status]
	}
	if q.Role == "user" {
		f["$or"] = bson.A{bson.M{"role": "user"}, bson.M{"role": ""}, bson.M{"role": nil}}
	} else if q.Role != "" {
		f["role"] = q.Role
	}
	if q.Platform != "" {
		f["guest_platform"] = q.Platform
	}
	if q.Client != "" {
		f["client_app_id"] = q.Client
	}
	if q.Source != "" {
		f["acquisition.channel"] = q.Source
	}
	if q.LoginType == "guest" {
		f["binding_state"] = "guest"
	} else if q.LoginType == "registered" {
		f["binding_state"] = "bound"
	} else if q.LoginType != "" {
		f["admin_provider"] = q.LoginType
	}
	if len(adminWindow(q.Created)) > 0 {
		f["created_at"] = adminWindow(q.Created)
	}
	if len(adminWindow(q.Seen)) > 0 {
		f["last_seen_at"] = adminWindow(q.Seen)
	}
	if q.ExactEmail != "" {
		f["admin_email"] = q.ExactEmail
	}
	if q.Search != "" {
		pattern := bson.Regex{Pattern: regexp.QuoteMeta(q.Search), Options: "i"}
		f["$and"] = bson.A{bson.M{"$or": bson.A{bson.M{"admin_email": pattern}, bson.M{"display_name": pattern}, bson.M{"_id": q.Search}}}}
	}
	if q.UserID != "" {
		f["_id"] = q.UserID
	}
	return f
}
func adminUsersPipeline(q a.Query) bson.A {
	p := adminEmailLookup("_id")
	// Provider flags derive from stored bindings; password credentials are email login.
	p = append(p, bson.M{"$lookup": bson.M{"from": schema.CollectionIdentities, "localField": "_id", "foreignField": "user_id", "as": "admin_identities"}}, bson.M{"$set": bson.M{"admin_provider": bson.M{"$concatArrays": bson.A{"$admin_identities.provider", bson.M{"$cond": bson.A{bson.M{"$ne": bson.A{"$admin_email", ""}}, bson.A{"email"}, bson.A{}}}}}}})
	return append(p, bson.M{"$match": adminUserFilter(q)}, bson.M{"$unset": "admin_identities"})
}
func (r *mongoAdminViewRepository) Users(ctx context.Context, q a.Query) (a.UserPage, error) {
	pipeline := adminUsersPipeline(q)
	pipeline = append(pipeline, bson.M{"$facet": bson.M{"rows": bson.A{bson.M{"$sort": bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}}, bson.M{"$skip": (q.Page - 1) * q.Limit}, bson.M{"$limit": q.Limit}}, "total": bson.A{bson.M{"$count": "n"}}}})
	cursor, err := r.data.database.Collection(schema.CollectionUsers).Aggregate(ctx, pipeline)
	if err != nil {
		return a.UserPage{}, err
	}
	defer cursor.Close(ctx)
	var result struct {
		Rows  []adminUserRow
		Total []struct{ N int64 }
	}
	if cursor.Next(ctx) {
		if err := cursor.Decode(&result); err != nil {
			return a.UserPage{}, err
		}
	}
	page := a.UserPage{Users: make([]a.User, 0, len(result.Rows))}
	if len(result.Total) > 0 {
		page.Total = result.Total[0].N
	}
	for _, row := range result.Rows {
		page.Users = append(page.Users, toAdminUser(row))
	}
	return page, cursor.Err()
}
func (r *mongoAdminViewRepository) User(ctx context.Context, id string) (a.User, error) {
	p, err := r.Users(ctx, a.Query{UserID: id, Page: 1, Limit: 1})
	if err != nil {
		return a.User{}, err
	}
	if len(p.Users) == 0 {
		return a.User{}, a.ErrNotFound
	}
	return p.Users[0], nil
}
func (r *mongoAdminViewRepository) UserKPIs(ctx context.Context, q a.Query) (a.UserKPIs, error) {
	window := q.Created
	q.Created = a.Window{}
	q.Search = ""
	q.ExactEmail = ""
	pipeline := adminUsersPipeline(q)
	registered := bson.M{"binding_state": "bound"}
	if len(adminWindow(window)) > 0 {
		registered["created_at"] = adminWindow(window)
	}
	active := bson.M{"delta_diamonds": bson.M{"$lt": 0}, "reason": "generation_reserved"}
	if len(adminWindow(window)) > 0 {
		active["created_at"] = adminWindow(window)
	}
	pipeline = append(pipeline, bson.M{"$facet": bson.M{
		"total": bson.A{bson.M{"$count": "n"}}, "registered": bson.A{bson.M{"$match": registered}, bson.M{"$count": "n"}},
		"active": bson.A{bson.M{"$lookup": bson.M{"from": schema.CollectionLedgerEntries, "let": bson.M{"uid": "$_id"}, "pipeline": bson.A{bson.M{"$match": active}, bson.M{"$match": bson.M{"$expr": bson.M{"$eq": bson.A{"$account_id", "$$uid"}}}}, bson.M{"$limit": 1}}, "as": "activity"}}, bson.M{"$match": bson.M{"activity.0": bson.M{"$exists": true}}}, bson.M{"$group": bson.M{"_id": nil, "n": bson.M{"$sum": 1}, "pwa": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$acquisition.channel", "pwa"}}, 1, 0}}}}}},
	}})
	cursor, err := r.data.database.Collection(schema.CollectionUsers).Aggregate(ctx, pipeline)
	if err != nil {
		return a.UserKPIs{}, err
	}
	defer cursor.Close(ctx)
	var row struct {
		Total, Registered []struct{ N int64 }
		Active            []struct{ N, Pwa int64 }
	}
	if cursor.Next(ctx) {
		if err := cursor.Decode(&row); err != nil {
			return a.UserKPIs{}, err
		}
	}
	result := a.UserKPIs{}
	if len(row.Total) > 0 {
		result.Total = row.Total[0].N
	}
	if len(row.Registered) > 0 {
		result.Registered = row.Registered[0].N
	}
	if len(row.Active) > 0 {
		result.Active = row.Active[0].N
		result.PWAActive = row.Active[0].Pwa
	}
	return result, cursor.Err()
}

func (r *mongoAdminViewRepository) ChangeUser(ctx context.Context, actor a.Actor, in a.UserChange) (a.User, error) {
	var result a.User
	err := NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		user, err := r.User(tx, in.UserID)
		if err != nil {
			return err
		}
		if user.Role == "super_admin" || (actor.Role != "super_admin" && user.Role != "user" && user.Role != "editor") {
			return a.ErrForbidden
		}
		if user.Status == "deleted" {
			return a.ErrConflict
		}
		now := time.Now().UTC()
		fields := bson.M{"updated_at": now}
		if in.Status != "" {
			fields["account_status"] = map[string]string{"active": "normal", "suspended": "banned", "deleted": "deleted"}[in.Status]
		} else {
			fields["role"] = in.Role
		}
		_, err = r.data.database.Collection(schema.CollectionUsers).UpdateOne(tx, bson.M{"_id": in.UserID}, bson.M{"$set": fields, "$inc": bson.M{"session_version": 1}})
		if err != nil {
			return err
		}
		_, err = r.data.database.Collection(schema.CollectionSessions).UpdateMany(tx, bson.M{"user_id": in.UserID, "revoked_at": nil}, bson.M{"$set": bson.M{"revoked_at": now}})
		if err != nil {
			return err
		}
		if in.Status == "deleted" {
			_, err = r.data.database.Collection(schema.CollectionCredentials).UpdateMany(tx, bson.M{"user_id": in.UserID, "active": true}, bson.M{"$set": bson.M{"active": false, "updated_at": now}})
			if err != nil {
				return err
			}
		}
		_, err = r.data.database.Collection(adminAuditCollection).InsertOne(tx, bson.M{"_id": uuid.NewString(), "actor_id": actor.ID, "target_id": in.UserID, "action": "user_change", "status": in.Status, "role": in.Role, "created_at": now})
		if err != nil {
			return err
		}
		result, err = r.User(tx, in.UserID)
		return err
	})
	return result, err
}

// Joined ledger/order/feedback rows reuse a safe user+email projection.
func adminOwnerLookup(field string) bson.A {
	p := bson.A{bson.M{"$lookup": bson.M{"from": schema.CollectionUsers, "localField": field, "foreignField": "_id", "as": "admin_user"}}, bson.M{"$unwind": bson.M{"path": "$admin_user", "preserveNullAndEmptyArrays": true}}}
	return append(p, adminEmailLookup(field)...)
}
func adminOwnerFilter(q a.Query) bson.M {
	f := bson.M{}
	if q.Email != "" {
		f["admin_email"] = bson.Regex{Pattern: regexp.QuoteMeta(q.Email), Options: "i"}
	}
	if q.Client != "" {
		f["admin_user.client_app_id"] = q.Client
	}
	if q.Country != "" {
		f["admin_user.geo.country"] = q.Country
	}
	if q.Source != "" {
		f["admin_user.acquisition.channel"] = q.Source
	}
	return f
}
func adminNotFound(err error) error {
	if errors.Is(err, mongo.ErrNoDocuments) {
		return a.ErrNotFound
	}
	return err
}

var adminAfter = options.FindOneAndUpdate().SetReturnDocument(options.After)
