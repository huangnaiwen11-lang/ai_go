package video_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"ai-business-service/internal/biz/creations"
	bizvideo "ai-business-service/internal/biz/video"
	"ai-business-service/internal/executionv2"
)

func TestCompileTemplateVideo图片路径创建单步I2V(t *testing.T) {
	compiled, err := bizvideo.CompileTemplateVideo(validTemplateRecipe(), bizvideo.TemplateVideoInput{
		UserImageURL: "https://uploads.example.test/source.png",
		Duration:     5,
	})

	if err != nil {
		t.Fatalf("CompileTemplateVideo() error = %v", err)
	}
	if compiled.Product.Output != "video" || compiled.Product.Video.DurationSeconds != 5 || compiled.Product.Video.ReferenceImageCount != 1 || len(compiled.Plan) != 1 || compiled.Plan[0].Atom != creations.AtomImageToVideo || compiled.DeferredImageToVideo != nil {
		t.Fatalf("图片路径编译结果错误：%#v", compiled)
	}
	snapshot, err := executionv2.Compile(executionv2.CapabilityImageToVideo, compiled.InitialSubmission.ModelSKU, compiled.InitialSubmission.Input)
	if err != nil || snapshot.ModelSKU != "ps-auto" || len(snapshot.Input.Assets) != 1 || snapshot.Input.Assets[0].Role != "source_image" || snapshot.Input.Assets[0].URL != "https://uploads.example.test/source.png" {
		t.Fatalf("图片路径 I2V 快照错误：snapshot=%#v err=%v", snapshot, err)
	}
}

func TestCompileTemplateVideo文本路径冻结首帧与I2V(t *testing.T) {
	compiled, err := bizvideo.CompileTemplateVideo(validTemplateRecipe(), bizvideo.TemplateVideoInput{
		UserPrompt: "海边漫步",
		Duration:   5,
	})

	if err != nil {
		t.Fatalf("CompileTemplateVideo() error = %v", err)
	}
	if len(compiled.Plan) != 2 || compiled.Plan[0].Atom != creations.AtomTextToImage || compiled.Plan[1].Atom != creations.AtomImageToVideo || compiled.InitialSubmission.ModelSKU != "ps-image-v1" || compiled.DeferredImageToVideo == nil {
		t.Fatalf("文本路径编译结果错误：%#v", compiled)
	}
	if _, err := executionv2.Compile(executionv2.CapabilityTextToImage, compiled.InitialSubmission.ModelSKU, compiled.InitialSubmission.Input); err != nil {
		t.Fatalf("首帧快照不合法：%v", err)
	}
	if _, err := executionv2.CompileDeferredImageToVideo(compiled.DeferredImageToVideo.ModelSKU, compiled.DeferredImageToVideo.InputTemplate); err != nil {
		t.Fatalf("冻结 I2V 配方不合法：%v", err)
	}
}

func TestCompileTemplateVideo拒绝混合输入与非法配方(t *testing.T) {
	_, err := bizvideo.CompileTemplateVideo(validTemplateRecipe(), bizvideo.TemplateVideoInput{
		UserImageURL: "https://uploads.example.test/source.png",
		UserPrompt:   "不能混用",
		Duration:     5,
	})
	if !errors.Is(err, bizvideo.ErrInvalidTemplateVideoRequest) {
		t.Fatalf("混合输入错误 = %v，期望 ErrInvalidTemplateVideoRequest", err)
	}
	invalid := validTemplateRecipe()
	invalid.I2V.ModelSKU = "client-controlled-model"
	_, err = bizvideo.CompileTemplateVideo(invalid, bizvideo.TemplateVideoInput{UserImageURL: "https://uploads.example.test/source.png", Duration: 5})
	if !errors.Is(err, bizvideo.ErrInvalidTemplateVideoRequest) {
		t.Fatalf("非法配方错误 = %v，期望 ErrInvalidTemplateVideoRequest", err)
	}
}

func TestUsecase拒绝空内容访问且不创建任务(t *testing.T) {
	creator := &recordingCreator{}
	usecase := bizvideo.NewUsecase(memoryTemplateReader{recipe: validTemplateRecipe()}, creator, allowedOwnedImageReader{})

	_, err := usecase.Create(t.Context(), bizvideo.CreateCommand{
		UserID: "user-1", IdempotencyKey: "gateway:video:image:request-1", TemplateID: "video-template-1",
		Input: bizvideo.TemplateVideoInput{UserImageURL: "https://uploads.example.test/source.png", Duration: 5},
	})

	if !errors.Is(err, bizvideo.ErrInvalidTemplateVideoRequest) || creator.calls != 0 {
		t.Fatalf("Create() error = %v，创建次数 = %d", err, creator.calls)
	}
}

