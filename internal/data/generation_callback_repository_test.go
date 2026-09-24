package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"ai-business-service/internal/executionv2"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestMongoCallback首帧完成只创建一次第二步事件(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createTwoStepVideoWithRecipe()
	firstNonce := "nonce-frame-" + uuid.NewString()
	secondNonce := "nonce-frame-" + uuid.NewString()
	fixture.track(schema.CollectionAssets, "asset:"+fixture.firstStepID+":result")
	fixture.track(schema.CollectionOutboxEvents, outbox.SubmissionEventID(fixture.secondStepID))
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+firstNonce)
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+secondNonce)
	conflictingNonce := "nonce-frame-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+conflictingNonce)

	first := fixture.completedFirstFrame(firstNonce)
	if err := fixture.usecase.Handle(fixture.ctx, first); err != nil {
		t.Fatalf("确认首帧回调: %v", err)
	}
	firstReceipt := fixture.readReceipt(firstNonce)
	fixture.assertSecondStepReadyAndOnePendingEvent()

	if err := fixture.usecase.Handle(fixture.ctx, first); err != nil {
		t.Fatalf("重放同一首帧回调: %v", err)
	}
	sameNonceDifferentResult := first
	sameNonceDifferentResult.ResultURL = fixture.alternateResultURL()
	if err := fixture.usecase.Handle(fixture.ctx, sameNonceDifferentResult); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("同 nonce 不同结果 = %v，期望无效回调", err)
	}
	sameNonceDifferentTerminal := first
	sameNonceDifferentTerminal.Terminal = generation.CallbackTerminalFailed
	sameNonceDifferentTerminal.MediaType = ""
	sameNonceDifferentTerminal.ResultURL = ""
	if err := fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.store.Fail(txCtx, generation.FailedCallback{
			CreationID: fixture.creationID, StepID: sameNonceDifferentTerminal.StepID, ExternalRef: sameNonceDifferentTerminal.ExternalRef,
			JobID: sameNonceDifferentTerminal.JobID, Capability: sameNonceDifferentTerminal.Capability, Terminal: sameNonceDifferentTerminal.Terminal,
			NonceHash: sameNonceDifferentTerminal.NonceHash, PayloadDigest: sameNonceDifferentTerminal.PayloadDigest,
			CallbackVersion: sameNonceDifferentTerminal.CallbackVersion, At: fixture.now,
		})
	}); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("同 nonce 不同终态 = %v，期望无效回调", err)
	}
	if receipt := fixture.readReceipt(firstNonce); !receipt.ReceivedAt.Equal(firstReceipt.ReceivedAt) {
		t.Fatal("冲突重放修改了首次回执时间")
	}
	sameNonceDifferentDigest := first
	sameNonceDifferentDigest.PayloadDigest = "digest-frame-conflict"
	if err := fixture.usecase.Handle(fixture.ctx, sameNonceDifferentDigest); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("同 nonce 不同摘要 = %v，期望无效回调", err)
	}
	second := fixture.completedFirstFrame(secondNonce)
	if err := fixture.usecase.Handle(fixture.ctx, second); err != nil {
		t.Fatalf("重放相同首帧终态: %v", err)
	}
	conflicting := fixture.completedFirstFrame(conflictingNonce)
	conflicting.ResultURL = fixture.alternateResultURL()
	if err := fixture.usecase.Handle(fixture.ctx, conflicting); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("不同首帧结果终态 = %v，期望无效回调", err)
	}
	fixture.assertSecondStepReadyAndOnePendingEvent()
	fixture.assertCount(schema.CollectionAssets, bson.D{{Key: "_id", Value: "asset:" + fixture.firstStepID + ":result"}}, 1)
	fixture.assertCount(schema.CollectionCallbackReceipts, bson.D{{Key: "_id", Value: "generation.callback:" + firstNonce}}, 1)
	fixture.assertCount(schema.CollectionCallbackReceipts, bson.D{{Key: "_id", Value: "generation.callback:" + secondNonce}}, 1)
	fixture.assertCount(schema.CollectionCallbackReceipts, bson.D{{Key: "_id", Value: "generation.callback:" + conflictingNonce}}, 0)
}

