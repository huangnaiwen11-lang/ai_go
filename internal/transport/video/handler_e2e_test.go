package video

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
	bizvideo "ai-business-service/internal/biz/video"
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
	videoE2ESigningKey = "video-e2e-signing-key-at-least-thirty-two-characters"
	videoE2EFrameURL   = "https://assets.example.test/video-e2e-frame.png"
	videoE2EResultURL  = "https://assets.example.test/video-e2e-result.mp4"
)

// 该端到端套件的重点是预扣、Outbox 和回调收敛；素材所有权由 media/data 集成测试
// 覆盖，因此这里用已确认素材读取器隔离测试关注点。
type staticOwnedImageReader struct{}

func (staticOwnedImageReader) ResolveOwnedImage(_ context.Context, _ string, reference string) (string, error) {
	return reference, nil
}

// 单图路径必须在预扣后只投递一个 I2V 事件，且错误技术原子的可信回调不得改变任务。
func TestHandler视频模板单图I2V本地端到端闭环(t *testing.T) {
	fixture := newVideoE2EFixture(t)
	userID, sessionID, templateID := uuid.NewString(), uuid.NewString(), "video-image-"+uuid.NewString()
	fixture.seedBoundUser(userID, sessionID, 50)
	fixture.seedTemplate(templateID)
	handler := NewHandler(fixture.authenticator, fixture.creator, fixture.statuses)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"`+templateID+`","imageUrl":"https://uploads.example.test/source.png","durationSeconds":5}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	request.Header.Set("X-Request-Id", uuid.NewString())
	handler.ServeHTTP(recorder, request)
	creationID := fixture.assertCreated(t, recorder, 5)
	steps := fixture.trackCreation(creationID)
	if len(steps) != 1 || steps[0].Atom != string(creations.AtomImageToVideo) {
		t.Fatalf("单图步骤 = %#v", steps)
	}
	fixture.assertTechnicalOutbox(steps[0].ID, "image_to_video")
	jobID := fixture.markSubmitted(steps[0].ID)

	// 已提交 I2V 步骤不能被伪造成 image_edit 回调完成。
	wrong := httptest.NewRecorder()
	fixture.callback.ServeHTTP(wrong, fixture.signedCompleted(steps[0].ID, jobID, "video-e2e-invalid-"+uuid.NewString(), "image_edit", "ps-edit-v1", "image", videoE2EFrameURL))
	if wrong.Code == http.StatusOK {
		t.Fatal("错误能力的回调不应确认图生视频任务")
	}
	fixture.complete(steps[0].ID, jobID, "image_to_video", "ps-auto", "video", videoE2EResultURL)
	fixture.assertCompletedStatus(handler, sessionID, creationID)
}

// 文本路径必须先确认首帧，才原子激活唯一的第二个 I2V Outbox，之后才能完成视频任务。
func TestHandler视频模板文本首帧到I2V本地端到端闭环(t *testing.T) {
	fixture := newVideoE2EFixture(t)
	userID, sessionID, templateID := uuid.NewString(), uuid.NewString(), "video-text-"+uuid.NewString()
	fixture.seedBoundUser(userID, sessionID, 50)
	fixture.seedTemplate(templateID)
	handler := NewHandler(fixture.authenticator, fixture.creator, fixture.statuses)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"`+templateID+`","prompt":"海边漫步","durationSeconds":5}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	request.Header.Set("X-Request-Id", uuid.NewString())
	handler.ServeHTTP(recorder, request)
	creationID := fixture.assertCreated(t, recorder, 5)
	steps := fixture.trackCreation(creationID)
	if len(steps) != 2 || steps[0].Atom != string(creations.AtomTextToImage) || steps[1].Atom != string(creations.AtomImageToVideo) || steps[1].SubmitStatus != string(creations.StepSubmitStatusBlocked) {
		t.Fatalf("文本两步骤 = %#v", steps)
	}
	fixture.track(schema.CollectionGenerationStepRecipes, steps[1].ID)
	fixture.assertTechnicalOutbox(steps[0].ID, "text_to_image")
	firstJobID := fixture.markSubmitted(steps[0].ID)
	fixture.complete(steps[0].ID, firstJobID, "text_to_image", "ps-image-v1", "image", videoE2EFrameURL)
	fixture.track(schema.CollectionAssets, "asset:"+steps[0].ID+":result")
	fixture.track(schema.CollectionOutboxEvents, outbox.SubmissionEventID(steps[1].ID))
	fixture.assertTechnicalOutbox(steps[1].ID, "image_to_video")
	secondJobID := fixture.markSubmitted(steps[1].ID)
	fixture.complete(steps[1].ID, secondJobID, "image_to_video", "ps-auto", "video", videoE2EResultURL)
	fixture.assertCompletedStatus(handler, sessionID, creationID)
}

