package t2i

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/contentreview"
	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	bizt2i "ai-business-service/internal/biz/t2i"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	platform "ai-business-service/internal/integrations/generation"
	callbacktransport "ai-business-service/internal/transport/generationcallback"
	"ai-business-service/internal/transport/sessionauth"
	"ai-business-service/internal/worker"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	t2iE2ESigningKey = "t2i-e2e-signing-key-at-least-thirty-two-characters"
	t2iE2EResultURL  = "https://asset.invalid/t2i-e2e-result.png"
)

// TestHandler本地端到端创建预扣投递回调和状态读取 验证公开 T2I 的完整本地闭环。
// 测试不会启动 Worker，也不会向真实生成中台发送请求；投递状态由受控仓储调用模拟。
func TestHandler本地端到端创建预扣投递回调和状态读取(t *testing.T) {
	fixture := newT2IE2EMongoFixture(t)
	userID := uuid.NewString()
	sessionID := uuid.NewString()
	fixture.seedBoundSessionUser(userID, sessionID, 20)

	handler := NewHandler(fixture.authenticator, fixture.t2iUsecase, fixture.statusUsecase)
	requestID := uuid.NewString()
	createRecorder := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"prompt":"一只在山顶的猫","negativePrompt":"模糊","aspectRatio":"1:1"}`))
	createRequest.Header.Set("Authorization", "Bearer "+sessionID)
	createRequest.Header.Set("X-Request-Id", requestID)
	handler.ServeHTTP(createRecorder, createRequest)

	creationID, stepID := assertT2IE2ECreateResponse(t, createRecorder)
	fixture.trackCreationFacts(creationID, stepID)
	fixture.assertReservedFacts(userID, creationID, stepID)

	// 模拟 Worker 已向中台取得稳定任务 ID。该步骤不发 HTTP，也不启动后台轮询循环。
	eventID := outbox.SubmissionEventID(stepID)
	claimed, err := fixture.outbox.ClaimByIDAndType(fixture.ctx, "t2i-e2e", eventID, outbox.EventTypeGenerationSubmission, fixture.now, fixture.now.Add(time.Minute))
	if err != nil || claimed == nil || claimed.LeaseToken == "" {
		t.Fatalf("模拟领取 Outbox 事件失败：event=%#v err=%v", claimed, err)
	}
	jobID := "t2i-e2e-job-" + uuid.NewString()
	if err := fixture.tx.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.submissions.MarkSubmitted(txCtx, bizgeneration.SubmittedCommand{
			EventID: eventID, LeaseToken: claimed.LeaseToken, JobID: jobID, At: fixture.now,
		})
	}); err != nil {
		t.Fatalf("模拟标记生成中台已接收失败：%v", err)
	}

	// 只有 HMAC 验签通过的回调才能把任务置为完成并写入最终图片资产。
	nonce := "t2i-e2e-callback-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+sha256Hex(nonce))
	fixture.track(schema.CollectionAssets, "asset:"+stepID+":result")
	callbackRecorder := httptest.NewRecorder()
	fixture.callbackHandler.ServeHTTP(callbackRecorder, signedE2ECompletedCallbackRequest(
		t, stepID, jobID, nonce, fixture.now, "text_to_image", "ps-image-v1",
	))
	if callbackRecorder.Code != http.StatusOK || callbackRecorder.Body.String() != `{"ok":true}` {
		t.Fatalf("可信完成回调响应 = status:%d body:%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}

	assertT2IE2ECompletedStatus(t, handler, sessionID, creationID)
}

func TestHandler模板图编辑本地端到端闭环(t *testing.T) {
	fixture := newT2IE2EMongoFixture(t)
	userID, sessionID := uuid.NewString(), uuid.NewString()
	fixture.seedBoundSessionUser(userID, sessionID, 20)
	fixture.seedImageEditRecipe("dress-up")
	handler := NewHandler(fixture.authenticator, fixture.t2iUsecase, fixture.statusUsecase, fixture.imageEditUsecase)
	createRecorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"dress-up","inputImages":["https://uploads.example/source.png"],"prompt":"red dress"}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	request.Header.Set("X-Request-Id", uuid.NewString())
	handler.ServeHTTP(createRecorder, request)
	creationID, stepID := assertT2IE2ECreateResponse(t, createRecorder)
	fixture.trackCreationFacts(creationID, stepID)
	fixture.assertReservedFacts(userID, creationID, stepID)
	assertI2IOutboxTechnicalPayload(t, fixture, stepID)
	eventID := outbox.SubmissionEventID(stepID)
	claimed, err := fixture.outbox.ClaimByIDAndType(fixture.ctx, "i2i-e2e", eventID, outbox.EventTypeGenerationSubmission, fixture.now, fixture.now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("模拟领取 I2I Outbox：%v", err)
	}
	jobID := "i2i-e2e-job-" + uuid.NewString()
	if err := fixture.tx.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.submissions.MarkSubmitted(txCtx, bizgeneration.SubmittedCommand{EventID: eventID, LeaseToken: claimed.LeaseToken, JobID: jobID, At: fixture.now})
	}); err != nil {
		t.Fatalf("模拟 I2I 中台受理：%v", err)
	}
	nonce := "i2i-e2e-callback-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+sha256Hex(nonce))
	fixture.track(schema.CollectionAssets, "asset:"+stepID+":result")
	callbackRecorder := httptest.NewRecorder()
	// 回调携带的技术原子必须与已落库步骤一致，不能复用文生图的回调配方。
	fixture.callbackHandler.ServeHTTP(callbackRecorder, signedE2ECompletedCallbackRequest(
		t, stepID, jobID, nonce, fixture.now, "image_edit", "ps-edit-apparel-v1",
	))
	if callbackRecorder.Code != http.StatusOK {
		t.Fatalf("I2I 可信回调状态 = %d", callbackRecorder.Code)
	}
	assertT2IE2ECompletedStatus(t, handler, sessionID, creationID)
}

// T2I 已被中台受理后，可信失败回调必须自动冲正预扣，且相同回调重放不得二次退款。
func TestHandler文生图失败回调自动冲正(t *testing.T) {
	fixture := newT2IE2EMongoFixture(t)
	userID, sessionID := uuid.NewString(), uuid.NewString()
	fixture.seedBoundSessionUser(userID, sessionID, 20)
	handler := NewHandler(fixture.authenticator, fixture.t2iUsecase, fixture.statusUsecase)

	createRecorder := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"prompt":"一只在山顶的猫"}`))
	createRequest.Header.Set("Authorization", "Bearer "+sessionID)
	createRequest.Header.Set("X-Request-Id", uuid.NewString())
	handler.ServeHTTP(createRecorder, createRequest)
	creationID, stepID := assertT2IE2ECreateResponse(t, createRecorder)
	fixture.trackCreationFacts(creationID, stepID)

	jobID := fixture.markSubmitted(stepID, "t2i-failed")
	nonce := "t2i-e2e-failed-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+sha256Hex(nonce))
	callbackRecorder := httptest.NewRecorder()
	fixture.callbackHandler.ServeHTTP(callbackRecorder, signedE2EFailedCallbackRequest(stepID, jobID, nonce, fixture.now, "text_to_image", "ps-image-v1"))
	if callbackRecorder.Code != http.StatusOK {
		t.Fatalf("T2I 可信失败回调状态 = %d，body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
	replayRecorder := httptest.NewRecorder()
	fixture.callbackHandler.ServeHTTP(replayRecorder, signedE2EFailedCallbackRequest(stepID, jobID, nonce, fixture.now, "text_to_image", "ps-image-v1"))
	if replayRecorder.Code != http.StatusOK {
		t.Fatalf("T2I 失败回调重放状态 = %d，body=%s", replayRecorder.Code, replayRecorder.Body.String())
	}
	fixture.assertReversedAfterFailure(userID, creationID, handler, sessionID, "GENERATION_FAILED")
}

// 模板图编辑收到中台明确拒绝时，必须在同一本地事务内标记提交失败、冲正预扣并结案 Outbox。
// 重新投递已结案事件不能产生第二次中台调用或第二笔退款。
func TestHandler模板图编辑明确拒绝自动冲正(t *testing.T) {
	fixture := newT2IE2EMongoFixture(t)
	userID, sessionID, templateID := uuid.NewString(), uuid.NewString(), "i2i-rejected"
	fixture.seedBoundSessionUser(userID, sessionID, 20)
	fixture.seedImageEditRecipe(templateID)
	handler := NewHandler(fixture.authenticator, fixture.t2iUsecase, fixture.statusUsecase, fixture.imageEditUsecase)

	createRecorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"`+templateID+`","inputImages":["https://uploads.example/source.png"],"prompt":"red dress"}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	request.Header.Set("X-Request-Id", uuid.NewString())
	handler.ServeHTTP(createRecorder, request)
	creationID, stepID := assertT2IE2ECreateResponse(t, createRecorder)
	fixture.trackCreationFacts(creationID, stepID)

	client := &t2iE2ENeverSubmitClient{}
	submitter := worker.NewGenerationSubmissionWorker(
		fixture.outbox, fixture.submissions, fixture.ledger, fixture.tx, client,
		nil, "t2i-e2e-rejected", func() time.Time { return fixture.now },
	)
	eventID := outbox.SubmissionEventID(stepID)
	if err := submitter.DeliverOnce(fixture.ctx, eventID); err != nil {
		t.Fatalf("执行 I2I 明确拒绝提交器：%v", err)
	}
	if err := submitter.DeliverOnce(fixture.ctx, eventID); err != nil {
		t.Fatalf("重放 I2I 明确拒绝提交器：%v", err)
	}
	if client.calls != 1 {
		t.Fatalf("I2I 明确拒绝的中台调用次数 = %d，期望 1", client.calls)
	}
	fixture.track(schema.CollectionLedgerEntries, "reverse:"+creationID)
	fixture.assertReversedAfterFailure(userID, creationID, handler, sessionID, "GENERATION_SUBMISSION_FAILED")
	fixture.assertOutboxFailed(eventID)
}

