// Command local-admin-fixture 为本地管理后台准备四类联调数据：
// 套餐/VIP 覆盖、Apps、UTM 链接，以及审核投影的 legacy 源集合。
//
// 只连接本机 MongoDB，不调用 PayCores、商店、生成供应商或任何外部渠道。
//
// 写入的文档归属本命令：apps/utmlinks 的 _id 用 local-seed-* 前缀或固定 ObjectID，
// legacy 审核源写入**独立 database**（默认 ai-host-local，即旧 Node 本地库名），
// 与 cling_main 完全隔离 —— 因为 cmd/admin-review-import 硬性要求 legacy 与 target
// 不能是同一个 database，且 Go 侧读取审核只认 admin_review_items 投影。
//
// 重复运行会把种子文档恢复成快照（upsert $set）。也就是说，用管理后台手工改过
// 这些种子文档之后，再跑一次 fixture 会覆盖回去 —— 这是刻意的：种子数据不是业务数据。
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"ai-business-service/internal/biz/adminpricing"
	"ai-business-service/internal/biz/adminreview"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	defaultLocalAdminMongoURI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
	defaultLegacyDatabase     = "ai-host-local"
	localAdminFixtureTimeout  = 30 * time.Second

	// 样例开关：with-samples 会额外写入刻意畸形的 legacy 文档，用来演示
	// admin-review-import 的 orphan/conflict 分类；clean 只写可导入的干净数据。
	samplesClean       = "clean"
	samplesWithSamples = "with-samples"

	// localSeedAppIDPrefix 标明 apps 种子的归属，避免与真实 App 注册表混淆。
	localSeedAppIDPrefix = "local-seed-app-"
)

// seedBaseTime 固定所有种子文档的时间，保证重复运行产出逐字节相同的快照。
var seedBaseTime = time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 只做参数解析与本地隔离校验；任何拒绝都在连库之前返回 2，
// 因此这条路径可以脱离 MongoDB 单测。
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("local-admin-fixture", flag.ContinueOnError)
	flags.SetOutput(stderr)
	mongoURI := flags.String("mongo-uri", defaultLocalAdminMongoURI, "仅允许本机 rs0 MongoDB 连接串")
	legacyDatabase := flags.String("legacy-database", defaultLegacyDatabase, "legacy 审核源集合写入的 database（必须与 cling_main 不同）")
	samples := flags.String("samples", samplesClean, "clean | with-samples（后者额外写入畸形文档以演示 orphan/conflict 分类）")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}

	if *samples != samplesClean && *samples != samplesWithSamples {
		fmt.Fprintln(stderr, "--samples 必须是 clean 或 with-samples。")
		return 2
	}
	if *legacyDatabase == "" || *legacyDatabase == "cling_main" {
		fmt.Fprintln(stderr, "--legacy-database 不能为空，也不能是 cling_main（审核导入要求 legacy 与 target 分离）。")
		return 2
	}
	config, err := localAdminMongoConfig(*mongoURI)
	if err != nil {
		fmt.Fprintln(stderr, "本地 MongoDB 配置无效。")
		return 2
	}

	summary, err := seedLocalAdminData(context.Background(), config, *legacyDatabase, *samples)
	if err != nil {
		fmt.Fprintln(stderr, "本地管理后台种子数据写入失败：", err)
		return 1
	}
	printSummary(stdout, summary, *legacyDatabase, *samples)
	return 0
}

