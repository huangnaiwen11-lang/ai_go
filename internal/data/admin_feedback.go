package data

import (
	"context"
	"time"

	a "ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type adminFeedbackRow struct {
	model.FeedbackDocument `bson:",inline"`
	Status, Response       string
	Note                   string             `bson:"review_note"`
	ReviewedAt             *time.Time         `bson:"reviewed_at"`
	User                   model.UserDocument `bson:"admin_user"`
	UserEmail              string             `bson:"admin_email"`
}

func (r *mongoAdminViewRepository) Feedbacks(ctx context.Context, q a.Query) (a.FeedbackPage, error) {
	f := bson.M{}
	if q.Type != "" && q.Type != "all" {
		f["type"] = q.Type
	}
	if q.Status != "" && q.Status != "all" {
		f["status"] = q.Status
	}
	p := bson.A{bson.M{"$set": bson.M{"status": bson.M{"$ifNull": bson.A{"$status", "pending"}}}}, bson.M{"$match": f}}
	p = append(p, adminOwnerLookup("user_id")...)
	p = append(p, adminPageFacet(q))
	cur, err := r.data.database.Collection(schema.CollectionFeedbacks).Aggregate(ctx, p)
	if err != nil {
		return a.FeedbackPage{}, err
	}
	defer cur.Close(ctx)
	var row struct {
		Rows  []adminFeedbackRow
		Total []struct{ N int64 }
	}
	if cur.Next(ctx) {
		if err := cur.Decode(&row); err != nil {
			return a.FeedbackPage{}, err
		}
	}
	result := a.FeedbackPage{Feedbacks: make([]a.Feedback, 0, len(row.Rows))}
	if len(row.Total) > 0 {
		result.Total = row.Total[0].N
	}
	for _, d := range row.Rows {
		f := a.Feedback{ID: d.ID, UserID: d.UserID, Type: d.Type, Message: d.Message, Email: d.Email, Status: d.Status, Response: d.Response, Note: d.Note, CreatedAt: d.CreatedAt, ReviewedAt: d.ReviewedAt, Attachments: make([]a.Attachment, 0, len(d.Attachments))}
		if d.User.ID != "" {
			user := toAdminUser(adminUserRow{UserDocument: d.User, Email: d.UserEmail})
			f.User = &user
		}
		for _, att := range d.Attachments {
			f.Attachments = append(f.Attachments, a.Attachment{ID: att.ID, URL: att.DownloadURL})
		}
		result.Feedbacks = append(result.Feedbacks, f)
	}
	return result, cur.Err()
}
func (r *mongoAdminViewRepository) FeedbackStats(ctx context.Context) (a.FeedbackStats, error) {
	result := a.FeedbackStats{ByType: map[string]int64{"bug": 0, "feature": 0, "content": 0, "payment": 0, "praise": 0, "other": 0}}
	p := bson.A{bson.M{"$group": bson.M{"_id": bson.M{"status": bson.M{"$ifNull": bson.A{"$status", "pending"}}, "type": "$type"}, "n": bson.M{"$sum": 1}}}}
	cur, err := r.data.database.Collection(schema.CollectionFeedbacks).Aggregate(ctx, p)
	if err != nil {
		return result, err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var d struct {
			Key struct{ Status, Type string } `bson:"_id"`
			N   int64
		}
		if err := cur.Decode(&d); err != nil {
			return result, err
		}
		result.Total += d.N
		result.ByType[d.Key.Type] += d.N
		if d.Key.Status == "pending" {
			result.Pending += d.N
		}
		if d.Key.Status == "resolved" {
			result.Resolved += d.N
		}
	}
	return result, cur.Err()
}
func (r *mongoAdminViewRepository) Respond(ctx context.Context, actor a.Actor, in a.Reply) error {
	return NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		var current adminFeedbackRow
		if err := r.data.database.Collection(schema.CollectionFeedbacks).FindOne(tx, bson.M{"_id": in.ID}).Decode(&current); err != nil {
			return adminNotFound(err)
		}
		if current.Status == "resolved" {
			if current.Response == in.Response && current.Note == in.Note {
				return nil
			}
			return a.ErrConflict
		}
		if current.Status != "" && current.Status != "pending" {
			return a.ErrConflict
		}
		user, err := r.User(tx, current.UserID)
		if err != nil {
			return err
		}
		if user.Status == "deleted" {
			return a.ErrConflict
		}
		now := time.Now().UTC()
		id := "feedback-reply-" + in.ID
		_, err = r.data.database.Collection(schema.CollectionFeedbacks).UpdateOne(tx, bson.M{"_id": in.ID}, bson.M{"$set": bson.M{"status": "resolved", "response": in.Response, "review_note": in.Note, "reviewed_at": now, "reviewed_by": actor.ID}})
		if err != nil {
			return err
		}
		_, err = r.data.database.Collection(schema.CollectionNotifications).InsertOne(tx, model.NotificationDocument{ID: id, UserID: current.UserID, Type: "feedback_reply", Title: "反馈回复", Body: in.Response, Data: map[string]any{"feedbackId": in.ID}, Read: false, CreatedAt: now})
		if err != nil {
			return err
		}
		_, err = r.data.database.Collection(adminAuditCollection).InsertOne(tx, bson.M{"_id": id, "actor_id": actor.ID, "target_id": in.ID, "action": "feedback_reply", "created_at": now})
		return err
	})
}
