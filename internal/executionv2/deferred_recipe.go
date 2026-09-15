package executionv2

import (
	"encoding/json"
	"sync"
)

// DeferredImageToVideoRecipe 是创建时冻结、等待首帧成功后才可完整编译的 I2V 技术配方。
type DeferredImageToVideoRecipe struct {
	Digest string

	mu            sync.Mutex
	modelSKU      string
	inputTemplate []byte
	bound         bool
}

// CompileDeferredImageToVideo 校验并冻结不带首帧的 I2V 技术输入模板。
func CompileDeferredImageToVideo(modelSKU string, inputTemplate []byte) (*DeferredImageToVideoRecipe, error) {
	if len(inputTemplate) == 0 || len(inputTemplate) > maxRawInputBytes || !isAllowedModelSKU(CapabilityImageToVideo, modelSKU) {
		return nil, ErrInvalidSnapshot
	}
	if err := ValidateJSON(inputTemplate); err != nil {
		return nil, ErrInvalidSnapshot
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(inputTemplate, &raw); err != nil || raw == nil || !hasOnlyKeys(raw, "prompt", "negativePrompt", "parameters") {
		return nil, ErrInvalidSnapshot
	}
	if _, found := raw["parameters"]; !found {
		return nil, ErrInvalidSnapshot
	}
	input, err := parseInput(raw)
	if err != nil {
		return nil, err
	}
	if len(input.Assets) != 0 {
		return nil, ErrInvalidSnapshot
	}
	if err := validateParameters(input.Parameters); err != nil {
		return nil, ErrInvalidParameters
	}
	frozen := append([]byte(nil), inputTemplate...)
	return &DeferredImageToVideoRecipe{
		modelSKU:      modelSKU,
		Digest:        snapshotDigest(CapabilityImageToVideo, modelSKU, frozen),
		inputTemplate: frozen,
	}, nil
}

// ModelSKU 返回创建时冻结的 I2V 模型 SKU。
func (recipe *DeferredImageToVideoRecipe) ModelSKU() string {
	if recipe == nil {
		return ""
	}
	recipe.mu.Lock()
	defer recipe.mu.Unlock()
	return recipe.modelSKU
}

// FrozenInputTemplate 返回冻结字节的副本，供同一创建事务持久化而不暴露内部切片。
func (recipe *DeferredImageToVideoRecipe) FrozenInputTemplate() []byte {
	if recipe == nil {
		return nil
	}
	recipe.mu.Lock()
	defer recipe.mu.Unlock()
	return append([]byte(nil), recipe.inputTemplate...)
}

// BindOpeningFrame 仅允许将一张安全 HTTPS 首帧绑定一次，并复用完整快照合同再次校验。
func (recipe *DeferredImageToVideoRecipe) BindOpeningFrame(openingFrameURL string) (*Snapshot, error) {
	if recipe == nil || !isHTTPSAssetURL(openingFrameURL) {
		return nil, ErrInvalidSnapshot
	}
	recipe.mu.Lock()
	defer recipe.mu.Unlock()
	if recipe.bound || len(recipe.inputTemplate) == 0 || !isAllowedModelSKU(CapabilityImageToVideo, recipe.modelSKU) {
		return nil, ErrInvalidSnapshot
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recipe.inputTemplate, &raw); err != nil || raw == nil {
		return nil, ErrInvalidSnapshot
	}
	assets, err := json.Marshal([]Asset{{Role: "opening_frame", URL: openingFrameURL}})
	if err != nil {
		return nil, ErrInvalidSnapshot
	}
	raw["assets"] = assets
	boundInput, err := json.Marshal(raw)
	if err != nil {
		return nil, ErrInvalidSnapshot
	}
	snapshot, err := Compile(CapabilityImageToVideo, recipe.modelSKU, boundInput)
	if err != nil {
		return nil, err
	}
	recipe.bound = true
	return snapshot, nil
}
