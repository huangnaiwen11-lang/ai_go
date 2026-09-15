package migrate

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"ai-business-service/internal/conf"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const localTestMongoURIEnv = "CLING_TEST_MONGO_URI"

var expectedCollectionNames = []string{
	"accounts",
	"users",
	"credentials",
	"identities",
	"sessions",
	"templates",
	"assets",
	"creations",
	"creation_steps",
	"generation_step_recipes",
	"reservations",
	"daily_quotas",
	"ledger_entries",
	"subscriptions",
	"payment_products",
	"payment_orders",
	"payment_receipts",
	"payment_callback_nonces",
	"callback_receipts",
	"outbox_events",
	"feedbacks",
	"notifications",
	"notification_preferences",
}

var expectedIndexSpecs = []struct {
	collection         string
	name               string
	keys               bson.D
	unique             bool
	sparse             bool
	expireAfterSeconds *int32
}{
	{collection: "identities", name: "ux_identities_provider_subject", keys: bson.D{{Key: "provider", Value: int32(1)}, {Key: "subject", Value: int32(1)}}, unique: true},
	{collection: "sessions", name: "ix_sessions_user_revoked", keys: bson.D{{Key: "user_id", Value: int32(1)}, {Key: "revoked_at", Value: int32(1)}}},
	{collection: "credentials", name: "ux_credentials_active_email", keys: bson.D{{Key: "email_normalized", Value: int32(1)}}, unique: true},
	{collection: "users", name: "ux_users_guest_platform_device", keys: bson.D{{Key: "guest_platform", Value: int32(1)}, {Key: "guest_device_id", Value: int32(1)}}, unique: true, sparse: true},
	{collection: "templates", name: "ux_templates_template_version", keys: bson.D{{Key: "template_id", Value: int32(1)}, {Key: "version", Value: int32(1)}}, unique: true},
	{collection: "templates", name: "ix_templates_enabled_surface_sort", keys: bson.D{{Key: "enabled", Value: int32(1)}, {Key: "content_surface", Value: int32(1)}, {Key: "sort_order", Value: int32(1)}}},
	{collection: "assets", name: "ix_assets_owner_created", keys: bson.D{{Key: "owner_type", Value: int32(1)}, {Key: "owner_id", Value: int32(1)}, {Key: "created_at", Value: int32(-1)}}},
	{collection: "creations", name: "ux_creations_idempotency_key", keys: bson.D{{Key: "idempotency_key", Value: int32(1)}}, unique: true},
	{collection: "creations", name: "ix_creations_user_created", keys: bson.D{{Key: "user_id", Value: int32(1)}, {Key: "created_at", Value: int32(-1)}}},
	{collection: "creations", name: "ix_creations_user_output_created_id", keys: bson.D{{Key: "user_id", Value: int32(1)}, {Key: "product_output", Value: int32(1)}, {Key: "created_at", Value: int32(-1)}, {Key: "_id", Value: int32(-1)}}},
	{collection: "creation_steps", name: "ux_creation_steps_creation_sequence", keys: bson.D{{Key: "creation_id", Value: int32(1)}, {Key: "sequence", Value: int32(1)}}, unique: true},
	{collection: "creation_steps", name: "ix_creation_steps_external_execution", keys: bson.D{{Key: "external_execution_id", Value: int32(1)}}},
	{collection: "creation_steps", name: "ux_creation_steps_external_execution_present", keys: bson.D{{Key: "external_execution_id", Value: int32(1)}}, unique: true, sparse: true},
	{collection: "generation_step_recipes", name: "ux_generation_step_recipes_step_id", keys: bson.D{{Key: "step_id", Value: int32(1)}}, unique: true},
	{collection: "reservations", name: "ux_reservations_creation_id", keys: bson.D{{Key: "creation_id", Value: int32(1)}}, unique: true},
	{collection: "daily_quotas", name: "ux_daily_quotas_user_kind_date", keys: bson.D{{Key: "user_id", Value: int32(1)}, {Key: "quota_kind", Value: int32(1)}, {Key: "local_date", Value: int32(1)}}, unique: true},
	{collection: "ledger_entries", name: "ux_ledger_entries_idempotency_key", keys: bson.D{{Key: "idempotency_key", Value: int32(1)}}, unique: true},
	{collection: "ledger_entries", name: "ix_ledger_entries_account_created", keys: bson.D{{Key: "account_id", Value: int32(1)}, {Key: "created_at", Value: int32(-1)}}},
	{collection: "payment_products", name: "ux_payment_products_product_version", keys: bson.D{{Key: "product_id", Value: int32(1)}, {Key: "version", Value: int32(1)}}, unique: true},
	{collection: "payment_orders", name: "ix_payment_orders_user_created", keys: bson.D{{Key: "user_id", Value: int32(1)}, {Key: "created_at", Value: int32(-1)}}},
	{collection: "payment_orders", name: "ux_payment_orders_provider_provider_order", keys: bson.D{{Key: "provider", Value: int32(1)}, {Key: "provider_order_id", Value: int32(1)}}, unique: true},
	{collection: "payment_receipts", name: "ux_payment_receipts_provider_external_transaction", keys: bson.D{{Key: "provider", Value: int32(1)}, {Key: "external_transaction_id", Value: int32(1)}}, unique: true},
	{collection: "payment_callback_nonces", name: "ux_payment_callback_nonces_nonce_hash", keys: bson.D{{Key: "nonce_hash", Value: int32(1)}}, unique: true},
	{collection: "payment_callback_nonces", name: "ix_payment_callback_nonces_expires_at_ttl", keys: bson.D{{Key: "expires_at", Value: int32(1)}}, expireAfterSeconds: int32Pointer(0)},
	{collection: "callback_receipts", name: "ux_callback_receipts_source_nonce_hash", keys: bson.D{{Key: "source", Value: int32(1)}, {Key: "nonce_hash", Value: int32(1)}}, unique: true},
	{collection: "callback_receipts", name: "ix_callback_receipts_step_terminal", keys: bson.D{{Key: "source", Value: int32(1)}, {Key: "step_id", Value: int32(1)}, {Key: "job_id", Value: int32(1)}, {Key: "capability", Value: int32(1)}, {Key: "terminal", Value: int32(1)}}},
	{collection: "outbox_events", name: "ix_outbox_events_status_next_attempt", keys: bson.D{{Key: "delivery_status", Value: int32(1)}, {Key: "next_attempt_at", Value: int32(1)}}},
	{collection: "outbox_events", name: "ix_outbox_events_claim_type_status_next_attempt_lease_until", keys: bson.D{{Key: "event_type", Value: int32(1)}, {Key: "delivery_status", Value: int32(1)}, {Key: "next_attempt_at", Value: int32(1)}, {Key: "lease_until", Value: int32(1)}}},
	{collection: "notifications", name: "ix_notifications_user_read_created", keys: bson.D{{Key: "user_id", Value: int32(1)}, {Key: "read", Value: int32(1)}, {Key: "created_at", Value: int32(-1)}}},
}