// localAdminMongoConfig 固化「只允许本机 rs0、必须开事务」的隔离约束。
// 它复用与常驻服务相同的 conf.ValidateLocalMongo，因此 uri 里出现非本机主机或
// 非 27017 端口时会在连库之前就被拒绝。
func localAdminMongoConfig(uri string) (*conf.Data, error) {
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

type seedSummary struct {
	PricingConfigs int64
	Apps           int64
	UTMLinks       int64
	LegacySources  map[string]int64
	UserIDs        []string
}

// seedLocalAdminData 建 schema 后依次写入四类种子；任何一步失败都整体报错，
// 不留下「只写了一部分」的中间态（每一步都是幂等 upsert，可安全重跑）。
func seedLocalAdminData(parent context.Context, config *conf.Data, legacyDatabase, samples string) (seedSummary, error) {
	summary := seedSummary{LegacySources: map[string]int64{}}
	if config == nil || config.GetMongo() == nil {
		return summary, errors.New("local MongoDB config is required")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(config.GetMongo().GetUri()))
	if err != nil {
		return summary, fmt.Errorf("connect local MongoDB: %w", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	ctx, cancel := context.WithTimeout(parent, localAdminFixtureTimeout)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		return summary, fmt.Errorf("ping local MongoDB: %w", err)
	}
	database := client.Database(config.GetMongo().GetDatabase())
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		return summary, fmt.Errorf("initialize local MongoDB schema: %w", err)
	}

	userIDs, err := seedReviewUserIDs(ctx, database)
	if err != nil {
		return summary, err
	}
	summary.UserIDs = userIDs

	if summary.PricingConfigs, err = upsertDocuments(ctx, database.Collection(schema.CollectionAdminPricingConfigs), pricingConfigDocuments()); err != nil {
		return summary, err
	}
	if summary.Apps, err = upsertDocuments(ctx, database.Collection(schema.CollectionApps), appDocuments()); err != nil {
		return summary, err
	}
	if summary.UTMLinks, err = upsertDocuments(ctx, database.Collection(schema.CollectionUTMLinks), utmLinkDocuments()); err != nil {
		return summary, err
	}

	legacy := client.Database(legacyDatabase)
	for source, documents := range legacyReviewDocuments(userIDs, samples == samplesWithSamples) {
		written, err := upsertDocuments(ctx, legacy.Collection(source), documents)
		if err != nil {
			return summary, err
		}
		summary.LegacySources[source] = written
	}
	if samples == samplesClean {
		if err := removeKnownFixtureSampleDocuments(ctx, legacy); err != nil {
			return summary, err
		}
	}
	return summary, nil
}

// seedReviewUserIDs 复用库里已有的真实用户 id，让审核列表能连带渲染出用户信息；
// 库里没有用户时退回固定 id，保证 fixture 在空库上也能独立跑通。
func seedReviewUserIDs(ctx context.Context, database *mongo.Database) ([]string, error) {
	cursor, err := database.Collection(schema.CollectionUsers).Find(ctx, bson.M{},
		options.Find().SetProjection(bson.M{"_id": 1}).SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(3))
	if err != nil {
		return nil, fmt.Errorf("list seed users: %w", err)
	}
	defer cursor.Close(ctx)
	ids := make([]string, 0, 3)
	for cursor.Next(ctx) {
		var row struct {
			ID string `bson:"_id"`
		}
		if err := cursor.Decode(&row); err != nil {
			return nil, fmt.Errorf("decode seed user: %w", err)
		}
		if row.ID != "" {
			ids = append(ids, row.ID)
		}
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate seed users: %w", err)
	}
	fallback := []string{"local-seed-user-1", "local-seed-user-2", "local-seed-user-3"}
	for len(ids) < 3 {
		ids = append(ids, fallback[len(ids)])
	}
	return ids, nil
}

// upsertDocuments 按 _id 幂等覆盖写入；_id 本身不参与 $set（Mongo 不允许改 _id）。
func upsertDocuments(ctx context.Context, collection *mongo.Collection, documents []bson.M) (int64, error) {
	if collection == nil {
		return 0, errors.New("mongo collection is required")
	}
	var written int64
	for _, document := range documents {
		id, exists := document["_id"]
		if !exists {
			return written, fmt.Errorf("seed document without _id：%v", document)
		}
		set := make(bson.M, len(document))
		for key, value := range document {
			if key != "_id" {
				set[key] = value
			}
		}
		if _, err := collection.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": set}, options.UpdateOne().SetUpsert(true)); err != nil {
			return written, fmt.Errorf("upsert seed document %v: %w", id, err)
		}
		written++
	}
	return written, nil
}