func TestMongoCallback拒绝被篡改的延迟配方并完整回滚(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createTwoStepVideoWithRecipe()
	nonce := "nonce-tampered-recipe-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+nonce)
	fixture.track(schema.CollectionAssets, "asset:"+fixture.firstStepID+":result")
	fixture.track(schema.CollectionOutboxEvents, outbox.SubmissionEventID(fixture.secondStepID))

	result, err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).UpdateOne(fixture.ctx,
		bson.D{{Key: "_id", Value: fixture.secondStepID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "input_template", Value: []byte(`{"prompt":"tampered","parameters":{"durationSeconds":5}}`)}}}},
	)
	if err != nil || result.MatchedCount != 1 {
		t.Fatalf("篡改测试延迟配方: %v", err)
	}

	if err := fixture.usecase.Handle(fixture.ctx, fixture.completedFirstFrame(nonce)); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("篡改延迟配方回调 = %v，期望固定领域错误", err)
	}
	fixture.assertSecondStepBlockedAndRecipePending()
	fixture.assertCount(schema.CollectionAssets, bson.D{{Key: "_id", Value: "asset:" + fixture.firstStepID + ":result"}}, 0)
	fixture.assertCount(schema.CollectionOutboxEvents, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(fixture.secondStepID)}}, 0)
	fixture.assertCount(schema.CollectionCallbackReceipts, bson.D{{Key: "_id", Value: "generation.callback:" + nonce}}, 0)
}

// The legacy execution.v2 callback path must never activate a B2B deferred
// recipe. B2B binds its opening frame only after the materializer verifies the
// immutable R2 object; accepting it here would turn an unowned provider URL
// into a second-stage request.
func TestMongoCallback拒绝由旧回调激活B2B延迟配方(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createTwoStepVideoWithRecipe()
	nonce := "nonce-b2b-deferred-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+nonce)
	fixture.track(schema.CollectionAssets, "asset:"+fixture.firstStepID+":result")
	fixture.track(schema.CollectionOutboxEvents, outbox.SubmissionEventID(fixture.secondStepID))

	result, err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).UpdateOne(fixture.ctx,
		bson.D{{Key: "_id", Value: fixture.secondStepID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "protocol", Value: string(creations.DeferredRecipeProtocolB2B)}}}},
	)
	if err != nil || result.MatchedCount != 1 {
		t.Fatalf("mark deferred recipe as B2B: %v", err)
	}

	if err := fixture.usecase.Handle(fixture.ctx, fixture.completedFirstFrame(nonce)); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("legacy callback activating B2B deferred recipe = %v, want ErrInvalidCallbackEvent", err)
	}
	fixture.assertSecondStepBlockedAndRecipePending()
	fixture.assertCount(schema.CollectionAssets, bson.D{{Key: "_id", Value: "asset:" + fixture.firstStepID + ":result"}}, 0)
	fixture.assertCount(schema.CollectionOutboxEvents, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(fixture.secondStepID)}}, 0)
	fixture.assertCount(schema.CollectionCallbackReceipts, bson.D{{Key: "_id", Value: "generation.callback:" + nonce}}, 0)
}

func TestMongoCallback同Nonce同事实重放保留首次回执时间(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createSubmittedImageStep()
	nonce := "nonce-retained-time-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+nonce)
	fixture.track(schema.CollectionAssets, "asset:"+fixture.firstStepID+":result")
	firstAt := fixture.now
	command := generation.CompletedCallback{
		CreationID: fixture.creationID, StepID: fixture.firstStepID, ExternalRef: fixture.firstStepID, JobID: fixture.firstJobID,
		Capability: executionv2.CapabilityTextToImage, MediaType: "image", ResultURL: fixture.resultURL(),
		NonceHash: nonce, PayloadDigest: "digest-retained-time", CallbackVersion: "2", At: firstAt,
	}
	if err := fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.store.Complete(txCtx, command)
	}); err != nil {
		t.Fatalf("首次确认完成回调: %v", err)
	}

	command.At = firstAt.Add(time.Minute)
	if err := fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.store.Complete(txCtx, command)
	}); !errors.Is(err, generation.ErrCallbackAlreadyConfirmed) {
		t.Fatalf("较晚时间的同一回调 = %v，期望已确认", err)
	}
	if receipt := fixture.readReceipt(nonce); !receipt.ReceivedAt.Equal(firstAt) {
		t.Fatalf("回执 received_at = %s，期望首次时间 %s", receipt.ReceivedAt.UTC(), firstAt.UTC())
	}
}

