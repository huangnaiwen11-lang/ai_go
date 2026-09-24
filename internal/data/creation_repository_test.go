package data

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/worker"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestMongo订阅投影缺失时十秒视频被权益门禁拒绝(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 100)
	request := fixture.videoRequest(userID, uuid.NewString(), 10)

	_, err := fixture.usecase.CreateReserved(fixture.ctx, request)

	if !errors.Is(err, shared.ErrVIPRequired) {
		t.Fatalf("CreateReserved() error = %v, want ErrVIPRequired", err)
	}
	fixture.assertNoCreationFacts(request.IdempotencyKey, userID)
}

func TestMongoCreateReservedOutbox写入首步骤提交事件且不持久化技术快照(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 20)
	request := fixture.imageRequest(userID, uuid.NewString())
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"technical-only-prompt","assets":[],"parameters":{}}`),
	}

	result, err := fixture.usecase.CreateReserved(fixture.ctx, request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(result)
	fixture.trackInitialSubmissionEvent(result)
	if len(result.Steps) != 1 || result.Steps[0].SubmitStatus != creations.StepSubmitStatusReady {
		t.Fatalf("Steps = %#v, want one ready first step", result.Steps)
	}
	eventID := outbox.SubmissionEventID(result.Steps[0].ID)
	var event model.OutboxEventDocument
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: eventID}},
	).Decode(&event); err != nil {
		t.Fatalf("读取首步骤 outbox 事件: %v", err)
	}
	if event.AggregateID != result.Creation.ID || event.EventType != string(outbox.EventTypeGenerationSubmission) || event.DeliveryStatus != string(outbox.DeliveryStatusPending) {
		t.Fatalf("outbox event = %#v, want pending event for creation %q", event, result.Creation.ID)
	}
	var payload struct {
		ModelSKU string          `json:"model_sku"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal outbox payload: %v", err)
	}
	if payload.ModelSKU != request.InitialSubmission.ModelSKU || string(payload.Input) != string(request.InitialSubmission.Input) {
		t.Fatalf("outbox payload = %#v, want technical submission snapshot", payload)
	}

	var creation model.CreationDocument
	if err := fixture.database.Collection(schema.CollectionCreations).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: result.Creation.ID}},
	).Decode(&creation); err != nil {
		t.Fatalf("读取创作文档: %v", err)
	}
	var step model.CreationStepDocument
	if err := fixture.database.Collection(schema.CollectionCreationSteps).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: result.Steps[0].ID}},
	).Decode(&step); err != nil {
		t.Fatalf("读取创作步骤文档: %v", err)
	}
	creationBytes, err := bson.Marshal(creation)
	if err != nil {
		t.Fatalf("编码创作文档: %v", err)
	}
	stepBytes, err := bson.Marshal(step)
	if err != nil {
		t.Fatalf("编码创作步骤文档: %v", err)
	}
	if strings.Contains(string(creationBytes), "technical-only-prompt") || strings.Contains(string(stepBytes), "technical-only-prompt") {
		t.Fatal("技术输入不得写入 creations 或 creation_steps 文档")
	}
}

func TestMongoCreateReserved文生视频同事务冻结第二步配方(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedActiveSubscription(userID, entitlement.SubscriptionBillingPeriodMonthly)
	fixture.seedDailyQuota(uuid.NewString(), userID, "vip_daily_video", 3, 0)
	request := fixture.videoRequest(userID, uuid.NewString(), 5)
	request.Plan = []creations.StepPlan{
		{Sequence: 1, Atom: creations.AtomTextToImage},
		{Sequence: 2, Atom: creations.AtomImageToVideo},
	}
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"first","assets":[],"parameters":{}}`),
	}
	request.DeferredImageToVideo = &creations.DeferredImageToVideo{
		ModelSKU:      "ps-auto",
		InputTemplate: json.RawMessage(`{"prompt":"move","parameters":{"durationSeconds":5}}`),
	}

	result, err := fixture.usecase.CreateReserved(fixture.ctx, request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(result)
	fixture.trackInitialSubmissionEvent(result)
	fixture.trackDocument(schema.CollectionGenerationStepRecipes, result.Steps[1].ID)
	var recipe model.DeferredRecipeDocument
	if err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(
		fixture.ctx,
		bson.D{{Key: "step_id", Value: result.Steps[1].ID}},
	).Decode(&recipe); err != nil {
		t.Fatalf("读取第二步冻结配方: %v", err)
	}
	if recipe.ID != result.Steps[1].ID || recipe.StepID != result.Steps[1].ID || recipe.CreationID != result.Creation.ID || recipe.Atom != string(creations.AtomImageToVideo) || recipe.ModelSKU != "ps-auto" || recipe.Digest == "" || recipe.Status != string(creations.DeferredRecipeStatusPending) || string(recipe.InputTemplate) != string(request.DeferredImageToVideo.InputTemplate) || !recipe.CreatedAt.Equal(fixture.now) || !recipe.UpdatedAt.Equal(fixture.now) {
		t.Fatal("冻结配方字段不符合第二步技术配方约束")
	}

	var raw bson.M
	if err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: result.Steps[1].ID}},
	).Decode(&raw); err != nil {
		t.Fatalf("读取冻结配方原始字段: %v", err)
	}
	for _, field := range []string{
		"user_id",
		"template_id",
		"template_version",
		"price_diamonds",
		"ledger_entry",
		"payment",
		"vip",
		"vip_status",
		"diamond_balance",
		"balance",
		"assets",
		"asset_url",
		"opening_frame",
		"opening_frame_url",
	} {
		if _, found := raw[field]; found {
			t.Fatalf("冻结配方不得包含业务字段 %q", field)
		}
	}
}

// TestMongoDeferredRecipeWriter持久化未绑定B2B第二阶段配方 covers the
// storage union directly: the B2B half of a two-step video must not be
// mistaken for the legacy execution.v2 template before the first R2 frame
// exists.  In particular, no model_sku/input_template may survive alongside
// the canonical public-product bytes.
func TestMongoDeferredRecipeWriter持久化未绑定B2B第二阶段配方(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	stepID := uuid.NewString()
	deferred, err := creations.CompileDeferredB2BImageToVideo(creations.PublishedB2BProductRecipe{
		TemplateID: "template-video-1", TemplateVersion: 1, Atom: creations.AtomImageToVideo,
		ProductKey: "video-standard", Input: json.RawMessage(`{"durationSeconds":5,"prompt":"camera move"}`),
	}, nil)
	if err != nil {
		t.Fatalf("CompileDeferredB2BImageToVideo() error = %v", err)
	}
	writer := &mongoDeferredRecipeWriter{recipes: fixture.database.Collection(schema.CollectionGenerationStepRecipes)}
	recipe := &creations.DeferredRecipe{
		StepID: stepID, CreationID: "creation-" + stepID, Atom: creations.AtomImageToVideo,
		Protocol: creations.DeferredRecipeProtocolB2B, B2B: deferred, Digest: deferred.Digest,
		Status: creations.DeferredRecipeStatusPending, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}
	if err := writer.Create(fixture.ctx, recipe); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	fixture.trackDocument(schema.CollectionGenerationStepRecipes, stepID)

	var stored model.DeferredRecipeDocument
	if err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: stepID}}).Decode(&stored); err != nil {
		t.Fatalf("read stored deferred recipe: %v", err)
	}
	wantPayload, err := deferred.Recipe.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if stored.Protocol != string(creations.DeferredRecipeProtocolB2B) || stored.ModelSKU != "" || len(stored.InputTemplate) != 0 || string(stored.B2BRecipe) != string(wantPayload) || stored.Digest != deferred.Digest || stored.Status != string(creations.DeferredRecipeStatusPending) || !stored.CreatedAt.Equal(fixture.now) || !stored.UpdatedAt.Equal(fixture.now) {
		t.Fatalf("stored deferred recipe = %#v", stored)
	}
	var raw bson.M
	if err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: stepID}}).Decode(&raw); err != nil {
		t.Fatalf("read raw deferred recipe: %v", err)
	}
	for _, forbidden := range []string{"model_sku", "input_template"} {
		if _, exists := raw[forbidden]; exists {
			t.Fatalf("B2B deferred recipe unexpectedly persists %q", forbidden)
		}
	}

	invalid := *recipe
	invalid.StepID = uuid.NewString()
	invalid.ModelSKU = "legacy-model"
	if err := writer.Create(fixture.ctx, &invalid); err == nil {
		t.Fatal("Create() accepted a mixed B2B/execution.v2 deferred recipe")
	}
	fixture.assertDocumentCount(schema.CollectionGenerationStepRecipes, bson.D{{Key: "_id", Value: invalid.StepID}}, 0)
}

func TestMongoCreateReserved两步B2B同事务冻结归属与未绑定第二阶段配方(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedActiveSubscription(userID, entitlement.SubscriptionBillingPeriodMonthly)
	fixture.seedDailyQuota(uuid.NewString(), userID, "vip_daily_video", 3, 0)
	first := creations.B2BProductRecipe{ProductKey: "image-standard", Input: json.RawMessage(`{"prompt":"first frame","aspectRatio":"1:1"}`)}
	deferred, err := creations.CompileDeferredB2BImageToVideo(creations.PublishedB2BProductRecipe{
		TemplateID: "template-video-1", TemplateVersion: 1, Atom: creations.AtomImageToVideo,
		ProductKey: "video-standard", Input: json.RawMessage(`{"prompt":"camera move","durationSeconds":5}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.usecase = fixture.newUsecaseWithAdmissions(twoStepB2BCreationAdmissionResolver{})
	request := fixture.videoRequest(userID, uuid.NewString(), 5)
	request.InitialSubmission = nil
	request.DeferredImageToVideo = nil
	request.B2BSubmission = &first
	request.DeferredB2BImageToVideo = deferred

	result, err := fixture.usecase.CreateReserved(fixture.ctx, request)
	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(result)
	if len(result.Steps) != 2 || result.Steps[0].Route.Provider != creations.PolarStarB2BProvider || result.Steps[1].Route.Provider != creations.PolarStarB2BProvider || result.Steps[0].SubmitStatus != creations.StepSubmitStatusReady || result.Steps[1].SubmitStatus != creations.StepSubmitStatusBlocked {
		t.Fatalf("created B2B steps = %#v", result.Steps)
	}
	var recipe model.DeferredRecipeDocument
	if err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(fixture.ctx, bson.M{"_id": result.Steps[1].ID}).Decode(&recipe); err != nil {
		t.Fatal(err)
	}
	wantPayload, err := deferred.Recipe.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if recipe.Protocol != string(creations.DeferredRecipeProtocolB2B) || string(recipe.B2BRecipe) != string(wantPayload) || recipe.Digest != deferred.Digest || recipe.Status != string(creations.DeferredRecipeStatusPending) || recipe.ModelSKU != "" || len(recipe.InputTemplate) != 0 {
		t.Fatalf("stored B2B deferred recipe = %#v", recipe)
	}
	var event model.OutboxEventDocument
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(fixture.ctx, bson.M{"_id": outbox.SubmissionEventID(result.Steps[0].ID)}).Decode(&event); err != nil {
		t.Fatal(err)
	}
	storedFirst, err := creations.ParseB2BProductRecipe(event.Payload)
	if err != nil || storedFirst.ProductKey != first.ProductKey {
		t.Fatalf("first B2B event payload = %#v / %v", storedFirst, err)
	}
	count, err := fixture.database.Collection(schema.CollectionOutboxEvents).CountDocuments(fixture.ctx, bson.M{"_id": outbox.SubmissionEventID(result.Steps[1].ID)})
	if err != nil || count != 0 {
		t.Fatalf("second B2B event must not exist before owned frame: %d / %v", count, err)
	}
}

