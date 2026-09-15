// Command local-image-fixture 创建仅供本机 Go Gateway 联调的最小图片数据。
//
// 它不会读取 Node 配置、不会启动 Worker，也不会调用生成中台、PayCores 或远程数据库。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"time"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	defaultLocalImageMongoURI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
	imageFixtureTimeout       = 10 * time.Second
	imageFixtureTimezone      = "Asia/Shanghai"
)

// imageFixturePersona 把某个本地图片联调用户的身份、会话、账户和订阅集中管理，避免错配 ID。
type imageFixturePersona struct {
	User         model.UserDocument
	Session      model.SessionDocument
	Account      model.AccountDocument
	Subscription *model.SubscriptionDocument
}

// imageFixtureSet 同时提供预扣与 VIP 日免身份，以及两类受限 SFW 图片配方。
type imageFixtureSet struct {
	Prepaid       imageFixturePersona
	VIP           imageFixturePersona
	VIPImageQuota model.DailyQuotaDocument
	Freeform      model.TemplateDocument
	ImageEdit     model.TemplateDocument
}

func main() {
	mongoURI := flag.String("mongo-uri", defaultLocalImageMongoURI, "仅允许本机 rs0 MongoDB 连接串")
	flag.Parse()

	config, err := localImageMongoConfig(*mongoURI)
	if err != nil {
		fmt.Fprintln(os.Stderr, "本地 MongoDB 配置无效。")
		os.Exit(2)
	}
	if err := seedLocalImageFixture(context.Background(), config, time.Now().UTC()); err != nil {
		fmt.Fprintln(os.Stderr, "本地图片联调数据初始化失败。")
		os.Exit(1)
	}
}

// localImageMongoConfig 固化图片联调仅能使用的数据库名、副本集与本机事务配置。
func localImageMongoConfig(uri string) (*conf.Data, error) {
	config := &conf.Data{Mongo: &conf.Data_Mongo{
		Uri:                  uri,
		Database:             "cling_main",
		ReplicaSet:           "rs0",
		TransactionsRequired: true,
	}}
	if err := conf.ValidateLocalMongo(config); err != nil {
		return nil, err
	}
	return config, nil
}

