// Package creations_test 从模块外部验证创作领域公开的命令约束。
package creations_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
)

func TestRequestFingerprint稳定且覆盖每个业务字段(t *testing.T) {
	base := validVideoRequest()
	base.Plan = []creations.StepPlan{
		{Sequence: 1, Atom: creations.AtomTextToImage},
		{Sequence: 2, Atom: creations.AtomImageToVideo},
	}
	base.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"first","assets":[],"parameters":{}}`),
	}
	baseFingerprint, err := creations.RequestFingerprint(base)
	if err != nil {
		t.Fatalf("RequestFingerprint(base) error = %v", err)
	}

	stableFingerprint, err := creations.RequestFingerprint(base)
	if err != nil {
		t.Fatalf("RequestFingerprint(base retry) error = %v", err)
	}
	if stableFingerprint != baseFingerprint {
		t.Fatalf("相同命令指纹不稳定：got %q, want %q", stableFingerprint, baseFingerprint)
	}

	testCases := []struct {
		name   string
		mutate func(*creations.CreateReservedRequest)
	}{
		{
			name: "用户ID",
			mutate: func(request *creations.CreateReservedRequest) {
				request.UserID = "user-2"
			},
		},
		{
			name: "模板ID",
			mutate: func(request *creations.CreateReservedRequest) {
				request.TemplateID = "template-video-2"
			},
		},
		{
			name: "模板版本",
			mutate: func(request *creations.CreateReservedRequest) {
				request.TemplateVersion = 2
			},
		},
		{
			name: "输出类型",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Product.Output = entitlement.ProductOutputImage
			},
		},
		{
			name: "视频时长",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Product.Video.DurationSeconds = 10
			},
		},
		{
			name: "音频开关",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Product.Video.EnableAudio = true
			},
		},
		{
			name: "引用图数量",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Product.Video.ReferenceImageCount = 2
			},
		},
		{
			name: "输入摘要",
			mutate: func(request *creations.CreateReservedRequest) {
				request.InputDigest = strings.Repeat("b", 64)
			},
		},
		{
			name: "步骤数量",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Plan = append(request.Plan, creations.StepPlan{Sequence: 2, Atom: creations.AtomImageToVideo})
			},
		},
		{
			name: "步骤序号",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Plan[0].Sequence = 2
			},
		},
		{
			name: "步骤原子",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Plan[0].Atom = creations.AtomImageEdit
				request.InitialSubmission.ModelSKU = "ps-edit-v1"
				request.InitialSubmission.Input = json.RawMessage(`{"prompt":"edit","assets":[{"role":"source_image","url":"https://assets.example.test/a.png"}],"parameters":{}}`)
			},
		},
		{
			name: "初始提交模型SKU",
			mutate: func(request *creations.CreateReservedRequest) {
				request.InitialSubmission.ModelSKU = "ps-anime-v1"
			},
		},
		{
			name: "初始提交技术输入",
			mutate: func(request *creations.CreateReservedRequest) {
				request.InitialSubmission.Input = json.RawMessage(`{"prompt":"second","assets":[],"parameters":{}}`)
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := cloneRequest(base)
			testCase.mutate(&request)

			fingerprint, err := creations.RequestFingerprint(request)
			if err != nil {
				t.Fatalf("RequestFingerprint() error = %v", err)
			}
			if fingerprint == baseFingerprint {
				t.Fatal("仅改变一个业务字段后不得复用原请求指纹")
			}
		})
	}
}

func TestRequestFingerprint拒绝非法输入摘要(t *testing.T) {
	request := validImageRequest()

	for _, inputDigest := range []string{
		"",
		strings.Repeat("a", 63),
		strings.Repeat("A", 64),
		strings.Repeat("g", 64),
	} {
		t.Run(inputDigest, func(t *testing.T) {
			request.InputDigest = inputDigest
			fingerprint, err := creations.RequestFingerprint(request)
			if !errors.Is(err, creations.ErrInvalidCreateCommand) {
				t.Fatalf("RequestFingerprint() error = %v, want ErrInvalidCreateCommand", err)
			}
			if fingerprint != "" {
				t.Fatalf("RequestFingerprint() = %q, want empty fingerprint for invalid digest", fingerprint)
			}
		})
	}
}

func TestCreateReserved非法输入摘要不触发外部副作用(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	request := fixture.imageRequest()
	request.InputDigest = strings.Repeat("A", 64)

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, creations.ErrInvalidCreateCommand) {
		t.Fatalf("CreateReserved() error = %v, want ErrInvalidCreateCommand", err)
	}
	if fixture.users.findCalls != 0 || fixture.repository.findByIdempotencyCalls != 0 || fixture.transaction.calls != 0 || fixture.repository.createCalls != 0 || fixture.reservations.calls != 0 {
		t.Fatalf("非法输入摘要不得触发外部副作用：userFind=%d idempotencyFind=%d tx=%d create=%d reserve=%d", fixture.users.findCalls, fixture.repository.findByIdempotencyCalls, fixture.transaction.calls, fixture.repository.createCalls, fixture.reservations.calls)
	}
}

func TestValidatePlan拒绝Animate并接受文生视频两步计划(t *testing.T) {
	err := creations.ValidatePlan(
		entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
		[]creations.StepPlan{{Sequence: 1, Atom: "animate"}},
	)
	if err != creations.ErrInvalidStepPlan {
		t.Fatalf("ValidatePlan(animate) error = %v, want ErrInvalidStepPlan", err)
	}

	plan := []creations.StepPlan{
		{Sequence: 1, Atom: creations.AtomTextToImage},
		{Sequence: 2, Atom: creations.AtomImageToVideo},
	}
	if err := creations.ValidatePlan(entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo}, plan); err != nil {
		t.Fatalf("ValidatePlan(text-to-video) error = %v", err)
	}
}

func TestValidatePlan仅接受固定产品步骤组合(t *testing.T) {
	testCases := []struct {
		name    string
		product entitlement.GenerationRequest
		plan    []creations.StepPlan
		wantErr bool
	}{
		{
			name:    "图片文生图单步",
			product: entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
			plan:    []creations.StepPlan{{Sequence: 1, Atom: creations.AtomTextToImage}},
		},
		{
			name:    "图片模板编辑单步",
			product: entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
			plan:    []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageEdit}},
		},
		{
			name:    "视频图生视频单步",
			product: entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo},
			plan:    []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageToVideo}},
			wantErr: false,
		},
		{
			name:    "空计划",
			product: entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
			plan:    nil,
			wantErr: true,
		},
		{
			name:    "非连续序号",
			product: entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo},
			plan: []creations.StepPlan{
				{Sequence: 1, Atom: creations.AtomTextToImage},
				{Sequence: 3, Atom: creations.AtomImageToVideo},
			},
			wantErr: true,
		},
		{
			name:    "图片误用图生视频",
			product: entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
			plan:    []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageToVideo}},
			wantErr: true,
		},
		{
			name:    "视频误用单步文生图",
			product: entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo},
			plan:    []creations.StepPlan{{Sequence: 1, Atom: creations.AtomTextToImage}},
			wantErr: true,
		},
		{
			name:    "视频错误顺序",
			product: entitlement.GenerationRequest{Output: entitlement.ProductOutputVideo},
			plan: []creations.StepPlan{
				{Sequence: 1, Atom: creations.AtomImageToVideo},
				{Sequence: 2, Atom: creations.AtomTextToImage},
			},
			wantErr: true,
		},
		{
			name:    "未知原子",
			product: entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
			plan:    []creations.StepPlan{{Sequence: 1, Atom: "unknown"}},
			wantErr: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := creations.ValidatePlan(testCase.product, testCase.plan)
			if testCase.wantErr {
				if err != creations.ErrInvalidStepPlan {
					t.Fatalf("ValidatePlan() error = %v, want ErrInvalidStepPlan", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidatePlan() error = %v", err)
			}
		})
	}
}

func validImageRequest() creations.CreateReservedRequest {
	return creations.CreateReservedRequest{
		UserID:          "user-1",
		IdempotencyKey:  "idem-image-1",
		TemplateID:      "template-image-1",
		TemplateVersion: 1,
		Product: entitlement.GenerationRequest{
			Output: entitlement.ProductOutputImage,
		},
		Plan: []creations.StepPlan{{Sequence: 1, Atom: creations.AtomTextToImage}},
	}
}

func validVideoRequest() creations.CreateReservedRequest {
	return creations.CreateReservedRequest{
		UserID:          "user-1",
		IdempotencyKey:  "idem-video-1",
		TemplateID:      "template-video-1",
		TemplateVersion: 1,
		Product: entitlement.GenerationRequest{
			Output: entitlement.ProductOutputVideo,
			Video: entitlement.VideoOptions{
				DurationSeconds:     5,
				ReferenceImageCount: 1,
			},
		},
		InputDigest: strings.Repeat("a", 64),
		Plan:        []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageToVideo}},
	}
}

func cloneRequest(request creations.CreateReservedRequest) creations.CreateReservedRequest {
	cloned := request
	cloned.Plan = append([]creations.StepPlan(nil), request.Plan...)
	if request.InitialSubmission != nil {
		cloned.InitialSubmission = &creations.InitialSubmission{
			ModelSKU: request.InitialSubmission.ModelSKU,
			Input:    append(json.RawMessage(nil), request.InitialSubmission.Input...),
		}
	}
	return cloned
}

func TestCreateReserved以日免原子创建创作和步骤(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_image", fixture.localDate, 10, 0)

	result, err := fixture.usecase.CreateReserved(context.Background(), fixture.vipImageRequest())

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if result.Creation.Status != creations.CreationStatusPendingSubmission || result.Creation.Version != 1 {
		t.Fatalf("Creation = %#v, want pending creation with version 1", result.Creation)
	}
	if len(result.Steps) != 1 || result.Steps[0].SubmitStatus != creations.StepSubmitStatusReady {
		t.Fatalf("Steps = %#v, want one ready step", result.Steps)
	}
	if fixture.reservationSource(result.Creation.ID) != ledger.BenefitSourceDailyQuota {
		t.Fatalf("reservation source = %q, want daily_quota", fixture.reservationSource(result.Creation.ID))
	}
	if fixture.accountBalance("user-1") != 0 || fixture.quotaUsed("user-1", "vip_daily_image", fixture.localDate) != 1 {
		t.Fatal("日免创建必须占用一个图片单位且不能扣减零余额账户")
	}
}

// 创建响应所需的结算投影必须来自同一预留事务，不能在 HTTP 层另行读余额后猜测。
func TestCreateReserved返回本次预留与事务内余额快照(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)

	result, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())
	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if result.Reservation == nil || result.Reservation.Source != ledger.BenefitSourceDiamonds || result.Reservation.ChargedDiamonds != 20 {
		t.Fatalf("预留投影 = %#v", result.Reservation)
	}
	if result.DiamondBalanceAfter != 20 {
		t.Fatalf("事务内余额快照 = %d, want 20", result.DiamondBalanceAfter)
	}
}

func TestCreateReserved账本预留失败时整体回滚(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	fixture.reservations.failAfterWrite = errors.New("模拟账本写入后失败")

	_, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())

	if !errors.Is(err, fixture.reservations.failAfterWrite) {
		t.Fatalf("CreateReserved() error = %v, want reservation failure", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
	if fixture.accountBalance("user-1") != 20 {
		t.Fatalf("balance = %d, want 20 after rollback", fixture.accountBalance("user-1"))
	}
}

func TestCreateReserved账本日免写入后失败时整体回滚(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_image", fixture.localDate, 10, 0)
	fixture.reservations.failAfterWrite = errors.New("模拟日免预留写入后失败")

	_, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())

	if !errors.Is(err, fixture.reservations.failAfterWrite) {
		t.Fatalf("CreateReserved() error = %v, want reservation failure", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
	if fixture.quotaUsed("user-1", "vip_daily_image", fixture.localDate) != 0 || fixture.accountBalance("user-1") != 0 {
		t.Fatalf("日免失败后必须还原：used=%d balance=%d", fixture.quotaUsed("user-1", "vip_daily_image", fixture.localDate), fixture.accountBalance("user-1"))
	}
}

func TestCreateReserved同键同命令重试不重复扣费(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)
	request := fixture.imageRequest()

	first, err := fixture.usecase.CreateReserved(context.Background(), request)
	if err != nil {
		t.Fatalf("首次 CreateReserved() error = %v", err)
	}
	second, err := fixture.usecase.CreateReserved(context.Background(), request)
	if err != nil {
		t.Fatalf("重试 CreateReserved() error = %v", err)
	}
	if first.Creation.ID != second.Creation.ID {
		t.Fatalf("creation ID = %q, want %q", second.Creation.ID, first.Creation.ID)
	}
	if fixture.accountBalance("user-1") != 20 || fixture.reservationCount() != 1 {
		t.Fatalf("重试不得重复扣费：balance=%d reservations=%d", fixture.accountBalance("user-1"), fixture.reservationCount())
	}
}

func TestCreateReserved同键不同命令返回冲突(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*creations.CreateReservedRequest)
	}{
		{
			name: "输入摘要不同",
			mutate: func(request *creations.CreateReservedRequest) {
				request.InputDigest = strings.Repeat("b", 64)
			},
		},
		{
			name: "模板版本不同",
			mutate: func(request *creations.CreateReservedRequest) {
				request.TemplateVersion++
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCreationFixture(t)
			fixture.addBoundUser("user-1", "Asia/Shanghai")
			fixture.addAccount("user-1", 40)
			request := fixture.imageRequest()
			if _, err := fixture.usecase.CreateReserved(context.Background(), request); err != nil {
				t.Fatalf("首次 CreateReserved() error = %v", err)
			}

			conflicting := cloneRequest(request)
			testCase.mutate(&conflicting)
			_, err := fixture.usecase.CreateReserved(context.Background(), conflicting)
			if !errors.Is(err, creations.ErrCreationCommandConflict) {
				t.Fatalf("CreateReserved() error = %v, want ErrCreationCommandConflict", err)
			}
			if fixture.accountBalance("user-1") != 20 || fixture.reservationCount() != 1 {
				t.Fatalf("冲突命令不得产生新扣费：balance=%d reservations=%d", fixture.accountBalance("user-1"), fixture.reservationCount())
			}
		})
	}
}

func TestCreateReserved游客返回绑定门禁且不写入事实(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addGuestUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)

	_, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())

	if !errors.Is(err, shared.ErrAccountBindingRequired) {
		t.Fatalf("CreateReserved() error = %v, want ErrAccountBindingRequired", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
}

func TestCreateReserved封禁用户被拒绝且不写入事实(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addUser("user-1", identity.AccountStatusBanned, identity.BindingStateBound, "Asia/Shanghai")
	fixture.addAccount("user-1", 20)

	_, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())

	if !errors.Is(err, creations.ErrAccountUnavailable) {
		t.Fatalf("CreateReserved() error = %v, want ErrAccountUnavailable", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
}

func TestCreateReserved免费用户十秒视频需要VIP且不写入事实(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 100)

	_, err := fixture.usecase.CreateReserved(context.Background(), fixture.videoRequest(10, false))

	if !errors.Is(err, shared.ErrVIPRequired) {
		t.Fatalf("CreateReserved() error = %v, want ErrVIPRequired", err)
	}
	fixture.assertNoCreationFacts("idem-video-10")
}

func TestCreateReserved日免耗尽且余额不足不写入创作事实(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_image", fixture.localDate, 10, 10)
	fixture.addAccount("user-1", 19)

	_, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())

	if !errors.Is(err, shared.ErrInsufficientFunds) {
		t.Fatalf("CreateReserved() error = %v, want ErrInsufficientFunds", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
	if fixture.quotaUsed("user-1", "vip_daily_image", fixture.localDate) != 10 || fixture.accountBalance("user-1") != 19 || fixture.reservations.debitAttempts != 1 {
		t.Fatalf("日免条件占用失败后必须尝试条件扣钻且无副作用：used=%d balance=%d debitAttempts=%d", fixture.quotaUsed("user-1", "vip_daily_image", fixture.localDate), fixture.accountBalance("user-1"), fixture.reservations.debitAttempts)
	}
}

func TestCreateReserved订阅读取错误原样返回且使用事务上下文(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	fixture.subscriptions.failure = errors.New("模拟订阅投影读取失败")

	_, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())

	if err != fixture.subscriptions.failure {
		t.Fatalf("CreateReserved() error = %v, want exact subscription failure %v", err, fixture.subscriptions.failure)
	}
	fixture.assertNoCreationFacts("idem-image-1")
	if fixture.subscriptions.calls != 1 {
		t.Fatalf("subscription reader calls = %d, want 1", fixture.subscriptions.calls)
	}
	if marker := fixture.subscriptions.lastContext.Value(fixtureTxContextKey{}); marker != fixtureTxContextMarker {
		t.Fatalf("subscription reader must receive txCtx marker, got %#v", marker)
	}
}

func TestCreateReserved文生视频创建就绪与阻塞步骤(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 3, 0)

	result, err := fixture.usecase.CreateReserved(context.Background(), fixture.videoRequest(5, true))

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("Steps length = %d, want 2", len(result.Steps))
	}
	if result.Steps[0].Atom != creations.AtomTextToImage || result.Steps[0].SubmitStatus != creations.StepSubmitStatusReady {
		t.Fatalf("first step = %#v, want ready text_to_image", result.Steps[0])
	}
	if result.Steps[1].Atom != creations.AtomImageToVideo || result.Steps[1].SubmitStatus != creations.StepSubmitStatusBlocked {
		t.Fatalf("second step = %#v, want blocked image_to_video", result.Steps[1])
	}
}

func TestCreateReserved同事务写入首步骤提交事件(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	request := fixture.imageRequest()
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"x","assets":[],"parameters":{}}`),
	}

	result, err := fixture.usecase.CreateReserved(context.Background(), request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].SubmitStatus != creations.StepSubmitStatusReady {
		t.Fatalf("Steps = %#v, want one ready first step", result.Steps)
	}
	eventID := outbox.SubmissionEventID(result.Steps[0].ID)
	event := fixture.state.events[eventID]
	if event == nil {
		t.Fatalf("outbox event %q is missing", eventID)
	}
	if event.AggregateID != result.Creation.ID || event.DeliveryStatus != outbox.DeliveryStatusPending {
		t.Fatalf("event = %#v, want pending event for creation %q", event, result.Creation.ID)
	}
	var payload struct {
		ModelSKU string          `json:"model_sku"`
		Input    json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal event payload: %v", err)
	}
	if payload.ModelSKU != request.InitialSubmission.ModelSKU || !reflect.DeepEqual(payload.Input, request.InitialSubmission.Input) {
		t.Fatalf("payload = %#v, want technical snapshot %#v", payload, request.InitialSubmission)
	}
	if fixture.outbox.lastContext.Value(fixtureTxContextKey{}) != fixtureTxContextMarker {
		t.Fatal("outbox writer must receive transaction context")
	}
}