func TestMongoCallback终态写入拒绝非事务上下文(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createSubmittedImageStep()
	command := generation.CompletedCallback{
		CreationID: fixture.creationID, StepID: fixture.firstStepID, ExternalRef: fixture.firstStepID, JobID: fixture.firstJobID,
		Capability: executionv2.CapabilityTextToImage, MediaType: "image", ResultURL: fixture.resultURL(),
		NonceHash: "nonce-direct-" + uuid.NewString(), PayloadDigest: "digest-direct", CallbackVersion: "2", At: fixture.now,
	}
	if err := fixture.store.Complete(context.Background(), command); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("裸 Complete() = %v，期望无效回调", err)
	}
	if err := fixture.store.Fail(context.Background(), generation.FailedCallback{
		CreationID: command.CreationID, StepID: command.StepID, ExternalRef: command.ExternalRef, JobID: command.JobID,
		Capability: command.Capability, Terminal: generation.CallbackTerminalFailed, NonceHash: command.NonceHash + "-fail",
		PayloadDigest: command.PayloadDigest, CallbackVersion: command.CallbackVersion, At: command.At,
	}); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("裸 Fail() = %v，期望无效回调", err)
	}
	fixture.assertCount(schema.CollectionCallbackReceipts, bson.D{{Key: "nonce_hash", Value: command.NonceHash}}, 0)
	fixture.assertCount(schema.CollectionCreationSteps, bson.D{{Key: "_id", Value: fixture.firstStepID}, {Key: "submit_status", Value: string(creations.StepSubmitStatusSubmitted)}}, 1)
}

func TestMongoCallback单步图片完成收敛到创作成功(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createSubmittedImageStep()
	nonce := "nonce-image-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+nonce)
	fixture.track(schema.CollectionAssets, "asset:"+fixture.firstStepID+":result")

	if err := fixture.usecase.Handle(fixture.ctx, fixture.completedImage(nonce)); err != nil {
		t.Fatalf("确认单步图片回调: %v", err)
	}
	fixture.assertGenerationSucceeded()
	fixture.assertCount(schema.CollectionCreationSteps, bson.D{
		{Key: "_id", Value: fixture.firstStepID},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSucceeded)},
		{Key: "callback_version", Value: int64(2)},
	}, 1)
	fixture.assertCount(schema.CollectionAssets, bson.D{{Key: "_id", Value: "asset:" + fixture.firstStepID + ":result"}}, 1)
}

func TestMongoCallback二步图生视频完成收敛到创作成功(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createSubmittedI2VFinalStep()
	nonce := "nonce-video-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+nonce)
	fixture.track(schema.CollectionAssets, "asset:"+fixture.secondStepID+":result")

	if err := fixture.usecase.Handle(fixture.ctx, fixture.completedI2V(nonce)); err != nil {
		t.Fatalf("确认第二步图生视频回调: %v", err)
	}
	fixture.assertGenerationSucceeded()
	fixture.assertCount(schema.CollectionCreationSteps, bson.D{
		{Key: "_id", Value: fixture.secondStepID},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSucceeded)},
		{Key: "callback_version", Value: int64(2)},
	}, 1)
	fixture.assertCount(schema.CollectionAssets, bson.D{{Key: "_id", Value: "asset:" + fixture.secondStepID + ":result"}}, 1)
}

