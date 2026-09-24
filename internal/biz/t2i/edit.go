package t2i

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/executionv2"
)

// ErrInvalidImageEditRequest 表示模板配方或用户图片不满足模板图编辑的固定合同。
var ErrInvalidImageEditRequest = errors.New("t2i: invalid image edit request")

// ImageEditAsset 是模板配方中服务端控制的参考素材，客户端不能指定其角色或地址。
type ImageEditAsset struct {
	Role string
	URL  string
}

// ImageEditRecipe 是唯一启用图片模板版本的服务端编辑配方。
type ImageEditRecipe struct {
	TemplateID      string
	Version         int64
	ModelSKU        string
	Prompt          string
	NegativePrompt  string
	Parameters      json.RawMessage
	ReferenceAssets []ImageEditAsset
}

// ImageEditRecipeReader 按已验证用户的内容访问级别读取唯一可见模板配方。
type ImageEditRecipeReader interface {
	LoadImageEditRecipe(context.Context, string, string) (ImageEditRecipe, error)
}

// OwnedImageReader 只解析当前用户已确认、可用于生成的自有图片素材。
// 它是 I2I 与素材模块之间的领域边界：模板编辑不接触 Mongo 或文件存储实现。
type OwnedImageReader interface {
	ResolveOwnedImage(context.Context, string, string) (string, error)
}

// ImageEditInput 是客户端可填写的模板产品输入，不包含任何技术投递字段。
type ImageEditInput struct {
	UserImageURL string
	UserPrompt   string
	// AspectRatio 是用户选择的产品画幅；仅能覆盖服务端参数中的画幅，不接受模型或工作流字段。
	AspectRatio string
}

// CompiledImageEdit 是已冻结、可交给创作预留用例的单步图编辑计划。
type CompiledImageEdit struct {
	TemplateID        string
	TemplateVersion   int64
	Plan              []creations.StepPlan
	InitialSubmission *creations.InitialSubmission
}

// CompileImageEdit 将模板配方和唯一用户图片编译为 image_edit 技术快照。
func CompileImageEdit(recipe ImageEditRecipe, input ImageEditInput) (*CompiledImageEdit, error) {
	if strings.TrimSpace(recipe.TemplateID) == "" || recipe.Version <= 0 || strings.TrimSpace(recipe.Prompt) == "" || len(recipe.ReferenceAssets) > 1 {
		return nil, ErrInvalidImageEditRequest
	}
	parameters, err := compileParameters(recipe.Parameters, input.AspectRatio)
	if err != nil {
		return nil, ErrInvalidImageEditRequest
	}
	assets := []ImageEditAsset{{Role: "source_image", URL: strings.TrimSpace(input.UserImageURL)}}
	assets = append(assets, recipe.ReferenceAssets...)
	rawAssets := make([]executionv2.Asset, 0, len(assets))
	for _, asset := range assets {
		rawAssets = append(rawAssets, executionv2.Asset{Role: asset.Role, URL: asset.URL, MediaType: "image"})
	}
	prompt := strings.TrimSpace(recipe.Prompt)
	if extra := strings.TrimSpace(input.UserPrompt); extra != "" {
		prompt += "，" + extra
	}
	rawInput, err := json.Marshal(struct {
		Prompt         string              `json:"prompt"`
		NegativePrompt string              `json:"negativePrompt"`
		Assets         []executionv2.Asset `json:"assets"`
		Parameters     map[string]any      `json:"parameters"`
	}{prompt, strings.TrimSpace(recipe.NegativePrompt), rawAssets, parameters})
	if err != nil {
		return nil, ErrInvalidImageEditRequest
	}
	if _, err := executionv2.Compile(executionv2.CapabilityImageEdit, recipe.ModelSKU, rawInput); err != nil {
		return nil, ErrInvalidImageEditRequest
	}
	return &CompiledImageEdit{TemplateID: recipe.TemplateID, TemplateVersion: recipe.Version, Plan: []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageEdit}}, InitialSubmission: &creations.InitialSubmission{ModelSKU: recipe.ModelSKU, Input: rawInput}}, nil
}

// ImageEditCommand 是已认证 HTTP 请求转换后的模板图编辑创建命令。
type ImageEditCommand struct {
	UserID         string
	ContentAccess  string
	IdempotencyKey string
	TemplateID     string
	UserImageURL   string
	UserPrompt     string
	AspectRatio    string
}