// 模板图编辑在审核受限用户被拒绝时，必须在请求生成中台前没收预扣而非退款。
func TestHandler模板图编辑审核没收不退款(t *testing.T) {
	fixture := newT2IE2EMongoFixture(t)
	userID, sessionID, templateID := uuid.NewString(), uuid.NewString(), "i2i-confiscated"
	fixture.seedBoundSessionUser(userID, sessionID, 20)
	if _, err := fixture.database.Collection(schema.CollectionUsers).UpdateOne(fixture.ctx, bson.D{{Key: "_id", Value: userID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "content_access", Value: identity.ContentAccessReviewRestricted}}}}); err != nil {
		t.Fatalf("设置审核受限用户：%v", err)
	}
	fixture.seedImageEditRecipe(templateID)
	handler := NewHandler(fixture.authenticator, fixture.t2iUsecase, fixture.statusUsecase, fixture.imageEditUsecase)

	createRecorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"`+templateID+`","inputImages":["https://uploads.example/source.png"],"prompt":"red dress"}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	request.Header.Set("X-Request-Id", uuid.NewString())
	handler.ServeHTTP(createRecorder, request)
	creationID, stepID := assertT2IE2ECreateResponse(t, createRecorder)
	fixture.trackCreationFacts(creationID, stepID)

	client := &t2iE2ENeverSubmitClient{}
	submitter := worker.NewGenerationSubmissionWorker(
		fixture.outbox, fixture.submissions, fixture.ledger, fixture.tx, client,
		t2iE2ERejectedReviewer{}, "t2i-e2e-review", func() time.Time { return fixture.now },
	)
	if err := submitter.DeliverOnce(fixture.ctx, outbox.SubmissionEventID(stepID)); err != nil {
		t.Fatalf("执行 I2I 审核提交器：%v", err)
	}
	if client.calls != 0 {
		t.Fatalf("I2I 审核拒绝后仍请求生成中台 %d 次", client.calls)
	}
	fixture.assertConfiscatedWithoutRefund(userID, creationID, handler, sessionID)
}

