// Package t2i 提供公开文生图接口所需的模板编译与应用编排。
package t2i

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/executionv2"
)

const freeformTemplateID = "t2i-freeform"

var errInvalidFreeformRecipe = errors.New("t2i: invalid freeform recipe")

// FreeformRecipe 是已从模板目录读取的、服务端控制的自由文生图技术配方。
// 它不来自 HTTP 请求，因此模型 SKU 与技术参数不会被客户端篡改。
type FreeformRecipe struct {
	TemplateID string
	Version    int64
	ModelSKU   string
	Parameters json.RawMessage
}

// FreeformRecipeReader 是自由文生图模板配方的只读边界。
// 模板存储细节仅由 data 层实现，T2I 业务层不直接依赖 MongoDB 或 BSON。
type FreeformRecipeReader interface {
	LoadFreeformRecipe(context.Context) (FreeformRecipe, error)
}

// CreateInput 是公开 T2I 创建请求中允许用户填写的产品字段。
// 模型、回调、LoRA 和工作流等技术投递字段不属于该类型。
type CreateInput struct {
	Prompt         string
	NegativePrompt string
	AspectRatio    string
}

// CompiledFreeform 是可安全交给创作预留用例的服务端编译结果。
// 初始提交快照由模板配方和有限用户输入共同产生，并在创建事务前固定。
type CompiledFreeform struct {
	TemplateID        string
	TemplateVersion   int64
	Plan              []creations.StepPlan
	InitialSubmission *creations.InitialSubmission
}

// CompileFreeform 将启用的自由文生图模板配方编译为一个固定的文生图步骤。
// 这里复用 executionv2 的合同校验，确保技术参数、模型 SKU 和输入形状均可安全投递。
func CompileFreeform(recipe FreeformRecipe, input CreateInput) (*CompiledFreeform, error) {
	if recipe.TemplateID != freeformTemplateID || recipe.Version <= 0 {
		return nil, errInvalidFreeformRecipe
	}

	parameters, err := compileParameters(recipe.Parameters, input.AspectRatio)
	if err != nil {
		return nil, errInvalidFreeformRecipe
	}
	rawInput, err := json.Marshal(struct {
		Prompt         string         `json:"prompt"`
		NegativePrompt string         `json:"negativePrompt"`
		Assets         []any          `json:"assets"`
		Parameters     map[string]any `json:"parameters"`
	}{
		Prompt:         strings.TrimSpace(input.Prompt),
		NegativePrompt: strings.TrimSpace(input.NegativePrompt),
		Assets:         []any{},
		Parameters:     parameters,
	})
	if err != nil {
		return nil, errInvalidFreeformRecipe
	}

	// Compile 同时校验模型 SKU、参数白名单和文生图不允许素材这三个不变量。
	if _, err := executionv2.Compile(executionv2.CapabilityTextToImage, recipe.ModelSKU, rawInput); err != nil {
		return nil, errInvalidFreeformRecipe
	}

	return &CompiledFreeform{
		TemplateID:      recipe.TemplateID,
		TemplateVersion: recipe.Version,
		Plan: []creations.StepPlan{{
			Sequence: 1,
			Atom:     creations.AtomTextToImage,
		}},
		InitialSubmission: &creations.InitialSubmission{
			ModelSKU: recipe.ModelSKU,
			Input:    rawInput,
		},
	}, nil
}

// compileParameters 保持配方参数的 JSON 安全边界，并仅允许用户覆盖画幅。
func compileParameters(raw json.RawMessage, aspectRatio string) (map[string]any, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errInvalidFreeformRecipe
	}
	// 先扫描原始 JSON，避免 json.Unmarshal 对重复键“后值覆盖前值”的宽松行为。
	if err := executionv2.ValidateJSON(raw); err != nil {
		return nil, errInvalidFreeformRecipe
	}
	parameters := make(map[string]any)
	if err := json.Unmarshal(raw, &parameters); err != nil || parameters == nil {
		return nil, errInvalidFreeformRecipe
	}
	if normalized := strings.TrimSpace(aspectRatio); normalized != "" {
		parameters["aspectRatio"] = normalized
	}
	return parameters, nil
}
