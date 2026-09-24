package generationcallback

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"ai-business-service/internal/executionv2"
	"ai-business-service/internal/gateway"
	platform "ai-business-service/internal/integrations/generation"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	handlerSigningKey = "handler-signing-key-at-least-thirty-two-characters"
	handlerResultURL  = "https://asset.invalid/result.png"
)

var handlerNow = func() time.Time { return time.UnixMilli(1_788_768_550_000).UTC() }

func TestHandler验签后确认回调(t *testing.T) {
	for _, path := range []string{callbackPathV1, callbackPathLegacy} {
		t.Run(path, func(t *testing.T) {
			handler := NewHandler(newHandlerVerifier(t), handlerUsecaseFunc(func(context.Context, bizgeneration.VerifiedCallbackEvent) error { return nil }))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, signedHandlerRequest(t, http.MethodPost, path, handlerCompletedBody(), handlerNow()))
			if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
				t.Fatal("成功回调未得到固定确认")
			}
		})
	}
}

func TestHandler拒绝不可信HTTP输入(t *testing.T) {
	verifier := newHandlerVerifier(t)
	valid := handlerCompletedBody()
	cases := []struct {
		name     string
		request  *http.Request
		wantCode int
		wantBody string
	}{
		{name: "错误方法", request: signedHandlerRequest(t, http.MethodGet, "/api/v1/internal/generation-callback", valid, handlerNow()), wantCode: http.StatusMethodNotAllowed, wantBody: `{"error":"invalid_request"}`},
		{name: "错误路径", request: signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/other", valid, handlerNow()), wantCode: http.StatusNotFound, wantBody: `{"error":"invalid_request"}`},
		{name: "编码斜杠", request: signedHandlerRequest(t, http.MethodPost, "/api%2Fv1/internal/generation-callback", valid, handlerNow()), wantCode: http.StatusNotFound, wantBody: `{"error":"invalid_request"}`},
		{name: "编码点", request: signedHandlerRequest(t, http.MethodPost, "/api%2ev1/internal/generation-callback", valid, handlerNow()), wantCode: http.StatusNotFound, wantBody: `{"error":"invalid_request"}`},
		{name: "双重编码", request: signedHandlerRequest(t, http.MethodPost, "/api%252Fv1/internal/generation-callback", valid, handlerNow()), wantCode: http.StatusNotFound, wantBody: `{"error":"invalid_request"}`},
		{name: "父级路径", request: signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/%2e%2e/generation-callback", valid, handlerNow()), wantCode: http.StatusNotFound, wantBody: `{"error":"invalid_request"}`},
		{name: "重复斜杠", request: signedHandlerRequest(t, http.MethodPost, "/api//v1/internal/generation-callback", valid, handlerNow()), wantCode: http.StatusNotFound, wantBody: `{"error":"invalid_request"}`},
		{name: "查询参数", request: signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/generation-callback?x=1", valid, handlerNow()), wantCode: http.StatusBadRequest, wantBody: `{"error":"invalid_request"}`},
		{name: "空查询", request: signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/generation-callback?", valid, handlerNow()), wantCode: http.StatusBadRequest, wantBody: `{"error":"invalid_request"}`},
		{name: "签名错误", request: invalidSignatureHandlerRequest(t, valid), wantCode: http.StatusUnauthorized, wantBody: `{"error":"invalid_callback"}`},
		{name: "过期", request: signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/generation-callback", valid, handlerNow().Add(-6*time.Minute)), wantCode: http.StatusUnauthorized, wantBody: `{"error":"invalid_callback"}`},
		{name: "未知事件", request: signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/generation-callback", handlerUnknownEventBody(), handlerNow()), wantCode: http.StatusUnauthorized, wantBody: `{"error":"invalid_callback"}`},
		{name: "过大正文", request: signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/generation-callback", []byte(strings.Repeat("a", 1024*1024+1)), handlerNow()), wantCode: http.StatusRequestEntityTooLarge, wantBody: `{"error":"invalid_request"}`},
		{name: "正好正文上限", request: signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/generation-callback", []byte(strings.Repeat("a", 1024*1024)), handlerNow()), wantCode: http.StatusUnauthorized, wantBody: `{"error":"invalid_callback"}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := NewHandler(verifier, handlerUsecaseFunc(func(context.Context, bizgeneration.VerifiedCallbackEvent) error { return nil }))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, testCase.request)
			if recorder.Code != testCase.wantCode || recorder.Body.String() != testCase.wantBody {
				t.Fatal("不可信请求未得到固定错误码")
			}
		})
	}
}

func TestHandler将领域错误映射为固定错误码(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{name: "重复回执", err: bizgeneration.ErrCallbackAlreadyConfirmed, wantCode: http.StatusOK, wantBody: `{"ok":true}`},
		{name: "业务冲突", err: bizgeneration.ErrInvalidCallbackEvent, wantCode: http.StatusConflict, wantBody: `{"error":"callback_conflict"}`},
		{name: "内部错误", err: errors.New("failure"), wantCode: http.StatusInternalServerError, wantBody: `{"error":"internal_error"}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := NewHandler(newHandlerVerifier(t), handlerUsecaseFunc(func(context.Context, bizgeneration.VerifiedCallbackEvent) error { return testCase.err }))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/generation-callback", handlerCompletedBody(), handlerNow()))
			if recorder.Code != testCase.wantCode || recorder.Body.String() != testCase.wantBody {
				t.Fatal("领域错误未得到固定错误码")
			}
		})
	}
}

func TestHandler受控终态回放确认成功(t *testing.T) {
	handler := NewHandler(newHandlerVerifier(t), handlerUsecaseFunc(func(context.Context, bizgeneration.VerifiedCallbackEvent) error {
		return bizgeneration.ErrCallbackAlreadyConfirmed
	}))
	body := handlerCompletedBody()
	nonce := "handler-replay-" + uuid.NewString()
	for _, requestNonce := range []string{nonce, nonce, "handler-replay-" + uuid.NewString()} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, signedHandlerRequestWithNonce(t, http.MethodPost, callbackPathV1, body, handlerNow(), requestNonce))
		if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
			t.Fatal("受控终态回放未确认")
		}
	}
}

func TestHandler本地E2E首帧完成激活I2V技术载荷(t *testing.T) {
	fixture := newHandlerMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedReservedVideoPrerequisites(userID)
	result, err := fixture.creations.CreateReserved(fixture.ctx, fixture.videoRequest(userID))
	if err != nil || result == nil || result.Creation == nil || len(result.Steps) != 2 {
		t.Fatal("创建双步骤预留失败")
	}
	fixture.trackCreationResult(result)
	fixture.assertReservedFacts(result.Creation.ID)
	firstEventID := outbox.SubmissionEventID(result.Steps[0].ID)
	var firstEvent model.OutboxEventDocument
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: firstEventID}}).Decode(&firstEvent); err != nil {
		t.Fatal("首步骤发件箱未持久化")
	}
	if firstEvent.EventType != string(outbox.EventTypeGenerationSubmission) || firstEvent.DeliveryStatus != string(outbox.DeliveryStatusPending) {
		t.Fatal("首步骤发件箱初始状态不正确")
	}
	if firstEvent.NextAttemptAt.After(fixture.now) {
		t.Fatal("首步骤发件箱尚未到达可领取时刻")
	}
	claimed, err := fixture.outbox.ClaimByIDAndType(fixture.ctx, "handler-e2e", firstEventID, outbox.EventTypeGenerationSubmission, fixture.now, fixture.now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatal("首步骤发件箱未能模拟领取")
	}
	if err := fixture.tx.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.submissions.MarkSubmitted(txCtx, bizgeneration.SubmittedCommand{EventID: firstEventID, LeaseToken: claimed.LeaseToken, JobID: fixture.firstJobID, At: fixture.now})
	}); err != nil {
		t.Fatal("首步骤未能模拟已提交")
	}

	nonce := "handler-e2e-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+handlerSHA256(nonce))
	handler := NewHandler(newHandlerVerifier(t), fixture.callbacks)
	upstream, err := url.Parse("http://node.invalid")
	if err != nil {
		t.Fatal(err)
	}
	callbackGateway := gateway.New(gateway.Config{DefaultUpstream: upstream, GenerationCallback: handler})
	recorder := httptest.NewRecorder()
	callbackGateway.Handler().ServeHTTP(recorder, signedHandlerRequestWithNonce(t, http.MethodPost, callbackPathV1, handlerCompletedBodyFor(result.Steps[0].ID, fixture.firstJobID), fixture.now, nonce))
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
		t.Fatal("首帧完成回调未得到确认")
	}
	for _, replayNonce := range []string{nonce, "handler-e2e-" + uuid.NewString()} {
		if replayNonce != nonce {
			fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+handlerSHA256(replayNonce))
		}
		recorder = httptest.NewRecorder()
		callbackGateway.Handler().ServeHTTP(recorder, signedHandlerRequestWithNonce(t, http.MethodPost, callbackPathV1, handlerCompletedBodyFor(result.Steps[0].ID, fixture.firstJobID), fixture.now, replayNonce))
		if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
			t.Fatal("真实终态回放未确认")
		}
	}

	secondEventID := outbox.SubmissionEventID(result.Steps[1].ID)
	fixture.track(schema.CollectionOutboxEvents, secondEventID)
	var secondStep model.CreationStepDocument
	if err := fixture.database.Collection(schema.CollectionCreationSteps).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: result.Steps[1].ID}}).Decode(&secondStep); err != nil || secondStep.SubmitStatus != string(creations.StepSubmitStatusReady) {
		t.Fatal("第二步骤未被激活")
	}
	var secondEvent model.OutboxEventDocument
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: secondEventID}}).Decode(&secondEvent); err != nil || secondEvent.DeliveryStatus != string(outbox.DeliveryStatusPending) {
		t.Fatal("第二步骤发件箱未就绪")
	}
	snapshot, err := executionv2.ParseSubmissionPayload(secondEvent.Payload)
	if err != nil || snapshot.Capability != executionv2.CapabilityImageToVideo || len(snapshot.Input.Assets) != 1 || snapshot.Input.Assets[0].Role != "opening_frame" || snapshot.Input.Assets[0].URL != handlerResultURL {
		t.Fatal("第二步骤载荷不是受控 I2V 快照")
	}
	execution, err := platform.ExecutionFromSubmissionPayload(result.Steps[1].ID, secondEvent.Payload)
	if err != nil || execution.Capability != executionv2.CapabilityImageToVideo || len(execution.Input.Assets) != 1 || execution.Input.Assets[0].Role != "opening_frame" {
		t.Fatal("第二步骤载荷无法构造 I2V 执行请求")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(secondEvent.Payload, &fields); err != nil || len(fields) != 3 || fields["capability"] == nil || fields["model_sku"] == nil || fields["input"] == nil {
		t.Fatal("第二步骤载荷包含非技术字段")
	}
}

type handlerUsecaseFunc func(context.Context, bizgeneration.VerifiedCallbackEvent) error

type handlerMongoFixture struct {
	t           *testing.T
	ctx         context.Context
	database    *mongo.Database
	now         time.Time
	tx          shared.TxRunner
	creations   *creations.Usecase
	outbox      outbox.Repository
	submissions bizgeneration.SubmissionStore
	callbacks   *bizgeneration.CallbackUsecase
	cleanup     []handlerMongoDocument
	firstJobID  string
	quotaID     string
}

type handlerMongoDocument struct {
	collection string
	id         string
}

func newHandlerMongoFixture(t *testing.T) *handlerMongoFixture {
	t.Helper()
	uri := os.Getenv("CLING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("未配置本机 rs0 测试连接")
	}
	config := &conf.Data{Mongo: &conf.Data_Mongo{Uri: uri, Database: "cling_main", ReplicaSet: "rs0", TransactionsRequired: true}}
	if err := conf.ValidateLocalMongo(config); err != nil {
		t.Fatal("本地 MongoDB 配置不符合 rs0 约束")
	}
	rawClient, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal("无法连接本机 MongoDB")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rawClient.Disconnect(ctx) != nil {
			t.Error("本机 MongoDB 连接清理失败")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := rawClient.Ping(ctx, nil); err != nil {
		cancel()
		t.Fatal("本机 MongoDB 不可用")
	}
	t.Cleanup(cancel)
	storage, closeStorage, err := data.NewData(config)
	if err != nil {
		t.Fatal("无法构造本机数据层")
	}
	t.Cleanup(closeStorage)
	database := rawClient.Database("cling_main")
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatal("本机 schema 初始化失败")
	}
	now := handlerNow()
	tx := data.NewTxRunner(storage)
	ledgerUsecase := ledger.NewUsecaseWithClock(data.NewLedgerRepository(storage), tx, func() time.Time { return now })
	callbackStore := data.NewGenerationCallbackRepository(storage)
	fixture := &handlerMongoFixture{
		t: t, ctx: ctx, database: database, now: now, tx: tx,
		outbox: data.NewOutboxRepository(storage), submissions: data.NewGenerationSubmissionRepository(storage),
		callbacks: dataCallbackUsecase(callbackStore, ledgerUsecase, tx, now), firstJobID: "job-frame-" + uuid.NewString(),
	}
	fixture.creations = creations.NewUsecaseWithClock(
		data.NewUserRepository(storage), data.NewSubscriptionRepository(storage), entitlement.NewUsecase(),
		data.NewCreationRepository(storage), ledgerUsecase, fixture.outbox, tx, nil, func() time.Time { return now },
	)
	t.Cleanup(fixture.deleteTracked)
	return fixture
}

func dataCallbackUsecase(store bizgeneration.CallbackStore, ledgerUsecase *ledger.Usecase, tx shared.TxRunner, now time.Time) *bizgeneration.CallbackUsecase {
	linker, ok := store.(bizgeneration.CallbackLinker)
	if !ok {
		return nil
	}
	return bizgeneration.NewCallbackUsecaseWithClock(store, linker, ledgerUsecase, tx, func() time.Time { return now })
}

func (fixture *handlerMongoFixture) seedReservedVideoPrerequisites(userID string) {
	fixture.track(schema.CollectionUsers, userID)
	fixture.track(schema.CollectionSubscriptions, userID)
	fixture.quotaID = uuid.NewString()
	fixture.track(schema.CollectionDailyQuotas, fixture.quotaID)
	if _, err := fixture.database.Collection(schema.CollectionUsers).InsertOne(fixture.ctx, model.UserDocument{ID: userID, AccountStatus: string(identity.AccountStatusNormal), BindingState: string(identity.BindingStateBound), Timezone: "Asia/Shanghai", SessionVersion: 1, ContentAccess: "standard", CreatedAt: fixture.now, UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatal("无法写入本地测试用户")
	}
	if _, err := fixture.database.Collection(schema.CollectionSubscriptions).InsertOne(fixture.ctx, model.SubscriptionDocument{UserID: userID, Status: string(entitlement.SubscriptionStatusActive), BillingPeriod: string(entitlement.SubscriptionBillingPeriodMonthly), StartsAt: fixture.now.Add(-time.Hour), ExpiresAt: fixture.now.Add(24 * time.Hour), CreatedAt: fixture.now.Add(-time.Hour), UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatal("无法写入本地测试订阅")
	}
	if _, err := fixture.database.Collection(schema.CollectionDailyQuotas).InsertOne(fixture.ctx, model.DailyQuotaDocument{ID: fixture.quotaID, UserID: userID, QuotaKind: "vip_daily_video", LocalDate: fixture.now.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02"), Limit: 3, UsedCount: 0, UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatal("无法写入本地测试额度")
	}
}

func (fixture *handlerMongoFixture) videoRequest(userID string) creations.CreateReservedRequest {
	return creations.CreateReservedRequest{
		UserID: userID, IdempotencyKey: uuid.NewString(), TemplateID: "local-handler-e2e", TemplateVersion: 1,
		Product:              entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo, Video: entitlement.VideoOptions{DurationSeconds: 5, ReferenceImageCount: 1}},
		InputDigest:          strings.Repeat("b", sha256.Size*2),
		Plan:                 []creations.StepPlan{{Sequence: 1, Atom: creations.AtomTextToImage}, {Sequence: 2, Atom: creations.AtomImageToVideo}},
		InitialSubmission:    &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"first","assets":[],"parameters":{}}`)},
		DeferredImageToVideo: &creations.DeferredImageToVideo{ModelSKU: "ps-auto", InputTemplate: json.RawMessage(`{"prompt":"motion","parameters":{"durationSeconds":5}}`)},
	}
}

func (fixture *handlerMongoFixture) trackCreationResult(result *creations.CreateReservedResult) {
	fixture.track(schema.CollectionCreations, result.Creation.ID)
	fixture.track(schema.CollectionReservations, "reservation:"+result.Creation.ID)
	fixture.track(schema.CollectionLedgerEntries, "reserve:"+result.Creation.ID)
	for _, step := range result.Steps {
		fixture.track(schema.CollectionCreationSteps, step.ID)
	}
	fixture.track(schema.CollectionGenerationStepRecipes, result.Steps[1].ID)
	fixture.track(schema.CollectionOutboxEvents, outbox.SubmissionEventID(result.Steps[0].ID))
	fixture.track(schema.CollectionAssets, "asset:"+result.Steps[0].ID+":result")
}

func (fixture *handlerMongoFixture) assertReservedFacts(creationID string) {
	var reservation model.ReservationDocument
	if err := fixture.database.Collection(schema.CollectionReservations).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: "reservation:" + creationID}}).Decode(&reservation); err != nil || reservation.CreationID != creationID || reservation.Status != string(ledger.ReservationStatusReserved) || reservation.BenefitSource != string(ledger.BenefitSourceDailyQuota) {
		fixture.t.Fatal("预留事实不符合预期")
	}
	var entry model.LedgerEntryDocument
	if err := fixture.database.Collection(schema.CollectionLedgerEntries).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: "reserve:" + creationID}}).Decode(&entry); err != nil || entry.CreationID != creationID || entry.ReservationID != reservation.ID || entry.DeltaDiamonds != 0 {
		fixture.t.Fatal("预留账本事实不符合预期")
	}
	var quota model.DailyQuotaDocument
	if err := fixture.database.Collection(schema.CollectionDailyQuotas).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.quotaID}}).Decode(&quota); err != nil || quota.UsedCount != 1 || quota.Limit != 3 {
		fixture.t.Fatal("日免额度事实不符合预期")
	}
}