// TestMongoCreateReservedOutbox快照可被工作者首次提交验证创建侧和投递侧共用同一冻结技术合同。
// 它禁止测试绕过 CreateReserved 直接伪造完整中台 Execution，避免两侧合同悄然漂移。
func TestMongoCreateReservedOutbox快照可被工作者首次提交(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 20)
	request := fixture.imageRequest(userID, uuid.NewString())
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"worker-contract-prompt","assets":[],"parameters":{"width":1024}}`),
	}

	result, err := fixture.usecase.CreateReserved(fixture.ctx, request)
	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(result)
	fixture.trackInitialSubmissionEvent(result)
	eventID := outbox.SubmissionEventID(result.Steps[0].ID)
	var pending model.OutboxEventDocument
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&pending); err != nil {
		t.Fatalf("读取创建侧 outbox: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(pending.Payload, &payload); err != nil {
		t.Fatalf("解析创建侧技术载荷: %v", err)
	}
	if len(payload) != 3 || payload["capability"] == nil || payload["model_sku"] == nil || payload["input"] == nil {
		t.Fatalf("创建侧载荷字段 = %v，want capability/model_sku/input 三个技术字段", payload)
	}
	for _, forbidden := range []string{"diamond", "balance", "vip", "payment", "user"} {
		if strings.Contains(string(pending.Payload), forbidden) {
			t.Fatalf("创建侧技术载荷泄露业务字段 %q: %s", forbidden, pending.Payload)
		}
	}

	postCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, httpRequest *http.Request) {
		if httpRequest.Method != http.MethodPost || httpRequest.URL.Path != "/api/v2/executions" {
			t.Fatalf("首次提交请求 = %s %s，want POST /api/v2/executions", httpRequest.Method, httpRequest.URL.Path)
		}
		postCalls++
		var submitted platform.Execution
		if err := json.NewDecoder(httpRequest.Body).Decode(&submitted); err != nil {
			t.Fatalf("解析中台提交合同: %v", err)
		}
		if submitted.ExternalRef != result.Steps[0].ID || submitted.Capability != platform.CapabilityTextToImage || submitted.ModelSKU != "ps-image-v1" || submitted.Input.Prompt != "worker-contract-prompt" {
			t.Fatalf("中台提交合同 = %#v，want 创建侧冻结技术快照", submitted)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-created","status":"accepted"}}`))
	}))
	t.Cleanup(server.Close)
	client, err := platform.NewClientWithCallbackOrigin(
		server.URL,
		"test-generation-request-hmac-key-12345",
		"https://api.example.test",
		server.Client(),
		func() time.Time { return fixture.now },
		func() string { return "nonce-create-worker" },
	)
	if err != nil {
		t.Fatalf("创建测试中台客户端: %v", err)
	}
	submissionWorker := worker.NewGenerationSubmissionWorker(
		fixture.outboxRepository,
		NewGenerationSubmissionRepository(&Data{database: fixture.database}),
		fixture.ledgerUsecase,
		fixture.txRunner,
		worker.LocalClientRouter{Local: client},
		nil,
		"worker-create-contract",
		func() time.Time { return fixture.now },
	)

	if err := submissionWorker.DeliverOnce(fixture.ctx, eventID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 1 {
		t.Fatalf("首次提交 POST 次数 = %d，want 1", postCalls)
	}
	var delivered model.OutboxEventDocument
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&delivered); err != nil {
		t.Fatalf("读取结案 outbox: %v", err)
	}
	if delivered.DeliveryStatus != string(outbox.DeliveryStatusDelivered) || delivered.JobID != "job-created" {
		t.Fatalf("结案 outbox = %#v，want delivered/job-created", delivered)
	}
}

