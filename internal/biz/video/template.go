// Package video 承载用户选择视频模板后的服务端编译与状态业务。
package video

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/executionv2"
)

// ErrInvalidTemplateVideoRequest 表示视频模板、用户输入或编译结果不满足固定合同。
var ErrInvalidTemplateVideoRequest = errors.New("video: invalid template video request")

// TechnicalRecipe 是模板控制的一份生成中台技术配方。
// 客户端不能填写模型、提示词模板、负面提示词或参数。
type TechnicalRecipe struct {
	ModelSKU       string
	Prompt         string
	NegativePrompt string
	Parameters     json.RawMessage
}

// TemplateRecipe 是唯一启用视频模板的服务端配方。
// I2V 用于两条路径；T2I 仅在文本生成视频路径中必需。
type TemplateRecipe struct {
	TemplateID string
	Version    int64
	I2V        TechnicalRecipe
	T2I        *TechnicalRecipe
}

// TemplateRecipeReader 按已验证内容访问级别读取唯一可见视频模板。
type TemplateRecipeReader interface {
	LoadTemplateVideo(context.Context, string, string) (TemplateRecipe, error)
}

// OwnedImageReader 由 Go 自有素材域实现，确保 I2V 只能引用当前用户已确认的图片。
// 视频模块只依赖窄接口，底层可从本地文件无缝替换为 R2/S3。
type OwnedImageReader interface {
	ResolveOwnedImage(context.Context, string, string) (string, error)
}

// TemplateVideoInput 是用户选择模板后允许填写的最小产品输入。
// 图片与文本必须二选一，避免把两个已审核编排混合成新流程。
type TemplateVideoInput struct {
	UserImageURL string
	UserPrompt   string
	Duration     int32
	EnableAudio  bool
}

// CompiledTemplateVideo 是冻结后交给创作预扣用例的内部计划。
type CompiledTemplateVideo struct {
	TemplateID           string
	TemplateVersion      int64
	Product              entitlement.GenerationRequest
	InputDigest          string
	Plan                 []creations.StepPlan
	InitialSubmission    *creations.InitialSubmission
	DeferredImageToVideo *creations.DeferredImageToVideo
}

// CompileTemplateVideo 将图片或文本模板输入编译为唯一的内部执行计划。
func CompileTemplateVideo(recipe TemplateRecipe, input TemplateVideoInput) (*CompiledTemplateVideo, error) {
	if strings.TrimSpace(recipe.TemplateID) == "" || recipe.Version <= 0 || !validVideoDuration(input.Duration) {
		return nil, ErrInvalidTemplateVideoRequest
	}
	imageURL := strings.TrimSpace(input.UserImageURL)
	userPrompt := strings.TrimSpace(input.UserPrompt)
	if (imageURL == "") == (userPrompt == "") || imageURL != input.UserImageURL {
		return nil, ErrInvalidTemplateVideoRequest
	}

	i2vInput, err := compileI2VInput(recipe.I2V, imageURL, input.Duration)
	if err != nil {
		return nil, ErrInvalidTemplateVideoRequest
	}
	product := entitlement.GenerationRequest{
		Output: entitlement.ProductOutputVideo,
		Video: entitlement.VideoOptions{
			DurationSeconds:     input.Duration,
			EnableAudio:         input.EnableAudio,
			ReferenceImageCount: 0,
		},
	}
	compiled := &CompiledTemplateVideo{
		TemplateID:      recipe.TemplateID,
		TemplateVersion: recipe.Version,
		Product:         product,
	}

	if imageURL != "" {
		compiled.Product.Video.ReferenceImageCount = 1
		if _, err := executionv2.Compile(executionv2.CapabilityImageToVideo, recipe.I2V.ModelSKU, i2vInput); err != nil {
			return nil, ErrInvalidTemplateVideoRequest
		}
		compiled.Plan = []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageToVideo}}
		compiled.InitialSubmission = &creations.InitialSubmission{ModelSKU: recipe.I2V.ModelSKU, Input: i2vInput}
		compiled.InputDigest = digestTechnicalInputs(i2vInput)
		return compiled, nil
	}

	if recipe.T2I == nil {
		return nil, ErrInvalidTemplateVideoRequest
	}
	firstFrameInput, err := compileT2IInput(*recipe.T2I, userPrompt)
	if err != nil {
		return nil, ErrInvalidTemplateVideoRequest
	}
	if _, err := executionv2.Compile(executionv2.CapabilityTextToImage, recipe.T2I.ModelSKU, firstFrameInput); err != nil {
		return nil, ErrInvalidTemplateVideoRequest
	}
	if _, err := executionv2.CompileDeferredImageToVideo(recipe.I2V.ModelSKU, i2vInput); err != nil {
		return nil, ErrInvalidTemplateVideoRequest
	}
	compiled.Plan = []creations.StepPlan{
		{Sequence: 1, Atom: creations.AtomTextToImage},
		{Sequence: 2, Atom: creations.AtomImageToVideo},
	}
	compiled.InitialSubmission = &creations.InitialSubmission{ModelSKU: recipe.T2I.ModelSKU, Input: firstFrameInput}
	compiled.DeferredImageToVideo = &creations.DeferredImageToVideo{ModelSKU: recipe.I2V.ModelSKU, InputTemplate: i2vInput}
	compiled.InputDigest = digestTechnicalInputs(firstFrameInput, i2vInput)
	return compiled, nil
}

// CreateCommand 是完成认证和 HTTP 字段校验后的视频模板创建命令。
type CreateCommand struct {
	UserID         string
	ContentAccess  string
	IdempotencyKey string
	TemplateID     string
	Input          TemplateVideoInput
}

