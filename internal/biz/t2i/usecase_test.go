package t2i

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
)

// 公开 T2I 创建只能把服务端模板编译结果交给既有预扣用例，不能先读余额或自行写账本。
func TestUsecase创建自由文生图并复用预留(t *testing.T) {
	creator := &recordingReservedCreator{result: &creations.CreateReservedResult{}}
	usecase := NewUsecase(staticFreeformRecipeReader{recipe: validFreeformRecipe()}, creator)

	_, err := usecase.Create(context.Background(), CreateCommand{
		UserID:         "user-1",
		IdempotencyKey: "gateway:t2i:550e8400-e29b-41d4-a716-446655440000",
		Prompt:         "studio portrait",
		NegativePrompt: "blur",
		AspectRatio:    "1:1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	request := creator.request
	if request.UserID != "user-1" || request.IdempotencyKey != "gateway:t2i:550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("预留命令身份或幂等键 = %#v", request)
	}
	if request.Product != (entitlement.GenerationRequest{Output: entitlement.ProductOutputImage}) {
		t.Fatalf("产品 = %#v", request.Product)
	}
	if len(request.InputDigest) != 64 || request.InitialSubmission == nil || request.InitialSubmission.ModelSKU != "ps-image-v1" {
		t.Fatalf("冻结创建命令 = %#v", request)
	}
	var input map[string]any
	if err := json.Unmarshal(request.InitialSubmission.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input["prompt"] != "studio portrait" || input["negativePrompt"] != "blur" {
		t.Fatalf("用户输入 = %#v", input)
	}
}

// 模板读取失败时不能创建占位或预扣，避免在没有受控技术快照的情况下扣费。
func TestUsecase模板读取失败不调用预留(t *testing.T) {
	creator := &recordingReservedCreator{}
	usecase := NewUsecase(staticFreeformRecipeReader{err: errors.New("模板库不可用")}, creator)

	_, err := usecase.Create(context.Background(), CreateCommand{UserID: "user-1", IdempotencyKey: "gateway:t2i:request", Prompt: "portrait"})
	if err == nil {
		t.Fatal("模板读取失败仍创建了任务")
	}
	if creator.called {
		t.Fatal("模板读取失败仍调用了创作预留")
	}
}

// 用户上传的图片必须先由自有素材域确认归属。模板编辑不能接受任意外链，
// 否则用户可能借此使用其他账户的私有图片，或绕过内容与存储治理。
func TestImageEditUsecase只使用当前用户已确认的素材(t *testing.T) {
	creator := &recordingReservedCreator{result: &creations.CreateReservedResult{}}
	media := &recordingOwnedImageReader{resolvedURL: "https://local-media.invalid/assets/media-1"}
	usecase := NewImageEditUsecase(staticImageEditRecipeReader{recipe: validImageEditRecipe()}, creator, media)

	_, err := usecase.Create(context.Background(), ImageEditCommand{
		UserID: "user-1", ContentAccess: "standard", IdempotencyKey: "gateway:i2i:request-1",
		TemplateID: "dress-up", UserImageURL: "https://local-media.invalid/assets/media-1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if media.userID != "user-1" || media.reference != "https://local-media.invalid/assets/media-1" {
		t.Fatalf("素材归属校验输入 = %#v", media)
	}
	if !creator.called {
		t.Fatal("已确认素材没有进入既有预扣创建流程")
	}
}

// 越权或不存在素材必须在预扣之前失败，不能创建占位任务、更不能扣钻。
func TestImageEditUsecase素材不属于当前用户时不预扣(t *testing.T) {
	creator := &recordingReservedCreator{}
	usecase := NewImageEditUsecase(
		staticImageEditRecipeReader{recipe: validImageEditRecipe()}, creator,
		&recordingOwnedImageReader{err: errors.New("素材不属于当前用户")},
	)

	_, err := usecase.Create(context.Background(), ImageEditCommand{
		UserID: "user-1", ContentAccess: "standard", IdempotencyKey: "gateway:i2i:request-2",
		TemplateID: "dress-up", UserImageURL: "https://local-media.invalid/assets/other-user-media",
	})
	if err == nil {
		t.Fatal("越权素材仍被接受")
	}
	if creator.called {
		t.Fatal("越权素材仍进入了预扣创建")
	}
}

type staticFreeformRecipeReader struct {
	recipe FreeformRecipe
	err    error
}

func (reader staticFreeformRecipeReader) LoadFreeformRecipe(context.Context) (FreeformRecipe, error) {
	return reader.recipe, reader.err
}

type recordingReservedCreator struct {
	called  bool
	request creations.CreateReservedRequest
	result  *creations.CreateReservedResult
}

type recordingOwnedImageReader struct {
	userID      string
	reference   string
	resolvedURL string
	err         error
}

func (reader *recordingOwnedImageReader) ResolveOwnedImage(_ context.Context, userID, reference string) (string, error) {
	reader.userID = userID
	reader.reference = reference
	return reader.resolvedURL, reader.err
}

type staticImageEditRecipeReader struct {
	recipe ImageEditRecipe
	err    error
}

func (reader staticImageEditRecipeReader) LoadImageEditRecipe(context.Context, string, string) (ImageEditRecipe, error) {
	return reader.recipe, reader.err
}

func validImageEditRecipe() ImageEditRecipe {
	return ImageEditRecipe{
		TemplateID: "dress-up", Version: 1, ModelSKU: "ps-edit-apparel-v1", Prompt: "换装模板提示词",
		Parameters: json.RawMessage(`{"steps":28}`),
	}
}

func (creator *recordingReservedCreator) CreateReserved(_ context.Context, request creations.CreateReservedRequest) (*creations.CreateReservedResult, error) {
	creator.called = true
	creator.request = request
	return creator.result, nil
}

func validFreeformRecipe() FreeformRecipe {
	return FreeformRecipe{
		TemplateID: "t2i-freeform",
		Version:    1,
		ModelSKU:   "ps-image-v1",
		Parameters: json.RawMessage(`{"steps":28}`),
	}
}