func TestMongoCreateReservedOutbox同幂等重放不新增事件(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 40)
	request := fixture.imageRequest(userID, uuid.NewString())
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"retry-once","assets":[],"parameters":{}}`),
	}

	first, err := fixture.usecase.CreateReserved(fixture.ctx, request)
	if err != nil {
		t.Fatalf("首次 CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(first)
	fixture.trackInitialSubmissionEvent(first)
	second, err := fixture.usecase.CreateReserved(fixture.ctx, request)
	if err != nil {
		t.Fatalf("重放 CreateReserved() error = %v", err)
	}
	if second.Creation.ID != first.Creation.ID {
		t.Fatalf("replayed creation ID = %q, want %q", second.Creation.ID, first.Creation.ID)
	}
	fixture.assertDocumentCount(schema.CollectionOutboxEvents, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(first.Steps[0].ID)}}, 1)
	fixture.assertDocumentCount(schema.CollectionOutboxEvents, bson.D{{Key: "aggregate_id", Value: first.Creation.ID}}, 1)
	fixture.assertDocumentCount(schema.CollectionReservations, bson.D{{Key: "_id", Value: "reservation:" + first.Creation.ID}}, 1)
	fixture.assertDocumentCount(schema.CollectionLedgerEntries, bson.D{{Key: "_id", Value: "reserve:" + first.Creation.ID}}, 1)
}

func TestMongoCreateReservedOutbox同键不同技术快照返回冲突(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 40)
	request := fixture.imageRequest(userID, uuid.NewString())
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"first-snapshot","assets":[],"parameters":{}}`),
	}
	first, err := fixture.usecase.CreateReserved(fixture.ctx, request)
	if err != nil {
		t.Fatalf("首次 CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(first)
	fixture.trackInitialSubmissionEvent(first)
	conflicting := request
	conflicting.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: request.InitialSubmission.ModelSKU,
		Input:    json.RawMessage(`{"prompt":"second-snapshot","assets":[],"parameters":{}}`),
	}

	_, err = fixture.usecase.CreateReserved(fixture.ctx, conflicting)

	if !errors.Is(err, creations.ErrCreationCommandConflict) {
		t.Fatalf("CreateReserved() error = %v, want ErrCreationCommandConflict", err)
	}
	fixture.assertDocumentCount(schema.CollectionOutboxEvents, bson.D{{Key: "aggregate_id", Value: first.Creation.ID}}, 1)
	fixture.assertAccountBalance(userID, 20)
}