func TestMongoCallback拒绝未知或不匹配的步骤事实(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*callbackMongoFixture, *generation.VerifiedCallbackEvent)
	}{
		{
			name: "未知步骤",
			mutate: func(fixture *callbackMongoFixture, event *generation.VerifiedCallbackEvent) {
				event.StepID = uuid.NewString()
				event.ExternalRef = event.StepID
			},
		},
		{
			name: "任务不匹配",
			mutate: func(_ *callbackMongoFixture, event *generation.VerifiedCallbackEvent) {
				event.JobID = "job-other"
			},
		},
		{
			name: "能力不匹配",
			mutate: func(_ *callbackMongoFixture, event *generation.VerifiedCallbackEvent) {
				event.Capability = executionv2.CapabilityImageToVideo
				event.MediaType = "video"
			},
		},
		{
			name: "保存原子不匹配",
			mutate: func(fixture *callbackMongoFixture, _ *generation.VerifiedCallbackEvent) {
				fixture.updateStep(fixture.firstStepID, bson.D{{Key: "atom", Value: string(creations.AtomImageEdit)}})
			},
		},
		{
			name: "回调版本不匹配",
			mutate: func(_ *callbackMongoFixture, event *generation.VerifiedCallbackEvent) {
				event.CallbackVersion = "3"
			},
		},
		{
			name: "步骤状态不接受终态",
			mutate: func(fixture *callbackMongoFixture, _ *generation.VerifiedCallbackEvent) {
				fixture.updateStep(fixture.firstStepID, bson.D{{Key: "submit_status", Value: string(creations.StepSubmitStatusReady)}})
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCallbackMongoFixture(t)
			fixture.createSubmittedImageStep()
			nonce := "nonce-mismatch-" + uuid.NewString()
			event := fixture.completedImage(nonce)
			testCase.mutate(fixture, &event)

			if err := fixture.usecase.Handle(fixture.ctx, event); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
				t.Fatalf("不匹配回调 = %v，期望无效回调", err)
			}
			fixture.assertCount(schema.CollectionCallbackReceipts, bson.D{{Key: "nonce_hash", Value: nonce}}, 0)
		})
	}
}

func TestMongoCallback失败回执和步骤CAS不改账本事实(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createSubmittedImageStep()
	firstNonce := "nonce-failed-" + uuid.NewString()
	secondNonce := "nonce-failed-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+firstNonce)
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+secondNonce)
	cancelledNonce := "nonce-failed-" + uuid.NewString()
	fixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+cancelledNonce)

	command := generation.FailedCallback{
		CreationID: fixture.creationID, StepID: fixture.firstStepID, ExternalRef: fixture.firstStepID,
		JobID: fixture.firstJobID, Capability: executionv2.CapabilityTextToImage,
		Terminal: generation.CallbackTerminalFailed, NonceHash: firstNonce, PayloadDigest: "digest-failed-1",
		CallbackVersion: "2", At: fixture.now,
	}
	err := fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.store.Fail(txCtx, command)
	})
	if err != nil {
		t.Fatalf("确认技术失败: %v", err)
	}
	fixture.assertGenerationFailed()
	fixture.assertFailureDoesNotWriteAccountingOrOutbox()

	command.NonceHash = secondNonce
	err = fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.store.Fail(txCtx, command)
	})
	if !errors.Is(err, generation.ErrCallbackAlreadyConfirmed) {
		t.Fatalf("同一失败终态 CAS = %v，期望已确认", err)
	}
	command.NonceHash = cancelledNonce
	command.Terminal = generation.CallbackTerminalCancelled
	err = fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return fixture.store.Fail(txCtx, command)
	})
	if !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("失败后取消终态 = %v，期望无效回调", err)
	}
	fixture.assertGenerationFailed()
	fixture.assertFailureDoesNotWriteAccountingOrOutbox()

	cancelledFixture := newCallbackMongoFixture(t)
	cancelledFixture.createSubmittedImageStep()
	cancelledSuccessNonce := "nonce-cancelled-" + uuid.NewString()
	cancelledFixture.track(schema.CollectionCallbackReceipts, "generation.callback:"+cancelledSuccessNonce)
	cancelledCommand := generation.FailedCallback{
		CreationID: cancelledFixture.creationID, StepID: cancelledFixture.firstStepID, ExternalRef: cancelledFixture.firstStepID,
		JobID: cancelledFixture.firstJobID, Capability: executionv2.CapabilityTextToImage,
		Terminal: generation.CallbackTerminalCancelled, NonceHash: cancelledSuccessNonce, PayloadDigest: "digest-cancelled-1",
		CallbackVersion: "2", At: cancelledFixture.now,
	}
	err = cancelledFixture.runner.WithinTx(cancelledFixture.ctx, func(txCtx context.Context) error {
		return cancelledFixture.store.Fail(txCtx, cancelledCommand)
	})
	if err != nil {
		t.Fatalf("确认技术取消: %v", err)
	}
	cancelledFixture.assertGenerationFailed()
	cancelledFixture.assertFailureDoesNotWriteAccountingOrOutbox()
}