// ImageEditUsecase 读取模板并复用已有的预扣、账本和 Outbox 事务创建图片编辑任务。
type ImageEditUsecase struct {
	recipes   ImageEditRecipeReader
	products  creations.B2BProductRecipeReader
	creations reservedCreator
	images    OwnedImageReader
}

func NewImageEditUsecase(recipes ImageEditRecipeReader, creator reservedCreator, images OwnedImageReader) *ImageEditUsecase {
	return &ImageEditUsecase{recipes: recipes, creations: creator, images: images}
}

// NewImageEditUsecaseWithB2BProductRecipes selects published public-product
// compilation. The legacy recipe reader remains only the authority for visible
// template identity/version and content access; its execution.v2 fields are
// never copied into a B2B submission.
func NewImageEditUsecaseWithB2BProductRecipes(recipes ImageEditRecipeReader, products creations.B2BProductRecipeReader, creator reservedCreator, images OwnedImageReader) *ImageEditUsecase {
	return &ImageEditUsecase{recipes: recipes, products: products, creations: creator, images: images}
}

func (usecase *ImageEditUsecase) Create(ctx context.Context, command ImageEditCommand) (*creations.CreateReservedResult, error) {
	if usecase == nil || usecase.recipes == nil || usecase.creations == nil || usecase.images == nil || strings.TrimSpace(command.UserID) == "" || strings.TrimSpace(command.TemplateID) == "" || strings.TrimSpace(command.UserImageURL) == "" {
		return nil, ErrInvalidImageEditRequest
	}
	ownedImageURL, err := usecase.images.ResolveOwnedImage(ctx, command.UserID, command.UserImageURL)
	if err != nil {
		return nil, ErrInvalidImageEditRequest
	}
	recipe, err := usecase.recipes.LoadImageEditRecipe(ctx, command.TemplateID, command.ContentAccess)
	if err != nil {
		return nil, err
	}
	request := creations.CreateReservedRequest{
		UserID:          command.UserID,
		IdempotencyKey:  command.IdempotencyKey,
		TemplateID:      recipe.TemplateID,
		TemplateVersion: recipe.Version,
		Product:         entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
		Plan:            []creations.StepPlan{{Sequence: 1, Atom: creations.AtomImageEdit}},
	}
	if usecase.products != nil {
		product, err := usecase.products.LoadB2BProductRecipe(ctx, recipe.TemplateID, recipe.Version, creations.AtomImageEdit)
		if err != nil {
			return nil, err
		}
		product, err = product.Normalize()
		if err != nil || product.TemplateID != recipe.TemplateID || product.TemplateVersion != recipe.Version || product.Atom != creations.AtomImageEdit {
			return nil, creations.ErrB2BProductRecipeUnavailable
		}
		compiled, err := product.CompileB2BProductRecipe(imageEditB2BOverrides(command.UserPrompt, command.AspectRatio), []creations.B2BAsset{{Role: "source_image", URL: ownedImageURL}})
		if err != nil {
			return nil, err
		}
		digest, err := compiled.Digest()
		if err != nil {
			return nil, creations.ErrB2BProductRecipeUnavailable
		}
		request.InputDigest = digest
		request.B2BSubmission = &compiled
		return usecase.creations.CreateReserved(ctx, request)
	}
	compiled, err := CompileImageEdit(recipe, ImageEditInput{UserImageURL: ownedImageURL, UserPrompt: command.UserPrompt, AspectRatio: command.AspectRatio})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(compiled.InitialSubmission.Input)
	request.InputDigest = hex.EncodeToString(digest[:])
	request.InitialSubmission = compiled.InitialSubmission
	return usecase.creations.CreateReserved(ctx, request)
}

func imageEditB2BOverrides(userPrompt, aspectRatio string) map[string]json.RawMessage {
	overrides := make(map[string]json.RawMessage, 2)
	if prompt := strings.TrimSpace(userPrompt); prompt != "" {
		encoded, _ := json.Marshal(prompt)
		overrides["prompt"] = encoded
	}
	if ratio := strings.TrimSpace(aspectRatio); ratio != "" {
		encoded, _ := json.Marshal(ratio)
		overrides["aspectRatio"] = encoded
	}
	return overrides
}
