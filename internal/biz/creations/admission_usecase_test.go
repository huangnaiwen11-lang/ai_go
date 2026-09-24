package creations_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/outbox"
)

// stubAdmissionResolver 记录调用并把预置结果原样返回，用于验证用例层的归属冻结行为。
type stubAdmissionResolver struct {
	admission creations.StepAdmission
	err       error
	requests  []creations.AdmissionRequest
}

func (resolver *stubAdmissionResolver) ResolveAdmission(_ context.Context, request creations.AdmissionRequest) (creations.StepAdmission, error) {
	resolver.requests = append(resolver.requests, request)
	if resolver.err != nil {
		return creations.StepAdmission{}, resolver.err
	}
	return resolver.admission, nil
}

func frozenB2BRecipe() creations.B2BProductRecipe {
	return creations.B2BProductRecipe{
		ProductKey:  "ps-image-v1",
		TemplateKey: "portrait",
		Input:       json.RawMessage(`{"prompt":"海边的灯塔","aspectRatio":"1:1"}`),
	}
}

func frozenB2BRoute() creations.ExecutionRoute {
	return creations.ExecutionRoute{
		Provider:        creations.PolarStarB2BProvider,
		AccountRef:      "acct-main",
		ContractVersion: creations.B2BContractVersion,
		MappingVersion:  "polarstar.image.v1",
	}
}

// 归属解析失败时不得写入创作、步骤、预留、分录或发件箱事件。
func TestCreateReserved归属解析失败时不写入任何事实(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)
	resolver := &stubAdmissionResolver{err: creations.ErrAdmissionDependencyUnavailable}
	fixture.withAdmissions(resolver)

	_, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest())

	if !errors.Is(err, creations.ErrAdmissionDependencyUnavailable) {
		t.Fatalf("CreateReserved() error = %v, want ErrAdmissionDependencyUnavailable", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
	if len(resolver.requests) != 1 {
		t.Fatalf("解析调用次数 = %d, want 1", len(resolver.requests))
	}
}

// 解析失败必须发生在写入之前：连事务都不应该开启。
func TestCreateReserved归属解析失败时不开启事务(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)
	fixture.withAdmissions(&stubAdmissionResolver{err: creations.ErrAdmissionDependencyUnavailable})

	if _, err := fixture.usecase.CreateReserved(context.Background(), fixture.imageRequest()); !errors.Is(err, creations.ErrAdmissionDependencyUnavailable) {
		t.Fatalf("CreateReserved() error = %v, want ErrAdmissionDependencyUnavailable", err)
	}
	if fixture.transaction.calls != 0 || fixture.repository.createCalls != 0 || fixture.reservations.calls != 0 {
		t.Fatalf("解析失败不得开启事务或写入事实：tx=%d create=%d reserve=%d", fixture.transaction.calls, fixture.repository.createCalls, fixture.reservations.calls)
	}
}

// B2B 归属必须原样落到步骤上，并把公开配方冻结成发件箱载荷。
func TestCreateReserved冻结B2B归属与公开配方(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)
	recipe := frozenB2BRecipe()
	resolver := &stubAdmissionResolver{admission: creations.StepAdmission{Route: frozenB2BRoute(), B2B: &recipe}}
	fixture.withAdmissions(resolver)
	request := fixture.imageRequest()
	request.B2BSubmission = &recipe

	result, err := fixture.usecase.CreateReserved(context.Background(), request)

	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Route != frozenB2BRoute() {
		t.Fatalf("Steps = %#v, want one step frozen to %#v", result.Steps, frozenB2BRoute())
	}
	if len(resolver.requests) != 1 {
		t.Fatalf("解析调用次数 = %d, want 1", len(resolver.requests))
	}
	seen := resolver.requests[0]
	if seen.UserID != "user-1" || seen.TemplateID != "template-image-1" || seen.Atom != creations.AtomTextToImage || seen.Sequence != 1 {
		t.Fatalf("解析请求 = %#v, want 首步骤归属事实", seen)
	}
	if seen.B2B == nil || seen.B2B.ProductKey != recipe.ProductKey {
		t.Fatalf("解析请求未携带公开配方：%#v", seen.B2B)
	}
	if len(fixture.state.events) != 1 {
		t.Fatalf("发件箱事件数 = %d, want 1", len(fixture.state.events))
	}
	for _, event := range fixture.state.events {
		parsed, err := creations.ParseB2BProductRecipe(event.Payload)
		if err != nil {
			t.Fatalf("发件箱载荷不是可还原的公开配方：%v", err)
		}
		if parsed.ProductKey != recipe.ProductKey || parsed.TemplateKey != recipe.TemplateKey || string(parsed.Input) != string(recipe.Input) {
			t.Fatalf("发件箱载荷 = %#v, want %#v", parsed, recipe)
		}
	}
}

// 编译器已按公开合同编译，但归属仍解析成本地：这是静默降级，必须拒绝而不是丢配方。
func TestCreateReserved拒绝已编译配方被解析为本地归属(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)
	recipe := frozenB2BRecipe()
	fixture.withAdmissions(&stubAdmissionResolver{admission: creations.StepAdmission{Route: creations.LocalExecutionRoute()}})
	request := fixture.imageRequest()
	request.B2BSubmission = &recipe

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, creations.ErrB2BProductRecipeUnavailable) {
		t.Fatalf("CreateReserved() error = %v, want ErrB2BProductRecipeUnavailable", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
}

// 两步计划的第二步还没有公开配方可用，此时冻结 B2B 归属只会产出无法提交的步骤。
func TestCreateReserved拒绝两步计划冻结B2B归属(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 100)
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 10, 0)
	recipe := frozenB2BRecipe()
	fixture.withAdmissions(&stubAdmissionResolver{admission: creations.StepAdmission{Route: frozenB2BRoute(), B2B: &recipe}})
	request := fixture.videoRequest(5, false)
	request.InitialSubmission = nil
	request.B2BSubmission = &recipe

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, creations.ErrB2BProductRecipeUnavailable) {
		t.Fatalf("CreateReserved() error = %v, want ErrB2BProductRecipeUnavailable", err)
	}
	fixture.assertNoCreationFacts("idem-video-5")
}