// I2V 不能接受任意 HTTPS 图片。图片必须先经 Go 自有素材域确认归属，
// 且授权失败必须发生在创建占位与预扣之前。
func TestUsecase图片素材未授权时不创建视频任务(t *testing.T) {
	creator := &recordingCreator{}
	usecase := bizvideo.NewUsecase(
		memoryTemplateReader{recipe: validTemplateRecipe()}, creator,
		deniedOwnedImageReader{},
	)

	_, err := usecase.Create(t.Context(), bizvideo.CreateCommand{
		UserID: "user-1", ContentAccess: "standard", IdempotencyKey: "gateway:video:image:request-2", TemplateID: "video-template-1",
		Input: bizvideo.TemplateVideoInput{UserImageURL: "https://local-media.invalid/assets/550e8400-e29b-41d4-a716-446655440000", Duration: 5},
	})

	if !errors.Is(err, bizvideo.ErrInvalidTemplateVideoRequest) || creator.calls != 0 {
		t.Fatalf("Create() error = %v，创建次数 = %d", err, creator.calls)
	}
}

// B2B 只支持已经有用户自有首帧的单步 I2V。文本到视频仍依赖首帧素材发布与
// 第二步激活，未实现前必须在预扣前拒绝，不能伪装成单步产品。
func TestUsecase按发布B2B产品配方创建单步I2V(t *testing.T) {
	creator := &recordingCreator{}
	products := &recordingB2BProductRecipeReader{recipe: creations.PublishedB2BProductRecipe{
		TemplateID: "video-template-1", TemplateVersion: 1, Atom: creations.AtomImageToVideo,
		ProductKey: "video-standard", Input: json.RawMessage(`{"prompt":"cinematic movement"}`),
		AllowedUserInputs: []string{"durationSeconds"},
	}}
	usecase := bizvideo.NewUsecaseWithB2BProductRecipes(memoryTemplateReader{recipe: validTemplateRecipe()}, products, creator, allowedOwnedImageReader{})
	_, err := usecase.Create(t.Context(), bizvideo.CreateCommand{
		UserID: "user-1", ContentAccess: "standard", IdempotencyKey: "gateway:video:image:b2b-request", TemplateID: "video-template-1",
		Input: bizvideo.TemplateVideoInput{UserImageURL: "https://assets.example.com/source.png", Duration: 10},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if creator.calls != 1 || creator.request.InitialSubmission != nil || creator.request.B2BSubmission == nil {
		t.Fatalf("B2B 创建请求 = %#v", creator.request)
	}
	if creator.request.Product.Video.DurationSeconds != 10 || creator.request.Product.Video.ReferenceImageCount != 1 || len(creator.request.Plan) != 1 || creator.request.Plan[0].Atom != creations.AtomImageToVideo {
		t.Fatalf("B2B 视频产品或计划 = %#v", creator.request)
	}
	if string(creator.request.B2BSubmission.Input) != `{"durationSeconds":10,"prompt":"cinematic movement"}` || len(creator.request.B2BSubmission.Assets) != 1 || creator.request.B2BSubmission.Assets[0] != (creations.B2BAsset{Role: "source_image", URL: "https://assets.example.com/source.png"}) {
		t.Fatalf("B2B 视频配方 = %#v", creator.request.B2BSubmission)
	}
}

func TestUsecaseB2B文本视频在预扣前拒绝(t *testing.T) {
	creator := &recordingCreator{}
	usecase := bizvideo.NewUsecaseWithB2BProductRecipes(memoryTemplateReader{recipe: validTemplateRecipe()}, &recordingB2BProductRecipeReader{}, creator, allowedOwnedImageReader{})
	_, err := usecase.Create(t.Context(), bizvideo.CreateCommand{
		UserID: "user-1", ContentAccess: "standard", IdempotencyKey: "gateway:video:text:b2b-request", TemplateID: "video-template-1",
		Input: bizvideo.TemplateVideoInput{UserPrompt: "海边漫步", Duration: 5},
	})
	if !errors.Is(err, creations.ErrB2BProductRecipeUnavailable) || creator.calls != 0 {
		t.Fatalf("Create() error = %v, create calls = %d", err, creator.calls)
	}
}

func TestUsecase按发布B2B产品配方创建两步文本视频(t *testing.T) {
	creator := &recordingCreator{}
	products := &recordingB2BProductRecipeReader{recipes: map[creations.StepAtom]creations.PublishedB2BProductRecipe{
		creations.AtomTextToImage: {
			TemplateID: "video-template-1", TemplateVersion: 1, Atom: creations.AtomTextToImage,
			ProductKey: "image-standard", Input: json.RawMessage(`{"prompt":"cinematic frame","aspectRatio":"9:16"}`),
			AllowedUserInputs: []string{"prompt"}, PromptUserInputMode: creations.PromptUserInputModeAppend,
		},
		creations.AtomImageToVideo: {
			TemplateID: "video-template-1", TemplateVersion: 1, Atom: creations.AtomImageToVideo,
			ProductKey: "video-standard", Input: json.RawMessage(`{"prompt":"cinematic motion"}`),
			AllowedUserInputs: []string{"durationSeconds"},
		},
	}}
	usecase := bizvideo.NewUsecaseWithB2BProductRecipes(memoryTemplateReader{recipe: validTemplateRecipe()}, products, creator, allowedOwnedImageReader{})
	_, err := usecase.Create(t.Context(), bizvideo.CreateCommand{
		UserID: "user-1", ContentAccess: "standard", IdempotencyKey: "gateway:video:text:b2b-two-step", TemplateID: "video-template-1",
		Input: bizvideo.TemplateVideoInput{UserPrompt: "海边漫步", Duration: 5},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if creator.calls != 1 || creator.request.InitialSubmission != nil || creator.request.DeferredImageToVideo != nil || creator.request.B2BSubmission == nil || creator.request.DeferredB2BImageToVideo == nil {
		t.Fatalf("two-step B2B creation request = %#v", creator.request)
	}
	if len(creator.request.Plan) != 2 || creator.request.Plan[0].Atom != creations.AtomTextToImage || creator.request.Plan[1].Atom != creations.AtomImageToVideo || creator.request.Product.Video.ReferenceImageCount != 0 || creator.request.Product.Video.DurationSeconds != 5 {
		t.Fatalf("two-step B2B plan/product = %#v", creator.request)
	}
	if string(creator.request.B2BSubmission.Input) != `{"aspectRatio":"9:16","prompt":"cinematic frame，海边漫步"}` || len(creator.request.B2BSubmission.Assets) != 0 {
		t.Fatalf("first B2B product = %#v", creator.request.B2BSubmission)
	}
	deferred := creator.request.DeferredB2BImageToVideo
	if deferred.Digest == "" || string(deferred.Recipe.Input) != `{"durationSeconds":5,"prompt":"cinematic motion"}` || len(deferred.Recipe.Assets) != 0 {
		t.Fatalf("unbound second B2B product = %#v", deferred)
	}
}

func validTemplateRecipe() bizvideo.TemplateRecipe {
	return bizvideo.TemplateRecipe{
		TemplateID: "video-template-1",
		Version:    1,
		I2V: bizvideo.TechnicalRecipe{
			ModelSKU:       "ps-auto",
			Prompt:         "服务端动作提示词",
			NegativePrompt: "模糊",
			Parameters:     json.RawMessage(`{"durationSeconds":5}`),
		},
		T2I: &bizvideo.TechnicalRecipe{
			ModelSKU:       "ps-image-v1",
			Prompt:         "服务端首帧提示词",
			NegativePrompt: "模糊",
			Parameters:     json.RawMessage(`{"aspectRatio":"9:16"}`),
		},
	}
}

type memoryTemplateReader struct{ recipe bizvideo.TemplateRecipe }

func (reader memoryTemplateReader) LoadTemplateVideo(_ context.Context, _, _ string) (bizvideo.TemplateRecipe, error) {
	return reader.recipe, nil
}

type recordingCreator struct {
	calls   int
	request creations.CreateReservedRequest
}

func (creator *recordingCreator) CreateReserved(_ context.Context, request creations.CreateReservedRequest) (*creations.CreateReservedResult, error) {
	creator.calls++
	creator.request = request
	return &creations.CreateReservedResult{}, nil
}

type recordingB2BProductRecipeReader struct {
	recipe  creations.PublishedB2BProductRecipe
	recipes map[creations.StepAtom]creations.PublishedB2BProductRecipe
	err     error
}

func (reader *recordingB2BProductRecipeReader) LoadB2BProductRecipe(_ context.Context, _ string, _ int64, atom creations.StepAtom) (creations.PublishedB2BProductRecipe, error) {
	if reader.recipes != nil {
		return reader.recipes[atom], reader.err
	}
	return reader.recipe, reader.err
}

type deniedOwnedImageReader struct{}

func (deniedOwnedImageReader) ResolveOwnedImage(context.Context, string, string) (string, error) {
	return "", errors.New("素材不属于当前用户")
}

type allowedOwnedImageReader struct{}

func (allowedOwnedImageReader) ResolveOwnedImage(_ context.Context, _ string, reference string) (string, error) {
	return reference, nil
}
