package data

import (
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// 已发布目录的时间戳刻意取远期值，避免与同一集合里的其它用例互相影响「最新版本」。
var (
	mappingCatalogOlder = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	mappingCatalogNewer = time.Date(2099, 1, 2, 0, 0, 0, 0, time.UTC)
)

func newMappingCatalogCollection(t *testing.T) (*mongo.Database, *mongo.Collection) {
	t.Helper()
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("ensure local schema: %v", err)
	}
	return database, database.Collection(schema.CollectionGenerationModelMappings)
}

func seedMappingCatalog(t *testing.T, collection *mongo.Collection, document model.MappingCatalogDocument) {
	t.Helper()
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if _, err := collection.InsertOne(ctx, document); err != nil {
		t.Fatalf("插入目录版本: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		if _, err := collection.DeleteOne(ctx, bson.M{"_id": document.ID}); err != nil {
			t.Errorf("清理目录版本 %q: %v", document.ID, err)
		}
	})
}

func publishedMappingCatalogDocument(version string, publishedAt time.Time) model.MappingCatalogDocument {
	return model.MappingCatalogDocument{
		ID: version, Status: string(generation.MappingCatalogStatusPublished),
		SourceVersion: "catalog-" + version, PublishedAt: publishedAt,
		Models: testMappingModelDocuments(),
	}
}

// testMappingModelDocuments 返回结构完整、可直接落库的映射条目。
func testMappingModelDocuments() []model.MappingModelDocument {
	return []model.MappingModelDocument{
		{
			Capability: "text_to_image", ProductKey: "image", PublicModel: "ps-image-v1",
			AllowedInputs: []string{"prompt", "aspectRatio", "seed"},
			Sizes:         []model.MappingSizeDocument{{Width: 1024, Height: 1024}},
			AspectRatios:  []string{"1:1", "16:9"},
			Enabled:       true,
		},
		{
			Capability: "image_to_video", ProductKey: "video", PublicModel: "ps-video-v1",
			AllowedInputs: []string{"prompt", "imageUrl", "durationSeconds"},
			Sizes:         []model.MappingSizeDocument{{Width: 1280, Height: 720}},
			Durations:     []int32{5, 10},
			Templates:     []model.MappingTemplateDocument{{Key: "portrait", ID: "tpl-portrait-1"}},
			Enabled:       true,
		},
	}
}

func TestMongoMappingCatalogReadsLatestAndExactPublishedVersion(t *testing.T) {
	database, collection := newMappingCatalogCollection(t)
	olderVersion, newerVersion := "test-mapping-older-"+uuid.NewString(), "test-mapping-newer-"+uuid.NewString()
	seedMappingCatalog(t, collection, publishedMappingCatalogDocument(olderVersion, mappingCatalogOlder))
	seedMappingCatalog(t, collection, publishedMappingCatalogDocument(newerVersion, mappingCatalogNewer))

	store := NewMappingCatalogRepository(&Data{database: database})
	ctx, cancel := newMongoTestContext()
	defer cancel()

	latest, err := store.PublishedCatalog(ctx, "")
	if err != nil {
		t.Fatalf("PublishedCatalog(latest) error = %v", err)
	}
	if latest.Version != newerVersion {
		t.Fatalf("PublishedCatalog(latest) = %q, want %q", latest.Version, newerVersion)
	}
	exact, err := store.PublishedCatalog(ctx, olderVersion)
	if err != nil {
		t.Fatalf("PublishedCatalog(%q) error = %v", olderVersion, err)
	}
	if exact.Version != olderVersion || exact.SourceVersion != "catalog-"+olderVersion {
		t.Fatalf("PublishedCatalog(%q) = %#v", olderVersion, exact)
	}
	if !exact.PublishedAt.Equal(mappingCatalogOlder) {
		t.Fatalf("published_at = %v, want %v", exact.PublishedAt, mappingCatalogOlder)
	}
}

