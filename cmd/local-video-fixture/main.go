// Command local-video-fixture 创建仅供本机 Go Gateway 联调的最小视频数据。
//
// 它不会读取 Node 配置、不会调用生成中台，也不会访问 PayCores 或任何远程数据库。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
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
	defaultLocalMongoURI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
	fixtureTimeout       = 10 * time.Second
	fixtureTimezone      = "Asia/Shanghai"
)

// fixturePersona 把一次联调所需的同一用户各集合文档组织在一起，避免调用方混用 ID。
type fixturePersona struct {
	User         model.UserDocument
	Session      model.SessionDocument
	Account      model.AccountDocument
	Subscription *model.SubscriptionDocument
}

// fixtureSet 同时提供预扣和 VIP 日免两个互不共享状态的联调身份。
type fixtureSet struct {
	Prepaid       fixturePersona
	VIP           fixturePersona
	VIPVideoQuota model.DailyQuotaDocument
	Template      model.TemplateDocument
}

func main() {
	mongoURI := flag.String("mongo-uri", defaultLocalMongoURI, "仅允许本机 rs0 MongoDB 连接串")
	flag.Parse()

	config, err := localMongoConfig(*mongoURI)
	if err != nil {
		fmt.Fprintln(os.Stderr, "本地 MongoDB 配置无效。")
		os.Exit(2)
	}
	if err := seedLocalFixture(context.Background(), config, time.Now().UTC()); err != nil {
		fmt.Fprintln(os.Stderr, "本地视频联调数据初始化失败。")
		os.Exit(1)
	}
}