func TestCreateReserved预留失败时不遗留Outbox事件(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	fixture.reservations.failAfterWrite = errors.New("模拟账本写入后失败")
	request := fixture.imageRequest()
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"x","assets":[],"parameters":{}}`),
	}

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, fixture.reservations.failAfterWrite) {
		t.Fatalf("CreateReserved() error = %v, want reservation failure", err)
	}
	fixture.assertNoCreationFacts(request.IdempotencyKey)
	if fixture.outbox.calls != 0 {
		t.Fatalf("reservation failure must not enqueue event, calls = %d", fixture.outbox.calls)
	}
}

func TestCreateReserved写入提交事件失败时整体回滚(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	fixture.outbox.failure = errors.New("模拟发件箱写入失败")
	request := fixture.imageRequest()
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"x","assets":[],"parameters":{}}`),
	}

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, fixture.outbox.failure) {
		t.Fatalf("CreateReserved() error = %v, want outbox failure", err)
	}
	fixture.assertNoCreationFacts(request.IdempotencyKey)
	if fixture.accountBalance("user-1") != 20 {
		t.Fatalf("balance = %d, want rollback to 20", fixture.accountBalance("user-1"))
	}
}

func TestCreateReserved同键不同初始提交返回冲突且不新增事件(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)
	request := fixture.imageRequest()
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"first","assets":[],"parameters":{}}`),
	}
	first, err := fixture.usecase.CreateReserved(context.Background(), request)
	if err != nil {
		t.Fatalf("首次 CreateReserved() error = %v", err)
	}
	conflicting := cloneRequest(request)
	conflicting.InitialSubmission.Input = json.RawMessage(`{"prompt":"second","assets":[],"parameters":{}}`)

	_, err = fixture.usecase.CreateReserved(context.Background(), conflicting)

	if !errors.Is(err, creations.ErrCreationCommandConflict) {
		t.Fatalf("CreateReserved() error = %v, want ErrCreationCommandConflict", err)
	}
	if fixture.reservationCount() != 1 || len(fixture.state.events) != 1 || fixture.state.events[outbox.SubmissionEventID(first.Steps[0].ID)] == nil {
		t.Fatalf("conflicting replay must retain exactly first facts: reservations=%d events=%d", fixture.reservationCount(), len(fixture.state.events))
	}
}

func TestCreateReserved无初始提交时不写入Outbox事件(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)

	_, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if fixture.outbox.calls != 0 || len(fixture.state.events) != 0 {
		t.Fatalf("request without initial submission must not enqueue events: calls=%d events=%d", fixture.outbox.calls, len(fixture.state.events))
	}
}

func TestCreateReserved文生视频只写入首步骤提交事件(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 3, 0)
	request := fixture.videoRequest(5, true)
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"first frame","assets":[],"parameters":{}}`),
	}

	result, err := fixture.usecase.CreateReserved(context.Background(), request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if len(result.Steps) != 2 || result.Steps[0].Atom != creations.AtomTextToImage || result.Steps[0].SubmitStatus != creations.StepSubmitStatusReady || result.Steps[1].Atom != creations.AtomImageToVideo || result.Steps[1].SubmitStatus != creations.StepSubmitStatusBlocked {
		t.Fatalf("Steps = %#v, want ready text-to-image then blocked image-to-video", result.Steps)
	}
	if len(fixture.state.events) != 1 || fixture.state.events[outbox.SubmissionEventID(result.Steps[0].ID)] == nil {
		t.Fatalf("events = %#v, want exactly first step submission event", fixture.state.events)
	}
	if fixture.state.events[outbox.SubmissionEventID(result.Steps[1].ID)] != nil {
		t.Fatal("blocked second step must not have an outbox event")
	}
}