// 固定验收清单必须覆盖全部声明；无数据库时也执行，避免集成测试跳过后掩盖清单漂移。
func TestFrozenSchemaCoversAllDeclarations(t *testing.T) {
	collections := make(map[string]bool, len(expectedCollectionNames))
	for _, name := range expectedCollectionNames {
		if collections[name] {
			t.Errorf("固定集合清单重复声明 %q", name)
		}
		collections[name] = true
	}
	for _, name := range schema.AllCollections() {
		if !collections[name] {
			t.Errorf("集合 %q 尚未纳入固定验收清单", name)
		}
		delete(collections, name)
	}
	for name := range collections {
		t.Errorf("固定验收清单保留了未声明的集合 %q", name)
	}

	type indexKey struct{ collection, name string }
	indexes := make(map[indexKey]bool, len(expectedIndexSpecs))
	for _, spec := range expectedIndexSpecs {
		key := indexKey{spec.collection, spec.name}
		if indexes[key] {
			t.Errorf("固定索引清单重复声明 %v", key)
		}
		indexes[key] = true
	}
	for _, spec := range schema.AllIndexes() {
		key := indexKey{spec.Collection, spec.Name}
		if !indexes[key] {
			t.Errorf("索引 %v 尚未纳入固定验收清单", key)
		}
		delete(indexes, key)
	}
	for key := range indexes {
		t.Errorf("固定验收清单保留了未声明的索引 %v", key)
	}
}