// 两步 B2B 任务必须在预扣事务内同时冻结两条 route 和未绑定的第二阶段配方；
// 第一阶段事件仍只能携带第一阶段的公开配方，不能提前把不存在的首帧塞进去。
func TestCreateReserved冻结两步B2B归属与未绑定第二阶段配方(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 100)
	fixture.addActiveSubscription("user-1")
	fixture.addDailyQuota("user-1", "vip_daily_video", fixture.localDate, 10, 0)
	fixture.withAdmissions(twoStepB2BAdmissionResolver{})

	first := frozenB2BRecipe()
	second, err := creations.CompileDeferredB2BImageToVideo(creations.PublishedB2BProductRecipe{
		TemplateID: "template-video", TemplateVersion: 1, Atom: creations.AtomImageToVideo,
		ProductKey: "video-standard", Input: json.RawMessage(`{"prompt":"cinematic motion","durationSeconds":5}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := fixture.videoRequest(5, false)
	request.InitialSubmission = nil
	request.DeferredImageToVideo = nil
	request.B2BSubmission = &first
	request.DeferredB2BImageToVideo = second

	result, err := fixture.usecase.CreateReserved(context.Background(), request)
	if err != nil {
		t.Fatalf("CreateReserved() error = %v", err)
	}
	if len(result.Steps) != 2 || result.Steps[0].Route != frozenB2BRoute() || result.Steps[1].Route != frozenB2BRoute() || result.Steps[0].SubmitStatus != creations.StepSubmitStatusReady || result.Steps[1].SubmitStatus != creations.StepSubmitStatusBlocked {
		t.Fatalf("steps = %#v", result.Steps)
	}
	deferred := fixture.state.deferredRecipes[result.Steps[1].ID]
	if deferred == nil || deferred.Protocol != creations.DeferredRecipeProtocolB2B || deferred.B2B == nil || deferred.B2B.Digest != second.Digest || deferred.ModelSKU != "" || len(deferred.InputTemplate) != 0 || deferred.Status != creations.DeferredRecipeStatusPending {
		t.Fatalf("deferred recipe = %#v", deferred)
	}
	if len(fixture.state.events) != 1 {
		t.Fatalf("events = %#v, want exactly first-step submission", fixture.state.events)
	}
	for eventID, event := range fixture.state.events {
		if eventID != outbox.SubmissionEventID(result.Steps[0].ID) {
			t.Fatalf("unexpected event id %q", eventID)
		}
		parsed, err := creations.ParseB2BProductRecipe(event.Payload)
		if err != nil || parsed.ProductKey != first.ProductKey {
			t.Fatalf("first-step payload = %#v / %v", parsed, err)
		}
	}
}

type twoStepB2BAdmissionResolver struct{}

func (twoStepB2BAdmissionResolver) ResolveAdmission(_ context.Context, request creations.AdmissionRequest) (creations.StepAdmission, error) {
	if request.B2B == nil {
		return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
	}
	copyRecipe, err := request.B2B.Normalize()
	if err != nil {
		return creations.StepAdmission{}, err
	}
	return creations.StepAdmission{Route: frozenB2BRoute(), B2B: &copyRecipe}, nil
}

func (resolver twoStepB2BAdmissionResolver) ResolveAdmissions(ctx context.Context, requests []creations.AdmissionRequest) ([]creations.StepAdmission, error) {
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

// 同一请求同时编译两种合同说明调用方不确定目标 provider，必须在事务前拒绝。
func TestCreateReserved拒绝同时编译两种合同(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)
	recipe := frozenB2BRecipe()
	fixture.withAdmissions(&stubAdmissionResolver{admission: creations.StepAdmission{Route: frozenB2BRoute(), B2B: &recipe}})
	request := fixture.imageRequest()
	request.B2BSubmission = &recipe
	request.InitialSubmission = &creations.InitialSubmission{
		ModelSKU: "ps-image-v1",
		Input:    json.RawMessage(`{"prompt":"first","assets":[],"parameters":{}}`),
	}

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, creations.ErrInvalidCreateCommand) {
		t.Fatalf("CreateReserved() error = %v, want ErrInvalidCreateCommand", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
}

// 未装配 B2B admission 时，默认装配下的 B2B 配方必须在事务前被拒绝。
func TestCreateReserved默认装配拒绝公开配方(t *testing.T) {
	fixture := newCreationFixture(t)
	fixture.addBoundUser("user-1", "Asia/Shanghai")
	fixture.addAccount("user-1", 40)
	recipe := frozenB2BRecipe()
	request := fixture.imageRequest()
	request.B2BSubmission = &recipe

	_, err := fixture.usecase.CreateReserved(context.Background(), request)

	if !errors.Is(err, creations.ErrAdmissionConfigurationUnavailable) {
		t.Fatalf("CreateReserved() error = %v, want ErrAdmissionConfigurationUnavailable", err)
	}
	fixture.assertNoCreationFacts("idem-image-1")
}