// localMongoConfig 固化开发命令的数据库名、副本集和事务要求；配置校验会拒绝远程主机。
func localMongoConfig(uri string) (*conf.Data, error) {
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

// newFixtureSet 生成完全独立的本地联调数据，不使用固定账号或会话标识。
func newFixtureSet(now time.Time) fixtureSet {
	templateID := "local-video-5-" + uuid.NewString()
	vip := newFixturePersona(now, 0, &model.SubscriptionDocument{
		Status:        string(entitlement.SubscriptionStatusActive),
		BillingPeriod: string(entitlement.SubscriptionBillingPeriodMonthly),
		StartsAt:      now.Add(-time.Hour),
		ExpiresAt:     now.Add(24 * time.Hour),
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	return fixtureSet{
		Prepaid: newFixturePersona(now, 50, nil),
		VIP:     vip,
		VIPVideoQuota: model.DailyQuotaDocument{
			ID:        "local-vip-video-quota:" + vip.User.ID + ":" + fixtureLocalDate(now),
			UserID:    vip.User.ID,
			QuotaKind: "vip_daily_video",
			LocalDate: fixtureLocalDate(now),
			UsedCount: 0,
			Limit:     3,
			UpdatedAt: now,
		},
		Template: newSFWVideoTemplate(templateID, now),
	}
}

// fixtureLocalDate 使用用户首次固化的时区生成日期，保证额度键与权益模块一致。
func fixtureLocalDate(now time.Time) string {
	location, err := time.LoadLocation(fixtureTimezone)
	if err != nil {
		panic(fmt.Sprintf("load local fixture timezone: %v", err))
	}
	return now.In(location).Format("2006-01-02")
}

// newFixturePersona 创建已绑定的 Go 用户及其不透明会话；会话 ID 就是 Bearer Token。
func newFixturePersona(now time.Time, balance int64, subscription *model.SubscriptionDocument) fixturePersona {
	userID := uuid.NewString()
	persona := fixturePersona{
		User: model.UserDocument{
			ID:             userID,
			AccountStatus:  string(identity.AccountStatusNormal),
			BindingState:   string(identity.BindingStateBound),
			Timezone:       fixtureTimezone,
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

// newSFWVideoTemplate 固定生成 5 秒单图视频的服务端配方，客户端不可覆盖技术字段。
func newSFWVideoTemplate(templateID string, now time.Time) model.TemplateDocument {
	parameters, err := bson.Marshal(bson.M{
		"kind": "template_video",
		"i2v": bson.M{
			"model_sku":       "ps-auto",
			"prompt":          "服务端本地联调动作",
			"negative_prompt": "模糊",
			"parameters":      bson.M{"durationSeconds": 5},
		},
		"t2i": bson.M{
			"model_sku":       "ps-image-v1",
			"prompt":          "服务端本地联调首帧",
			"negative_prompt": "模糊",
			"parameters":      bson.M{"aspectRatio": "9:16"},
		},
	})
	if err != nil {
		// 固定 BSON 字面量无法序列化意味着程序本身已损坏，不能静默生成残缺模板。
		panic(fmt.Sprintf("marshal local video template parameters: %v", err))
	}
	return model.TemplateDocument{
		ID:             uuid.NewString(),
		TemplateID:     templateID,
		Version:        1,
		ContentSurface: string(catalog.ContentSurfaceSFW),
		Mode:           string(catalog.ProductModeTemplateVideo),
		Enabled:        true,
		// 仅供隔离本地验收使用的公开展示元数据；技术配方仍完全留在 Parameters 中。
		Title:           "本地五秒视频模板",
		CoverURL:        "/legacy/templates/covers/bridge-tile-1.jpg",
		VideoURL:        "/legacy/templates/videos/bridge-tile-1.mp4",
		PreviewVideoURL: "/legacy/templates/videos/bridge-tile-1.mp4",
		Parameters:      parameters,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// seedLocalFixture 先确保本地 schema，再用单个事务写入两套身份与一个 SFW 模板。
// 所有文档均使用新 UUID；写入冲突会整体回滚，绝不覆盖已有本地数据。
func seedLocalFixture(parent context.Context, config *conf.Data, now time.Time) error {
	if config == nil || config.GetMongo() == nil {
		return errors.New("local MongoDB config is required")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(config.GetMongo().GetUri()))
	if err != nil {
		return fmt.Errorf("connect local MongoDB: %w", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	ctx, cancel := context.WithTimeout(parent, fixtureTimeout)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("ping local MongoDB: %w", err)
	}
	database := client.Database(config.GetMongo().GetDatabase())
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		return fmt.Errorf("initialize local MongoDB schema: %w", err)
	}

	fixture := newFixtureSet(now)
	session, err := client.StartSession()
	if err != nil {
		return fmt.Errorf("start local MongoDB session: %w", err)
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(transactionContext context.Context) (any, error) {
		return nil, insertFixtureSet(transactionContext, database, fixture)
	})
	if err != nil {
		return fmt.Errorf("insert local video fixture: %w", err)
	}
	printFixture(fixture)
	return nil
}

// insertFixtureSet 只执行 InsertOne；唯一索引冲突会阻止覆盖并让外层事务回滚。
func insertFixtureSet(ctx context.Context, database *mongo.Database, fixture fixtureSet) error {
	if database == nil {
		return errors.New("local MongoDB database is required")
	}
	for _, persona := range []fixturePersona{fixture.Prepaid, fixture.VIP} {
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
	if _, err := database.Collection(schema.CollectionDailyQuotas).InsertOne(ctx, fixture.VIPVideoQuota); err != nil {
		return err
	}
	_, err := database.Collection(schema.CollectionTemplates).InsertOne(ctx, fixture.Template)
	return err
}

// printFixture 只输出本次新建的本地测试标识，便于手工请求时携带正确的 Go 会话。
func printFixture(fixture fixtureSet) {
	fmt.Printf("预扣用户 Authorization: Bearer %s\n", fixture.Prepaid.Session.ID)
	fmt.Printf("VIP 用户 Authorization: Bearer %s\n", fixture.VIP.Session.ID)
	fmt.Printf("视频模板 templateId: %s\n", fixture.Template.TemplateID)
	fmt.Println("预扣验证：预扣用户以 5 秒图片视频请求，初始 50 钻，成功后余额应为 0。")
	fmt.Println("VIP 验证：VIP 用户以同一模板请求，余额为 0 但仍可使用每日免费视频额度。")
}