func TestMongoCallback关联器只接受已保存步骤关联(t *testing.T) {
	fixture := newCallbackMongoFixture(t)
	fixture.createSubmittedImageStep()

	event := generation.VerifiedCallbackEvent{
		StepID: fixture.firstStepID, ExternalRef: fixture.firstStepID, JobID: fixture.firstJobID,
		Capability: executionv2.CapabilityTextToImage, MediaType: "image", ResultURL: fixture.resultURL(),
		NonceHash: "nonce-link-1", PayloadDigest: "digest-link-1", CallbackVersion: "2",
	}
	linked, err := fixture.store.Link(fixture.ctx, event)
	if err != nil {
		t.Fatalf("关联已保存步骤: %v", err)
	}
	command, err := generation.NewLinkedCallback(fixture.creationID, event)
	if err != nil || linked != command {
		t.Fatalf("关联结果未使用已保存的创作归属")
	}

	event.JobID = "job-other"
	if _, err := fixture.store.Link(fixture.ctx, event); !errors.Is(err, generation.ErrInvalidCallbackEvent) {
		t.Fatalf("错误任务关联 = %v，期望无效回调", err)
	}
}

type callbackMongoFixture struct {
	t            *testing.T
	ctx          context.Context
	database     *mongo.Database
	runner       *MongoTxRunner
	store        *mongoGenerationCallbackRepository
	usecase      *generation.CallbackUsecase
	now          time.Time
	creationID   string
	firstStepID  string
	secondStepID string
	firstJobID   string
	secondJobID  string
	userID       string
}

func newCallbackMongoFixture(t *testing.T) *callbackMongoFixture {
	t.Helper()
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		cancel()
		t.Fatalf("初始化本地 schema: %v", err)
	}
	cancel()
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	data := &Data{client: client, database: database}
	store := NewGenerationCallbackRepository(data)
	runner := NewMongoTxRunner(client)
	now := time.Date(2026, time.September, 7, 5, 0, 0, 0, time.UTC)
	fixture := &callbackMongoFixture{
		t: t, ctx: ctx, database: database, runner: runner, store: store,
		now: now, creationID: uuid.NewString(), firstStepID: uuid.NewString(), secondStepID: uuid.NewString(), firstJobID: "job-frame-" + uuid.NewString(), secondJobID: "job-video-" + uuid.NewString(), userID: uuid.NewString(),
	}
	fixture.usecase = generation.NewCallbackUsecaseWithClock(store, store, nil, runner, func() time.Time { return now })
	return fixture
}

func (fixture *callbackMongoFixture) createTwoStepVideoWithRecipe() {
	fixture.insertCreation()
	fixture.insertStep(fixture.firstStepID, 1, creations.AtomTextToImage, creations.StepSubmitStatusSubmitted, fixture.firstJobID)
	fixture.insertStep(fixture.secondStepID, 2, creations.AtomImageToVideo, creations.StepSubmitStatusBlocked, "")
	recipe, err := executionv2.CompileDeferredImageToVideo("ps-auto", []byte(`{"prompt":"motion","parameters":{"durationSeconds":5}}`))
	if err != nil {
		fixture.t.Fatalf("编译测试冻结配方: %v", err)
	}
	fixture.track(schema.CollectionGenerationStepRecipes, fixture.secondStepID)
	if _, err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).InsertOne(fixture.ctx, model.DeferredRecipeDocument{
		ID: fixture.secondStepID, StepID: fixture.secondStepID, CreationID: fixture.creationID,
		Atom: string(creations.AtomImageToVideo), ModelSKU: recipe.ModelSKU(), InputTemplate: recipe.FrozenInputTemplate(),
		Digest: recipe.Digest, Status: string(creations.DeferredRecipeStatusPending), CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}); err != nil {
		fixture.t.Fatalf("写入测试冻结配方: %v", err)
	}
}

func (fixture *callbackMongoFixture) createSubmittedImageStep() {
	fixture.insertCreation()
	fixture.insertStep(fixture.firstStepID, 1, creations.AtomTextToImage, creations.StepSubmitStatusSubmitted, fixture.firstJobID)
}