func TestMongoMappingCatalogPreservesEntryFields(t *testing.T) {
	database, collection := newMappingCatalogCollection(t)
	version := "test-mapping-fields-" + uuid.NewString()
	seedMappingCatalog(t, collection, publishedMappingCatalogDocument(version, mappingCatalogOlder))

	store := NewMappingCatalogRepository(&Data{database: database})
	ctx, cancel := newMongoTestContext()
	defer cancel()

	catalog, err := store.PublishedCatalog(ctx, version)
	if err != nil {
		t.Fatalf("PublishedCatalog() error = %v", err)
	}
	entry, ok := catalog.Entry("video")
	if !ok {
		t.Fatalf("目录缺少 video 条目: %#v", catalog.Entries)
	}
	if entry.Capability != "image_to_video" || entry.PublicModel != "ps-video-v1" || !entry.Enabled {
		t.Fatalf("video 条目 = %#v", entry)
	}
	if len(entry.Durations) != 2 || entry.Durations[1] != 10 {
		t.Fatalf("durations = %#v", entry.Durations)
	}
	if len(entry.Sizes) != 1 || entry.Sizes[0].Width != 1280 || entry.Sizes[0].Height != 720 {
		t.Fatalf("sizes = %#v", entry.Sizes)
	}
	if len(entry.Templates) != 1 || entry.Templates[0].Key != "portrait" || entry.Templates[0].ID != "tpl-portrait-1" {
		t.Fatalf("templates = %#v", entry.Templates)
	}
}

func TestMongoMappingCatalogFailsClosedForUnpublishedOrUnknownVersion(t *testing.T) {
	database, collection := newMappingCatalogCollection(t)
	draftVersion := "test-mapping-draft-" + uuid.NewString()
	draft := publishedMappingCatalogDocument(draftVersion, mappingCatalogNewer)
	// 草稿比任何已发布版本都新：状态过滤必须让它对新任务不可见。
	draft.Status = "draft"
	seedMappingCatalog(t, collection, draft)

	store := NewMappingCatalogRepository(&Data{database: database})
	ctx, cancel := newMongoTestContext()
	defer cancel()

	if _, err := store.PublishedCatalog(ctx, draftVersion); !errors.Is(err, generation.ErrMappingCatalogUnavailable) {
		t.Fatalf("读取草稿版本 error = %v, want ErrMappingCatalogUnavailable", err)
	}
	if _, err := store.PublishedCatalog(ctx, "test-mapping-missing-"+uuid.NewString()); !errors.Is(err, generation.ErrMappingCatalogUnavailable) {
		t.Fatalf("读取不存在版本 error = %v, want ErrMappingCatalogUnavailable", err)
	}
}

func TestMongoMappingCatalogRejectsMalformedPublishedDocument(t *testing.T) {
	database, collection := newMappingCatalogCollection(t)
	version := "test-mapping-malformed-" + uuid.NewString()
	document := publishedMappingCatalogDocument(version, mappingCatalogOlder)
	document.Models[0].Capability = "text_to_video"
	seedMappingCatalog(t, collection, document)

	store := NewMappingCatalogRepository(&Data{database: database})
	ctx, cancel := newMongoTestContext()
	defer cancel()

	if _, err := store.PublishedCatalog(ctx, version); !errors.Is(err, generation.ErrInvalidMappingCatalog) {
		t.Fatalf("读取结构不合法的版本 error = %v, want ErrInvalidMappingCatalog", err)
	}
}

func TestMongoMappingCatalogWithoutDatabaseFailsClosed(t *testing.T) {
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if _, err := NewMappingCatalogRepository(nil).PublishedCatalog(ctx, ""); !errors.Is(err, generation.ErrMappingCatalogUnavailable) {
		t.Fatalf("未配置数据库 error = %v, want ErrMappingCatalogUnavailable", err)
	}
}
