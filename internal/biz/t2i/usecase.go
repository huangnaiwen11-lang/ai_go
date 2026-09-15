package t2i

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

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
	creations reservedCreator
}

// NewUsecase 创建自由文生图应用用例。
func NewUsecase(recipes FreeformRecipeReader, creations reservedCreator) *Usecase {
	return &Usecase{recipes: recipes, creations: creations}
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
	compiled, err := CompileFreeform(recipe, CreateInput{
		Prompt:         command.Prompt,
		NegativePrompt: command.NegativePrompt,
		AspectRatio:    command.AspectRatio,
	})
	if err != nil {
		return nil, err
	}

	// 只持久化冻结输入的摘要，创作记录和日志均不保存原始提示词。
	inputDigest := sha256.Sum256(compiled.InitialSubmission.Input)
	return usecase.creations.CreateReserved(ctx, creations.CreateReservedRequest{
		UserID:          command.UserID,
		IdempotencyKey:  command.IdempotencyKey,
		TemplateID:      compiled.TemplateID,
		TemplateVersion: compiled.TemplateVersion,
		Product: entitlement.GenerationRequest{
			Output: entitlement.ProductOutputImage,
		},
		InputDigest:       hex.EncodeToString(inputDigest[:]),
		Plan:              compiled.Plan,
		InitialSubmission: compiled.InitialSubmission,
	})
}
