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
)

// reservedCreator 是 T2I 应用层调用既有创作预留能力所需的最小边界。
// 预扣、日免并发控制和 Outbox 写入始终由 creations 用例在同一事务中处理。
type reservedCreator interface {
	CreateReserved(context.Context, creations.CreateReservedRequest) (*creations.CreateReservedResult, error)
}

// Usecase 协调自由文生图的模板读取、技术配方冻结和创作预留。
// 它不直接查询余额、不写 MongoDB，也不调用生成中台。
type Usecase struct {
	recipes   FreeformRecipeReader
	products  creations.B2BProductRecipeReader
	creations reservedCreator
}

// NewUsecase 创建自由文生图应用用例。
func NewUsecase(recipes FreeformRecipeReader, creations reservedCreator) *Usecase {
	return &Usecase{recipes: recipes, creations: creations}
}

// NewUsecaseWithB2BProductRecipes wires the explicit B2B compilation path.
// The composition root selects this constructor only for a B2B new-task
// provider; a nil reader is intentionally not treated as a local fallback.
func NewUsecaseWithB2BProductRecipes(recipes FreeformRecipeReader, products creations.B2BProductRecipeReader, creator reservedCreator) *Usecase {
	return &Usecase{recipes: recipes, products: products, creations: creator}
}

// CreateCommand 是通过 Gateway 认证后的公开文生图创建命令。
// 幂等键由 Gateway 从请求 ID 构造，客户端不可以在请求体中定义它。
type CreateCommand struct {
	UserID         string
	IdempotencyKey string
	Prompt         string
	NegativePrompt string
	AspectRatio    string
}

// Create 冻结当前唯一启用模板配方，并复用创作预留事务创建占位、账本预留与 Outbox。
func (usecase *Usecase) Create(ctx context.Context, command CreateCommand) (*creations.CreateReservedResult, error) {
	if usecase == nil || usecase.recipes == nil || usecase.creations == nil {
		return nil, errors.New("t2i usecase is not configured")
	}
	recipe, err := usecase.recipes.LoadFreeformRecipe(ctx)
	if err != nil {
		return nil, err
	}
	input := CreateInput{
		Prompt:         command.Prompt,
		NegativePrompt: command.NegativePrompt,
		AspectRatio:    command.AspectRatio,
	}
	request := creations.CreateReservedRequest{
		UserID:          command.UserID,
		IdempotencyKey:  command.IdempotencyKey,
		TemplateID:      recipe.TemplateID,
		TemplateVersion: recipe.Version,
		Product: entitlement.GenerationRequest{
			Output: entitlement.ProductOutputImage,
		},
		Plan: []creations.StepPlan{{Sequence: 1, Atom: creations.AtomTextToImage}},
	}
	if usecase.products != nil {
		product, err := usecase.products.LoadB2BProductRecipe(ctx, recipe.TemplateID, recipe.Version, creations.AtomTextToImage)
		if err != nil {
			return nil, err
		}
		product, err = product.Normalize()
		if err != nil || product.TemplateID != recipe.TemplateID || product.TemplateVersion != recipe.Version || product.Atom != creations.AtomTextToImage {
			return nil, creations.ErrB2BProductRecipeUnavailable
		}
		compiled, err := product.CompileB2BProductRecipe(freeformB2BOverrides(input), nil)
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
	compiled, err := CompileFreeform(recipe, input)
	if err != nil {
		return nil, err
	}
	// 只持久化冻结输入的摘要，创作记录和日志均不保存原始提示词。
	inputDigest := sha256.Sum256(compiled.InitialSubmission.Input)
	request.InputDigest = hex.EncodeToString(inputDigest[:])
	request.InitialSubmission = compiled.InitialSubmission
	return usecase.creations.CreateReserved(ctx, request)
}

func freeformB2BOverrides(input CreateInput) map[string]json.RawMessage {
	fields := map[string]json.RawMessage{}
	// prompt 与 negativePrompt 的空值语义沿用旧 freeform 编译器：它们是本次
	// 用户输入的一部分，不能因为发布默认值存在就被静默替换。
	for key, value := range map[string]string{
		"prompt":         strings.TrimSpace(input.Prompt),
		"negativePrompt": strings.TrimSpace(input.NegativePrompt),
	} {
		encoded, _ := json.Marshal(value)
		fields[key] = encoded
	}
	if aspectRatio := strings.TrimSpace(input.AspectRatio); aspectRatio != "" {
		encoded, _ := json.Marshal(aspectRatio)
		fields["aspectRatio"] = encoded
	}
	return fields
}