func TestInitializerRejectsNilDatabase(t *testing.T) {
	err := NewInitializer(nil).Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() 未拒绝未配置的数据库")
	}
	if got, want := err.Error(), "local MongoDB schema initializer is not configured"; got != want {
		t.Fatalf("Ensure() error = %q, want %q", got, want)
	}
}

func TestInitializerRejectsNilReceiver(t *testing.T) {
	var initializer *Initializer
	err := initializer.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() 未拒绝空接收者")
	}
	if got, want := err.Error(), "local MongoDB schema initializer is not configured"; got != want {
		t.Fatalf("Ensure() error = %q, want %q", got, want)
	}
}

func TestEnsureCreatesAllCollectionsAndIsIdempotent(t *testing.T) {
	database := newLocalTestDatabase(t)
	initializer := NewInitializer(database)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := initializer.Ensure(ctx); err != nil {
		t.Fatalf("第一次 Ensure() error = %v", err)
	}

	names, err := database.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatalf("列出集合名称: %v", err)
	}
	if got, want := len(names), len(expectedCollectionNames); got != want {
		t.Fatalf("Ensure() 后集合数量 = %d, want %d", got, want)
	}
	collections := make(map[string]struct{}, len(names))
	for _, name := range names {
		collections[name] = struct{}{}
	}
	for _, name := range expectedCollectionNames {
		if _, ok := collections[name]; !ok {
			t.Errorf("Ensure() 后缺少集合 %q", name)
		}
	}

	if err := initializer.Ensure(ctx); err != nil {
		t.Fatalf("第二次 Ensure() error = %v", err)
	}
}

func TestEnsureCreatesAllDeclaredIndexes(t *testing.T) {
	database := newLocalTestDatabase(t)
	ensureLocalSchema(t, database)

	for _, expected := range expectedIndexSpecs {
		expected := expected
		t.Run(expected.collection+"/"+expected.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			specifications, err := database.Collection(expected.collection).Indexes().ListSpecifications(ctx)
			if err != nil {
				t.Fatalf("列出索引规格: %v", err)
			}
			var actual *mongo.IndexSpecification
			for index := range specifications {
				if specifications[index].Name == expected.name {
					actual = &specifications[index]
					break
				}
			}
			if actual == nil {
				t.Fatalf("集合 %q 缺少索引 %q", expected.collection, expected.name)
			}

			var actualKeys bson.D
			if err := bson.Unmarshal(actual.KeysDocument, &actualKeys); err != nil {
				t.Fatalf("解码索引 %q 的键: %v", expected.name, err)
			}
			if !reflect.DeepEqual(actualKeys, expected.keys) {
				t.Errorf("索引 %q 键 = %#v, want %#v", expected.name, actualKeys, expected.keys)
			}
			actualUnique := actual.Unique != nil && *actual.Unique
			if actualUnique != expected.unique {
				t.Errorf("索引 %q unique = %t, want %t", expected.name, actualUnique, expected.unique)
			}
			actualSparse := actual.Sparse != nil && *actual.Sparse
			if actualSparse != expected.sparse {
				t.Errorf("索引 %q sparse = %t, want %t", expected.name, actualSparse, expected.sparse)
			}
			if !reflect.DeepEqual(actual.ExpireAfterSeconds, expected.expireAfterSeconds) {
				t.Errorf("索引 %q expireAfterSeconds = %#v, want %#v", expected.name, actual.ExpireAfterSeconds, expected.expireAfterSeconds)
			}
		})
	}
}

func int32Pointer(value int32) *int32 {
	return &value
}

