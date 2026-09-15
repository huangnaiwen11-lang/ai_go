package data

import (
	"context"
	"reflect"
	"testing"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data/model"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// 只验证生成的 Mongo 聚合合同，不连接 Mongo，也不声称执行过数据库集成测试。
func TestPaymentReadPipelineSelectsLatestPublishedBeforePriceFiltering(t *testing.T) {
	want := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{{Key: "publish_status", Value: "published"}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "product_id", Value: 1}, {Key: "version", Value: -1}}}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$product_id"}, {Key: "snapshot", Value: bson.D{{Key: "$first", Value: "$$ROOT"}}}}}},
		bson.D{{Key: "$replaceRoot", Value: bson.D{{Key: "newRoot", Value: "$snapshot"}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "product_id", Value: 1}}}},
	}
	if got := publishedPaymentSnapshotsPipeline(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pipeline = %#v", got)
	}
}

func TestPaymentReadSnapshotConversion(t *testing.T) {
	got := paymentProductSnapshotToBiz(model.PaymentProductDocument{ProductID: "p", Version: 3, DiamondAmount: 100, AmountCents: 999, Currency: "USD", Label: "label", PublishStatus: "published"})
	want := payments.PaymentProduct{ID: "p", Version: 3, DiamondAmount: 100, AmountCents: 999, Currency: "USD", Label: "label", PublishStatus: payments.ProductPublishStatusPublished}
	if got != want {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestPaymentReadUnconfiguredRepositoryFails(t *testing.T) {
	repo := &mongoPaymentRepository{}
	if _, err := repo.ListPublishedProductSnapshots(context.Background()); err == nil {
		t.Fatal("unconfigured read succeeded")
	}
	if NewPaymentReadRepository(nil) != nil {
		t.Fatal("nil data should not create a repository")
	}
}