func assertT2IE2ECreateResponse(t *testing.T, recorder *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("创建响应状态 = %d，body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			ImageID        string `json:"imageId"`
			JobID          string `json:"jobId"`
			Status         string `json:"status"`
			Cost           int64  `json:"cost"`
			CoinsCharged   int64  `json:"coinsCharged"`
			CoinsRemaining int64  `json:"coinsRemaining"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析创建响应：%v", err)
	}
	if !response.Success || response.Data.Status != "generating" || response.Data.Cost != 20 || response.Data.CoinsCharged != 20 || response.Data.CoinsRemaining != 0 {
		t.Fatalf("创建响应投影错误：%s", recorder.Body.String())
	}
	if _, err := uuid.Parse(response.Data.ImageID); err != nil {
		t.Fatalf("imageId 不是 Go UUID：%q", response.Data.ImageID)
	}
	if _, err := uuid.Parse(response.Data.JobID); err != nil {
		t.Fatalf("jobId 不是内部步骤 UUID：%q", response.Data.JobID)
	}
	return response.Data.ImageID, response.Data.JobID
}

func assertT2IE2ECompletedStatus(t *testing.T, handler http.Handler, sessionID, creationID string) {
	t.Helper()
	statusesRecorder := httptest.NewRecorder()
	statusesRequest := httptest.NewRequest(http.MethodPost, statusesPath, strings.NewReader(`{"imageIds":["`+creationID+`"]}`))
	statusesRequest.Header.Set("Authorization", "Bearer "+sessionID)
	handler.ServeHTTP(statusesRecorder, statusesRequest)
	assertT2IE2EStatusBody(t, statusesRecorder, "批量状态")

	detailRecorder := httptest.NewRecorder()
	detailRequest := httptest.NewRequest(http.MethodGet, detailPathPrefix+creationID, nil)
	detailRequest.Header.Set("Authorization", "Bearer "+sessionID)
	handler.ServeHTTP(detailRecorder, detailRequest)
	assertT2IE2EStatusBody(t, detailRecorder, "单条状态")
}

func assertT2IE2EStatusBody(t *testing.T, recorder *httptest.ResponseRecorder, name string) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s响应状态 = %d，body=%s", name, recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"generationStatus":"completed"`) || !strings.Contains(recorder.Body.String(), `"imageUrl":"`+t2iE2EResultURL+`"`) {
		t.Fatalf("%s未返回可信完成图片：%s", name, recorder.Body.String())
	}
}