// newImageFixtureSet 生成不共享账号和会话 ID 的图片联调数据。
func newImageFixtureSet(now time.Time) imageFixtureSet {
	vip := newImageFixturePersona(now, 0, &model.SubscriptionDocument{
		Status:        string(entitlement.SubscriptionStatusActive),
		BillingPeriod: string(entitlement.SubscriptionBillingPeriodMonthly),
		StartsAt:      now.Add(-time.Hour),
		ExpiresAt:     now.Add(24 * time.Hour),
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	return imageFixtureSet{
		Prepaid: newImageFixturePersona(now, 20, nil),
		VIP:     vip,
		VIPImageQuota: model.DailyQuotaDocument{
			ID:        "local-vip-image-quota:" + vip.User.ID + ":" + imageFixtureLocalDate(now),
			UserID:    vip.User.ID,
			QuotaKind: "vip_daily_image",
			LocalDate: imageFixtureLocalDate(now),
			UsedCount: 0,
			Limit:     10,
			UpdatedAt: now,
		},
		Freeform:  newSFWFreeformRecipe(now),
		ImageEdit: newSFWImageEditTemplate(now),
	}
}

// imageFixtureLocalDate 以首次固化的用户时区构建日免键，与权益模块的日期语义一致。
func imageFixtureLocalDate(now time.Time) string {
	location, err := time.LoadLocation(imageFixtureTimezone)
	if err != nil {
		panic(fmt.Sprintf("load local image fixture timezone: %v", err))
	}
	return now.In(location).Format("2006-01-02")
}

// newImageFixturePersona 创建已经绑定的 Go 自有会话；Session.ID 是本地 Bearer Token。
func newImageFixturePersona(now time.Time, balance int64, subscription *model.SubscriptionDocument) imageFixturePersona {
	userID := uuid.NewString()
	persona := imageFixturePersona{
		User: model.UserDocument{
			ID:             userID,
			AccountStatus:  string(identity.AccountStatusNormal),
			BindingState:   string(identity.BindingStateBound),
			Timezone:       imageFixtureTimezone,
			SessionVersion: 1,
			ContentAccess:  identity.ContentAccessStandard,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
		Session: model.SessionDocument{
			ID:             uuid.NewString(),
			UserID:         userID,
			SessionVersion: 1,
			ExpiresAt:      now.Add(12 * time.Hour),
		},
		Account: model.AccountDocument{
			ID:             userID,
			DiamondBalance: balance,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
	}
	if subscription != nil {
		copy := *subscription
		copy.UserID = userID
		persona.Subscription = &copy
	}
	return persona
}

// newSFWFreeformRecipe 固化自由文生图唯一允许的服务端技术配方。
func newSFWFreeformRecipe(now time.Time) model.TemplateDocument {
	parameters, err := bson.Marshal(bson.M{
		"model_sku":  "ps-image-v1",
		"parameters": bson.M{"steps": 28},
	})
	if err != nil {
		panic(fmt.Sprintf("marshal local freeform image parameters: %v", err))
	}
	return model.TemplateDocument{
		ID:             uuid.NewString(),
		TemplateID:     "t2i-freeform",
		Version:        1,
		ContentSurface: string(catalog.ContentSurfaceSFW),
		Mode:           string(catalog.ProductModeTemplateImage),
		Enabled:        true,
		Parameters:     parameters,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// newSFWImageEditTemplate 创建单图输入的换装模板；模型、参考图和技术参数均由服务端冻结。
func newSFWImageEditTemplate(now time.Time) model.TemplateDocument {
	parameters, err := bson.Marshal(bson.M{
		"kind":            "image_edit",
		"model_sku":       "ps-edit-apparel-v1",
		"prompt":          "服务端本地联调换装模板",
		"negative_prompt": "模糊",
		"parameters":      bson.M{"steps": 28},
		"input_rule":      bson.M{"user_image_count": 1, "user_role": "source_image"},
		"reference_assets": bson.A{
			bson.M{"role": "guide_image", "url": "https://assets.example.test/local-image-guide.png"},
		},
	})
	if err != nil {
		panic(fmt.Sprintf("marshal local image edit parameters: %v", err))
	}
	return model.TemplateDocument{
		ID: uuid.NewString(),
		// 固定标识只用于本机 fixture，让目录卡片与服务端冻结配方指向同一模板。
		TemplateID:     "local-image-edit-dress-up",
		Version:        1,
		ContentSurface: string(catalog.ContentSurfaceSFW),
		Mode:           string(catalog.ProductModeTemplateImage),
		Enabled:        true,
		Parameters:     parameters,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// seedLocalImageFixture 初始化本机 schema 后，以单事务写入全部图片联调文档。
// 全部写入均为 InsertOne；唯一索引冲突会让事务回滚，绝不覆盖已有本地数据。
func seedLocalImageFixture(parent context.Context, config *conf.Data, now time.Time) error {
	if config == nil || config.GetMongo() == nil {
		return errors.New("local MongoDB config is required")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(config.GetMongo().GetUri()))
	if err != nil {
		return fmt.Errorf("connect local MongoDB: %w", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	ctx, cancel := context.WithTimeout(parent, imageFixtureTimeout)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("ping local MongoDB: %w", err)
	}
	database := client.Database(config.GetMongo().GetDatabase())
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		return fmt.Errorf("initialize local MongoDB schema: %w", err)
	}

	fixture := newImageFixtureSet(now)
	existingTemplates, err := loadImageFixtureTemplates(ctx, database, fixture)
	if err != nil {
		return fmt.Errorf("load local image fixture templates: %w", err)
	}
	templatesToInsert, err := planImageFixtureTemplateInserts(existingTemplates, fixture)
	if err != nil {
		return err
	}
	session, err := client.StartSession()
	if err != nil {
		return fmt.Errorf("start local MongoDB session: %w", err)
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(transactionContext context.Context) (any, error) {
		return nil, insertImageFixtureSet(transactionContext, database, fixture, templatesToInsert)
	})
	if err != nil {
		return fmt.Errorf("insert local image fixture: %w", err)
	}
	printImageFixture(fixture)
	return nil
}

// loadImageFixtureTemplates 只读取 fixture 固定标识的同版本模板，避免重复运行时误覆盖已有配方。
func loadImageFixtureTemplates(ctx context.Context, database *mongo.Database, fixture imageFixtureSet) (map[string]model.TemplateDocument, error) {
	if database == nil {
		return nil, errors.New("local MongoDB database is required")
	}
	cursor, err := database.Collection(schema.CollectionTemplates).Find(ctx, bson.M{
		"template_id": bson.M{"$in": bson.A{fixture.Freeform.TemplateID, fixture.ImageEdit.TemplateID}},
		"version":     fixture.Freeform.Version,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	existing := make(map[string]model.TemplateDocument)
	for cursor.Next(ctx) {
		var document model.TemplateDocument
		if err := cursor.Decode(&document); err != nil {
			return nil, err
		}
		existing[document.TemplateID] = document
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return existing, nil
}

// planImageFixtureTemplateInserts 仅复用字段和技术配方均一致的模板；不一致时宁可失败，也不覆盖本地既有数据。
func planImageFixtureTemplateInserts(existing map[string]model.TemplateDocument, fixture imageFixtureSet) ([]model.TemplateDocument, error) {
	var insertions []model.TemplateDocument
	for _, expected := range []model.TemplateDocument{fixture.Freeform, fixture.ImageEdit} {
		actual, found := existing[expected.TemplateID]
		if !found {
			insertions = append(insertions, expected)
			continue
		}
		if !sameImageFixtureTemplate(actual, expected) {
			return nil, fmt.Errorf("本地模板 %q 已存在但配方不兼容，拒绝覆盖", expected.TemplateID)
		}
	}
	return insertions, nil
}

// sameImageFixtureTemplate 比较用户可见属性与冻结技术配方，忽略每次初始化不同的文档 ID 和时间戳。
func sameImageFixtureTemplate(actual model.TemplateDocument, expected model.TemplateDocument) bool {
	if actual.TemplateID != expected.TemplateID ||
		actual.Version != expected.Version ||
		actual.ContentSurface != expected.ContentSurface ||
		actual.Mode != expected.Mode ||
		actual.Enabled != expected.Enabled {
		return false
	}
	var actualParameters bson.M
	var expectedParameters bson.M
	if bson.Unmarshal(actual.Parameters, &actualParameters) != nil || bson.Unmarshal(expected.Parameters, &expectedParameters) != nil {
		return false
	}
	return reflect.DeepEqual(actualParameters, expectedParameters)
}

// insertImageFixtureSet 只插入本次新建的模板和随机账号文档，确保不会触碰 Node 或既有本地数据。
func insertImageFixtureSet(ctx context.Context, database *mongo.Database, fixture imageFixtureSet, templatesToInsert []model.TemplateDocument) error {
	if database == nil {
		return errors.New("local MongoDB database is required")
	}
	for _, persona := range []imageFixturePersona{fixture.Prepaid, fixture.VIP} {
		if _, err := database.Collection(schema.CollectionUsers).InsertOne(ctx, persona.User); err != nil {
			return err
		}
		if _, err := database.Collection(schema.CollectionSessions).InsertOne(ctx, persona.Session); err != nil {
			return err
		}
		if _, err := database.Collection(schema.CollectionAccounts).InsertOne(ctx, persona.Account); err != nil {
			return err
		}
		if persona.Subscription != nil {
			if _, err := database.Collection(schema.CollectionSubscriptions).InsertOne(ctx, persona.Subscription); err != nil {
				return err
			}
		}
	}
	if _, err := database.Collection(schema.CollectionDailyQuotas).InsertOne(ctx, fixture.VIPImageQuota); err != nil {
		return err
	}
	for _, template := range templatesToInsert {
		if _, err := database.Collection(schema.CollectionTemplates).InsertOne(ctx, template); err != nil {
			return err
		}
	}
	return nil
}

// printImageFixture 仅输出本次测试需要的会话和模板标识，绝不输出连接串或素材地址。
func printImageFixture(fixture imageFixtureSet) {
	fmt.Printf("预扣用户 Authorization: Bearer %s\n", fixture.Prepaid.Session.ID)
	fmt.Printf("VIP 用户 Authorization: Bearer %s\n", fixture.VIP.Session.ID)
	fmt.Printf("T2I 配方 templateId: %s\n", fixture.Freeform.TemplateID)
	fmt.Printf("I2I 模板 templateId: %s\n", fixture.ImageEdit.TemplateID)
	fmt.Println("预扣验证：预扣用户提交一张自由文生图或模板图编辑，初始 20 钻，成功后余额应为 0。")
	fmt.Println("VIP 验证：VIP 用户余额为 0，仍可使用当天 10 张免费图片额度。")
}