func TestUniqueIndexesRejectDuplicateBusinessFacts(t *testing.T) {
	database := newLocalTestDatabase(t)
	ensureLocalSchema(t, database)

	facts := []struct {
		name                string
		collection          string
		document            func(string) bson.D
		counterexampleField string
		counterexampleValue func() any
	}{
		{
			name:       "identities 复用 provider 和 subject",
			collection: schema.CollectionIdentities,
			document: func(id string) bson.D {
				key := uuid.NewString()
				return bson.D{{Key: "_id", Value: id}, {Key: "provider", Value: "test-provider-" + key}, {Key: "subject", Value: "test-subject-" + key}}
			},
			counterexampleField: "subject",
			counterexampleValue: func() any {
				return "test-subject-" + uuid.NewString()
			},
		},
		{
			name:       "creations 复用 idempotency_key",
			collection: schema.CollectionCreations,
			document: func(id string) bson.D {
				return bson.D{{Key: "_id", Value: id}, {Key: "idempotency_key", Value: "test-idempotency-" + uuid.NewString()}}
			},
		},
		{
			name:       "creation_steps 复用 creation_id 和 sequence",
			collection: schema.CollectionCreationSteps,
			document: func(id string) bson.D {
				return bson.D{{Key: "_id", Value: id}, {Key: "creation_id", Value: "test-creation-" + uuid.NewString()}, {Key: "sequence", Value: 1}}
			},
			counterexampleField: "sequence",
			counterexampleValue: func() any {
				return 2
			},
		},
		{
			name:       "generation_step_recipes 复用 step_id",
			collection: schema.CollectionGenerationStepRecipes,
			document: func(id string) bson.D {
				return bson.D{{Key: "_id", Value: id}, {Key: "step_id", Value: "test-step-" + uuid.NewString()}}
			},
		},
		{
			name:       "reservations 复用 creation_id",
			collection: schema.CollectionReservations,
			document: func(id string) bson.D {
				return bson.D{{Key: "_id", Value: id}, {Key: "creation_id", Value: "test-creation-" + uuid.NewString()}}
			},
		},
		{
			name:       "daily_quotas 复用 user_id quota_kind 和 local_date",
			collection: schema.CollectionDailyQuotas,
			document: func(id string) bson.D {
				return bson.D{{Key: "_id", Value: id}, {Key: "user_id", Value: "test-user-" + uuid.NewString()}, {Key: "quota_kind", Value: "test-quota"}, {Key: "local_date", Value: "2026-09-05"}}
			},
			counterexampleField: "local_date",
			counterexampleValue: func() any {
				return "2026-09-06"
			},
		},
		{
			name:       "ledger_entries 复用 idempotency_key",
			collection: schema.CollectionLedgerEntries,
			document: func(id string) bson.D {
				return bson.D{{Key: "_id", Value: id}, {Key: "idempotency_key", Value: "test-idempotency-" + uuid.NewString()}}
			},
		},
		{
			name:       "payment_products 复用 product_id 和 version",
			collection: schema.CollectionPaymentProducts,
			document: func(id string) bson.D {
				return bson.D{{Key: "_id", Value: id}, {Key: "product_id", Value: "test-product-" + uuid.NewString()}, {Key: "version", Value: 1}}
			},
		},
		{
			name:       "payment_orders 复用 provider 和 provider_order_id",
			collection: schema.CollectionPaymentOrders,
			document: func(id string) bson.D {
				key := uuid.NewString()
				return bson.D{{Key: "_id", Value: id}, {Key: "provider", Value: "test-provider-" + key}, {Key: "provider_order_id", Value: "test-provider-order-" + key}}
			},
			counterexampleField: "provider_order_id",
			counterexampleValue: func() any {
				return "test-provider-order-" + uuid.NewString()
			},
		},
		{
			name:       "payment_receipts 复用 provider 和 external_transaction_id",
			collection: schema.CollectionPaymentReceipts,
			document: func(id string) bson.D {
				key := uuid.NewString()
				return bson.D{{Key: "_id", Value: id}, {Key: "provider", Value: "test-provider-" + key}, {Key: "external_transaction_id", Value: "test-transaction-" + key}}
			},
			counterexampleField: "external_transaction_id",
			counterexampleValue: func() any {
				return "test-transaction-" + uuid.NewString()
			},
		},
		{
			name:       "callback_receipts 复用 source 和 nonce_hash",
			collection: schema.CollectionCallbackReceipts,
			document: func(id string) bson.D {
				key := uuid.NewString()
				return bson.D{{Key: "_id", Value: id}, {Key: "source", Value: "test-source-" + key}, {Key: "nonce_hash", Value: "test-nonce-" + key}}
			},
			counterexampleField: "nonce_hash",
			counterexampleValue: func() any {
				return "test-nonce-" + uuid.NewString()
			},
		},
	}
	if got := len(facts); got != 11 {
		t.Fatalf("唯一业务事实数量 = %d, want 11", got)
	}

	for _, fact := range facts {
		fact := fact
		t.Run(fact.name, func(t *testing.T) {
			firstID := uuid.NewString()
			secondID := uuid.NewString()
			ids := bson.A{firstID, secondID}
			counterexampleID := ""
			if fact.counterexampleField != "" {
				counterexampleID = uuid.NewString()
				ids = append(ids, counterexampleID)
			}
			collection := database.Collection(fact.collection)
			cleanupDocumentsByID(t, collection, ids)

			first := fact.document(firstID)
			second := fact.document(secondID)
			copyBusinessFactValues(first, second)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := collection.InsertOne(ctx, first); err != nil {
				t.Fatalf("插入第一份业务事实: %v", err)
			}
			if _, err := collection.InsertOne(ctx, second); !mongo.IsDuplicateKeyError(err) {
				t.Fatalf("插入重复业务事实 error = %v, want duplicate key error", err)
			}
			if fact.counterexampleField == "" {
				return
			}

			counterexample := fact.document(counterexampleID)
			copyBusinessFactValues(first, counterexample)
			replaceBusinessFactValue(counterexample, fact.counterexampleField, fact.counterexampleValue())
			if _, err := collection.InsertOne(ctx, counterexample); err != nil {
				t.Fatalf("插入仅修改复合键字段 %q 的业务事实: %v", fact.counterexampleField, err)
			}
		})
	}
}