type reservedCreator interface {
	CreateReserved(context.Context, creations.CreateReservedRequest) (*creations.CreateReservedResult, error)
}

// Usecase 读取模板并复用统一的创作、预扣、账本和 Outbox 事务。
type Usecase struct {
	recipes   TemplateRecipeReader
	creations reservedCreator
	images    OwnedImageReader
}

// NewUsecase 创建视频模板应用用例。
func NewUsecase(recipes TemplateRecipeReader, creator reservedCreator, images OwnedImageReader) *Usecase {
	return &Usecase{recipes: recipes, creations: creator, images: images}
}

// Create 按已选模板冻结技术计划并提交创作预扣事务。
func (usecase *Usecase) Create(ctx context.Context, command CreateCommand) (*creations.CreateReservedResult, error) {
	if usecase == nil || usecase.recipes == nil || usecase.creations == nil || strings.TrimSpace(command.UserID) == "" || strings.TrimSpace(command.TemplateID) == "" || strings.TrimSpace(command.IdempotencyKey) == "" || !validContentAccess(command.ContentAccess) {
		return nil, ErrInvalidTemplateVideoRequest
	}
	if command.Input.UserImageURL != "" {
		if usecase.images == nil {
			return nil, ErrInvalidTemplateVideoRequest
		}
		ownedImageURL, err := usecase.images.ResolveOwnedImage(ctx, command.UserID, command.Input.UserImageURL)
		if err != nil || strings.TrimSpace(ownedImageURL) == "" {
			return nil, ErrInvalidTemplateVideoRequest
		}
		command.Input.UserImageURL = ownedImageURL
	}
	recipe, err := usecase.recipes.LoadTemplateVideo(ctx, command.TemplateID, command.ContentAccess)
	if err != nil {
		return nil, err
	}
	compiled, err := CompileTemplateVideo(recipe, command.Input)
	if err != nil {
		return nil, err
	}
	return usecase.creations.CreateReserved(ctx, creations.CreateReservedRequest{
		UserID:               command.UserID,
		IdempotencyKey:       command.IdempotencyKey,
		TemplateID:           compiled.TemplateID,
		TemplateVersion:      compiled.TemplateVersion,
		Product:              compiled.Product,
		InputDigest:          compiled.InputDigest,
		Plan:                 compiled.Plan,
		InitialSubmission:    compiled.InitialSubmission,
		DeferredImageToVideo: compiled.DeferredImageToVideo,
	})
}

func compileI2VInput(recipe TechnicalRecipe, sourceImageURL string, duration int32) ([]byte, error) {
	parameters, err := parseTechnicalParameters(recipe.Parameters)
	if err != nil || !matchesDuration(parameters, duration) || strings.TrimSpace(recipe.ModelSKU) == "" || strings.TrimSpace(recipe.Prompt) == "" {
		return nil, ErrInvalidTemplateVideoRequest
	}
	if sourceImageURL == "" {
		// 延迟配方在首帧回调前不得出现 assets；绑定时由既有合同唯一注入 opening_frame。
		return json.Marshal(struct {
			Prompt         string         `json:"prompt"`
			NegativePrompt string         `json:"negativePrompt"`
			Parameters     map[string]any `json:"parameters"`
		}{strings.TrimSpace(recipe.Prompt), strings.TrimSpace(recipe.NegativePrompt), parameters})
	}
	return json.Marshal(struct {
		Prompt         string              `json:"prompt"`
		NegativePrompt string              `json:"negativePrompt"`
		Assets         []executionv2.Asset `json:"assets"`
		Parameters     map[string]any      `json:"parameters"`
	}{strings.TrimSpace(recipe.Prompt), strings.TrimSpace(recipe.NegativePrompt), []executionv2.Asset{{Role: "source_image", URL: sourceImageURL, MediaType: "image"}}, parameters})
}

func compileT2IInput(recipe TechnicalRecipe, userPrompt string) ([]byte, error) {
	parameters, err := parseTechnicalParameters(recipe.Parameters)
	if err != nil || strings.TrimSpace(recipe.ModelSKU) == "" || strings.TrimSpace(recipe.Prompt) == "" || strings.TrimSpace(userPrompt) == "" {
		return nil, ErrInvalidTemplateVideoRequest
	}
	prompt := strings.TrimSpace(recipe.Prompt) + "，" + strings.TrimSpace(userPrompt)
	return json.Marshal(struct {
		Prompt         string              `json:"prompt"`
		NegativePrompt string              `json:"negativePrompt"`
		Assets         []executionv2.Asset `json:"assets"`
		Parameters     map[string]any      `json:"parameters"`
	}{prompt, strings.TrimSpace(recipe.NegativePrompt), []executionv2.Asset{}, parameters})
}

func parseTechnicalParameters(raw json.RawMessage) (map[string]any, error) {
	var parameters map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &parameters) != nil || parameters == nil {
		return nil, ErrInvalidTemplateVideoRequest
	}
	return parameters, nil
}

func matchesDuration(parameters map[string]any, duration int32) bool {
	value, exists := parameters["durationSeconds"]
	if !exists {
		return false
	}
	parsed, ok := value.(float64)
	return ok && parsed == float64(duration)
}

func validVideoDuration(duration int32) bool {
	return duration == 5 || duration == 10 || duration == 15
}

func validContentAccess(contentAccess string) bool {
	return contentAccess == identity.ContentAccessStandard || contentAccess == identity.ContentAccessReviewRestricted
}

func digestTechnicalInputs(inputs ...[]byte) string {
	hash := sha256.New()
	for _, input := range inputs {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(input)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