// I2V 已被中台受理后若通过可信失败回调结束，必须自动冲正本次预扣，且重复回调不能二次退款。
func TestHandler视频模板I2V失败回调自动冲正(t *testing.T) {
	fixture := newVideoE2EFixture(t)
	userID, sessionID, templateID := uuid.NewString(), uuid.NewString(), "video-failed-"+uuid.NewString()
	fixture.seedBoundUser(userID, sessionID, 50)
	fixture.seedTemplate(templateID)
	handler := NewHandler(fixture.authenticator, fixture.creator, fixture.statuses)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"`+templateID+`","imageUrl":"https://uploads.example.test/source.png","durationSeconds":5}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	request.Header.Set("X-Request-Id", uuid.NewString())
	handler.ServeHTTP(recorder, request)
	creationID := fixture.assertCreated(t, recorder, 5)
	steps := fixture.trackCreation(creationID)
	if len(steps) != 1 {
		t.Fatalf("I2V 步骤数量 = %d，期望 1", len(steps))
	}
	jobID := fixture.markSubmitted(steps[0].ID)

	nonce := "video-e2e-failed-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+hashText(nonce))
	failed := fixture.signedFailed(steps[0].ID, jobID, nonce, "image_to_video", "ps-auto")
	callbackRecorder := httptest.NewRecorder()
	fixture.callback.ServeHTTP(callbackRecorder, failed)
	if callbackRecorder.Code != http.StatusOK {
		t.Fatalf("可信失败回调状态 = %d，body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
	// 原始报文与 nonce 完全相同的重放必须被幂等确认，而不是再次增加退款分录。
	replayRecorder := httptest.NewRecorder()
	fixture.callback.ServeHTTP(replayRecorder, fixture.signedFailed(steps[0].ID, jobID, nonce, "image_to_video", "ps-auto"))
	if replayRecorder.Code != http.StatusOK {
		t.Fatalf("失败回调重放状态 = %d，body=%s", replayRecorder.Code, replayRecorder.Body.String())
	}
	fixture.assertReversedAfterFailure(userID, creationID, handler, sessionID)
}

// 审核受限用户的 I2V 被拒绝时，提交器必须在请求中台前没收预扣；余额和日免均不得回退。
func TestHandler视频模板I2V审核没收不退款(t *testing.T) {
	fixture := newVideoE2EFixture(t)
	userID, sessionID, templateID := uuid.NewString(), uuid.NewString(), "video-confiscated-"+uuid.NewString()
	fixture.seedBoundUser(userID, sessionID, 50)
	if _, err := fixture.database.Collection(schema.CollectionUsers).UpdateOne(fixture.ctx, bson.D{{Key: "_id", Value: userID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "content_access", Value: identity.ContentAccessReviewRestricted}}}}); err != nil {
		t.Fatalf("设置审核受限用户：%v", err)
	}
	fixture.seedTemplate(templateID)
	handler := NewHandler(fixture.authenticator, fixture.creator, fixture.statuses)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"`+templateID+`","imageUrl":"https://uploads.example.test/source.png","durationSeconds":5}`))
	request.Header.Set("Authorization", "Bearer "+sessionID)
	request.Header.Set("X-Request-Id", uuid.NewString())
	handler.ServeHTTP(recorder, request)
	creationID := fixture.assertCreated(t, recorder, 5)
	steps := fixture.trackCreation(creationID)
	if len(steps) != 1 {
		t.Fatalf("I2V 步骤数量 = %d，期望 1", len(steps))
	}

	client := &videoE2ENeverSubmitClient{}
	submitter := worker.NewGenerationSubmissionWorker(
		fixture.outbox, fixture.submissions, fixture.ledger, fixture.tx, worker.LocalClientRouter{Local: client},
		videoE2ERejectedReviewer{}, "video-e2e-review", func() time.Time { return fixture.now },
	)
	if err := submitter.DeliverOnce(fixture.ctx, outbox.SubmissionEventID(steps[0].ID)); err != nil {
		t.Fatalf("执行审核提交器：%v", err)
	}
	if client.calls != 0 {
		t.Fatalf("审核拒绝后仍请求生成中台 %d 次", client.calls)
	}
	fixture.assertConfiscatedWithoutRefund(userID, creationID, handler, sessionID)
}

type videoE2EFixture struct {
	t             *testing.T
	ctx           context.Context
	database      *mongo.Database
	now           time.Time
	tx            shared.TxRunner
	outbox        outbox.Repository
	submissions   bizgeneration.SubmissionStore
	ledger        *ledger.Usecase
	authenticator *sessionauth.Authenticator
	creator       *bizvideo.Usecase
	statuses      *bizvideo.StatusUsecase
	callback      http.Handler
	cleanup       []videoE2EDocument
}

type videoE2EDocument struct{ collection, id string }

func newVideoE2EFixture(t *testing.T) *videoE2EFixture {
	t.Helper()
	uri := os.Getenv("CLING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("未配置本机 rs0 测试连接")
	}
	config := &conf.Data{Mongo: &conf.Data_Mongo{Uri: uri, Database: "cling_main", ReplicaSet: "rs0", TransactionsRequired: true}}
	if err := conf.ValidateLocalMongo(config); err != nil {
		t.Fatalf("本地 MongoDB 配置不合法：%v", err)
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
		t.Fatalf("构造本地数据层：%v", err)
	}
	t.Cleanup(closeStorage)
	database := rawClient.Database("cling_main")
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本机 schema：%v", err)
	}
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	tx := data.NewTxRunner(storage)
	ledgerUsecase := ledger.NewUsecaseWithClock(data.NewLedgerRepository(storage), tx, func() time.Time { return now })
	creationUsecase := creations.NewUsecaseWithClock(data.NewUserRepository(storage), data.NewSubscriptionRepository(storage), entitlement.NewUsecase(), data.NewCreationRepository(storage), ledgerUsecase, data.NewOutboxRepository(storage), tx, nil, func() time.Time { return now })
	identityUsecase := identity.NewUsecase(data.NewUserRepository(storage), data.NewIdentityRepository(storage), data.NewSessionRepository(storage), tx)
	callbackStore := data.NewGenerationCallbackRepository(storage)
	callbackUsecase := bizgeneration.NewCallbackUsecaseWithClock(callbackStore, callbackStore, ledgerUsecase, tx, func() time.Time { return now })
	verifier, err := platform.NewCallbackVerifier(videoE2ESigningKey, func() time.Time { return now })
	if err != nil {
		t.Fatalf("构造回调验签器：%v", err)
	}
	fixture := &videoE2EFixture{t: t, ctx: ctx, database: database, now: now, tx: tx, outbox: data.NewOutboxRepository(storage), submissions: data.NewGenerationSubmissionRepository(storage), ledger: ledgerUsecase, authenticator: sessionauth.NewAuthenticatorWithClock(identityUsecase, func() time.Time { return now }), creator: bizvideo.NewUsecase(data.NewVideoTemplateRepository(storage), creationUsecase, staticOwnedImageReader{}), statuses: bizvideo.NewStatusUsecase(data.NewVideoStatusRepository(storage)), callback: callbacktransport.NewHandler(verifier, callbackUsecase)}
	t.Cleanup(fixture.deleteTracked)
	return fixture
}

func (fixture *videoE2EFixture) seedBoundUser(userID, sessionID string, balance int64) {
	fixture.track(schema.CollectionUsers, userID)
	fixture.track(schema.CollectionSessions, sessionID)
	fixture.track(schema.CollectionAccounts, userID)
	user := model.UserDocument{ID: userID, AccountStatus: string(identity.AccountStatusNormal), BindingState: string(identity.BindingStateBound), Timezone: "Asia/Shanghai", SessionVersion: 1, ContentAccess: string(identity.ContentAccessStandard), CreatedAt: fixture.now, UpdatedAt: fixture.now}
	if _, err := fixture.database.Collection(schema.CollectionUsers).InsertOne(fixture.ctx, user); err != nil {
		fixture.t.Fatal(err)
	}
	if _, err := fixture.database.Collection(schema.CollectionSessions).InsertOne(fixture.ctx, model.SessionDocument{ID: sessionID, UserID: userID, SessionVersion: 1, ExpiresAt: fixture.now.Add(time.Hour)}); err != nil {
		fixture.t.Fatal(err)
	}
	if _, err := fixture.database.Collection(schema.CollectionAccounts).InsertOne(fixture.ctx, model.AccountDocument{ID: userID, DiamondBalance: balance, CreatedAt: fixture.now, UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *videoE2EFixture) seedTemplate(templateID string) {
	id := uuid.NewString()
	fixture.track(schema.CollectionTemplates, id)
	parameters, err := bson.Marshal(bson.M{"kind": "template_video", "i2v": bson.M{"model_sku": "ps-auto", "prompt": "服务端动作", "negative_prompt": "模糊", "parameters": bson.M{"durationSeconds": 5}}, "t2i": bson.M{"model_sku": "ps-image-v1", "prompt": "服务端首帧", "negative_prompt": "模糊", "parameters": bson.M{"aspectRatio": "9:16"}}})
	if err != nil {
		fixture.t.Fatal(err)
	}
	document := model.TemplateDocument{ID: id, TemplateID: templateID, Version: 1, ContentSurface: string(catalog.ContentSurfaceSFW), Mode: string(catalog.ProductModeTemplateVideo), Enabled: true, Parameters: parameters, CreatedAt: fixture.now, UpdatedAt: fixture.now}
	if _, err := fixture.database.Collection(schema.CollectionTemplates).InsertOne(fixture.ctx, document); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *videoE2EFixture) assertCreated(t *testing.T, recorder *httptest.ResponseRecorder, duration int32) string {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("创建状态=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			TaskID    string `json:"taskId"`
			CoinsUsed int64  `json:"coinsUsed"`
			Duration  int32  `json:"durationSeconds"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success || response.Data.TaskID == "" || response.Data.CoinsUsed != 50 || response.Data.Duration != duration {
		t.Fatalf("创建响应=%s", recorder.Body.String())
	}
	return response.Data.TaskID
}