func TestCreateReserved文生视频同事务冻结第二步配方(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 3, 0)
	request := fixture.videoRequest(5, true)
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"first frame","assets":[],"parameters":{}}`),
	}
	request.DeferredImageToVideo = &creations.DeferredImageToVideo{
		ModelSKU:      "ps-auto",
		InputTemplate: json.RawMessage(`{"prompt":"move","parameters":{"durationSeconds":5}}`),
	}

	result, err := fixture.usecase.CreateReserved(context.Background(), request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("Steps = %#v, want two steps", result.Steps)
	}
	fixture.assertDeferredRecipe(result.Steps[1].ID, result.Creation.ID, "ps-auto")
}

func TestCreateReserved延迟配方非法命令在事务前拒绝且无写入(t *testing.T) {
	testCases := []struct {
		name    string
		request func(*creationFixture) creations.CreateReservedRequest
	}{
		{
			name: "图片任务携带配方",
			request: func(fixture *creationFixture) creations.CreateReservedRequest {
				request := fixture.imageRequest()
				request.DeferredImageToVideo = validDeferredImageToVideo()
				return request
			},
		},
		{
			name: "双步骤视频缺配方",
			request: func(fixture *creationFixture) creations.CreateReservedRequest {
				request := fixture.videoRequest(5, true)
				request.DeferredImageToVideo = nil
				return request
			},
		},
		{
			name: "第一步不是文生图",
			request: func(fixture *creationFixture) creations.CreateReservedRequest {
				request := fixture.videoRequest(5, true)
				request.Plan[0].Atom = creations.AtomImageToVideo
				request.DeferredImageToVideo = validDeferredImageToVideo()
				return request
			},
		},
		{
			name: "第二步不是图生视频",
			request: func(fixture *creationFixture) creations.CreateReservedRequest {
				request := fixture.videoRequest(5, true)
				request.Plan[1].Atom = creations.AtomTextToImage
				request.DeferredImageToVideo = validDeferredImageToVideo()
				return request
			},
		},
		{
			name: "配方带素材",
			request: func(fixture *creationFixture) creations.CreateReservedRequest {
				request := fixture.videoRequest(5, true)
				request.DeferredImageToVideo = &creations.DeferredImageToVideo{ModelSKU: "ps-auto", InputTemplate: json.RawMessage(`{"prompt":"move","assets":[{"role":"opening_frame","url":"https://assets.example.test/frame.png"}],"parameters":{}}`)}
				return request
			},
		},
		{
			name: "配方带支付字段",
			request: func(fixture *creationFixture) creations.CreateReservedRequest {
				request := fixture.videoRequest(5, true)
				request.DeferredImageToVideo = &creations.DeferredImageToVideo{ModelSKU: "ps-auto", InputTemplate: json.RawMessage(`{"prompt":"move","parameters":{"payment_id":"payment"}}`)}
				return request
			},
		},
		{
			name: "配方带模板字段",
			request: func(fixture *creationFixture) creations.CreateReservedRequest {
				request := fixture.videoRequest(5, true)
				request.DeferredImageToVideo = &creations.DeferredImageToVideo{ModelSKU: "ps-auto", InputTemplate: json.RawMessage(`{"prompt":"move","template_id":"template","parameters":{}}`)}
				return request
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCreationFixture(t)
			fixture.addBoundUser("user-1", "Asia/Shanghai")
			fixture.addAccount("user-1", 20)
			request := testCase.request(fixture)

			_, err := fixture.usecase.CreateReserved(context.Background(), request)

			if !errors.Is(err, creations.ErrInvalidCreateCommand) && !errors.Is(err, creations.ErrInvalidStepPlan) {
				t.Fatalf("CreateReserved() error = %v, want invalid command or plan", err)
			}
			if fixture.transaction.calls != 0 || fixture.users.findCalls != 0 || fixture.repository.createCalls != 0 || fixture.reservations.calls != 0 || fixture.outbox.calls != 0 || fixture.recipes.calls != 0 {
				t.Fatalf("非法延迟配方必须在事务前拒绝：tx=%d userFind=%d create=%d reserve=%d outbox=%d recipes=%d", fixture.transaction.calls, fixture.users.findCalls, fixture.repository.createCalls, fixture.reservations.calls, fixture.outbox.calls, fixture.recipes.calls)
			}
			fixture.assertNoCreationFacts(request.IdempotencyKey)
		})
	}
}

func TestCreateReserved延迟配方写入失败回滚全部事实(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 3, 0)
	fixture.recipes.failure = errors.New("recipe writer failure")
	request := fixture.videoRequest(5, true)
	request.InitialSubmission = &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"first","assets":[],"parameters":{}}`)}
	request.DeferredImageToVideo = validDeferredImageToVideo()

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, fixture.recipes.failure) {
		t.Fatalf("CreateReserved() error = %v, want recipe writer failure", err)
	}
	if fixture.recipes.calls != 1 {
		t.Fatalf("recipe writer calls = %d, want 1", fixture.recipes.calls)
	}
	fixture.assertNoCreationFacts(request.IdempotencyKey)
}