// pricingConfigDocuments 只写「覆盖值」，不复制内置目录 —— 内置套餐/权益仍是
// Go 侧 StaticCoinPackages / StaticVIPConfig 的事实来源，删掉这两条文档只是回到默认值。
//
// value 用原生 map[string]any 而不是 bson.M：bson.M 是具名类型，业务层的 asMap
// 只接受 map[string]any / map[string]string。生产路径之所以没问题，是因为
// data.normalizePricingBSONValue 会先把驱动解出的 bson.D/bson.M 递归归一；
// 种子直接写原生 map 就与归一后的形状一致，不需要绕过那一层。
func pricingConfigDocuments() []bson.M {
	coinOverrides := map[string]any{
		// 改价 + 改赠送：证明覆盖生效。
		"coins_1000": map[string]any{"priceInCents": 899, "bonusCoins": 350, "badge": "SEED"},
		// 停用一个小额包：证明 enabled:false 会从可售列表消失。
		"coins_200": map[string]any{"enabled": false},
		// 自定义套餐：证明 _custom 条目会被追加进目录。
		"coins_seed_777": map[string]any{
			"_custom": true, "coins": 7777, "bonusCoins": 777, "priceInCents": 4977,
			"label": map[string]any{"en": "Seed 777", "zh": "种子 777"}, "icon": "🎁", "enabled": true,
		},
	}
	return []bson.M{
		{
			"_id":         adminpricing.CoinPackagesKey + "|all",
			"key":         adminpricing.CoinPackagesKey,
			"value":       coinOverrides,
			"category":    "wallet",
			"description": "本地种子：套餐覆盖（改价 / 停用 / 自定义套餐）",
			"environment": "all",
			"enabled":     true,
			"updated_by":  "local-admin-fixture",
			"version":     int64(1),
			"created_at":  seedBaseTime,
			"updated_at":  seedBaseTime,
		},
		{
			"_id":         "wallet.firstRechargeBonusCoins|all",
			"key":         "wallet.firstRechargeBonusCoins",
			"value":       int64(120),
			"category":    "wallet",
			"description": "本地种子：首充奖励金币数",
			"environment": "all",
			"enabled":     true,
			"updated_by":  "local-admin-fixture",
			"version":     int64(1),
			"created_at":  seedBaseTime,
			"updated_at":  seedBaseTime,
		},
	}
}

// appDocuments 覆盖 web / ios / android 三个平台与 active / suspended 两种状态，
// 让 Apps 页面的平台分布与状态标签都有可渲染的真实行。
func appDocuments() []bson.M {
	stats := func(users, messages, revenue int64) bson.M {
		return bson.M{"totalUsers": users, "totalMessages": messages, "totalRevenue": revenue}
	}
	return []bson.M{
		{
			"_id": localSeedAppIDPrefix + "web", "name": "Seed Web Console", "platform": "web",
			"description": "本地种子：网页端接入应用", "domain": "seed-web.example.com",
			"apiUrl": "https://api.seed-web.example.com", "version": "1.4.0", "status": "active",
			"clientId": "seed-web-client", "iconUrl": "", "contact": "seed-web@example.com",
			"source": "local-seed", "stats": stats(128, 4096, 51200),
			"createdBy": "local-admin-fixture", "createdAt": seedBaseTime, "updatedAt": seedBaseTime,
		},
		{
			"_id": localSeedAppIDPrefix + "ios", "name": "Seed iOS App", "platform": "ios",
			"description": "本地种子：iOS 应用（含 bundleId / appKey）", "domain": "seed-ios.example.com",
			"bundleId": "com.example.seed.ios", "appKey": "seed-ios-app-key", "version": "2.1.0",
			"status": "active", "source": "local-seed", "stats": stats(64, 2048, 25600),
			"createdBy": "local-admin-fixture", "createdAt": seedBaseTime, "updatedAt": seedBaseTime,
		},
		{
			"_id": localSeedAppIDPrefix + "android", "name": "Seed Android App", "platform": "android",
			"description": "本地种子：Android 应用（已暂停，用于状态标签）", "domain": "seed-android.example.com",
			"packageName": "com.example.seed.android", "version": "1.0.3", "status": "suspended",
			"source": "local-seed", "stats": stats(32, 512, 6400),
			"createdBy": "local-admin-fixture", "createdAt": seedBaseTime, "updatedAt": seedBaseTime,
		},
	}
}