type t2iE2EMongoFixture struct {
	t                *testing.T
	ctx              context.Context
	database         *mongo.Database
	now              time.Time
	tx               shared.TxRunner
	outbox           outbox.Repository
	submissions      bizgeneration.SubmissionStore
	ledger           *ledger.Usecase
	authenticator    *sessionauth.Authenticator
	t2iUsecase       *bizt2i.Usecase
	imageEditUsecase *bizt2i.ImageEditUsecase
	statusUsecase    *bizt2i.StatusUsecase
	callbackHandler  http.Handler
	cleanup          []t2iE2EDocument
}

type t2iE2EDocument struct {
	collection string
	id         string
}

func newT2IE2EMongoFixture(t *testing.T) *t2iE2EMongoFixture {
	t.Helper()
	uri := os.Getenv("CLING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("未配置本机 rs0 测试连接")
	}
	config := &conf.Data{Mongo: &conf.Data_Mongo{Uri: uri, Database: "cling_main", ReplicaSet: "rs0", TransactionsRequired: true}}
	if err := conf.ValidateLocalMongo(config); err != nil {
		t.Fatalf("本地 MongoDB 配置不符合 rs0 约束：%v", err)
	}
	rawClient, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("连接本机 MongoDB：%v", err)
	}
	t.Cleanup(func() { _ = rawClient.Disconnect(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	if err := rawClient.Ping(ctx, nil); err != nil {
		t.Fatalf("本机 MongoDB 不可用：%v", err)
	}
	storage, closeStorage, err := data.NewData(config)
	if err != nil {
		t.Fatalf("构造本机数据层：%v", err)
	}
	t.Cleanup(closeStorage)
	database := rawClient.Database("cling_main")
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本机 schema：%v", err)
	}

	now := time.Date(2026, time.September, 8, 10, 0, 0, 0, time.UTC)
	tx := data.NewTxRunner(storage)
	ledgerUsecase := ledger.NewUsecaseWithClock(data.NewLedgerRepository(storage), tx, func() time.Time { return now })
	creationUsecase := creations.NewUsecaseWithClock(
		data.NewUserRepository(storage), data.NewSubscriptionRepository(storage), entitlement.NewUsecase(),
		data.NewCreationRepository(storage), ledgerUsecase, data.NewOutboxRepository(storage), tx, func() time.Time { return now },
	)
	identityUsecase := identity.NewUsecase(
		data.NewUserRepository(storage), data.NewIdentityRepository(storage), data.NewSessionRepository(storage), tx,
	)
	callbackStore := data.NewGenerationCallbackRepository(storage)
	// Mongo 回调仓储同时实现终态落库与已持久化步骤关联两个窄接口。
	callbackUsecase := bizgeneration.NewCallbackUsecaseWithClock(callbackStore, callbackStore, ledgerUsecase, tx, func() time.Time { return now })
	verifier, err := platform.NewCallbackVerifier(t2iE2ESigningKey, func() time.Time { return now })
	if err != nil {
		t.Fatalf("创建本地回调验签器：%v", err)
	}
	fixture := &t2iE2EMongoFixture{
		t: t, ctx: ctx, database: database, now: now, tx: tx,
		outbox: data.NewOutboxRepository(storage), submissions: data.NewGenerationSubmissionRepository(storage), ledger: ledgerUsecase,
		authenticator: sessionauth.NewAuthenticatorWithClock(identityUsecase, func() time.Time { return now }),
		// T2I 配方持久化由 data 层集成测试覆盖；此处固定技术配方，
		// 避免端到端测试与正在运行的本地 fixture 争用生产固定模板记录。
		t2iUsecase: bizt2i.NewUsecase(staticFreeformRecipeReader{recipe: localT2IFreeformRecipe()}, creationUsecase),
		// 端到端测试只验证预扣、Outbox 与可信回调链；素材领域的真实 Mongo 所有权
		// 已由 data/transport media 测试覆盖，因此此处使用固定的已确认素材读取器隔离关注点。
		imageEditUsecase: bizt2i.NewImageEditUsecase(data.NewImageEditRecipeRepository(storage), creationUsecase, staticOwnedImageReader{}),
		statusUsecase:    bizt2i.NewStatusUsecase(data.NewImageStatusRepository(storage)),
		callbackHandler:  callbacktransport.NewHandler(verifier, callbackUsecase),
	}
	t.Cleanup(fixture.deleteTracked)
	return fixture
}

// staticFreeformRecipeReader 为 HTTP 端到端测试提供已冻结的服务端技术配方。
// 它不模拟创作、账本或回调链路，相关数据仍由真实 MongoDB 仓储读写。
type staticFreeformRecipeReader struct {
	recipe bizt2i.FreeformRecipe
	err    error
}

type staticOwnedImageReader struct{}

func (staticOwnedImageReader) ResolveOwnedImage(_ context.Context, _ string, reference string) (string, error) {
	return reference, nil
}

func (reader staticFreeformRecipeReader) LoadFreeformRecipe(context.Context) (bizt2i.FreeformRecipe, error) {
	return reader.recipe, reader.err
}

func localT2IFreeformRecipe() bizt2i.FreeformRecipe {
	return bizt2i.FreeformRecipe{
		// 业务层固定校验该服务端内部模板标识，保持与生产配方一致。
		TemplateID: "t2i-freeform",
		Version:    1,
		ModelSKU:   "ps-image-v1",
		Parameters: json.RawMessage(`{"steps":28}`),
	}
}

func (fixture *t2iE2EMongoFixture) seedBoundSessionUser(userID, sessionID string, balance int64) {
	fixture.track(schema.CollectionUsers, userID)
	fixture.track(schema.CollectionSessions, sessionID)
	fixture.track(schema.CollectionAccounts, userID)
	user := model.UserDocument{ID: userID, AccountStatus: string(identity.AccountStatusNormal), BindingState: string(identity.BindingStateBound), Timezone: "Asia/Shanghai", SessionVersion: 1, ContentAccess: string(identity.ContentAccessStandard), CreatedAt: fixture.now, UpdatedAt: fixture.now}
	if _, err := fixture.database.Collection(schema.CollectionUsers).InsertOne(fixture.ctx, user); err != nil {
		fixture.t.Fatalf("写入本地测试用户：%v", err)
	}
	session := model.SessionDocument{ID: sessionID, UserID: userID, SessionVersion: 1, ExpiresAt: fixture.now.Add(time.Hour)}
	if _, err := fixture.database.Collection(schema.CollectionSessions).InsertOne(fixture.ctx, session); err != nil {
		fixture.t.Fatalf("写入本地测试会话：%v", err)
	}
	if _, err := fixture.database.Collection(schema.CollectionAccounts).InsertOne(fixture.ctx, model.AccountDocument{ID: userID, DiamondBalance: balance, CreatedAt: fixture.now, UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatalf("写入本地测试账户：%v", err)
	}
}

// seedImageEditRecipe 写入仅供本地端到端测试使用的 SFW 模板编辑配方。
func (fixture *t2iE2EMongoFixture) seedImageEditRecipe(templateID string) {
	id := uuid.NewString()
	fixture.track(schema.CollectionTemplates, id)
	parameters, err := bson.Marshal(bson.M{
		"kind": "image_edit", "model_sku": "ps-edit-apparel-v1", "prompt": "模板提示词", "negative_prompt": "模糊",
		"parameters": bson.M{"steps": 28}, "input_rule": bson.M{"user_image_count": 1, "user_role": "source_image"},
		"reference_assets": bson.A{bson.M{"role": "guide_image", "url": "https://assets.example/guide.png"}},
	})
	if err != nil {
		fixture.t.Fatal(err)
	}
	document := model.TemplateDocument{ID: id, TemplateID: templateID, Version: 1, ContentSurface: string(catalog.ContentSurfaceSFW), Mode: string(catalog.ProductModeTemplateImage), Enabled: true, Parameters: parameters, CreatedAt: fixture.now, UpdatedAt: fixture.now}
	if _, err := fixture.database.Collection(schema.CollectionTemplates).InsertOne(fixture.ctx, document); err != nil {
		fixture.t.Fatalf("写入本地 I2I 配方：%v", err)
	}
}

func (fixture *t2iE2EMongoFixture) trackCreationFacts(creationID, stepID string) {
	fixture.track(schema.CollectionCreations, creationID)
	fixture.track(schema.CollectionCreationSteps, stepID)
	fixture.track(schema.CollectionReservations, "reservation:"+creationID)
	fixture.track(schema.CollectionLedgerEntries, "reserve:"+creationID)
	fixture.track(schema.CollectionOutboxEvents, outbox.SubmissionEventID(stepID))
}

func (fixture *t2iE2EMongoFixture) assertReservedFacts(userID, creationID, stepID string) {
	var reservation model.ReservationDocument
	if err := fixture.database.Collection(schema.CollectionReservations).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: "reservation:" + creationID}}).Decode(&reservation); err != nil || reservation.Status != string(ledger.ReservationStatusReserved) || reservation.PriceDiamonds != 20 || reservation.ReservedDiamonds != 20 {
		fixture.t.Fatalf("预扣事实错误：reservation=%#v err=%v", reservation, err)
	}
	var entry model.LedgerEntryDocument
	if err := fixture.database.Collection(schema.CollectionLedgerEntries).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: "reserve:" + creationID}}).Decode(&entry); err != nil || entry.DeltaDiamonds != -20 || entry.CreationID != creationID {
		fixture.t.Fatalf("预扣账本事实错误：entry=%#v err=%v", entry, err)
	}
	var event model.OutboxEventDocument
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(stepID)}}).Decode(&event); err != nil || event.DeliveryStatus != string(outbox.DeliveryStatusPending) || event.EventType != string(outbox.EventTypeGenerationSubmission) {
		fixture.t.Fatalf("Outbox 事实错误：event=%#v err=%v", event, err)
	}
	var account model.AccountDocument
	if err := fixture.database.Collection(schema.CollectionAccounts).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account); err != nil || account.DiamondBalance != 0 {
		fixture.t.Fatalf("预扣后账户余额错误：account=%#v err=%v", account, err)
	}
}