func TestMongoCreateReservedOutbox预留失败时不遗留事件(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 19)
	request := fixture.imageRequest(userID, uuid.NewString())
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"insufficient-funds","assets":[],"parameters":{}}`),
	}

	_, err := fixture.usecase.CreateReserved(fixture.ctx, request)

	if !errors.Is(err, shared.ErrInsufficientFunds) {
		t.Fatalf("CreateReserved() error = %v, want ErrInsufficientFunds", err)
	}
	captures := fixture.capturingCreationRepository.Captures()
	if len(captures) != 1 || len(captures[0].StepIDs) != 1 {
		t.Fatalf("failed transaction captures = %#v, want one creation with one step", captures)
	}
	fixture.trackCapturedCreationFacts(captures[0])
	fixture.trackDocument(schema.CollectionOutboxEvents, outbox.SubmissionEventID(captures[0].StepIDs[0]))
	fixture.assertNoCreationFacts(request.IdempotencyKey, userID)
	fixture.assertDocumentCount(schema.CollectionOutboxEvents, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(captures[0].StepIDs[0])}}, 0)
}

func TestMongoCreateReservedOutboxWriter失败时事务回滚(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 20)
	writer := &failingOutboxWriter{failure: errors.New("模拟发件箱写入失败")}
	fixture.usecase = fixture.newUsecaseWithWriter(writer)
	request := fixture.imageRequest(userID, uuid.NewString())
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"writer-failure","assets":[],"parameters":{}}`),
	}

	_, err := fixture.usecase.CreateReserved(fixture.ctx, request)

	if !errors.Is(err, writer.failure) {
		t.Fatalf("CreateReserved() error = %v, want outbox failure", err)
	}
	if writer.calls != 1 {
		t.Fatalf("writer calls = %d, want 1", writer.calls)
	}
	captures := fixture.capturingCreationRepository.Captures()
	if len(captures) != 1 || len(captures[0].StepIDs) != 1 {
		t.Fatalf("failed transaction captures = %#v, want one creation with one step", captures)
	}
	fixture.trackCapturedCreationFacts(captures[0])
	fixture.trackDocument(schema.CollectionOutboxEvents, outbox.SubmissionEventID(captures[0].StepIDs[0]))
	fixture.assertDocumentCount(schema.CollectionCreations, bson.D{{Key: "_id", Value: captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionCreationSteps, bson.D{{Key: "_id", Value: captures[0].StepIDs[0]}}, 0)
	fixture.assertDocumentCount(schema.CollectionReservations, bson.D{{Key: "_id", Value: "reservation:" + captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionLedgerEntries, bson.D{{Key: "_id", Value: "reserve:" + captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionOutboxEvents, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(captures[0].StepIDs[0])}}, 0)
	fixture.assertAccountBalance(userID, 20)
}

func TestMongoCreateReserved延迟配方写入失败时事务回滚(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedActiveSubscription(userID, entitlement.SubscriptionBillingPeriodMonthly)
	fixture.seedDailyQuota(uuid.NewString(), userID, "vip_daily_video", 3, 0)
	writer := &failingDeferredRecipeWriter{failure: errors.New("recipe writer failure")}
	fixture.capturingCreationRepository.recipeWriter = writer
	fixture.usecase = fixture.newUsecaseWithWriter(fixture.outboxRepository)
	request := fixture.videoRequest(userID, uuid.NewString(), 5)

	_, err := fixture.usecase.CreateReserved(fixture.ctx, request)

	if !errors.Is(err, writer.failure) {
		t.Fatalf("CreateReserved() error = %v, want recipe writer failure", err)
	}
	if writer.calls != 1 {
		t.Fatalf("recipe writer calls = %d, want 1", writer.calls)
	}
	captures := fixture.capturingCreationRepository.Captures()
	if len(captures) != 1 || len(captures[0].StepIDs) != 2 {
		t.Fatalf("配方失败前必须捕获双步骤创作，captures = %#v", captures)
	}
	fixture.trackCapturedCreationFacts(captures[0])
	fixture.trackDocument(schema.CollectionOutboxEvents, outbox.SubmissionEventID(captures[0].StepIDs[0]))
	fixture.trackDocument(schema.CollectionGenerationStepRecipes, captures[0].StepIDs[1])
	fixture.assertDocumentCount(schema.CollectionCreations, bson.D{{Key: "_id", Value: captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionCreationSteps, bson.D{{Key: "creation_id", Value: captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionReservations, bson.D{{Key: "_id", Value: "reservation:" + captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionLedgerEntries, bson.D{{Key: "_id", Value: "reserve:" + captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionOutboxEvents, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(captures[0].StepIDs[0])}}, 0)
	fixture.assertDocumentCount(schema.CollectionGenerationStepRecipes, bson.D{{Key: "step_id", Value: captures[0].StepIDs[1]}}, 0)
}

func TestMongoCreateReserved有效VIP图片日免写入创作步骤预留和指纹(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	dailyQuotaID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedActiveSubscription(userID, entitlement.SubscriptionBillingPeriodMonthly)
	fixture.seedAccount(userID, 0)
	fixture.seedDailyQuota(dailyQuotaID, userID, "vip_daily_image", 10, 0)
	request := fixture.imageRequest(userID, uuid.NewString())
	wantFingerprint, err := creations.RequestFingerprint(request)
	if err != nil {
		t.Fatalf("RequestFingerprint() error = %v", err)
	}

	result, err := fixture.usecase.CreateReserved(fixture.ctx, request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(result)
	if result.Creation.Status != creations.CreationStatusPendingSubmission {
		t.Fatalf("Creation status = %q, want pending_submission", result.Creation.Status)
	}
	if result.Creation.RequestFingerprint != wantFingerprint {
		t.Fatalf("Creation request fingerprint = %q, want %q", result.Creation.RequestFingerprint, wantFingerprint)
	}
	if len(result.Steps) != 1 || result.Steps[0].Atom != creations.AtomTextToImage || result.Steps[0].SubmitStatus != creations.StepSubmitStatusReady {
		t.Fatalf("Steps = %#v, want one ready text_to_image step", result.Steps)
	}

	found, err := fixture.creationRepository.FindByIdempotencyKey(fixture.ctx, request.IdempotencyKey)
	if err != nil {
		t.Fatalf("FindByIdempotencyKey() error = %v", err)
	}
	if found == nil || found.RequestFingerprint != wantFingerprint {
		t.Fatalf("从仓储回读的创作 = %#v，要求包含 request_fingerprint", found)
	}

	var reservation model.ReservationDocument
	if err := fixture.database.Collection(schema.CollectionReservations).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: "reservation:" + result.Creation.ID}},
	).Decode(&reservation); err != nil {
		t.Fatalf("读取预留: %v", err)
	}
	if reservation.BenefitSource != string(ledger.BenefitSourceDailyQuota) || reservation.ReservedDiamonds != 0 {
		t.Fatalf("预留来源/扣钻 = %q/%d, want daily_quota/0", reservation.BenefitSource, reservation.ReservedDiamonds)
	}
	if reservation.QuotaKind != "vip_daily_image" || reservation.QuotaLimit != 10 || reservation.QuotaUnits != 1 {
		t.Fatalf("预留额度快照 = %#v, want vip_daily_image/10/1", reservation)
	}

	var quota model.DailyQuotaDocument
	if err := fixture.database.Collection(schema.CollectionDailyQuotas).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: dailyQuotaID}},
	).Decode(&quota); err != nil {
		t.Fatalf("读取日免额度: %v", err)
	}
	if quota.UsedCount != 1 {
		t.Fatalf("日免已用 = %d, want 1", quota.UsedCount)
	}
}

// TestMongoCreateReserved固定业务时刻写入全部账本事实验证事务重试时可复用的业务时刻
// 会同时写入创作、账户、预留和账本分录，不能让账本仓储在事务回调中另取当前时间。
func TestMongoCreateReserved固定业务时刻写入全部账本事实(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 20)

	result, err := fixture.usecase.CreateReserved(fixture.ctx, fixture.imageRequest(userID, uuid.NewString()))
	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(result)

	var account model.AccountDocument
	if err := fixture.database.Collection(schema.CollectionAccounts).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: userID}},
	).Decode(&account); err != nil {
		t.Fatalf("读取账户事实: %v", err)
	}
	var reservation model.ReservationDocument
	if err := fixture.database.Collection(schema.CollectionReservations).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: "reservation:" + result.Creation.ID}},
	).Decode(&reservation); err != nil {
		t.Fatalf("读取预留事实: %v", err)
	}
	var entry model.LedgerEntryDocument
	if err := fixture.database.Collection(schema.CollectionLedgerEntries).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: "reserve:" + result.Creation.ID}},
	).Decode(&entry); err != nil {
		t.Fatalf("读取账本分录: %v", err)
	}

	if !result.Creation.CreatedAt.Equal(fixture.now) || !result.Creation.UpdatedAt.Equal(fixture.now) {
		t.Fatalf("创作时刻 = created:%s updated:%s, want %s", result.Creation.CreatedAt, result.Creation.UpdatedAt, fixture.now)
	}
	if !account.UpdatedAt.Equal(fixture.now) {
		t.Fatalf("账户更新时间 = %s, want 固定业务时刻 %s", account.UpdatedAt, fixture.now)
	}
	if !reservation.CreatedAt.Equal(fixture.now) || !reservation.UpdatedAt.Equal(fixture.now) {
		t.Fatalf("预留时刻 = created:%s updated:%s, want %s", reservation.CreatedAt, reservation.UpdatedAt, fixture.now)
	}
	if !entry.CreatedAt.Equal(fixture.now) {
		t.Fatalf("账本分录创建时间 = %s, want 固定业务时刻 %s", entry.CreatedAt, fixture.now)
	}
}

// TestMongoCreateReservedVIP文生视频固定业务时刻写入日免与双步骤事实覆盖日免预留路径。
// 文生视频必须先创建首帧步骤，再创建图生视频步骤；所有持久化时间均复用创作事务外冻结的时刻。
func TestMongoCreateReservedVIP文生视频固定业务时刻写入日免与双步骤事实(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	dailyQuotaID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedActiveSubscription(userID, entitlement.SubscriptionBillingPeriodMonthly)
	fixture.seedDailyQuota(dailyQuotaID, userID, "vip_daily_video", 3, 0)
	request := fixture.videoRequest(userID, uuid.NewString(), 10)
	request.Plan = []creations.StepPlan{
		{Sequence: 1, Atom: creations.AtomTextToImage},
		{Sequence: 2, Atom: creations.AtomImageToVideo},
	}
	request.DeferredImageToVideo = &creations.DeferredImageToVideo{
		ModelSKU:      "ps-auto",
		InputTemplate: json.RawMessage(`{"prompt":"move","parameters":{"durationSeconds":10}}`),
	}

	result, err := fixture.usecase.CreateReserved(fixture.ctx, request)
	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(result)
	fixture.trackDocument(schema.CollectionGenerationStepRecipes, result.Steps[1].ID)
	if len(result.Steps) != 2 || result.Steps[0].Atom != creations.AtomTextToImage || result.Steps[1].Atom != creations.AtomImageToVideo {
		t.Fatalf("创作步骤 = %#v, want text_to_image then image_to_video", result.Steps)
	}

	var creation model.CreationDocument
	if err := fixture.database.Collection(schema.CollectionCreations).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: result.Creation.ID}},
	).Decode(&creation); err != nil {
		t.Fatalf("读取创作事实: %v", err)
	}
	if !creation.CreatedAt.Equal(fixture.now) || !creation.UpdatedAt.Equal(fixture.now) {
		t.Fatalf("创作时刻 = created:%s updated:%s, want %s", creation.CreatedAt, creation.UpdatedAt, fixture.now)
	}
	for _, step := range result.Steps {
		var storedStep model.CreationStepDocument
		if err := fixture.database.Collection(schema.CollectionCreationSteps).FindOne(
			fixture.ctx,
			bson.D{{Key: "_id", Value: step.ID}},
		).Decode(&storedStep); err != nil {
			t.Fatalf("读取步骤 %q: %v", step.ID, err)
		}
		if !storedStep.CreatedAt.Equal(fixture.now) {
			t.Fatalf("步骤 %q 创建时间 = %s, want %s", step.ID, storedStep.CreatedAt, fixture.now)
		}
	}
	var recipe model.DeferredRecipeDocument
	if err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(
		fixture.ctx,
		bson.D{{Key: "step_id", Value: result.Steps[1].ID}},
	).Decode(&recipe); err != nil {
		t.Fatalf("读取冻结配方: %v", err)
	}
	if !recipe.CreatedAt.Equal(fixture.now) || !recipe.UpdatedAt.Equal(fixture.now) {
		t.Fatalf("冻结配方时刻 = created:%s updated:%s, want %s", recipe.CreatedAt, recipe.UpdatedAt, fixture.now)
	}

	var quota model.DailyQuotaDocument
	if err := fixture.database.Collection(schema.CollectionDailyQuotas).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: dailyQuotaID}},
	).Decode(&quota); err != nil {
		t.Fatalf("读取日免额度: %v", err)
	}
	if quota.UsedCount != 2 || !quota.UpdatedAt.Equal(fixture.now) {
		t.Fatalf("日免事实 = used:%d updated:%s, want used:2 updated:%s", quota.UsedCount, quota.UpdatedAt, fixture.now)
	}

	var reservation model.ReservationDocument
	if err := fixture.database.Collection(schema.CollectionReservations).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: "reservation:" + result.Creation.ID}},
	).Decode(&reservation); err != nil {
		t.Fatalf("读取预留事实: %v", err)
	}
	if !reservation.CreatedAt.Equal(fixture.now) || !reservation.UpdatedAt.Equal(fixture.now) {
		t.Fatalf("预留时刻 = created:%s updated:%s, want %s", reservation.CreatedAt, reservation.UpdatedAt, fixture.now)
	}
	var entry model.LedgerEntryDocument
	if err := fixture.database.Collection(schema.CollectionLedgerEntries).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: "reserve:" + result.Creation.ID}},
	).Decode(&entry); err != nil {
		t.Fatalf("读取账本分录: %v", err)
	}
	if !entry.CreatedAt.Equal(fixture.now) {
		t.Fatalf("账本分录创建时间 = %s, want %s", entry.CreatedAt, fixture.now)
	}
}