func (fixture *handlerMongoFixture) track(collection, id string) {
	fixture.cleanup = append(fixture.cleanup, handlerMongoDocument{collection: collection, id: id})
}

func (fixture *handlerMongoFixture) deleteTracked() {
	for index := len(fixture.cleanup) - 1; index >= 0; index-- {
		document := fixture.cleanup[index]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := fixture.database.Collection(document.collection).DeleteOne(ctx, bson.D{{Key: "_id", Value: document.id}})
		cancel()
		if err != nil {
			fixture.t.Error("本地测试文档清理失败")
		}
	}
}

func (function handlerUsecaseFunc) Handle(ctx context.Context, event bizgeneration.VerifiedCallbackEvent) error {
	return function(ctx, event)
}

func newHandlerVerifier(t *testing.T) *platform.CallbackVerifier {
	t.Helper()
	verifier, err := platform.NewCallbackVerifier(handlerSigningKey, handlerNow)
	if err != nil {
		t.Fatal("无法构造验签器")
	}
	return verifier
}

func signedHandlerRequest(t *testing.T, method, requestURI string, body []byte, at time.Time) *http.Request {
	t.Helper()
	return signedHandlerRequestWithNonce(t, method, requestURI, body, at, "handler-nonce-00000000000000000001")
}