func (fixture *videoE2EFixture) trackCreation(creationID string) []model.CreationStepDocument {
	fixture.track(schema.CollectionCreations, creationID)
	fixture.track(schema.CollectionReservations, "reservation:"+creationID)
	fixture.track(schema.CollectionLedgerEntries, "reserve:"+creationID)
	cursor, err := fixture.database.Collection(schema.CollectionCreationSteps).Find(fixture.ctx, bson.D{{Key: "creation_id", Value: creationID}}, options.Find().SetSort(bson.D{{Key: "sequence", Value: 1}}))
	if err != nil {
		fixture.t.Fatal(err)
	}
	defer cursor.Close(fixture.ctx)
	var steps []model.CreationStepDocument
	if err := cursor.All(fixture.ctx, &steps); err != nil {
		fixture.t.Fatal(err)
	}
	for _, step := range steps {
		fixture.track(schema.CollectionCreationSteps, step.ID)
		fixture.track(schema.CollectionOutboxEvents, outbox.SubmissionEventID(step.ID))
	}
	return steps
}

func (fixture *videoE2EFixture) assertTechnicalOutbox(stepID, wantCapability string) {
	var event model.OutboxEventDocument
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(stepID)}}).Decode(&event); err != nil {
		fixture.t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &payload); err != nil || len(payload) != 3 {
		fixture.t.Fatalf("Outbox 顶层必须仅三个技术字段：%s / %v", event.Payload, err)
	}
	var capability string
	_ = json.Unmarshal(payload["capability"], &capability)
	if capability != wantCapability || payload["model_sku"] == nil || payload["input"] == nil {
		fixture.t.Fatalf("Outbox 技术能力=%q payload=%s", capability, event.Payload)
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(payload["input"], &input) != nil {
		fixture.t.Fatal("无法解析技术输入")
	}
	for field := range input {
		if field != "prompt" && field != "negativePrompt" && field != "assets" && field != "parameters" {
			fixture.t.Fatalf("Outbox 含业务字段 %q", field)
		}
	}
}