func ensureLocalSchema(t *testing.T, database *mongo.Database) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
}

func newLocalTestDatabase(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv(localTestMongoURIEnv)
	if uri == "" {
		t.Skipf("设置 %s 后运行本地 MongoDB 集成测试", localTestMongoURIEnv)
	}

	dataConfig := &conf.Data{
		Mongo: &conf.Data_Mongo{
			Uri:                  uri,
			Database:             "cling_main",
			ReplicaSet:           "rs0",
			TransactionsRequired: true,
		},
	}
	if err := conf.ValidateLocalMongo(dataConfig); err != nil {
		t.Fatalf("拒绝不安全的 MongoDB 测试 URI: %v", err)
	}

	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetReplicaSet("rs0"))
	if err != nil {
		t.Fatalf("连接本地 MongoDB: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Disconnect(ctx); err != nil {
			t.Errorf("断开本地 MongoDB: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("探测本地 MongoDB: %v", err)
	}
	return client.Database("cling_main")
}

func cleanupDocumentsByID(t *testing.T, collection *mongo.Collection, ids bson.A) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 测试只能清理自身生成的随机 _id；逐条 DeleteOne 避免任何批量删除范围扩大。
		for _, id := range ids {
			filter := bson.D{{Key: "_id", Value: id}}
			if _, err := collection.DeleteOne(ctx, filter); err != nil {
				t.Errorf("按测试 _id 清理集合 %q 中的 %q: %v", collection.Name(), id, err)
			}
		}
	})
}

func copyBusinessFactValues(first, second bson.D) {
	for _, element := range first {
		if element.Key == "_id" {
			continue
		}
		for index := range second {
			if second[index].Key == element.Key {
				second[index].Value = element.Value
			}
		}
	}
}

func replaceBusinessFactValue(document bson.D, key string, value any) {
	for index := range document {
		if document[index].Key == key {
			document[index].Value = value
			return
		}
	}
}