// utmLinkDocuments 的 _id 必须是 ObjectID：Go 侧的更新/删除会把路径参数按 hex 解析，
// 用字符串 _id 会让列表能读、详情写不了。
func utmLinkDocuments() []bson.M {
	lastClick := seedBaseTime.Add(48 * time.Hour)
	return []bson.M{
		{
			"_id": seededObjectID(9001), "slug": "seed-x-twitter",
			"label": "X / Twitter 导量", "targetPath": "/",
			"utmSource": "twitter", "utmMedium": "social", "utmCampaign": "seed_launch",
			"utmContent": "seed-post-a", "utmTerm": "", "enabled": true, "clicks": int64(1520),
			"notes": "本地种子：启用中的社媒导量", "extraParams": bson.M{"ref": "seed-a"},
			"lastClickAt": lastClick, "createdAt": seedBaseTime, "updatedAt": seedBaseTime,
		},
		{
			"_id": seededObjectID(9002), "slug": "seed-yt-review",
			"label": "YouTube 测评", "targetPath": "/pricing",
			"utmSource": "youtube", "utmMedium": "video", "utmCampaign": "seed_review",
			"enabled": true, "clicks": int64(312), "notes": "本地种子：指向定价页",
			"createdAt": seedBaseTime, "updatedAt": seedBaseTime,
		},
		{
			"_id": seededObjectID(9003), "slug": "seed-paused-banner",
			"label": "已停用横幅", "targetPath": "/", "utmSource": "banner", "utmMedium": "display",
			"utmCampaign": "seed_paused", "enabled": false, "clicks": int64(0),
			"notes":     "本地种子：停用条目（用于验证筛选器）",
			"createdAt": seedBaseTime, "updatedAt": seedBaseTime,
		},
	}
}