func TestMongoCreateReserved余额不足时不写入创作步骤预留或分录(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 19)
	request := fixture.imageRequest(userID, uuid.NewString())

	_, err := fixture.usecase.CreateReserved(fixture.ctx, request)

	if !errors.Is(err, shared.ErrInsufficientFunds) {
		t.Fatalf("CreateReserved() error = %v, want ErrInsufficientFunds", err)
	}
	captures := fixture.capturingCreationRepository.Captures()
	if len(captures) != 1 || captures[0].CreationID == "" {
		t.Fatalf("余额不足前必须捕获一次创作写入，captures = %#v", captures)
	}
	fixture.trackCapturedCreationFacts(captures[0])
	fixture.assertDocumentCount(schema.CollectionCreations, bson.D{{Key: "_id", Value: captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionCreationSteps, bson.D{{Key: "creation_id", Value: captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionReservations, bson.D{{Key: "_id", Value: "reservation:" + captures[0].CreationID}}, 0)
	fixture.assertDocumentCount(schema.CollectionLedgerEntries, bson.D{{Key: "_id", Value: "reserve:" + captures[0].CreationID}}, 0)
	fixture.assertAccountBalance(userID, 19)
}

func TestMongoCreateReserved有效VIP十秒视频冻结两个日免单位(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	dailyQuotaID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedActiveSubscription(userID, entitlement.SubscriptionBillingPeriodMonthly)
	fixture.seedAccount(userID, 0)
	fixture.seedDailyQuota(dailyQuotaID, userID, "vip_daily_video", 3, 0)
	request := fixture.videoRequest(userID, uuid.NewString(), 10)

	result, err := fixture.usecase.CreateReserved(fixture.ctx, request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	fixture.trackCreationResult(result)
	var reservation model.ReservationDocument
	if err := fixture.database.Collection(schema.CollectionReservations).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: "reservation:" + result.Creation.ID}},
	).Decode(&reservation); err != nil {
		t.Fatalf("读取预留: %v", err)
	}
	if reservation.QuotaKind != "vip_daily_video" || reservation.QuotaLimit != 3 || reservation.QuotaUnits != 2 {
		t.Fatalf("十秒视频额度快照 = %#v, want vip_daily_video/3/2", reservation)
	}

	var quota model.DailyQuotaDocument
	if err := fixture.database.Collection(schema.CollectionDailyQuotas).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: dailyQuotaID}},
	).Decode(&quota); err != nil {
		t.Fatalf("读取日免额度: %v", err)
	}
	if quota.UsedCount != 2 {
		t.Fatalf("十秒视频日免已用 = %d, want 2", quota.UsedCount)
	}
}

func TestMongoCreateReserved并发相同幂等键只持久化一份事实(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	userID := uuid.NewString()
	fixture.seedBoundUser(userID)
	fixture.seedAccount(userID, 20)
	request := fixture.imageRequest(userID, uuid.NewString())

	start := make(chan struct{})
	results := make(chan creationReservedResult, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			result, err := fixture.usecase.CreateReserved(fixture.ctx, request)
			results <- creationReservedResult{result: result, err: err}
		}()
	}
	close(start)
	waitGroup.Wait()
	close(results)

	var creationID string
	for item := range results {
		if item.err != nil {
			t.Fatalf("并发 CreateReserved() error = %v", item.err)
		}
		if item.result == nil || item.result.Creation == nil {
			t.Fatalf("并发 CreateReserved() result = %#v, want creation", item.result)
		}
		fixture.trackCreationResult(item.result)
		if creationID == "" {
			creationID = item.result.Creation.ID
			continue
		}
		if item.result.Creation.ID != creationID {
			t.Fatalf("并发创作 ID = %q, want %q", item.result.Creation.ID, creationID)
		}
	}

	fixture.assertDocumentCount(schema.CollectionCreations, bson.D{{Key: "idempotency_key", Value: request.IdempotencyKey}}, 1)
	fixture.assertDocumentCount(schema.CollectionCreationSteps, bson.D{{Key: "creation_id", Value: creationID}}, 1)
	fixture.assertDocumentCount(schema.CollectionReservations, bson.D{{Key: "_id", Value: "reservation:" + creationID}}, 1)
	fixture.assertDocumentCount(schema.CollectionLedgerEntries, bson.D{{Key: "_id", Value: "reserve:" + creationID}}, 1)
	fixture.assertAccountBalance(userID, 0)
}

