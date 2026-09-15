package data

import (
	"context"
	"os"
	"testing"

	"ai-business-service/internal/biz/media"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoLocalUserMediaRepository保存素材且只允许所有者解析(t *testing.T) {
	uri := os.Getenv(testMongoURIEnv)
	if uri == "" {
		t.Skip("未配置本机 rs0 测试连接")
	}
	storage, cleanup, err := NewData(&conf.Data{Mongo: &conf.Data_Mongo{Uri: uri, Database: "cling_main", ReplicaSet: "rs0", TransactionsRequired: true}})
	if err != nil {
		t.Fatalf("NewData() error = %v", err)
	}
	t.Cleanup(cleanup)

	repository := NewLocalUserMediaRepository(storage, t.TempDir())
	image, err := repository.SaveImage(context.Background(), media.SaveImageCommand{
		OwnerID: "media-owner-" + t.Name(), Filename: "portrait.png", ContentType: "image/png", Content: tinyPNG,
	})
	if err != nil {
		t.Fatalf("SaveImage() error = %v", err)
	}
	t.Cleanup(func() {
		_, _ = storage.database.Collection(schema.CollectionAssets).DeleteOne(context.Background(), bson.D{{Key: "_id", Value: "user_media:" + image.ID}})
	})
	if image.Reference == "" {
		t.Fatal("保存后没有返回稳定素材引用")
	}
	if _, err := repository.ResolveOwnedImage(context.Background(), image.OwnerID, image.Reference); err != nil {
		t.Fatalf("所有者无法解析素材：%v", err)
	}
	if _, err := repository.ResolveOwnedImage(context.Background(), "another-user", image.Reference); err == nil {
		t.Fatal("其他用户错误地解析了素材")
	}
}

var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}