// legacyReviewDocuments 产出 4 个旧 Node 集合的种子。
//
// 只有 withSamples 为真时才追加 3 条**刻意畸形**的文档，用来演示导入器的
// orphan/conflict 分类：它们必然让 projection ready 保持 false（这是设计的门禁：
// 有未对账清楚的记录就不宣称投影可用）。先跑 with-samples 看对账报告，
// 再用 clean 覆盖回干净集合，才能真正导入。
func legacyReviewDocuments(userIDs []string, withSamples bool) map[string][]bson.M {
	user := func(index int) string { return userIDs[index%len(userIDs)] }
	day := func(offset int) time.Time { return seedBaseTime.AddDate(0, 0, offset) }

	images := []bson.M{
		{
			"_id": seededObjectID(101), "userId": user(0),
			"imageUrl": "https://pub-seed.r2.dev/ugc/seed/image-pending.png",
			"prompt":   "seed portrait, soft light", "negativePrompt": "blurry",
			"templateId": "tpl-seed-portrait", "templateTitle": "Seed Portrait",
			"status": "pending", "isPublic": false, "generationStatus": "completed",
			"createdAt": day(-6), "completedAt": day(-6), "updatedAt": day(-6),
		},
		{
			"_id": seededObjectID(102), "userId": user(1),
			"imageUrl": "https://pub-seed.r2.dev/ugc/seed/image-approved.png",
			"prompt":   "seed landscape, golden hour", "templateId": "tpl-seed-landscape",
			"templateTitle": "Seed Landscape", "status": "approved", "isPublic": true,
			"generationStatus": "completed", "reviewedBy": "local-admin-fixture",
			"createdAt": day(-5), "completedAt": day(-5), "updatedAt": day(-4),
		},
		{
			"_id": seededObjectID(103), "userId": user(2),
			"imageUrl": "https://pub-seed.r2.dev/ugc/seed/image-rejected.png",
			"prompt":   "seed rejected sample", "status": "rejected", "isPublic": false,
			"generationStatus": "completed", "reviewedBy": "local-admin-fixture",
			"rejectReason": "本地种子：演示拒绝原因展示",
			"createdAt":    day(-3), "completedAt": day(-3), "updatedAt": day(-2),
		},
		{
			// 老行没有 generationStatus：有真实输出即可推定成功，但不会被自动通过。
			"_id": seededObjectID(104), "userId": user(0),
			"imageUrl": "https://pub-seed.r2.dev/ugc/seed/image-legacy-row.png",
			"prompt":   "seed legacy row without generationStatus", "status": "active",
			"isPublic": true, "createdAt": day(-9), "updatedAt": day(-9),
		},
	}
	videos := []bson.M{
		{
			"_id": seededObjectID(201), "userId": user(0),
			"videoUrl": "https://pub-seed.r2.dev/videos/seed/video-pending.mp4",
			"prompt":   "seed camera pan", "templateId": "tpl-seed-video", "templateTitle": "Seed Video",
			"status": "pending", "isPublic": false, "generationStatus": "completed",
			"createdAt": day(-6), "completedAt": day(-6), "updatedAt": day(-6),
		},
		{
			"_id": seededObjectID(202), "userId": user(1),
			"videoUrl": "https://pub-seed.r2.dev/videos/seed/video-approved.mp4",
			"prompt":   "seed approved video", "status": "approved", "isPublic": true,
			"generationStatus": "completed", "createdAt": day(-4), "completedAt": day(-4), "updatedAt": day(-3),
		},
		{
			"_id": seededObjectID(203), "userId": user(2),
			"videoUrl": "https://pub-seed.r2.dev/videos/seed/video-hidden.mp4",
			"prompt":   "seed hidden video", "status": "hidden", "isPublic": false,
			"generationStatus": "completed", "createdAt": day(-2), "updatedAt": day(-2),
		},
	}
	animates := []bson.M{
		{
			// Animate 在旧 schema 里没有审核状态：一律导入为 pending，绝不自动通过。
			"_id": seededObjectID(301), "userId": user(0),
			"resultUrl": "https://pub-seed.r2.dev/animate/seed/animate-a.mp4",
			"prompt":    "seed animate a", "status": "completed", "isPublic": true,
			"createdAt": day(-8), "updatedAt": day(-8),
		},
		{
			"_id": seededObjectID(302), "userId": user(1),
			"resultUrl": "https://pub-seed.r2.dev/animate/seed/animate-b.mp4",
			"prompt":    "seed animate b", "status": "completed", "isPublic": false,
			"createdAt": day(-7), "updatedAt": day(-7),
		},
	}
	faceSwaps := []bson.M{
		{
			"_id": seededObjectID(401), "userId": user(0), "type": "video",
			"resultVideoUrl": "https://pub-seed.r2.dev/videos/seed/faceswap-video.mp4",
			"prompt":         "seed faceswap video", "status": "approved", "isPublic": true,
			"generationStatus": "completed", "createdAt": day(-5), "completedAt": day(-5), "updatedAt": day(-4),
		},
		{
			"_id": seededObjectID(402), "userId": user(1), "type": "image",
			"resultImageUrl": "https://pub-seed.r2.dev/ugc/seed/faceswap-image.png",
			"prompt":         "seed faceswap image", "status": "pending", "isPublic": false,
			"generationStatus": "completed", "createdAt": day(-1), "updatedAt": day(-1),
		},
	}
	if withSamples {
		// 2 条 orphan（缺必需证据）+ 1 条 conflict（状态不在契约内）。
		images = append(images, bson.M{
			"_id": seededObjectID(111), "userId": user(0),
			"prompt": "seed orphan: no imageUrl", "status": "pending",
			"createdAt": day(-1), "updatedAt": day(-1),
		}, bson.M{
			"_id": seededObjectID(112), "userId": user(1),
			"imageUrl": "https://pub-seed.r2.dev/ugc/seed/image-bad-status.png",
			"prompt":   "seed conflict: unsupported review status", "status": "awaiting_human",
			"createdAt": day(-1), "updatedAt": day(-1),
		})
		videos = append(videos, bson.M{
			"_id":      seededObjectID(211),
			"videoUrl": "https://pub-seed.r2.dev/videos/seed/video-orphan.mp4",
			"prompt":   "seed orphan: no userId", "status": "pending",
			"createdAt": day(-1), "updatedAt": day(-1),
		})
	}
	return map[string][]bson.M{
		adminreview.LegacyGeneratedImages: images,
		adminreview.LegacyGeneratedVideos: videos,
		adminreview.LegacyAnimates:        animates,
		adminreview.LegacyFaceSwapTasks:   faceSwaps,
	}
}