// markSubmitted 模拟中台已确认受理；它只驱动本地已冻结的 Outbox 状态，不发任何 HTTP 请求。
func (fixture *t2iE2EMongoFixture) markSubmitted(stepID, prefix string) string {
	fixture.t.Helper()
	eventID := outbox.SubmissionEventID(stepID)
	claimed, err := fixture.outbox.ClaimByIDAndType(fixture.ctx, prefix, eventID, outbox.EventTypeGenerationSubmission, fixture.now, fixture.now.Add(time.Minute))
	if err != nil || claimed == nil {
		fixture.t.Fatalf("领取 %s Outbox：event=%#v err=%v", prefix, claimed, err)
	}
	jobID := prefix + "-job-" + uuid.NewString()
	if err := fixture.tx.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.submissions.MarkSubmitted(txCtx, bizgeneration.SubmittedCommand{EventID: eventID, LeaseToken: claimed.LeaseToken, JobID: jobID, At: fixture.now})
	}); err != nil {
		fixture.t.Fatalf("标记 %s 已提交：%v", prefix, err)
	}
	return jobID
}

// assertReversedAfterFailure 验证可信技术失败回调的预留、余额和账本只收敛一次。
func (fixture *t2iE2EMongoFixture) assertReversedAfterFailure(userID, creationID string, handler http.Handler, sessionID, errorCode string) {
	fixture.t.Helper()
	fixture.track(schema.CollectionLedgerEntries, "reverse:"+creationID)
	var reservation model.ReservationDocument
	if err := fixture.database.Collection(schema.CollectionReservations).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: "reservation:" + creationID}}).Decode(&reservation); err != nil || reservation.Status != string(ledger.ReservationStatusReversed) {
		fixture.t.Fatalf("失败回调后的预留 = %#v，err=%v", reservation, err)
	}
	var account model.AccountDocument
	if err := fixture.database.Collection(schema.CollectionAccounts).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account); err != nil || account.DiamondBalance != 20 {
		fixture.t.Fatalf("失败回调后的余额 = %#v，err=%v", account, err)
	}
	entries, err := fixture.database.Collection(schema.CollectionLedgerEntries).CountDocuments(fixture.ctx, bson.D{{Key: "creation_id", Value: creationID}})
	if err != nil || entries != 2 {
		fixture.t.Fatalf("失败回调后的创作账本分录数 = %d，err=%v，期望 2", entries, err)
	}
	fixture.assertFailedStatus(handler, sessionID, creationID, errorCode)
}