func TestCreateReserved同幂等键不同第二步配方返回冲突(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 3, 0)
	request := fixture.videoRequest(5, true)

	first, err := fixture.usecase.CreateReserved(context.Background(), request)
	if err != nil {
		t.Fatalf("首次 CreateReserved() error = %v", err)
	}
	conflicting := request
	conflicting.DeferredImageToVideo = &creations.DeferredImageToVideo{
		ModelSKU:      "ps-auto",
		InputTemplate: json.RawMessage(`{"prompt":"different","parameters":{"durationSeconds":5}}`),
	}

	_, err = fixture.usecase.CreateReserved(context.Background(), conflicting)

	if !errors.Is(err, creations.ErrCreationCommandConflict) {
		t.Fatalf("CreateReserved() error = %v, want ErrCreationCommandConflict", err)
	}
	if fixture.reservationCount() != 1 || len(fixture.state.deferredRecipes) != 1 || fixture.state.deferredRecipes[first.Steps[1].ID] == nil {
		t.Fatal("冲突重放必须保留首个创作的预留和第二步配方")
	}
}

func TestCreateReserved接受单步骤图生视频(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 50)
	request := fixture.videoRequest(5, false)
	request.Plan = []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageToVideo}}
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-auto",
		Input:    json.RawMessage(`{"prompt":"move","assets":[{"role":"source_image","url":"https://assets.example.test/source.png"}],"parameters":{"durationSeconds":5}}`),
	}
	request.DeferredImageToVideo = nil

	result, err := fixture.usecase.CreateReserved(context.Background(), request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if result.Creation.Output != entitlement.ProductOutputVideo || len(result.Steps) != 1 || result.Steps[0].Atom != creations.AtomImageToVideo || result.Steps[0].SubmitStatus != creations.StepSubmitStatusReady {
		t.Fatalf("单步骤 I2V 创作事实错误：%#v", result)
	}
	if fixture.state.events[outbox.SubmissionEventID(result.Steps[0].ID)] == nil {
		t.Fatal("单步骤 I2V 必须写入首步骤 Outbox")
	}
}

func TestCreateReserved非文生视频计划在事务前拒绝(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*creations.CreateReservedRequest)
	}{
		{
			name: "首步不是文生图",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Plan = []creations.StepPlan{
					{Sequence: 1, Atom: creations.AtomImageToVideo},
					{Sequence: 2, Atom: creations.AtomImageToVideo},
				}
				request.DeferredImageToVideo = validDeferredImageToVideo()
			},
		},
		{
			name: "第二步不是图生视频",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Plan = []creations.StepPlan{
					{Sequence: 1, Atom: creations.AtomTextToImage},
					{Sequence: 2, Atom: creations.AtomTextToImage},
				}
				request.DeferredImageToVideo = validDeferredImageToVideo()
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCreationFixture(t)
			fixture.addBoundUser("user-1", "Asia/Shanghai")
			fixture.addAccount("user-1", 20)
			request := fixture.videoRequest(5, false)
			testCase.mutate(&request)

			_, err := fixture.usecase.CreateReserved(context.Background(), request)

			if !errors.Is(err, creations.ErrInvalidStepPlan) {
				t.Fatalf("CreateReserved() error = %v, want ErrInvalidStepPlan", err)
			}
			if fixture.transaction.calls != 0 || fixture.users.findCalls != 0 || fixture.repository.createCalls != 0 || fixture.reservations.calls != 0 || fixture.outbox.calls != 0 || fixture.recipes.calls != 0 {
				t.Fatalf("非法视频计划必须在事务前拒绝：tx=%d userFind=%d create=%d reserve=%d outbox=%d recipes=%d", fixture.transaction.calls, fixture.users.findCalls, fixture.repository.createCalls, fixture.reservations.calls, fixture.outbox.calls, fixture.recipes.calls)
			}
			fixture.assertNoCreationFacts(request.IdempotencyKey)
		})
	}
}

func TestCreateReserved双步骤视频缺首步快照在事务前拒绝(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 3, 0)
	request := fixture.videoRequest(5, true)
	request.InitialSubmission = nil

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, creations.ErrInvalidCreateCommand) {
		t.Fatalf("CreateReserved() error = %v, want ErrInvalidCreateCommand", err)
	}
	if fixture.transaction.calls != 0 || fixture.users.findCalls != 0 || fixture.repository.createCalls != 0 || fixture.reservations.calls != 0 || fixture.outbox.calls != 0 || fixture.recipes.calls != 0 {
		t.Fatalf("缺少首步快照必须在事务前拒绝：tx=%d userFind=%d create=%d reserve=%d outbox=%d recipes=%d", fixture.transaction.calls, fixture.users.findCalls, fixture.repository.createCalls, fixture.reservations.calls, fixture.outbox.calls, fixture.recipes.calls)
	}
	fixture.assertNoCreationFacts(request.IdempotencyKey)
}

func validDeferredImageToVideo() *creations.DeferredImageToVideo {
	return &creations.DeferredImageToVideo{
		ModelSKU:      "ps-auto",
		InputTemplate: json.RawMessage(`{"prompt":"move","parameters":{"durationSeconds":5}}`),
	}
}

func TestCreateReserved拒绝生成中台不支持的图片编辑SKU且不写事件(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	request := fixture.imageRequest()
	request.Plan = []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageEdit}}
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-edit-v1",
		Input:    json.RawMessage(`{"prompt":"edit","assets":[],"parameters":{}}`),
	}

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, creations.ErrInvalidCreateCommand) {
		t.Fatalf("CreateReserved() error = %v, want ErrInvalidCreateCommand", err)
	}
	if fixture.transaction.calls != 0 || fixture.outbox.calls != 0 || len(fixture.state.events) != 0 {
		t.Fatalf("非法 SKU 必须在事务前拒绝：tx=%d outbox=%d events=%d", fixture.transaction.calls, fixture.outbox.calls, len(fixture.state.events))
	}
}

