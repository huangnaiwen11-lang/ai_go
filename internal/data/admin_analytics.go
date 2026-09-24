package data

import (
	"context"
	"fmt"

	"ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func adminDate(field any) bson.M {
	return bson.M{"$dateToString": bson.M{"format": "%Y-%m-%d", "date": field, "timezone": "Asia/Shanghai"}}
}

func (r *mongoAdminViewRepository) Trends(ctx context.Context, metric string, window adminview.Window) ([]adminview.Trend, error) {
	collection, field := schema.CollectionUsers, "created_at"
	filter := bson.M{}
	switch metric {
	case "users":
	case "images", "videos":
		collection = schema.CollectionCreations
		if metric == "images" {
			filter["product_output"] = "image"
		} else {
			filter["product_output"] = "video"
		}
	case "revenue-trends":
		collection = schema.CollectionPaymentOrders
		field = "updated_at"
		filter["status"] = "paid"
		filter["currency"] = "USD"
	default:
		return nil, fmt.Errorf("unknown admin metric")
	}
	filter[field] = bson.M{"$gte": window.From, "$lt": window.Until}
	group := bson.M{"_id": adminDate("$" + field), "count": bson.M{"$sum": 1}}
	if metric == "revenue-trends" {
		group["cents"] = bson.M{"$sum": "$amount_cents"}
	}
	cursor, err := r.data.database.Collection(collection).Aggregate(ctx, bson.A{bson.M{"$match": filter}, bson.M{"$group": group}})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	buckets := map[string]adminview.Trend{}
	for cursor.Next(ctx) {
		var row struct {
			Date         string `bson:"_id"`
			Count, Cents int64
		}
		if err := cursor.Decode(&row); err != nil {
			return nil, err
		}
		buckets[row.Date] = adminview.Trend{Date: row.Date, Count: row.Count, Cents: row.Cents}
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	result := make([]adminview.Trend, 0)
	for day := window.From.In(adminview.Location); day.Before(window.Until); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		row := buckets[date]
		row.Date = date
		result = append(result, row)
	}
	return result, nil
}

// RevenueBreakdowns groups actual paid USD orders. Missing attribution is an
// explicit unknown bucket, never attributed to organic/US/Web by assumption.
func (r *mongoAdminViewRepository) RevenueBreakdowns(ctx context.Context, window adminview.Window) (adminview.RevenueBreakdown, error) {
	result := adminview.RevenueBreakdown{}
	for _, dimension := range []struct {
		field  string
		target *[]adminview.RevenueBucket
	}{
		{"$user.geo.country", &result.Country}, {"$user.acquisition.channel", &result.Source}, {"$user.guest_platform", &result.Client},
	} {
		key := bson.M{"$cond": bson.A{bson.M{"$in": bson.A{bson.M{"$ifNull": bson.A{dimension.field, ""}}, bson.A{"", "unknown"}}}, "unknown", dimension.field}}
		pipeline := bson.A{
			bson.M{"$match": bson.M{"status": "paid", "currency": "USD", "updated_at": bson.M{"$gte": window.From, "$lt": window.Until}}},
			bson.M{"$lookup": bson.M{"from": schema.CollectionUsers, "localField": "user_id", "foreignField": "_id", "as": "user"}},
			bson.M{"$unwind": bson.M{"path": "$user", "preserveNullAndEmptyArrays": true}},
			bson.M{"$group": bson.M{"_id": key, "cents": bson.M{"$sum": "$amount_cents"}, "d0": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{adminDate("$updated_at"), adminDate("$user.created_at")}}, "$amount_cents", 0}}}}},
			bson.M{"$sort": bson.D{{Key: "cents", Value: -1}, {Key: "_id", Value: 1}}}, bson.M{"$limit": 10},
		}
		cursor, err := r.data.database.Collection(schema.CollectionPaymentOrders).Aggregate(ctx, pipeline)
		if err != nil {
			return result, err
		}
		rows := make([]adminview.RevenueBucket, 0)
		for cursor.Next(ctx) {
			var row struct {
				Key       string `bson:"_id"`
				Cents, D0 int64
			}
			if err := cursor.Decode(&row); err != nil {
				cursor.Close(ctx)
				return result, err
			}
			rows = append(rows, adminview.RevenueBucket{Key: row.Key, Label: row.Key, Cents: row.Cents, D0Cents: row.D0})
		}
		err = cursor.Err()
		cursor.Close(ctx)
		if err != nil {
			return result, err
		}
		*dimension.target = rows
	}
	return result, nil
}