// fixtureSampleCleanupIDs lists only documents created by the with-samples
// branch.  Clean reruns delete these exact _ids one by one so a previous
// diagnostic run cannot keep the production projection fence closed.  Do not
// replace this with a collection-wide or predicate-based delete: legacy
// collections can contain non-fixture data.
func fixtureSampleCleanupIDs() map[string][]bson.ObjectID {
	return map[string][]bson.ObjectID{
		adminreview.LegacyGeneratedImages: {seededObjectID(111), seededObjectID(112)},
		adminreview.LegacyGeneratedVideos: {seededObjectID(211)},
	}
}

func removeKnownFixtureSampleDocuments(ctx context.Context, legacy *mongo.Database) error {
	for source, ids := range fixtureSampleCleanupIDs() {
		for _, id := range ids {
			if _, err := legacy.Collection(source).DeleteOne(ctx, bson.M{"_id": id}); err != nil {
				return fmt.Errorf("remove fixture sample %s/%s: %w", source, id.Hex(), err)
			}
		}
	}
	return nil
}

// seededObjectID 由固定 seed 构造确定性 ObjectID（时间戳段固定为 0x64b0，
// 其余字节来自 seed），避免手写 24 位 hex 时数错长度。同一个 seed 永远得到同一个 id，
// 因此重复运行 fixture 只会覆盖同一批文档。
func seededObjectID(seed uint64) bson.ObjectID {
	var raw [12]byte
	raw[0], raw[1] = 0x64, 0xb0
	binary.BigEndian.PutUint64(raw[4:], seed)
	return bson.ObjectID(raw[:])
}

func printSummary(writer io.Writer, summary seedSummary, legacyDatabase, samples string) {
	fmt.Fprintf(writer, "本地管理后台种子已写入（samples=%s）：\n", samples)
	fmt.Fprintf(writer, "  cling_main.admin_pricing_configs = %d\n", summary.PricingConfigs)
	fmt.Fprintf(writer, "  cling_main.apps                  = %d\n", summary.Apps)
	fmt.Fprintf(writer, "  cling_main.utmlinks              = %d\n", summary.UTMLinks)
	for _, source := range []string{
		adminreview.LegacyGeneratedImages,
		adminreview.LegacyGeneratedVideos,
		adminreview.LegacyAnimates,
		adminreview.LegacyFaceSwapTasks,
	} {
		fmt.Fprintf(writer, "  %s.%s = %d\n", legacyDatabase, source, summary.LegacySources[source])
	}
	fmt.Fprintln(writer, "审核投影未由本命令写入；下一步：")
	fmt.Fprintf(writer, "  1) dry-run 对账：admin-review-import --legacy-database %s --mode dry-run\n", legacyDatabase)
	fmt.Fprintf(writer, "  2) 确认干净后导入：admin-review-import --legacy-database %s --mode import --confirm-ready\n", legacyDatabase)
}