func TestCreateReserved图片编辑非法素材在事务前拒绝(t *testing.T) {
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"prompt":"edit","assets":[],"parameters":{}}`),
		json.RawMessage(`{"prompt":"edit","assets":[{"role":"source_image","url":"http://assets.example.test/a.png"}],"parameters":{}}`),
	} {
		fixture := newCreationFixture(t)
		fixture.addBoundUser("user-1", "Asia/Shanghai")
		fixture.addAccount("user-1", 20)
		request := fixture.imageRequest()
		request.Plan = []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageEdit}}
		request.InitialSubmission = &creations.InitialSubmission{ModelSKU: "ps-edit-v1", Input: input}

		_, err := fixture.usecase.CreateReserved(context.Background(), request)

		if !errors.Is(err, creations.ErrInvalidCreateCommand) {
			t.Fatalf("CreateReserved() error = %v, want ErrInvalidCreateCommand", err)
		}
		if fixture.transaction.calls != 0 || fixture.outbox.calls != 0 || len(fixture.state.events) != 0 {
			t.Fatalf("非法素材必须在事务前拒绝：tx=%d outbox=%d events=%d", fixture.transaction.calls, fixture.outbox.calls, len(fixture.state.events))
		}
	}
}

func TestCreateReserved允许受控技术快照字段(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 20)
	request := fixture.imageRequest()
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input: json.RawMessage(`{
			"prompt":"x",
			"negativePrompt":"blur",
			"assets":[],
			"parameters":{"width":1024,"aspectRatio":"1:1"}
		}`),
	}

	result, err := fixture.usecase.CreateReserved(context.Background(), request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if len(result.Steps) != 1 || fixture.state.events[outbox.SubmissionEventID(result.Steps[0].ID)] == nil {
		t.Fatalf("result/events = %#v / %#v, want first-step event", result, fixture.state.events)
	}
}

func TestCreateReserved拒绝非技术初始提交快照(t *testing.T) {
	testCases := []struct {
		name       string
		submission *creations.InitialSubmission
	}{
		{name: "空模型", submission: &creations.InitialSubmission{Input: json.RawMessage(`{"prompt":"x"}`)}},
		{name: "不安全模型格式", submission: &creations.InitialSubmission{ModelSKU: "ps image v1", Input: json.RawMessage(`{"prompt":"x"}`)}},
		{name: "输入不是对象", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`[]`)}},
		{name: "输入不是JSON", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`not-json`)}},
		{name: "用户业务包装", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"user_id":"user-1","prompt":"x"}`)}},
		{name: "原子业务包装", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"atom":"image_edit","prompt":"x"}`)}},
		{name: "参数价格字段", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[],"parameters":{"price_cents":100}}`)}},
		{name: "参数钻石余额字段", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[],"parameters":{"diamond_balance":20}}`)}},
		{name: "参数风控字段", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[],"parameters":{"risk_level":"high"}}`)}},
		{name: "素材数组支付字段", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[{"role":"reference","url":"https://assets.example.test/a.png","mediaType":"image/png","payment_id":"payment-1"}],"parameters":{}}`)}},
		{name: "提示词为空值", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":null,"assets":[],"parameters":{}}`)}},
		{name: "参数数值为空值", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[],"parameters":{"width":null}}`)}},
		{name: "素材字段为空值", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[{"role":"reference","url":null,"mediaType":"image/png"}],"parameters":{}}`)}},
		{name: "重复提示词隐藏空值", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":null,"prompt":"x","assets":[],"parameters":{}}`)}},
		{name: "重复参数对象隐藏价格字段", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[],"parameters":{"price_cents":100},"parameters":{"width":1024}}`)}},
		{name: "重复素材数组隐藏支付字段", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[{"role":"reference","url":"https://assets.example.test/a.png","mediaType":"image/png","payment_id":"payment-1"}],"assets":[],"parameters":{}}`)}},
		{name: "重复素材字段隐藏空值", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[{"role":"reference","url":null,"url":"https://assets.example.test/a.png","mediaType":"image/png"}],"parameters":{}}`)}},
		{name: "重复参数字段隐藏空值", submission: &creations.InitialSubmission{ModelSKU: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x","assets":[],"parameters":{"width":null,"width":1024}}`)}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCreationFixture(t)
			fixture.addBoundUser("user-1", "Asia/Shanghai")
			fixture.addAccount("user-1", 20)
			request := fixture.imageRequest()
			request.InitialSubmission = testCase.submission

			_, err := fixture.usecase.CreateReserved(context.Background(), request)

			if !errors.Is(err, creations.ErrInvalidCreateCommand) {
				t.Fatalf("CreateReserved() error = %v, want ErrInvalidCreateCommand", err)
			}
			if fixture.transaction.calls != 0 || fixture.users.findCalls != 0 {
				t.Fatalf("非法技术快照必须在事务前拒绝：tx=%d userFind=%d", fixture.transaction.calls, fixture.users.findCalls)
			}
			fixture.assertNoCreationFacts(request.IdempotencyKey)
		})
	}
}

func TestCreateReserved视频时长映射日免单位(t *testing.T) {
	testCases := []struct {
		duration int32
		units    int32
	}{
		{duration: 5, units: 1},
		{duration: 10, units: 2},
		{duration: 15, units: 3},
	}

	for _, testCase := range testCases {
		t.Run(strconv.Itoa(int(testCase.duration))+"秒", func(t *testing.T) {
			fixture := newCreationFixture(t)
			fixture.addBoundUser("user-1", "Asia/Shanghai")
			fixture.addActiveSubscription("user-1")
			fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 3, 0)

			result, err := fixture.usecase.CreateReserved(context.Background(), fixture.videoRequest(testCase.duration, false))
			if err != nil {
				t.Fatalf("CreateReserved() error = %v", err)
			}
			request := fixture.reservationRequest(result.Creation.ID)
			if request.Quota.Kind != "vip_daily_video" || request.Quota.Units != testCase.units {
				t.Fatalf("Quota = %#v, want vip_daily_video with %d units", request.Quota, testCase.units)
			}
		})
	}
}

func TestCreateReserved视频能力仅信任服务端订阅投影(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*creations.CreateReservedRequest)
	}{
		{
			name: "十秒视频",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Product.Video.DurationSeconds = 10
			},
		},
		{
			name: "开启音频",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Product.Video.EnableAudio = true
			},
		},
		{
			name: "多图视频",
			mutate: func(request *creations.CreateReservedRequest) {
				request.Product.Video.ReferenceImageCount = 2
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name+"/有服务端订阅", func(t *testing.T) {
			fixture := newCreationFixture(t)
			fixture.addBoundUser("user-1", "Asia/Shanghai")
			fixture.addActiveSubscription("user-1")
			fixture.addAccount("user-1", 200)
			fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 3, 0)
			request := fixture.videoRequest(5, false)
			testCase.mutate(&request)

			if _, err := fixture.usecase.CreateReserved(context.Background(), request); err != nil {
				t.Fatalf("CreateReserved() error = %v", err)
			}
		})

		t.Run(testCase.name+"/无服务端订阅", func(t *testing.T) {
			fixture := newCreationFixture(t)
			fixture.addBoundUser("user-1", "Asia/Shanghai")
			fixture.addAccount("user-1", 200)
			request := fixture.videoRequest(5, false)
			testCase.mutate(&request)

			_, err := fixture.usecase.CreateReserved(context.Background(), request)
			if !errors.Is(err, shared.ErrVIPRequired) {
				t.Fatalf("CreateReserved() error = %v, want ErrVIPRequired", err)
			}
		})
	}
}

func TestCreateReserved事务重试复用固定业务时刻(t *testing.T) {
	firstNow := time.Date(2026, time.September, 5, 15, 59, 59, 900_000_000, time.UTC)
	secondNow := time.Date(2026, time.September, 5, 16, 0, 0, 100_000_000, time.UTC)
	clock := &fixtureSequenceClock{values: []time.Time{firstNow, secondNow}}
	fixture := newCreationFixtureWithClock(t, clock.Now)
	fixture.transaction.retryOnce = true
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addSubscription("user-1", &entitlement.SubscriptionSnapshot{
		Status:        entitlement.SubscriptionStatusActive,
		BillingPeriod: entitlement.SubscriptionBillingPeriodMonthly,
		StartsAt:      firstNow.Add(-24 * time.Hour),
		ExpiresAt:     secondNow,
	})
	fixture.addDailyQuota("user-1", "vip_daily_video", "2026-09-05", 3, 0)

	result, err := fixture.usecase.CreateReserved(context.Background(), fixture.videoRequest(10, true))
	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if clock.calls != 1 {
		t.Fatalf("clock calls = %d, want 1", clock.calls)
	}
	if len(fixture.transaction.attempts) != 2 {
		t.Fatalf("transaction attempts = %d, want 2", len(fixture.transaction.attempts))
	}
	for index, attempt := range fixture.transaction.attempts {
		if !attempt.createdAt.Equal(firstNow) {
			t.Fatalf("attempt %d creation time = %s, want %s", index+1, attempt.createdAt, firstNow)
		}
		if !attempt.reserveRequest.BusinessAt.Equal(firstNow) {
			t.Fatalf("attempt %d reservation business time = %s, want %s", index+1, attempt.reserveRequest.BusinessAt, firstNow)
		}
		if attempt.reserveRequest.PriceDiamonds != 100 || attempt.reserveRequest.Quota.LocalDate != "2026-09-05" || attempt.reserveRequest.Quota.Limit != 3 || attempt.reserveRequest.Quota.Units != 2 {
			t.Fatalf("attempt %d reserve request = %#v, want fixed 10 秒 VIP 配额", index+1, attempt.reserveRequest)
		}
		if len(attempt.stepCreatedAt) != 2 {
			t.Fatalf("attempt %d step count = %d, want 2 for text-to-video", index+1, len(attempt.stepCreatedAt))
		}
		for stepIndex, createdAt := range attempt.stepCreatedAt {
			if !createdAt.Equal(firstNow) {
				t.Fatalf("attempt %d step %d creation time = %s, want %s", index+1, stepIndex+1, createdAt, firstNow)
			}
		}
	}
	if !reflect.DeepEqual(fixture.transaction.attempts[0].reserveRequest, fixture.transaction.attempts[1].reserveRequest) {
		t.Fatalf("事务重试必须复用完全相同的预留命令：first=%#v second=%#v", fixture.transaction.attempts[0].reserveRequest, fixture.transaction.attempts[1].reserveRequest)
	}
	if !result.Creation.CreatedAt.Equal(firstNow) {
		t.Fatalf("result creation time = %s, want %s", result.Creation.CreatedAt, firstNow)
	}
	if len(result.Steps) != 2 || result.Steps[0].Atom != creations.AtomTextToImage || result.Steps[1].Atom != creations.AtomImageToVideo {
		t.Fatalf("result steps = %#v, want text_to_image then image_to_video", result.Steps)
	}
	for _, observedNow := range fixture.entitlements.businessTimes {
		if !observedNow.Equal(firstNow) {
			t.Fatalf("权益计算时刻 = %s, want %s", observedNow, firstNow)
		}
	}
}