func signedHandlerRequestWithNonce(t *testing.T, method, requestURI string, body []byte, at time.Time, nonce string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, requestURI, bytes.NewReader(body))
	timestamp := strconv.FormatInt(at.UnixMilli(), 10)
	digest := sha256.Sum256(body)
	payload := strings.Join([]string{"generation-callback-v2", timestamp, method, request.URL.Path, nonce, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(handlerSigningKey))
	_, _ = mac.Write([]byte(payload))
	request.Header.Set("X-Generation-Callback-Version", "2")
	request.Header.Set("X-Generation-Callback-Timestamp", timestamp)
	request.Header.Set("X-Generation-Callback-Nonce", nonce)
	request.Header.Set("X-Generation-Callback-Signature", hex.EncodeToString(mac.Sum(nil)))
	return request
}

func invalidSignatureHandlerRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	request := signedHandlerRequest(t, http.MethodPost, "/api/v1/internal/generation-callback", body, handlerNow())
	request.Header.Set("X-Generation-Callback-Signature", strings.Repeat("0", sha256.Size*2))
	return request
}

func handlerCompletedBody() []byte {
	return []byte(`{"contractVersion":"execution.callback.v2","eventId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","deliveryId":"delivery-1","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","eventType":"execution.completed","jobId":"job-1","externalRef":"step-1","status":"completed","outputs":[{"role":"result","mediaType":"image","url":"https://asset.invalid/result.png"}],"usage":{},"timestamps":{},"executionRef":{"tenantId":"cling-ai","tenantSignupSource":"internal","capability":"text_to_image","modelSku":"ps-image-v1"},"metadata":{}}`)
}

func handlerUnknownEventBody() []byte {
	return []byte(`{"contractVersion":"execution.callback.v2","eventId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","deliveryId":"delivery-1","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","eventType":"execution.unknown","jobId":"job-1","externalRef":"step-1","status":"unknown","outputs":[],"usage":{},"timestamps":{},"executionRef":{"tenantId":"cling-ai","tenantSignupSource":"internal","capability":"text_to_image","modelSku":"ps-image-v1"},"metadata":{}}`)
}

func handlerCompletedBodyFor(stepID, jobID string) []byte {
	body := strings.Replace(string(handlerCompletedBody()), `"job-1"`, `"`+jobID+`"`, 1)
	body = strings.Replace(body, `"step-1"`, `"`+stepID+`"`, 1)
	return []byte(body)
}

func handlerSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