func TestMongo创作和订阅读仓储的缺失与步骤排序语义(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	missingKey := "missing-" + uuid.NewString()
	missingUserID := uuid.NewString()

	missingCreation, err := fixture.creationRepository.FindByIdempotencyKey(fixture.ctx, missingKey)
	if err != nil {
		t.Fatalf("FindByIdempotencyKey() error = %v", err)
	}
	if missingCreation != nil {
		t.Fatalf("FindByIdempotencyKey() = %#v, want nil for missing creation", missingCreation)
	}
	missingSubscription, err := fixture.subscriptionReader.FindByUserID(fixture.ctx, missingUserID)
	if err != nil {
		t.Fatalf("FindByUserID() error = %v", err)
	}
	if missingSubscription != nil {
		t.Fatalf("FindByUserID() = %#v, want nil for missing projection", missingSubscription)
	}

	creationID := uuid.NewString()
	firstStepID := uuid.NewString()
	secondStepID := uuid.NewString()
	fixture.trackDocument(schema.CollectionCreations, creationID)
	fixture.trackDocument(schema.CollectionCreationSteps, firstStepID)
	fixture.trackDocument(schema.CollectionCreationSteps, secondStepID)
	if _, err := fixture.database.Collection(schema.CollectionCreations).InsertOne(fixture.ctx, model.CreationDocument{
		ID:                 creationID,
		IdempotencyKey:     "steps-" + uuid.NewString(),
		UserID:             uuid.NewString(),
		TemplateID:         "template-1",
		TemplateVersion:    1,
		RequestFingerprint: strings.Repeat("a", 64),
		Status:             string(creations.CreationStatusPendingSubmission),
		Version:            1,
		CreatedAt:          fixture.now,
		UpdatedAt:          fixture.now,
	}); err != nil {
		t.Fatalf("种入测试创作: %v", err)
	}
	if _, err := fixture.database.Collection(schema.CollectionCreationSteps).InsertOne(fixture.ctx, model.CreationStepDocument{
		ID:           secondStepID,
		CreationID:   creationID,
		Sequence:     2,
		Atom:         string(creations.AtomImageToVideo),
		SubmitStatus: string(creations.StepSubmitStatusBlocked),
		CreatedAt:    fixture.now,
	}); err != nil {
		t.Fatalf("种入第二步: %v", err)
	}
	if _, err := fixture.database.Collection(schema.CollectionCreationSteps).InsertOne(fixture.ctx, model.CreationStepDocument{
		ID:           firstStepID,
		CreationID:   creationID,
		Sequence:     1,
		Atom:         string(creations.AtomTextToImage),
		SubmitStatus: string(creations.StepSubmitStatusReady),
		CreatedAt:    fixture.now,
	}); err != nil {
		t.Fatalf("种入第一步: %v", err)
	}

	steps, err := fixture.creationRepository.ListSteps(fixture.ctx, creationID)
	if err != nil {
		t.Fatalf("ListSteps() error = %v", err)
	}
	if len(steps) != 2 || steps[0].Sequence != 1 || steps[1].Sequence != 2 {
		t.Fatalf("ListSteps() = %#v, want sequence ascending", steps)
	}
}

func TestMongo创作仓储重复创作键同时保留领域和驱动错误(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	creationID := uuid.NewString()
	stepID := uuid.NewString()
	fixture.trackDocument(schema.CollectionCreations, creationID)
	fixture.trackDocument(schema.CollectionCreationSteps, stepID)
	creation := &creations.Creation{
		ID:                 creationID,
		IdempotencyKey:     "duplicate-creation-" + uuid.NewString(),
		UserID:             uuid.NewString(),
		TemplateID:         "template-1",
		TemplateVersion:    1,
		RequestFingerprint: strings.Repeat("a", 64),
		Status:             creations.CreationStatusPendingSubmission,
		Version:            1,
		CreatedAt:          fixture.now,
		UpdatedAt:          fixture.now,
	}
	steps := []creations.CreationStep{{
		ID:           stepID,
		CreationID:   creationID,
		Sequence:     1,
		Atom:         creations.AtomTextToImage,
		SubmitStatus: creations.StepSubmitStatusReady,
		CreatedAt:    fixture.now,
	}}
	repository := fixture.capturingCreationRepository.repository
	if err := repository.Create(fixture.ctx, creation, steps); err != nil {
		t.Fatalf("首次 Create() error = %v", err)
	}

	err := repository.Create(fixture.ctx, creation, steps)

	if !errors.Is(err, creations.ErrCreationAlreadyExists) {
		t.Fatalf("Create() error = %v，要求包含 ErrCreationAlreadyExists", err)
	}
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("Create() error = %v，要求保留 Mongo duplicate key 错误链", err)
	}
}

func TestMongo创作仓储重复步骤键同时保留领域和驱动错误(t *testing.T) {
	fixture := newCreationMongoFixture(t)
	firstCreationID := uuid.NewString()
	firstStepID := uuid.NewString()
	secondCreationID := uuid.NewString()
	secondStepID := uuid.NewString()
	fixture.trackDocument(schema.CollectionCreations, firstCreationID)
	fixture.trackDocument(schema.CollectionCreationSteps, firstStepID)
	fixture.trackDocument(schema.CollectionCreations, secondCreationID)
	fixture.trackDocument(schema.CollectionCreationSteps, secondStepID)
	repository := fixture.capturingCreationRepository.repository
	firstCreation := newDuplicateKeyTestCreation(fixture.now, firstCreationID)
	if err := repository.Create(fixture.ctx, firstCreation, []creations.CreationStep{{
		ID:           firstStepID,
		CreationID:   firstCreationID,
		Sequence:     1,
		Atom:         creations.AtomTextToImage,
		SubmitStatus: creations.StepSubmitStatusReady,
		CreatedAt:    fixture.now,
	}}); err != nil {
		t.Fatalf("首次 Create() error = %v", err)
	}
	secondCreation := newDuplicateKeyTestCreation(fixture.now, secondCreationID)
	err := repository.Create(fixture.ctx, secondCreation, []creations.CreationStep{{
		ID:           secondStepID,
		CreationID:   firstCreationID,
		Sequence:     1,
		Atom:         creations.AtomTextToImage,
		SubmitStatus: creations.StepSubmitStatusReady,
		CreatedAt:    fixture.now,
	}})

	if !errors.Is(err, creations.ErrCreationAlreadyExists) {
		t.Fatalf("Create() error = %v，要求包含 ErrCreationAlreadyExists", err)
	}
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("Create() error = %v，要求保留 Mongo duplicate key 错误链", err)
	}
}