func TestCreateReserved重复键竞争返回获胜创作且不重复预留(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)
	fixture.repository.simulateWinningRace = true

	result, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if fixture.repository.raceWinner == nil || result.Creation.ID != fixture.repository.raceWinner.creation.ID {
		t.Fatalf("result creation = %#v, want committed race winner %#v", result.Creation, fixture.repository.raceWinner)
	}
	if fixture.repository.raceWinner.reservation == nil || fixture.repository.raceWinner.ledgerEntry == nil {
		t.Fatalf("获胜事务必须包含预留与账本分录：%#v", fixture.repository.raceWinner)
	}
	if fixture.reservations.calls != 0 || len(fixture.state.reservations) != 0 || len(fixture.state.ledgerEntries) != 0 {
		t.Fatalf("失败方不得产生第二套预留或账本：calls=%d reservations=%d entries=%d", fixture.reservations.calls, len(fixture.state.reservations), len(fixture.state.ledgerEntries))
	}
}

func TestCreateReserved非VIP忽略遗留日免事实并条件扣钻(t *testing.T) {
	testCases := []struct {
		name        string
		kind        string
		storedLimit int32
		balance     int64
		request     func(*creationFixture) creations.CreateReservedRequest
		wantPrice   int64
	}{
		{
			name:        "图片遗留额度",
			kind:        "vip_daily_image",
			storedLimit: 10,
			balance:     40,
			request: func(fixture *creationFixture) creations.CreateReservedRequest {
				return fixture.imageRequest()
			},
			wantPrice: 20,
		},
		{
			name:        "视频遗留额度",
			kind:        "vip_daily_video",
			storedLimit: 3,
			balance:     100,
			request: func(fixture *creationFixture) creations.CreateReservedRequest {
				return fixture.videoRequest(5, false)
			},
			wantPrice: 50,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCreationFixture(t)
			fixture.addBoundUser("user-1", "Asia/Shanghai")
			fixture.addAccount("user-1", testCase.balance)
			fixture.addDailyQuota("user-1", testCase.kind, fixture.localDate, testCase.storedLimit, 0)

			result, err := fixture.usecase.CreateReserved(context.Background(), testCase.request(fixture))
			if err != nil {
				t.Fatalf("CreateReserved() error = %v", err)
			}
			reserveRequest := fixture.reservationRequest(result.Creation.ID)
			if reserveRequest.Quota.Limit != 0 || reserveRequest.Quota.Units != 0 {
				t.Fatalf("非 VIP 预留额度 = %#v, want disabled quota", reserveRequest.Quota)
			}
			if fixture.reservationSource(result.Creation.ID) != ledger.BenefitSourceDiamonds || fixture.quotaUsed("user-1", testCase.kind, fixture.localDate) != 0 || fixture.accountBalance("user-1") != testCase.balance-testCase.wantPrice {
				t.Fatalf("非 VIP 必须只条件扣钻：source=%q used=%d balance=%d", fixture.reservationSource(result.Creation.ID), fixture.quotaUsed("user-1", testCase.kind, fixture.localDate), fixture.accountBalance("user-1"))
			}
		})
	}
}

func TestFixtureReservation仅在额度上限一致时占用日免(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addAccount("user-1", 50)
	fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 10, 0)

	reservation, err := fixture.reservations.ReserveInTx(context.Background(), ledger.ReserveRequest{
		CreationID:    "creation-quota-limit-mismatch",
		UserID:        "user-1",
		PriceDiamonds: 50,
		Quota: ledger.QuotaReservation{
			Kind:      "vip_daily_video",
			LocalDate: fixture.localDate,
			Limit:     3,
			Units:     1,
		},
	})
	if err != nil {
		t.Fatalf("ReserveInTx() error = %v", err)
	}
	if reservation.Source != ledger.BenefitSourceDiamonds {
		t.Fatalf("reservation source = %q, want diamonds when stored limit differs", reservation.Source)
	}
	if fixture.quotaUsed("user-1", "vip_daily_video", fixture.localDate) != 0 || fixture.accountBalance("user-1") != 0 {
		t.Fatalf("额度不一致不得占用日免：used=%d balance=%d", fixture.quotaUsed("user-1", "vip_daily_video", fixture.localDate), fixture.accountBalance("user-1"))
	}
}

type creationFixture struct {
	t             *testing.T
	localDate     string
	state         *creationFixtureState
	users         *fixtureUserReader
	subscriptions *fixtureSubscriptionReader
	repository    *fixtureCreationRepository
	recipes       *fixtureDeferredRecipeWriter
	reservations  *fixtureReservationUsecase
	outbox        *fixtureOutboxWriter
	transaction   *fixtureTxRunner
	entitlements  *fixtureEntitlementEvaluator
	// admissions 为 nil 时用例只冻结本地归属，与生产默认装配一致。
	admissions creations.AdmissionResolver
	clock      func() time.Time
	usecase    *creations.Usecase
}

// withAdmissions 用指定归属解析器重建用例，供 admission 用例覆盖默认本地解析器。
func (fixture *creationFixture) withAdmissions(admissions creations.AdmissionResolver) *creationFixture {
	fixture.admissions = admissions
	fixture.usecase = creations.NewUsecaseWithClock(
		fixture.users,
		fixture.subscriptions,
		fixture.entitlements,
		fixture.repository,
		fixture.reservations,
		fixture.outbox,
		fixture.transaction,
		admissions,
		fixture.clock,
	)
	return fixture
}

func newCreationFixture(t *testing.T) *creationFixture {
	return newCreationFixtureWithClock(t, func() time.Time {
		return time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	})
}

func newCreationFixtureWithClock(t *testing.T, clock func() time.Time) *creationFixture {
	t.Helper()
	state := newCreationFixtureState()
	recipeWriter := &fixtureDeferredRecipeWriter{state: state}
	repository := &fixtureCreationRepository{state: state, recipeWriter: recipeWriter}
	reservations := &fixtureReservationUsecase{state: state, repository: repository}
	fixture := &creationFixture{
		t:             t,
		localDate:     "2026-09-05",
		state:         state,
		users:         &fixtureUserReader{state: state},
		subscriptions: &fixtureSubscriptionReader{subscriptions: make(map[string]*entitlement.SubscriptionSnapshot)},
		repository:    repository,
		recipes:       recipeWriter,
		reservations:  reservations,
		outbox:        &fixtureOutboxWriter{state: state},
		transaction:   &fixtureTxRunner{state: state},
		clock:         clock,
	}
	fixture.entitlements = &fixtureEntitlementEvaluator{}
	fixture.usecase = creations.NewUsecaseWithClock(
		fixture.users,
		fixture.subscriptions,
		fixture.entitlements,
		fixture.repository,
		fixture.reservations,
		fixture.outbox,
		fixture.transaction,
		fixture.admissions,
		clock,
	)
	return fixture
}

func (fixture *creationFixture) imageRequest() creations.CreateReservedRequest {
	request := validImageRequest()
	request.InputDigest = strings.Repeat("a", 64)
	return request
}

func (fixture *creationFixture) vipImageRequest() creations.CreateReservedRequest {
	return fixture.imageRequest()
}