func (fixture *callbackMongoFixture) createSubmittedI2VFinalStep() {
	fixture.insertCreation()
	fixture.insertStep(fixture.firstStepID, 1, creations.AtomTextToImage, creations.StepSubmitStatusSucceeded, fixture.firstJobID)
	result, err := fixture.database.Collection(schema.CollectionCreationSteps).UpdateOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.firstStepID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "callback_version", Value: int64(2)}}}})
	if err != nil || result.MatchedCount != 1 {
		fixture.t.Fatalf("准备已成功首步骤: %v", err)
	}
	fixture.insertStep(fixture.secondStepID, 2, creations.AtomImageToVideo, creations.StepSubmitStatusSubmitted, fixture.secondJobID)
}

func (fixture *callbackMongoFixture) insertCreation() {
	fixture.track(schema.CollectionCreations, fixture.creationID)
	if _, err := fixture.database.Collection(schema.CollectionCreations).InsertOne(fixture.ctx, model.CreationDocument{
		ID: fixture.creationID, IdempotencyKey: uuid.NewString(), UserID: fixture.userID, TemplateID: "template-test", TemplateVersion: 1,
		RequestFingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Status:             string(creations.CreationStatusPendingSubmission), Version: 1, CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}); err != nil {
		fixture.t.Fatalf("写入测试创作: %v", err)
	}
}

func (fixture *callbackMongoFixture) insertStep(id string, sequence int32, atom creations.StepAtom, status creations.StepSubmitStatus, jobID string) {
	fixture.track(schema.CollectionCreationSteps, id)
	if _, err := fixture.database.Collection(schema.CollectionCreationSteps).InsertOne(fixture.ctx, model.CreationStepDocument{
		ID: id, CreationID: fixture.creationID, Sequence: sequence, Atom: string(atom), SubmitStatus: string(status),
		ExternalExecutionID: jobID, CallbackVersion: 0, CreatedAt: fixture.now,
	}); err != nil {
		fixture.t.Fatalf("写入测试步骤: %v", err)
	}
}

func (fixture *callbackMongoFixture) updateStep(id string, values bson.D) {
	fixture.t.Helper()
	result, err := fixture.database.Collection(schema.CollectionCreationSteps).UpdateOne(fixture.ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: values}})
	if err != nil || result.MatchedCount != 1 {
		fixture.t.Fatalf("更新测试步骤: %v", err)
	}
}

func (fixture *callbackMongoFixture) completedFirstFrame(nonce string) generation.VerifiedCallbackEvent {
	return fixture.completedImage(nonce)
}

func (fixture *callbackMongoFixture) completedImage(nonce string) generation.VerifiedCallbackEvent {
	return generation.VerifiedCallbackEvent{
		StepID: fixture.firstStepID, ExternalRef: fixture.firstStepID, JobID: fixture.firstJobID,
		Capability: executionv2.CapabilityTextToImage, MediaType: "image", ResultURL: fixture.resultURL(),
		NonceHash: nonce, PayloadDigest: "digest-frame", CallbackVersion: "2",
	}
}

func (fixture *callbackMongoFixture) completedI2V(nonce string) generation.VerifiedCallbackEvent {
	return generation.VerifiedCallbackEvent{
		StepID: fixture.secondStepID, ExternalRef: fixture.secondStepID, JobID: fixture.secondJobID,
		Capability: executionv2.CapabilityImageToVideo, MediaType: "video", ResultURL: "https://assets.example.test/final.mp4",
		NonceHash: nonce, PayloadDigest: "digest-video", CallbackVersion: "2",
	}
}

func (fixture *callbackMongoFixture) resultURL() string {
	return "https://assets.example.test/frame.png"
}

func (fixture *callbackMongoFixture) alternateResultURL() string {
	return "https://assets.example.test/other-frame.png"
}

func (fixture *callbackMongoFixture) assertSecondStepReadyAndOnePendingEvent() {
	fixture.t.Helper()
	var step model.CreationStepDocument
	if err := fixture.database.Collection(schema.CollectionCreationSteps).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.secondStepID}}).Decode(&step); err != nil {
		fixture.t.Fatalf("读取第二步骤: %v", err)
	}
	if step.SubmitStatus != string(creations.StepSubmitStatusReady) {
		fixture.t.Fatalf("第二步骤状态 = %q，期望 ready", step.SubmitStatus)
	}
	fixture.assertCount(schema.CollectionOutboxEvents, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(fixture.secondStepID)}}, 1)
	var recipe model.DeferredRecipeDocument
	if err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.secondStepID}}).Decode(&recipe); err != nil {
		fixture.t.Fatalf("读取冻结配方: %v", err)
	}
	if recipe.Status != string(creations.DeferredRecipeStatusConsumed) {
		fixture.t.Fatalf("冻结配方状态 = %q，期望 consumed", recipe.Status)
	}
}