// assertOutboxFailed 验证明确拒绝不保留可重复领取的事件，防止后续 Worker 再次冲正。
func (fixture *t2iE2EMongoFixture) assertOutboxFailed(eventID string) {
	fixture.t.Helper()
	var event model.OutboxEventDocument
	err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&event)
	if err != nil || event.DeliveryStatus != string(outbox.DeliveryStatusFailed) {
		fixture.t.Fatalf("明确拒绝后的 Outbox = %#v，err=%v，期望 failed", event, err)
	}
}

// assertConfiscatedWithoutRefund 验证审核拒绝只迁移预留状态，不能退款或追加退款账本。
func (fixture *t2iE2EMongoFixture) assertConfiscatedWithoutRefund(userID, creationID string, handler http.Handler, sessionID string) {
	fixture.t.Helper()
	var reservation model.ReservationDocument
	if err := fixture.database.Collection(schema.CollectionReservations).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: "reservation:" + creationID}}).Decode(&reservation); err != nil || reservation.Status != string(ledger.ReservationStatusConfiscated) {
		fixture.t.Fatalf("审核没收后的预留 = %#v，err=%v", reservation, err)
	}
	var account model.AccountDocument
	if err := fixture.database.Collection(schema.CollectionAccounts).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account); err != nil || account.DiamondBalance != 0 {
		fixture.t.Fatalf("审核没收后的余额 = %#v，err=%v", account, err)
	}
	entries, err := fixture.database.Collection(schema.CollectionLedgerEntries).CountDocuments(fixture.ctx, bson.D{{Key: "creation_id", Value: creationID}})
	if err != nil || entries != 1 {
		fixture.t.Fatalf("审核没收后的创作账本分录数 = %d，err=%v，期望 1", entries, err)
	}
	fixture.assertFailedStatus(handler, sessionID, creationID, "CONTENT_CONFISCATED")
}