func (fixture *creationFixture) videoRequest(duration int32, _ bool) creations.CreateReservedRequest {
	request := validVideoRequest()
	request.IdempotencyKey = "idem-video-" + strconv.Itoa(int(duration))
	request.Product.Video.DurationSeconds = duration
	request.Plan = []creations.StepPlan{
		{Sequence: 1, Atom: creations.AtomTextToImage},
		{Sequence: 2, Atom: creations.AtomImageToVideo},
	}
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"first","assets":[],"parameters":{}}`),
	}
	request.DeferredImageToVideo = validDeferredImageToVideo()
	return request
}

func (fixture *creationFixture) activeSubscription() *entitlement.SubscriptionSnapshot {
	return &entitlement.SubscriptionSnapshot{
		Status:        entitlement.SubscriptionStatusActive,
		BillingPeriod: entitlement.SubscriptionBillingPeriodMonthly,
		StartsAt:      time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt:     time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (fixture *creationFixture) addBoundUser(userID, timezone string) {
	fixture.addUser(userID, identity.AccountStatusNormal, identity.BindingStateBound, timezone)
}

func (fixture *creationFixture) addActiveSubscription(userID string) {
	fixture.addSubscription(userID, fixture.activeSubscription())
}

func (fixture *creationFixture) addSubscription(userID string, subscription *entitlement.SubscriptionSnapshot) {
	fixture.subscriptions.subscriptions[userID] = subscription
}

func (fixture *creationFixture) addGuestUser(userID, timezone string) {
	fixture.addUser(userID, identity.AccountStatusNormal, identity.BindingStateGuest, timezone)
}

func (fixture *creationFixture) addUser(userID string, accountStatus identity.AccountStatus, bindingState identity.BindingState, timezone string) {
	fixture.state.users[userID] = &identity.User{
		ID:            userID,
		AccountStatus: accountStatus,
		BindingState:  bindingState,
		Timezone:      timezone,
	}
}

func (fixture *creationFixture) addAccount(userID string, balance int64) {
	fixture.state.accounts[userID] = balance
}

func (fixture *creationFixture) addDailyQuota(userID, kind, localDate string, limit, used int32) {
	fixture.state.quotas[quotaKey(userID, kind, localDate)] = &fixtureQuota{Limit: limit, Used: used}
}

func (fixture *creationFixture) accountBalance(userID string) int64 {
	return fixture.state.accounts[userID]
}

func (fixture *creationFixture) quotaUsed(userID, kind, localDate string) int32 {
	quota := fixture.state.quotas[quotaKey(userID, kind, localDate)]
	if quota == nil {
		return 0
	}
	return quota.Used
}

func (fixture *creationFixture) reservationSource(creationID string) ledger.BenefitSource {
	return fixture.state.reservations[creationID].Source
}

func (fixture *creationFixture) reservationRequest(creationID string) ledger.ReserveRequest {
	return fixture.state.reservationRequests[creationID]
}

func (fixture *creationFixture) reservationCount() int {
	return len(fixture.state.reservations)
}

func (fixture *creationFixture) assertNoCreationFacts(idempotencyKey string) {
	fixture.t.Helper()
	if creation := fixture.state.creations[idempotencyKey]; creation != nil {
		fixture.t.Fatalf("创作不应写入：%#v", creation)
	}
	if len(fixture.state.reservations) != 0 || len(fixture.state.steps) != 0 || len(fixture.state.ledgerEntries) != 0 || len(fixture.state.reservationRequests) != 0 || len(fixture.state.events) != 0 || len(fixture.state.deferredRecipes) != 0 {
		fixture.t.Fatalf("创作失败后不得遗留步骤、预留、分录、发件箱事件或延迟配方：steps=%d reservations=%d entries=%d requests=%d events=%d recipes=%d", len(fixture.state.steps), len(fixture.state.reservations), len(fixture.state.ledgerEntries), len(fixture.state.reservationRequests), len(fixture.state.events), len(fixture.state.deferredRecipes))
	}
}

func (fixture *creationFixture) assertDeferredRecipe(stepID, creationID, modelSKU string) {
	fixture.t.Helper()
	recipe := fixture.state.deferredRecipes[stepID]
	if recipe == nil || recipe.StepID != stepID || recipe.CreationID != creationID || recipe.Atom != creations.AtomImageToVideo || recipe.ModelSKU != modelSKU || recipe.Digest == "" || recipe.Status != creations.DeferredRecipeStatusPending || len(recipe.InputTemplate) == 0 || recipe.CreatedAt.IsZero() || !recipe.CreatedAt.Equal(recipe.UpdatedAt) {
		fixture.t.Fatal("缺少有效的第二步冻结配方")
	}
}

type creationFixtureState struct {
	users               map[string]*identity.User
	creations           map[string]*creations.Creation
	steps               map[string][]creations.CreationStep
	accounts            map[string]int64
	quotas              map[string]*fixtureQuota
	reservations        map[string]*ledger.Reservation
	ledgerEntries       map[string]*ledger.LedgerEntry
	reservationRequests map[string]ledger.ReserveRequest
	events              map[string]*outbox.Event
	deferredRecipes     map[string]*creations.DeferredRecipe
}

func newCreationFixtureState() *creationFixtureState {
	return &creationFixtureState{
		users:               make(map[string]*identity.User),
		creations:           make(map[string]*creations.Creation),
		steps:               make(map[string][]creations.CreationStep),
		accounts:            make(map[string]int64),
		quotas:              make(map[string]*fixtureQuota),
		reservations:        make(map[string]*ledger.Reservation),
		ledgerEntries:       make(map[string]*ledger.LedgerEntry),
		reservationRequests: make(map[string]ledger.ReserveRequest),
		events:              make(map[string]*outbox.Event),
		deferredRecipes:     make(map[string]*creations.DeferredRecipe),
	}
}

func (state *creationFixtureState) snapshot() *creationFixtureState {
	copyState := newCreationFixtureState()
	for userID, user := range state.users {
		userCopy := *user
		copyState.users[userID] = &userCopy
	}
	for key, creation := range state.creations {
		creationCopy := *creation
		copyState.creations[key] = &creationCopy
	}
	for creationID, steps := range state.steps {
		copyState.steps[creationID] = append([]creations.CreationStep(nil), steps...)
	}
	for userID, balance := range state.accounts {
		copyState.accounts[userID] = balance
	}
	for key, quota := range state.quotas {
		quotaCopy := *quota
		copyState.quotas[key] = &quotaCopy
	}
	for creationID, reservation := range state.reservations {
		reservationCopy := *reservation
		copyState.reservations[creationID] = &reservationCopy
	}
	for key, entry := range state.ledgerEntries {
		entryCopy := *entry
		copyState.ledgerEntries[key] = &entryCopy
	}
	for creationID, request := range state.reservationRequests {
		copyState.reservationRequests[creationID] = request
	}
	for eventID, event := range state.events {
		eventCopy := *event
		eventCopy.Payload = append([]byte(nil), event.Payload...)
		copyState.events[eventID] = &eventCopy
	}
	for stepID, recipe := range state.deferredRecipes {
		recipeCopy := *recipe
		recipeCopy.InputTemplate = append([]byte(nil), recipe.InputTemplate...)
		copyState.deferredRecipes[stepID] = &recipeCopy
	}
	return copyState
}

func (state *creationFixtureState) restore(snapshot *creationFixtureState) {
	state.users = snapshot.users
	state.creations = snapshot.creations
	state.steps = snapshot.steps
	state.accounts = snapshot.accounts
	state.quotas = snapshot.quotas
	state.reservations = snapshot.reservations
	state.ledgerEntries = snapshot.ledgerEntries
	state.reservationRequests = snapshot.reservationRequests
	state.events = snapshot.events
	state.deferredRecipes = snapshot.deferredRecipes
}

type fixtureQuota struct {
	Limit int32
	Used  int32
}

type fixtureUserReader struct {
	state     *creationFixtureState
	findCalls int
}

func (reader *fixtureUserReader) Find(_ context.Context, userID string) (*identity.User, error) {
	reader.findCalls++
	user := reader.state.users[userID]
	if user == nil {
		return nil, identity.ErrUserNotFound
	}
	copyUser := *user
	return &copyUser, nil
}

type fixtureSubscriptionReader struct {
	subscriptions map[string]*entitlement.SubscriptionSnapshot
	failure       error
	calls         int
	lastContext   context.Context
}

func (reader *fixtureSubscriptionReader) FindByUserID(ctx context.Context, userID string) (*entitlement.SubscriptionSnapshot, error) {
	reader.calls++
	reader.lastContext = ctx
	if reader.failure != nil {
		return nil, reader.failure
	}
	subscription := reader.subscriptions[userID]
	if subscription == nil {
		return nil, nil
	}
	copySubscription := *subscription
	return &copySubscription, nil
}

type fixtureCreationRepository struct {
	state                  *creationFixtureState
	recipeWriter           *fixtureDeferredRecipeWriter
	simulateWinningRace    bool
	raceWinner             *fixtureRaceWinner
	findByIdempotencyCalls int
	createCalls            int
}

func (repository *fixtureCreationRepository) DeferredRecipeWriter() creations.DeferredRecipeWriter {
	return repository.recipeWriter
}

type fixtureDeferredRecipeWriter struct {
	state   *creationFixtureState
	calls   int
	failure error
}

func (writer *fixtureDeferredRecipeWriter) Create(_ context.Context, recipe *creations.DeferredRecipe) error {
	writer.calls++
	if writer.failure != nil {
		return writer.failure
	}
	if recipe == nil {
		return errors.New("deferred recipe is required")
	}
	copyRecipe := *recipe
	copyRecipe.InputTemplate = append([]byte(nil), recipe.InputTemplate...)
	if recipe.B2B != nil {
		normalized, err := recipe.B2B.Normalize()
		if err != nil {
			return err
		}
		copyRecipe.B2B = &normalized
	}
	writer.state.deferredRecipes[recipe.StepID] = &copyRecipe
	return nil
}

func (repository *fixtureCreationRepository) FindByIdempotencyKey(_ context.Context, key string) (*creations.Creation, error) {
	repository.findByIdempotencyCalls++
	creation := repository.state.creations[key]
	if creation != nil {
		copyCreation := *creation
		return &copyCreation, nil
	}
	if repository.raceWinner != nil && repository.raceWinner.creation.IdempotencyKey == key {
		copyCreation := *repository.raceWinner.creation
		return &copyCreation, nil
	}
	return nil, nil
}

func (repository *fixtureCreationRepository) ListSteps(_ context.Context, creationID string) ([]creations.CreationStep, error) {
	if steps := repository.state.steps[creationID]; steps != nil {
		return append([]creations.CreationStep(nil), steps...), nil
	}
	if repository.raceWinner != nil && repository.raceWinner.creation.ID == creationID {
		return append([]creations.CreationStep(nil), repository.raceWinner.steps...), nil
	}
	return nil, nil
}

func (repository *fixtureCreationRepository) Create(_ context.Context, creation *creations.Creation, steps []creations.CreationStep) error {
	repository.createCalls++
	if repository.simulateWinningRace {
		repository.simulateWinningRace = false
		repository.raceWinner = newFixtureRaceWinner(creation, steps)
		return creations.ErrCreationAlreadyExists
	}
	if repository.state.creations[creation.IdempotencyKey] != nil {
		return creations.ErrCreationAlreadyExists
	}
	creationCopy := *creation
	repository.state.creations[creation.IdempotencyKey] = &creationCopy
	repository.state.steps[creation.ID] = append([]creations.CreationStep(nil), steps...)
	return nil
}

type fixtureRaceWinner struct {
	creation    *creations.Creation
	steps       []creations.CreationStep
	reservation *ledger.Reservation
	ledgerEntry *ledger.LedgerEntry
}

func newFixtureRaceWinner(creation *creations.Creation, steps []creations.CreationStep) *fixtureRaceWinner {
	creationCopy := *creation
	creationCopy.ID = "race-winner:" + creation.ID
	winnerSteps := make([]creations.CreationStep, len(steps))
	for index, step := range steps {
		stepCopy := step
		stepCopy.ID = "race-winner:" + step.ID
		stepCopy.CreationID = creationCopy.ID
		winnerSteps[index] = stepCopy
	}
	reservation := &ledger.Reservation{
		ID:              "reservation:" + creationCopy.ID,
		CreationID:      creationCopy.ID,
		UserID:          creationCopy.UserID,
		PriceDiamonds:   20,
		Source:          ledger.BenefitSourceDiamonds,
		ChargedDiamonds: 20,
		Status:          ledger.ReservationStatusReserved,
	}
	entry := &ledger.LedgerEntry{
		IdempotencyKey: "reserve:" + creationCopy.ID,
		CreationID:     creationCopy.ID,
		DeltaDiamonds:  -20,
		Reason:         "generation_reserved",
	}
	return &fixtureRaceWinner{
		creation:    &creationCopy,
		steps:       winnerSteps,
		reservation: reservation,
		ledgerEntry: entry,
	}
}

type fixtureReservationUsecase struct {
	state          *creationFixtureState
	repository     *fixtureCreationRepository
	failAfterWrite error
	calls          int
	debitAttempts  int
}

type fixtureOutboxWriter struct {
	state       *creationFixtureState
	failure     error
	calls       int
	lastContext context.Context
}

func (writer *fixtureOutboxWriter) Enqueue(ctx context.Context, event *outbox.Event) error {
	writer.calls++
	writer.lastContext = ctx
	if writer.failure != nil {
		return writer.failure
	}
	if event == nil {
		return errors.New("event is required")
	}
	eventCopy := *event
	eventCopy.Payload = append([]byte(nil), event.Payload...)
	writer.state.events[event.ID] = &eventCopy
	return nil
}

func (usecase *fixtureReservationUsecase) ReserveInTx(_ context.Context, request ledger.ReserveRequest) (*ledger.Reservation, error) {
	usecase.calls++
	if reservation := usecase.state.reservations[request.CreationID]; reservation != nil {
		return reservation, nil
	}

	consumedQuota := false
	if request.Quota.Limit > 0 && request.Quota.Units > 0 {
		quota := usecase.state.quotas[quotaKey(request.UserID, request.Quota.Kind, request.Quota.LocalDate)]
		if quota != nil && quota.Limit == request.Quota.Limit && quota.Used+request.Quota.Units <= quota.Limit {
			quota.Used += request.Quota.Units
			consumedQuota = true
		}
	}

	reservation := &ledger.Reservation{
		ID:            "reservation:" + request.CreationID,
		CreationID:    request.CreationID,
		UserID:        request.UserID,
		PriceDiamonds: request.PriceDiamonds,
		Quota:         request.Quota,
		Status:        ledger.ReservationStatusReserved,
	}
	if consumedQuota {
		reservation.Source = ledger.BenefitSourceDailyQuota
	} else {
		usecase.debitAttempts++
		balance := usecase.state.accounts[request.UserID]
		if balance < request.PriceDiamonds {
			return nil, shared.ErrInsufficientFunds
		}
		usecase.state.accounts[request.UserID] = balance - request.PriceDiamonds
		reservation.Source = ledger.BenefitSourceDiamonds
		reservation.ChargedDiamonds = request.PriceDiamonds
	}

	usecase.state.reservations[request.CreationID] = reservation
	usecase.state.reservationRequests[request.CreationID] = request
	usecase.state.ledgerEntries["reserve:"+request.CreationID] = &ledger.LedgerEntry{
		IdempotencyKey: "reserve:" + request.CreationID,
		CreationID:     request.CreationID,
		DeltaDiamonds:  -reservation.ChargedDiamonds,
		Reason:         "generation_reserved",
	}
	if usecase.failAfterWrite != nil {
		return nil, usecase.failAfterWrite
	}
	return reservation, nil
}

func (usecase *fixtureReservationUsecase) FindReservation(_ context.Context, creationID string) (*ledger.Reservation, error) {
	if winner := usecase.repository.raceWinner; winner != nil && winner.creation.ID == creationID {
		return winner.reservation, nil
	}
	return usecase.state.reservations[creationID], nil
}

func (usecase *fixtureReservationUsecase) CurrentDiamondBalance(_ context.Context, userID string) (int64, error) {
	if winner := usecase.repository.raceWinner; winner != nil && winner.creation.UserID == userID {
		return usecase.state.accounts[userID] - winner.reservation.ChargedDiamonds, nil
	}
	return usecase.state.accounts[userID], nil
}

type fixtureTxRunner struct {
	state     *creationFixtureState
	retryOnce bool
	attempts  []fixtureTransactionAttempt
	calls     int
}

type fixtureTxContextKey struct{}

var fixtureTxContextMarker = &struct{}{}

func (runner *fixtureTxRunner) WithinTx(ctx context.Context, callback func(context.Context) error) error {
	runner.calls++
	snapshot := runner.state.snapshot()
	txCtx := context.WithValue(ctx, fixtureTxContextKey{}, fixtureTxContextMarker)
	if err := callback(txCtx); err != nil {
		runner.state.restore(snapshot)
		return err
	}
	if runner.retryOnce {
		runner.attempts = append(runner.attempts, runner.captureAttempt())
		runner.state.restore(snapshot)
		if err := callback(txCtx); err != nil {
			runner.state.restore(snapshot)
			return err
		}
		runner.attempts = append(runner.attempts, runner.captureAttempt())
	}
	return nil
}

type fixtureTransactionAttempt struct {
	createdAt      time.Time
	stepCreatedAt  []time.Time
	reserveRequest ledger.ReserveRequest
}

func (runner *fixtureTxRunner) captureAttempt() fixtureTransactionAttempt {
	attempt := fixtureTransactionAttempt{}
	for _, creation := range runner.state.creations {
		attempt.createdAt = creation.CreatedAt
	}
	for _, steps := range runner.state.steps {
		for _, step := range steps {
			attempt.stepCreatedAt = append(attempt.stepCreatedAt, step.CreatedAt)
		}
	}
	for _, request := range runner.state.reservationRequests {
		attempt.reserveRequest = request
	}
	return attempt
}

type fixtureEntitlementEvaluator struct {
	businessTimes []time.Time
}

func (evaluator *fixtureEntitlementEvaluator) EvaluateGenerationAt(user entitlement.UserSnapshot, request entitlement.GenerationRequest, now time.Time) (*entitlement.GenerationDecision, error) {
	evaluator.businessTimes = append(evaluator.businessTimes, now)
	return entitlement.EvaluateGenerationAt(user, request, now)
}

func (evaluator *fixtureEntitlementEvaluator) ResolveDailyBenefitsAt(user entitlement.UserSnapshot, now time.Time) (*entitlement.DailyBenefits, error) {
	evaluator.businessTimes = append(evaluator.businessTimes, now)
	return entitlement.ResolveDailyBenefitsAt(user, now)
}

type fixtureSequenceClock struct {
	values []time.Time
	calls  int
}

func (clock *fixtureSequenceClock) Now() time.Time {
	index := clock.calls
	clock.calls++
	if index >= len(clock.values) {
		index = len(clock.values) - 1
	}
	return clock.values[index]
}

func quotaKey(userID, kind, localDate string) string {
	return userID + ":" + kind + ":" + localDate
}