func (fixture *callbackMongoFixture) assertSecondStepBlockedAndRecipePending() {
	fixture.t.Helper()
	var step model.CreationStepDocument
	if err := fixture.database.Collection(schema.CollectionCreationSteps).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.secondStepID}}).Decode(&step); err != nil {
		fixture.t.Fatalf("读取第二步骤: %v", err)
	}
	if step.SubmitStatus != string(creations.StepSubmitStatusBlocked) {
		fixture.t.Fatalf("第二步骤状态 = %q，期望 blocked", step.SubmitStatus)
	}
	var recipe model.DeferredRecipeDocument
	if err := fixture.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.secondStepID}}).Decode(&recipe); err != nil {
		fixture.t.Fatalf("读取冻结配方: %v", err)
	}
	if recipe.Status != string(creations.DeferredRecipeStatusPending) {
		fixture.t.Fatalf("冻结配方状态 = %q，期望 pending", recipe.Status)
	}
}

func (fixture *callbackMongoFixture) assertGenerationFailed() {
	fixture.t.Helper()
	var creation model.CreationDocument
	if err := fixture.database.Collection(schema.CollectionCreations).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.creationID}}).Decode(&creation); err != nil {
		fixture.t.Fatalf("读取失败创作: %v", err)
	}
	if creation.Status != string(creations.CreationStatusGenerationFailed) {
		fixture.t.Fatalf("创作状态 = %q，期望 generation_failed", creation.Status)
	}
}

func (fixture *callbackMongoFixture) assertGenerationSucceeded() {
	fixture.t.Helper()
	var creation model.CreationDocument
	if err := fixture.database.Collection(schema.CollectionCreations).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.creationID}}).Decode(&creation); err != nil {
		fixture.t.Fatalf("读取成功创作: %v", err)
	}
	if creation.Status != string(creations.CreationStatusSucceeded) {
		fixture.t.Fatalf("创作状态 = %q，期望 succeeded", creation.Status)
	}
}

func (fixture *callbackMongoFixture) assertFailureDoesNotWriteAccountingOrOutbox() {
	fixture.t.Helper()
	fixture.assertCount(schema.CollectionLedgerEntries, bson.D{{Key: "creation_id", Value: fixture.creationID}}, 0)
	fixture.assertCount(schema.CollectionReservations, bson.D{{Key: "creation_id", Value: fixture.creationID}}, 0)
	fixture.assertCount(schema.CollectionDailyQuotas, bson.D{{Key: "user_id", Value: fixture.userID}}, 0)
	fixture.assertCount(schema.CollectionOutboxEvents, bson.D{{Key: "aggregate_id", Value: fixture.creationID}}, 0)
}

func (fixture *callbackMongoFixture) readReceipt(nonce string) model.CallbackReceiptDocument {
	fixture.t.Helper()
	var receipt model.CallbackReceiptDocument
	if err := fixture.database.Collection(schema.CollectionCallbackReceipts).FindOne(fixture.ctx, bson.D{{Key: "_id", Value: "generation.callback:" + nonce}}).Decode(&receipt); err != nil {
		fixture.t.Fatalf("读取测试回执: %v", err)
	}
	return receipt
}

func (fixture *callbackMongoFixture) assertCount(collection string, filter bson.D, want int64) {
	fixture.t.Helper()
	got, err := fixture.database.Collection(collection).CountDocuments(fixture.ctx, filter)
	if err != nil {
		fixture.t.Fatalf("统计测试文档: %v", err)
	}
	if got != want {
		fixture.t.Fatalf("测试文档数量 = %d，期望 %d", got, want)
	}
}

func (fixture *callbackMongoFixture) track(collection, id string) {
	fixture.t.Helper()
	fixture.t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		if _, err := fixture.database.Collection(collection).DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
			fixture.t.Errorf("按测试标识清理文档: %v", err)
		}
	})
}