// assertFailedStatus 同时覆盖既有图片批量与单条查询的失败投影合同。
func (fixture *t2iE2EMongoFixture) assertFailedStatus(handler http.Handler, sessionID, creationID, errorCode string) {
	fixture.t.Helper()
	for _, request := range []*http.Request{httptest.NewRequest(http.MethodPost, statusesPath, strings.NewReader(`{"imageIds":["`+creationID+`"]}`)), httptest.NewRequest(http.MethodGet, detailPathPrefix+creationID, nil)} {
		request.Header.Set("Authorization", "Bearer "+sessionID)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"generationStatus":"failed"`) || !strings.Contains(recorder.Body.String(), `"generationErrorCode":"`+errorCode+`"`) {
			fixture.t.Fatalf("失败状态 = %d，body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

// assertI2IOutboxTechnicalPayload 锁定发给生成中台前的载荷边界。
// Outbox 只允许携带能力、模型和已冻结技术输入，严禁混入用户、模板、权益、账本或支付事实。
func assertI2IOutboxTechnicalPayload(t *testing.T, fixture *t2iE2EMongoFixture, stepID string) {
	t.Helper()
	var event model.OutboxEventDocument
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(
		fixture.ctx, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(stepID)}},
	).Decode(&event); err != nil {
		t.Fatalf("读取 I2I Outbox 载荷：%v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("解析 I2I Outbox 载荷：%v", err)
	}
	if len(payload) != 3 || payload["capability"] == nil || payload["model_sku"] == nil || payload["input"] == nil {
		t.Fatalf("I2I Outbox 顶层字段必须仅为 capability/model_sku/input：%s", event.Payload)
	}
	var capability, modelSKU string
	if err := json.Unmarshal(payload["capability"], &capability); err != nil || capability != "image_edit" {
		t.Fatalf("I2I Outbox capability = %q，err=%v", capability, err)
	}
	if err := json.Unmarshal(payload["model_sku"], &modelSKU); err != nil || modelSKU != "ps-edit-apparel-v1" {
		t.Fatalf("I2I Outbox model_sku = %q，err=%v", modelSKU, err)
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(payload["input"], &input); err != nil {
		t.Fatalf("解析 I2I Outbox 技术输入：%v", err)
	}
	allowedInputFields := map[string]struct{}{"prompt": {}, "negativePrompt": {}, "assets": {}, "parameters": {}}
	if len(input) != len(allowedInputFields) {
		t.Fatalf("I2I Outbox input 字段数量 = %d，期望 %d：%s", len(input), len(allowedInputFields), payload["input"])
	}
	for field := range input {
		if _, allowed := allowedInputFields[field]; !allowed {
			t.Fatalf("I2I Outbox input 含有非技术字段 %q：%s", field, payload["input"])
		}
	}
}

func (fixture *t2iE2EMongoFixture) track(collection, id string) {
	fixture.cleanup = append(fixture.cleanup, t2iE2EDocument{collection: collection, id: id})
}

func (fixture *t2iE2EMongoFixture) deleteTracked() {
	for index := len(fixture.cleanup) - 1; index >= 0; index-- {
		document := fixture.cleanup[index]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := fixture.database.Collection(document.collection).DeleteOne(ctx, bson.D{{Key: "_id", Value: document.id}})
		cancel()
		if err != nil {
			fixture.t.Errorf("按测试 _id 清理集合 %q 中的 %q：%v", document.collection, document.id, err)
		}
	}
}

// signedE2ECompletedCallbackRequest 构造经过 HMAC 签名的完成回调。
// capability 与 modelSKU 显式传入，确保每个技术原子的回调都与其持久化步骤严格对应。
func signedE2ECompletedCallbackRequest(t *testing.T, stepID, jobID, nonce string, at time.Time, capability, modelSKU string) *http.Request {
	t.Helper()
	body := []byte(`{"contractVersion":"execution.callback.v2","eventId":"` + strings.Repeat("a", 64) + `","deliveryId":"t2i-e2e-delivery","fingerprint":"` + strings.Repeat("a", 64) + `","eventType":"execution.completed","jobId":"` + jobID + `","externalRef":"` + stepID + `","status":"completed","outputs":[{"role":"result","mediaType":"image","url":"` + t2iE2EResultURL + `"}],"usage":{},"timestamps":{},"executionRef":{"tenantId":"cling-ai","tenantSignupSource":"internal","capability":"` + capability + `","modelSku":"` + modelSKU + `"},"metadata":{}}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/internal/generation-callback", bytes.NewReader(body))
	timestamp := strconv.FormatInt(at.UnixMilli(), 10)
	digest := sha256.Sum256(body)
	payload := strings.Join([]string{"generation-callback-v2", timestamp, http.MethodPost, request.URL.Path, nonce, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(t2iE2ESigningKey))
	_, _ = mac.Write([]byte(payload))
	request.Header.Set("X-Generation-Callback-Version", "2")
	request.Header.Set("X-Generation-Callback-Timestamp", timestamp)
	request.Header.Set("X-Generation-Callback-Nonce", nonce)
	request.Header.Set("X-Generation-Callback-Signature", hex.EncodeToString(mac.Sum(nil)))
	return request
}

// signedE2EFailedCallbackRequest 构造已签名的技术失败回调；上游错误文本不会进入业务断言。
func signedE2EFailedCallbackRequest(stepID, jobID, nonce string, at time.Time, capability, modelSKU string) *http.Request {
	body := []byte(`{"contractVersion":"execution.callback.v2","eventId":"` + strings.Repeat("a", 64) + `","deliveryId":"t2i-e2e-failed-delivery","fingerprint":"` + strings.Repeat("a", 64) + `","eventType":"execution.failed","jobId":"` + jobID + `","externalRef":"` + stepID + `","status":"failed","outputs":[],"error":{"code":"UPSTREAM_FAILED","message":"upstream-error-secret"},"usage":{},"timestamps":{},"executionRef":{"tenantId":"cling-ai","tenantSignupSource":"internal","capability":"` + capability + `","modelSku":"` + modelSKU + `"},"metadata":{}}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/internal/generation-callback", bytes.NewReader(body))
	timestamp := strconv.FormatInt(at.UnixMilli(), 10)
	digest := sha256.Sum256(body)
	payload := strings.Join([]string{"generation-callback-v2", timestamp, http.MethodPost, request.URL.Path, nonce, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(t2iE2ESigningKey))
	_, _ = mac.Write([]byte(payload))
	request.Header.Set("X-Generation-Callback-Version", "2")
	request.Header.Set("X-Generation-Callback-Timestamp", timestamp)
	request.Header.Set("X-Generation-Callback-Nonce", nonce)
	request.Header.Set("X-Generation-Callback-Signature", hex.EncodeToString(mac.Sum(nil)))
	return request
}

// t2iE2ERejectedReviewer 固定返回审核拒绝，用于验证真实本地 Mongo 没收事务。
type t2iE2ERejectedReviewer struct{}

func (t2iE2ERejectedReviewer) Review(context.Context, contentreview.Request) (contentreview.Decision, error) {
	return contentreview.Decision{Outcome: contentreview.OutcomeRejected}, nil
}

// t2iE2ENeverSubmitClient 记录中台调用；审核拒绝时 Submit 不得被触达。
type t2iE2ENeverSubmitClient struct{ calls int }

func (client *t2iE2ENeverSubmitClient) Submit(context.Context, platform.Execution) (platform.SubmissionResult, error) {
	client.calls++
	return platform.SubmissionResult{}, platform.ErrRejected
}

func (*t2iE2ENeverSubmitClient) Lookup(context.Context, string) (platform.LookupResult, error) {
	return platform.LookupResult{}, platform.ErrRejected
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