func newDuplicateKeyTestCreation(now time.Time, creationID string) *creations.Creation {
	return &creations.Creation{
		ID:                 creationID,
		IdempotencyKey:     "duplicate-step-" + uuid.NewString(),
		UserID:             uuid.NewString(),
		TemplateID:         "template-1",
		TemplateVersion:    1,
		RequestFingerprint: strings.Repeat("a", 64),
		Status:             creations.CreationStatusPendingSubmission,
		Version:            1,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

type creationReservedResult struct {
	result *creations.CreateReservedResult
	err    error
}

type creationMongoFixture struct {
	t                           *testing.T
	ctx                         context.Context
	database                    *mongo.Database
	now                         time.Time
	txRunner                    *MongoTxRunner
	userRepository              identity.UserRepository
	creationRepository          creations.Repository
	capturingCreationRepository *capturingCreationRepository
	subscriptionReader          entitlement.SubscriptionReader
	entitlementUsecase          *entitlement.Usecase
	ledgerUsecase               *ledger.Usecase
	outboxRepository            outbox.Repository
	usecase                     *creations.Usecase
}

func newCreationMongoFixture(t *testing.T) *creationMongoFixture {
	t.Helper()
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	t.Cleanup(cancel)
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	data := &Data{client: client, database: database}
	txRunner := NewMongoTxRunner(client)
	userRepository := NewUserRepository(data)
	subscriptionRepository := NewSubscriptionRepository(data)
	capturingCreationRepository := newCapturingCreationRepository(NewCreationRepository(data))
	ledgerRepository := NewLedgerRepository(data)
	outboxRepository := NewOutboxRepository(data)
	entitlementUsecase := entitlement.NewUsecase()
	ledgerUsecase := ledger.NewUsecase(ledgerRepository, txRunner)
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

	return &creationMongoFixture{
		t:                           t,
		ctx:                         ctx,
		database:                    database,
		now:                         now,
		txRunner:                    txRunner,
		userRepository:              userRepository,
		creationRepository:          capturingCreationRepository,
		capturingCreationRepository: capturingCreationRepository,
		subscriptionReader:          subscriptionRepository,
		entitlementUsecase:          entitlementUsecase,
		ledgerUsecase:               ledgerUsecase,
		outboxRepository:            outboxRepository,
		usecase: creations.NewUsecaseWithClock(
			userRepository,
			subscriptionRepository,
			entitlementUsecase,
			capturingCreationRepository,
			ledgerUsecase,
			outboxRepository,
			txRunner,
			nil,
			func() time.Time { return now },
		),
	}
}

func (fixture *creationMongoFixture) newUsecaseWithWriter(writer outbox.Writer) *creations.Usecase {
	fixture.t.Helper()
	return creations.NewUsecaseWithClock(
		fixture.userRepository,
		fixture.subscriptionReader,
		fixture.entitlementUsecase,
		fixture.capturingCreationRepository,
		fixture.ledgerUsecase,
		writer,
		fixture.txRunner,
		nil,
		func() time.Time { return fixture.now },
	)
}

func (fixture *creationMongoFixture) newUsecaseWithAdmissions(admissions creations.AdmissionResolver) *creations.Usecase {
	fixture.t.Helper()
	return creations.NewUsecaseWithClock(
		fixture.userRepository,
		fixture.subscriptionReader,
		fixture.entitlementUsecase,
		fixture.capturingCreationRepository,
		fixture.ledgerUsecase,
		fixture.outboxRepository,
		fixture.txRunner,
		admissions,
		func() time.Time { return fixture.now },
	)
}

type twoStepB2BCreationAdmissionResolver struct{}

func (twoStepB2BCreationAdmissionResolver) ResolveAdmission(_ context.Context, request creations.AdmissionRequest) (creations.StepAdmission, error) {
	if request.B2B == nil {
		return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
	}
	recipe, err := request.B2B.Normalize()
	if err != nil {
		return creations.StepAdmission{}, err
	}
	return creations.StepAdmission{Route: creations.ExecutionRoute{
		Provider: creations.PolarStarB2BProvider, AccountRef: "account-main", ContractVersion: creations.B2BContractVersion, MappingVersion: "mapping-video-v1",
	}, B2B: &recipe}, nil
}

func (resolver twoStepB2BCreationAdmissionResolver) ResolveAdmissions(ctx context.Context, requests []creations.AdmissionRequest) ([]creations.StepAdmission, error) {
	admissions := make([]creations.StepAdmission, 0, len(requests))
	for _, request := range requests {
		admission, err := resolver.ResolveAdmission(ctx, request)
		if err != nil {
			return nil, err
		}
		admissions = append(admissions, admission)
	}
	return admissions, nil
}

func (fixture *creationMongoFixture) imageRequest(userID, idempotencyKey string) creations.CreateReservedRequest {
	return creations.CreateReservedRequest{
		UserID:          userID,
		IdempotencyKey:  idempotencyKey,
		TemplateID:      "template-image-1",
		TemplateVersion: 1,
		Product: entitlement.GenerationRequest{
			Output: entitlement.ProductOutputImage,
		},
		InputDigest: strings.Repeat("a", 64),
		Plan:        []creations.StepPlan{{Sequence: 1, Atom: creations.AtomTextToImage}},
	}
}

func (fixture *creationMongoFixture) videoRequest(userID, idempotencyKey string, durationSeconds int32) creations.CreateReservedRequest {
	return creations.CreateReservedRequest{
		UserID:          userID,
		IdempotencyKey:  idempotencyKey,
		TemplateID:      "template-video-1",
		TemplateVersion: 1,
		Product: entitlement.GenerationRequest{
			Output: entitlement.ProductOutputVideo,
			Video: entitlement.VideoOptions{
				DurationSeconds:     durationSeconds,
				ReferenceImageCount: 1,
			},
		},
		InputDigest: strings.Repeat("b", 64),
		Plan: []creations.StepPlan{
			{Sequence: 1, Atom: creations.AtomTextToImage},
			{Sequence: 2, Atom: creations.AtomImageToVideo},
		},
		InitialSubmission: &creations.InitialSubmission{
			ModelSKU: "ps-image-v1",
			Input:    json.RawMessage(`{"prompt":"first","assets":[],"parameters":{}}`),
		},
		DeferredImageToVideo: &creations.DeferredImageToVideo{
			ModelSKU:      "ps-auto",
			InputTemplate: json.RawMessage(`{"prompt":"move","parameters":{"durationSeconds":5}}`),
		},
	}
}

func (fixture *creationMongoFixture) seedBoundUser(userID string) {
	fixture.trackDocument(schema.CollectionUsers, userID)
	if _, err := fixture.database.Collection(schema.CollectionUsers).InsertOne(fixture.ctx, model.UserDocument{
		ID:             userID,
		AccountStatus:  string(identity.AccountStatusNormal),
		BindingState:   string(identity.BindingStateBound),
		Timezone:       "Asia/Shanghai",
		SessionVersion: 1,
		ContentAccess:  "standard",
		CreatedAt:      fixture.now,
		UpdatedAt:      fixture.now,
	}); err != nil {
		fixture.t.Fatalf("种入测试用户: %v", err)
	}
}

func (fixture *creationMongoFixture) seedActiveSubscription(userID string, period entitlement.SubscriptionBillingPeriod) {
	fixture.trackDocument(schema.CollectionSubscriptions, userID)
	if _, err := fixture.database.Collection(schema.CollectionSubscriptions).InsertOne(fixture.ctx, model.SubscriptionDocument{
		UserID:        userID,
		Status:        string(entitlement.SubscriptionStatusActive),
		BillingPeriod: string(period),
		StartsAt:      fixture.now.Add(-24 * time.Hour),
		ExpiresAt:     fixture.now.Add(30 * 24 * time.Hour),
		CreatedAt:     fixture.now.Add(-24 * time.Hour),
		UpdatedAt:     fixture.now,
	}); err != nil {
		fixture.t.Fatalf("种入测试订阅投影: %v", err)
	}
}

func (fixture *creationMongoFixture) seedAccount(userID string, balance int64) {
	fixture.trackDocument(schema.CollectionAccounts, userID)
	if _, err := fixture.database.Collection(schema.CollectionAccounts).InsertOne(fixture.ctx, model.AccountDocument{
		ID:             userID,
		DiamondBalance: balance,
		CreatedAt:      fixture.now,
		UpdatedAt:      fixture.now,
	}); err != nil {
		fixture.t.Fatalf("种入测试账户: %v", err)
	}
}

func (fixture *creationMongoFixture) seedDailyQuota(id, userID, kind string, limit, used int32) {
	fixture.trackDocument(schema.CollectionDailyQuotas, id)
	if _, err := fixture.database.Collection(schema.CollectionDailyQuotas).InsertOne(fixture.ctx, model.DailyQuotaDocument{
		ID:        id,
		UserID:    userID,
		QuotaKind: kind,
		LocalDate: fixture.now.In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02"),
		UsedCount: used,
		Limit:     limit,
		UpdatedAt: fixture.now,
	}); err != nil {
		fixture.t.Fatalf("种入测试日免额度: %v", err)
	}
}

func (fixture *creationMongoFixture) trackCreationResult(result *creations.CreateReservedResult) {
	fixture.t.Helper()
	if result == nil || result.Creation == nil {
		fixture.t.Fatal("缺少需要清理的创作结果")
	}
	fixture.trackDocument(schema.CollectionCreations, result.Creation.ID)
	for _, step := range result.Steps {
		fixture.trackDocument(schema.CollectionCreationSteps, step.ID)
	}
	if len(result.Steps) == 2 {
		fixture.trackDocument(schema.CollectionGenerationStepRecipes, result.Steps[1].ID)
		fixture.trackDocument(schema.CollectionOutboxEvents, outbox.SubmissionEventID(result.Steps[0].ID))
	}
	fixture.trackDocument(schema.CollectionReservations, "reservation:"+result.Creation.ID)
	fixture.trackDocument(schema.CollectionLedgerEntries, "reserve:"+result.Creation.ID)
}

func (fixture *creationMongoFixture) trackInitialSubmissionEvent(result *creations.CreateReservedResult) {
	fixture.t.Helper()
	if result == nil || len(result.Steps) == 0 {
		fixture.t.Fatal("缺少需要清理的首步骤提交事件")
	}
	fixture.trackDocument(schema.CollectionOutboxEvents, outbox.SubmissionEventID(result.Steps[0].ID))
}

func (fixture *creationMongoFixture) trackCapturedCreationFacts(capture capturedCreationFacts) {
	fixture.t.Helper()
	fixture.trackDocument(schema.CollectionCreations, capture.CreationID)
	for _, stepID := range capture.StepIDs {
		fixture.trackDocument(schema.CollectionCreationSteps, stepID)
	}
	fixture.trackDocument(schema.CollectionReservations, "reservation:"+capture.CreationID)
	fixture.trackDocument(schema.CollectionLedgerEntries, "reserve:"+capture.CreationID)
}

type capturedCreationFacts struct {
	CreationID string
	StepIDs    []string
}

type capturingCreationRepository struct {
	repository   creations.Repository
	recipeWriter creations.DeferredRecipeWriter
	mutex        sync.Mutex
	captures     []capturedCreationFacts
}

type failingOutboxWriter struct {
	failure error
	calls   int
}

func (writer *failingOutboxWriter) Enqueue(_ context.Context, _ *outbox.Event) error {
	writer.calls++
	return writer.failure
}

type failingDeferredRecipeWriter struct {
	failure error
	calls   int
}

func (writer *failingDeferredRecipeWriter) Create(_ context.Context, _ *creations.DeferredRecipe) error {
	writer.calls++
	return writer.failure
}

func newCapturingCreationRepository(repository creations.Repository) *capturingCreationRepository {
	provider, ok := repository.(interface {
		DeferredRecipeWriter() creations.DeferredRecipeWriter
	})
	if !ok {
		return &capturingCreationRepository{repository: repository}
	}
	return &capturingCreationRepository{repository: repository, recipeWriter: provider.DeferredRecipeWriter()}
}

func (repository *capturingCreationRepository) DeferredRecipeWriter() creations.DeferredRecipeWriter {
	return repository.recipeWriter
}

func (repository *capturingCreationRepository) FindByIdempotencyKey(ctx context.Context, key string) (*creations.Creation, error) {
	return repository.repository.FindByIdempotencyKey(ctx, key)
}

func (repository *capturingCreationRepository) ListSteps(ctx context.Context, creationID string) ([]creations.CreationStep, error) {
	return repository.repository.ListSteps(ctx, creationID)
}

func (repository *capturingCreationRepository) Create(ctx context.Context, creation *creations.Creation, steps []creations.CreationStep) error {
	capture := capturedCreationFacts{CreationID: creation.ID, StepIDs: make([]string, 0, len(steps))}
	for _, step := range steps {
		capture.StepIDs = append(capture.StepIDs, step.ID)
	}
	repository.mutex.Lock()
	repository.captures = append(repository.captures, capture)
	repository.mutex.Unlock()
	return repository.repository.Create(ctx, creation, steps)
}

func (repository *capturingCreationRepository) Captures() []capturedCreationFacts {
	repository.mutex.Lock()
	defer repository.mutex.Unlock()
	captures := make([]capturedCreationFacts, 0, len(repository.captures))
	for _, capture := range repository.captures {
		copyCapture := capturedCreationFacts{
			CreationID: capture.CreationID,
			StepIDs:    append([]string(nil), capture.StepIDs...),
		}
		captures = append(captures, copyCapture)
	}
	return captures
}

var _ creations.Repository = (*capturingCreationRepository)(nil)

func (fixture *creationMongoFixture) trackDocument(collection, id string) {
	fixture.t.Helper()
	fixture.t.Cleanup(func() {
		cleanupContext, cancel := newMongoTestContext()
		defer cancel()
		if _, err := fixture.database.Collection(collection).DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: id}}); err != nil {
			fixture.t.Errorf("按测试 _id 清理集合 %q 中的 %q: %v", collection, id, err)
		}
	})
}