func (fixture *videoE2EFixture) markSubmitted(stepID string) string {
	eventID := outbox.SubmissionEventID(stepID)
	claimed, err := fixture.outbox.ClaimByIDAndType(fixture.ctx, "video-e2e", eventID, outbox.EventTypeGenerationSubmission, fixture.now, fixture.now.Add(time.Minute))
	if err != nil || claimed == nil {
		fixture.t.Fatalf("领取 Outbox：%#v %v", claimed, err)
	}
	jobID := "video-e2e-job-" + uuid.NewString()
	if err := fixture.tx.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.submissions.MarkSubmitted(txCtx, bizgeneration.SubmittedCommand{EventID: eventID, LeaseToken: claimed.LeaseToken, JobID: jobID, At: fixture.now})
	}); err != nil {
		fixture.t.Fatalf("标记已提交：%v", err)
	}
	return jobID
}

func (fixture *videoE2EFixture) complete(stepID, jobID, capability, modelSKU, mediaType, resultURL string) {
	nonce := "video-e2e-callback-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+hashText(nonce))
	fixture.track(schema.CollectionAssets, "asset:"+stepID+":result")
	recorder := httptest.NewRecorder()
	fixture.callback.ServeHTTP(recorder, fixture.signedCompleted(stepID, jobID, nonce, capability, modelSKU, mediaType, resultURL))
	if recorder.Code != http.StatusOK {
		fixture.t.Fatalf("可信回调失败 status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func (fixture *videoE2EFixture) signedCompleted(stepID, jobID, nonce, capability, modelSKU, mediaType, resultURL string) *http.Request {
	body := []byte(`{"contractVersion":"execution.callback.v2","eventId":"` + strings.Repeat("a", 64) + `","deliveryId":"video-e2e-delivery","fingerprint":"` + strings.Repeat("a", 64) + `","eventType":"execution.completed","jobId":"` + jobID + `","externalRef":"` + stepID + `","status":"completed","outputs":[{"role":"result","mediaType":"` + mediaType + `","url":"` + resultURL + `"}],"usage":{},"timestamps":{},"executionRef":{"tenantId":"cling-ai","tenantSignupSource":"internal","capability":"` + capability + `","modelSku":"` + modelSKU + `"},"metadata":{}}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/internal/generation-callback", bytes.NewReader(body))
	timestamp := strconv.FormatInt(fixture.now.UnixMilli(), 10)
	digest := sha256.Sum256(body)
	payload := strings.Join([]string{"generation-callback-v2", timestamp, http.MethodPost, request.URL.Path, nonce, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(videoE2ESigningKey))
	_, _ = mac.Write([]byte(payload))
	request.Header.Set("X-Generation-Callback-Version", "2")
	request.Header.Set("X-Generation-Callback-Timestamp", timestamp)
	request.Header.Set("X-Generation-Callback-Nonce", nonce)
	request.Header.Set("X-Generation-Callback-Signature", hex.EncodeToString(mac.Sum(nil)))
	return request
}

// signedFailed 生成符合 execution.callback.v2 合同的失败终态；错误文本不进入领域或断言。
func (fixture *videoE2EFixture) signedFailed(stepID, jobID, nonce, capability, modelSKU string) *http.Request {
	body := []byte(`{"contractVersion":"execution.callback.v2","eventId":"` + strings.Repeat("a", 64) + `","deliveryId":"video-e2e-failed-delivery","fingerprint":"` + strings.Repeat("a", 64) + `","eventType":"execution.failed","jobId":"` + jobID + `","externalRef":"` + stepID + `","status":"failed","outputs":[],"error":{"code":"UPSTREAM_FAILED","message":"upstream-error-secret"},"usage":{},"timestamps":{},"executionRef":{"tenantId":"cling-ai","tenantSignupSource":"internal","capability":"` + capability + `","modelSku":"` + modelSKU + `"},"metadata":{}}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/internal/generation-callback", bytes.NewReader(body))
	timestamp := strconv.FormatInt(fixture.now.UnixMilli(), 10)
	digest := sha256.Sum256(body)
	payload := strings.Join([]string{"generation-callback-v2", timestamp, http.MethodPost, request.URL.Path, nonce, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(videoE2ESigningKey))
	_, _ = mac.Write([]byte(payload))
	request.Header.Set("X-Generation-Callback-Version", "2")
	request.Header.Set("X-Generation-Callback-Timestamp", timestamp)
	request.Header.Set("X-Generation-Callback-Nonce", nonce)
	request.Header.Set("X-Generation-Callback-Signature", hex.EncodeToString(mac.Sum(nil)))
	return request
}

func (fixture *videoE2EFixture) assertCompletedStatus(handler http.Handler, sessionID, creationID string) {
	for _, request := range []*http.Request{httptest.NewRequest(http.MethodPost, statusesPath, strings.NewReader(`{"taskIds":["`+creationID+`"]}`)), httptest.NewRequest(http.MethodGet, detailPathPrefix+creationID, nil)} {
		request.Header.Set("Authorization", "Bearer "+sessionID)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"completed"`) || !strings.Contains(recorder.Body.String(), `"videoUrl":"`+videoE2EResultURL+`"`) {
			fixture.t.Fatalf("完成状态=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

// assertReversedAfterFailure 验证生成失败回调在一个业务闭环内完成状态收敛与一次性退款。
func (fixture *videoE2EFixture) assertReversedAfterFailure(userID, creationID string, handler http.Handler, sessionID string) {
	fixture.t.Helper()
	fixture.track(schema.CollectionLedgerEntries, "reverse:"+creationID)
	var reservation model.ReservationDocument
	if err := fixture.database.Collection(schema.CollectionReservations).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: "reservation:" + creationID}}).Decode(&reservation); err != nil || reservation.Status != string(ledger.ReservationStatusReversed) {
		fixture.t.Fatalf("失败回调后的预留 = %#v，err=%v", reservation, err)
	}
	var account model.AccountDocument
	if err := fixture.database.Collection(schema.CollectionAccounts).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account); err != nil || account.DiamondBalance != 50 {
		fixture.t.Fatalf("失败回调后的余额 = %#v，err=%v", account, err)
	}
	entries, err := fixture.database.Collection(schema.CollectionLedgerEntries).CountDocuments(fixture.ctx, bson.D{{Key: "creation_id", Value: creationID}})
	if err != nil || entries != 2 {
		fixture.t.Fatalf("失败回调后的创作账本分录数 = %d，err=%v，期望 2", entries, err)
	}
	fixture.assertFailedStatus(handler, sessionID, creationID, "GENERATION_FAILED")
}

// assertConfiscatedWithoutRefund 验证审核拒绝不会触发退款或额外账本分录。
func (fixture *videoE2EFixture) assertConfiscatedWithoutRefund(userID, creationID string, handler http.Handler, sessionID string) {
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

// assertFailedStatus 同时覆盖现网单条和批量视频轮询的失败安全投影。
func (fixture *videoE2EFixture) assertFailedStatus(handler http.Handler, sessionID, creationID, errorCode string) {
	fixture.t.Helper()
	for _, request := range []*http.Request{httptest.NewRequest(http.MethodPost, statusesPath, strings.NewReader(`{"taskIds":["`+creationID+`"]}`)), httptest.NewRequest(http.MethodGet, detailPathPrefix+creationID, nil)} {
		request.Header.Set("Authorization", "Bearer "+sessionID)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"failed"`) || !strings.Contains(recorder.Body.String(), `"generationErrorCode":"`+errorCode+`"`) {
			fixture.t.Fatalf("失败状态 = %d，body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

// videoE2ERejectedReviewer 固定返回审核拒绝，用于验证 I2V 没收路径不依赖真实审核服务。
type videoE2ERejectedReviewer struct{}

func (videoE2ERejectedReviewer) Review(context.Context, contentreview.Request) (contentreview.Decision, error) {
	return contentreview.Decision{Outcome: contentreview.OutcomeRejected}, nil
}

// videoE2ENeverSubmitClient 记录是否触达生成中台；审核拒绝时它不应被调用。
type videoE2ENeverSubmitClient struct{ calls int }

func (client *videoE2ENeverSubmitClient) Submit(context.Context, platform.Execution) (platform.SubmissionResult, error) {
	client.calls++
	return platform.SubmissionResult{}, platform.ErrRejected
}

func (*videoE2ENeverSubmitClient) Lookup(context.Context, string) (platform.LookupResult, error) {
	return platform.LookupResult{}, platform.ErrRejected
}

func (fixture *videoE2EFixture) track(collection, id string) {
	fixture.cleanup = append(fixture.cleanup, videoE2EDocument{collection, id})
}
func (fixture *videoE2EFixture) deleteTracked() {
	for index := len(fixture.cleanup) - 1; index >= 0; index-- {
		document := fixture.cleanup[index]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := fixture.database.Collection(document.collection).DeleteOne(ctx, bson.D{{Key: "_id", Value: document.id}})
		cancel()
		if err != nil {
			fixture.t.Errorf("清理 %s/%s: %v", document.collection, document.id, err)
		}
	}
}
func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