func (fixture *creationMongoFixture) assertNoCreationFacts(idempotencyKey, userID string) {
	fixture.t.Helper()
	fixture.assertDocumentCount(schema.CollectionCreations, bson.D{{Key: "idempotency_key", Value: idempotencyKey}}, 0)
	fixture.assertStepCountForIdempotencyKey(idempotencyKey, 0)
	fixture.assertDocumentCount(schema.CollectionReservations, bson.D{{Key: "user_id", Value: userID}}, 0)
	fixture.assertDocumentCount(schema.CollectionLedgerEntries, bson.D{{Key: "account_id", Value: userID}}, 0)
}

func (fixture *creationMongoFixture) assertStepCountForIdempotencyKey(idempotencyKey string, want int64) {
	fixture.t.Helper()
	cursor, err := fixture.database.Collection(schema.CollectionCreations).Aggregate(fixture.ctx, mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{{Key: "idempotency_key", Value: idempotencyKey}}}},
		bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: schema.CollectionCreationSteps},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "creation_id"},
			{Key: "as", Value: "steps"},
		}}},
		bson.D{{Key: "$unwind", Value: "$steps"}},
		bson.D{{Key: "$count", Value: "count"}},
	})
	if err != nil {
		fixture.t.Fatalf("按幂等键统计步骤: %v", err)
	}
	defer cursor.Close(fixture.ctx)
	var rows []struct {
		Count int64 `bson:"count"`
	}
	if err := cursor.All(fixture.ctx, &rows); err != nil {
		fixture.t.Fatalf("读取步骤统计: %v", err)
	}
	got := int64(0)
	if len(rows) == 1 {
		got = rows[0].Count
	}
	if got != want {
		fixture.t.Fatalf("幂等键 %q 的步骤数 = %d, want %d", idempotencyKey, got, want)
	}
}

func (fixture *creationMongoFixture) assertDocumentCount(collection string, filter bson.D, want int64) {
	fixture.t.Helper()
	got, err := fixture.database.Collection(collection).CountDocuments(fixture.ctx, filter)
	if err != nil {
		fixture.t.Fatalf("统计集合 %q: %v", collection, err)
	}
	if got != want {
		fixture.t.Fatalf("集合 %q 文档数 = %d, want %d, filter=%#v", collection, got, want, filter)
	}
}

func (fixture *creationMongoFixture) assertAccountBalance(userID string, want int64) {
	fixture.t.Helper()
	var account model.AccountDocument
	if err := fixture.database.Collection(schema.CollectionAccounts).FindOne(
		fixture.ctx,
		bson.D{{Key: "_id", Value: userID}},
	).Decode(&account); err != nil {
		fixture.t.Fatalf("读取测试账户: %v", err)
	}
	if account.DiamondBalance != want {
		fixture.t.Fatalf("账户钻石余额 = %d, want %d", account.DiamondBalance, want)
	}
}
