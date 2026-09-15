// Package executionv2 定义创作预扣与生成投递共同使用的执行快照合同。
package executionv2

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
)

const (
	contractVersion  = "execution.v2"
	maxPayloadBytes  = 1 << 20
	maxRawInputBytes = maxPayloadBytes - 256
	maxJSONDepth     = 32
	maxJSONNodes     = 10000
)

var (
	// ErrInvalidSnapshot 表示执行快照的能力、SKU、输入或素材不满足合同。
	ErrInvalidSnapshot = errors.New("execution.v2: invalid snapshot")
	// ErrInvalidParameters 表示技术参数超出执行合同允许的范围。
	ErrInvalidParameters = errors.New("execution.v2: invalid parameters")
)

// Capability 是生成中台支持的最小技术能力。
type Capability string

const (
	CapabilityTextToImage  Capability = "text_to_image"
	CapabilityImageEdit    Capability = "image_edit"
	CapabilityImageToVideo Capability = "image_to_video"
)

var allowedModelSKUs = map[Capability]map[string]struct{}{
	CapabilityTextToImage: {"ps-image-v1": {}, "ps-anime-v1": {}},
	CapabilityImageEdit: {
		"ps-edit-v1": {}, "ps-edit-body-v1": {}, "ps-edit-pose-v1": {}, "ps-edit-identity-v1": {}, "ps-edit-apparel-v1": {}, "ps-edit-compose-v1": {}, "ps-edit-perspective-v1": {}, "ps-edit-reshape-v1": {}, "ps-edit-reshape-detail-v1": {}, "ps-upscale-image-v1": {},
	},
	CapabilityImageToVideo: {"ps-auto": {}, "ps-anchor-v1": {}, "ps-rush-v1": {}, "ps-apex-v1": {}, "ps-reference-v1": {}},
}

var allowedAssetRoles = map[string]struct{}{
	"source_image": {}, "opening_frame": {}, "face_image": {}, "garment_image": {}, "reference_image": {}, "guide_image": {},
}

var allowedTechnicalScalarParameters = map[string]struct{}{
	"width": {}, "height": {}, "seed": {}, "steps": {}, "cfgScale": {}, "guidanceScale": {}, "strength": {}, "denoise": {}, "fps": {}, "durationSeconds": {}, "frameCount": {}, "aspectRatio": {}, "sampler": {}, "scheduler": {}, "format": {}, "outputFormat": {}, "quality": {}, "clipSkip": {}, "motionStrength": {}, "cameraMotion": {}, "style": {}, "tileSize": {}, "overlapSize": {},
}

var allowedTechnicalStructureParameters = map[string]struct{}{
	"render": {}, "sampling": {}, "generation": {}, "image": {}, "video": {}, "output": {}, "control": {}, "controls": {}, "adapter": {}, "adapters": {},
}

// Asset 是输入中经过合同校验的单个素材引用。
type Asset struct {
	Role      string `json:"role"`
	URL       string `json:"url"`
	MediaType string `json:"mediaType,omitempty"`
}

// Input 是原始输入解析后的技术形状。
type Input struct {
	Assets         []Asset        `json:"assets"`
	Prompt         string         `json:"prompt"`
	NegativePrompt string         `json:"negativePrompt"`
	Parameters     map[string]any `json:"parameters"`
}

// Snapshot 冻结已经校验的执行输入，供创作预留和生成投递复用。
type Snapshot struct {
	Capability Capability
	ModelSKU   string
	Input      Input
	Digest     string
	rawInput   []byte
}

// Compile 验证并冻结一份执行输入，所有失败都返回固定哨兵错误。
func Compile(capability Capability, modelSKU string, rawInput []byte) (*Snapshot, error) {
	if len(rawInput) == 0 || len(rawInput) > maxRawInputBytes || !isAllowedModelSKU(capability, modelSKU) {
		return nil, ErrInvalidSnapshot
	}
	if err := scanJSON(rawInput); err != nil {
		return nil, ErrInvalidSnapshot
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rawInput, &raw); err != nil || raw == nil || !hasOnlyKeys(raw, "prompt", "negativePrompt", "assets", "parameters") {
		return nil, ErrInvalidSnapshot
	}
	input, err := parseInput(raw)
	if err != nil {
		return nil, err
	}
	if err := validateAssets(capability, modelSKU, input.Assets); err != nil {
		return nil, err
	}
	if err := validateParameters(input.Parameters); err != nil {
		return nil, ErrInvalidParameters
	}
	frozen := append([]byte(nil), rawInput...)
	return &Snapshot{
		Capability: capability,
		ModelSKU:   modelSKU,
		Input:      cloneInput(input),
		Digest:     snapshotDigest(capability, modelSKU, frozen),
		rawInput:   frozen,
	}, nil
}

// ValidateJSON 以与执行快照一致的资源上限检查完整 JSON 和重复键。
func ValidateJSON(raw []byte) error {
	if !IsJSONSizeWithinLimit(raw) {
		return ErrInvalidSnapshot
	}
	return scanJSON(raw)
}

// IsJSONSizeWithinLimit 以常数时间判断原始 JSON 是否处于共享合同资源上限内。
func IsJSONSizeWithinLimit(raw []byte) bool {
	return len(raw) > 0 && len(raw) <= maxPayloadBytes
}

// MarshalSubmissionPayload 返回独立的任务发件箱投稿载荷字节。
func (snapshot *Snapshot) MarshalSubmissionPayload() ([]byte, error) {
	if snapshot == nil || len(snapshot.rawInput) == 0 || len(snapshot.rawInput) > maxRawInputBytes || !isAllowedModelSKU(snapshot.Capability, snapshot.ModelSKU) {
		return nil, ErrInvalidSnapshot
	}
	payload := make([]byte, 0, len(snapshot.rawInput)+len(snapshot.ModelSKU)+len(snapshot.Capability)+48)
	payload = append(payload, `{"capability":`...)
	encodedCapability, err := json.Marshal(snapshot.Capability)
	if err != nil {
		return nil, ErrInvalidSnapshot
	}
	payload = append(payload, encodedCapability...)
	payload = append(payload, `,"model_sku":`...)
	encodedSKU, err := json.Marshal(snapshot.ModelSKU)
	if err != nil {
		return nil, ErrInvalidSnapshot
	}
	payload = append(payload, encodedSKU...)
	payload = append(payload, `,"input":`...)
	payload = append(payload, snapshot.rawInput...)
	payload = append(payload, '}')
	if len(payload) > maxPayloadBytes {
		return nil, ErrInvalidSnapshot
	}
	return append([]byte(nil), payload...), nil
}

// ParseSubmissionPayload 将 Outbox 中的冻结技术载荷还原为同一份执行快照。
// 该入口只接受 Snapshot 自己输出的 capability/model_sku/input 三个技术字段，
// 防止 worker 自行猜测能力类型或接受账户、权益等业务字段。
func ParseSubmissionPayload(payload []byte) (*Snapshot, error) {
	if err := ValidateJSON(payload); err != nil {
		return nil, ErrInvalidSnapshot
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil || raw == nil || !hasOnlyKeys(raw, "capability", "model_sku", "input") {
		return nil, ErrInvalidSnapshot
	}
	var capability Capability
	var modelSKU string
	if !isJSONString(raw["capability"]) || json.Unmarshal(raw["capability"], &capability) != nil || !isJSONString(raw["model_sku"]) || json.Unmarshal(raw["model_sku"], &modelSKU) != nil {
		return nil, ErrInvalidSnapshot
	}
	if _, found := raw["input"]; !found {
		return nil, ErrInvalidSnapshot
	}
	return Compile(capability, modelSKU, raw["input"])
}

func parseInput(raw map[string]json.RawMessage) (Input, error) {
	promptRaw, ok := raw["prompt"]
	if !ok {
		return Input{}, ErrInvalidSnapshot
	}
	var prompt string
	if bytes.Equal(bytes.TrimSpace(promptRaw), []byte("null")) || json.Unmarshal(promptRaw, &prompt) != nil || strings.TrimSpace(prompt) == "" {
		return Input{}, ErrInvalidSnapshot
	}
	input := Input{Prompt: strings.TrimSpace(prompt), Parameters: map[string]any{}}
	if negative, ok := raw["negativePrompt"]; ok {
		if bytes.Equal(bytes.TrimSpace(negative), []byte("null")) || json.Unmarshal(negative, &input.NegativePrompt) != nil {
			return Input{}, ErrInvalidSnapshot
		}
		input.NegativePrompt = strings.TrimSpace(input.NegativePrompt)
	}
	if assets, ok := raw["assets"]; ok {
		var err error
		input.Assets, err = parseAssets(assets)
		if err != nil {
			return Input{}, ErrInvalidSnapshot
		}
	} else {
		input.Assets = []Asset{}
	}
	if parameters, ok := raw["parameters"]; ok {
		if bytes.Equal(bytes.TrimSpace(parameters), []byte("null")) || json.Unmarshal(parameters, &input.Parameters) != nil || input.Parameters == nil {
			return Input{}, ErrInvalidParameters
		}
	}
	return input, nil
}

func parseAssets(raw json.RawMessage) ([]Asset, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, ErrInvalidSnapshot
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, ErrInvalidSnapshot
	}
	assets := make([]Asset, 0, len(values))
	for _, value := range values {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(value, &fields); err != nil || fields == nil || !hasOnlyKeys(fields, "role", "url", "mediaType") {
			return nil, ErrInvalidSnapshot
		}
		role, roleOK := fields["role"]
		assetURL, urlOK := fields["url"]
		if !roleOK || !urlOK || !isJSONString(role) || !isJSONString(assetURL) {
			return nil, ErrInvalidSnapshot
		}
		asset := Asset{}
		_ = json.Unmarshal(role, &asset.Role)
		_ = json.Unmarshal(assetURL, &asset.URL)
		if mediaType, ok := fields["mediaType"]; ok {
			if !isJSONString(mediaType) {
				return nil, ErrInvalidSnapshot
			}
			_ = json.Unmarshal(mediaType, &asset.MediaType)
		}
		assets = append(assets, asset)
	}
	return assets, nil
}

func isJSONString(raw json.RawMessage) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func validateAssets(capability Capability, modelSKU string, assets []Asset) error {
	if capability == CapabilityTextToImage {
		if len(assets) != 0 {
			return ErrInvalidSnapshot
		}
		return nil
	}
	if len(assets) == 0 {
		return ErrInvalidSnapshot
	}
	primary := 0
	for _, asset := range assets {
		if _, ok := allowedAssetRoles[asset.Role]; !ok || !isHTTPSAssetURL(asset.URL) {
			return ErrInvalidSnapshot
		}
		if asset.Role == "reference_image" && modelSKU != "ps-reference-v1" {
			return ErrInvalidSnapshot
		}
		if capability == CapabilityImageToVideo {
			switch asset.Role {
			case "opening_frame", "source_image":
				primary++
			case "reference_image":
			default:
				return ErrInvalidSnapshot
			}
		}
	}
	if capability == CapabilityImageToVideo && primary != 1 {
		return ErrInvalidSnapshot
	}
	return nil
}

func isHTTPSAssetURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	return err == nil && trimmed == raw && !strings.Contains(raw, "#") && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func validateParameters(parameters map[string]any) error {
	return validateParameterObject(parameters)
}

func validateParameterObject(parameters map[string]any) error {
	for key, value := range parameters {
		if _, ok := allowedTechnicalScalarParameters[key]; ok {
			switch value.(type) {
			case string, bool, float64:
				continue
			default:
				return ErrInvalidParameters
			}
		}
		if _, ok := allowedTechnicalStructureParameters[key]; ok && validateParameterStructure(value) == nil {
			continue
		}
		return ErrInvalidParameters
	}
	return nil
}

func validateParameterStructure(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		return validateParameterObject(typed)
	case []any:
		for _, item := range typed {
			object, ok := item.(map[string]any)
			if !ok || validateParameterObject(object) != nil {
				return ErrInvalidParameters
			}
		}
		return nil
	default:
		return ErrInvalidParameters
	}
}

func scanJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	type container struct {
		object bool
		keys   map[string]struct{}
	}
	stack := make([]container, 0, maxJSONDepth)
	nodes := 0
	consumeValue := func() error {
		token, err := decoder.Token()
		if err != nil {
			return ErrInvalidSnapshot
		}
		nodes++
		if nodes > maxJSONNodes {
			return ErrInvalidSnapshot
		}
		switch delimiter := token.(type) {
		case json.Delim:
			switch delimiter {
			case '{':
				if len(stack) >= maxJSONDepth {
					return ErrInvalidSnapshot
				}
				stack = append(stack, container{object: true, keys: map[string]struct{}{}})
			case '[':
				if len(stack) >= maxJSONDepth {
					return ErrInvalidSnapshot
				}
				stack = append(stack, container{})
			default:
				return ErrInvalidSnapshot
			}
		}
		return nil
	}
	if err := consumeValue(); err != nil {
		return err
	}
	for len(stack) > 0 {
		frame := &stack[len(stack)-1]
		if !decoder.More() {
			token, err := decoder.Token()
			if err != nil || (frame.object && token != json.Delim('}')) || (!frame.object && token != json.Delim(']')) {
				return ErrInvalidSnapshot
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if frame.object {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok {
				return ErrInvalidSnapshot
			}
			nodes++
			if nodes > maxJSONNodes {
				return ErrInvalidSnapshot
			}
			if _, exists := frame.keys[key]; exists {
				return ErrInvalidSnapshot
			}
			frame.keys[key] = struct{}{}
		}
		if err := consumeValue(); err != nil {
			return err
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrInvalidSnapshot
	}
	return nil
}

func hasOnlyKeys(values map[string]json.RawMessage, allowed ...string) bool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range values {
		if _, ok := allowedSet[key]; !ok {
			return false
		}
	}
	return true
}

func cloneInput(input Input) Input {
	cloned := Input{Prompt: input.Prompt, NegativePrompt: input.NegativePrompt, Assets: make([]Asset, len(input.Assets))}
	copy(cloned.Assets, input.Assets)
	raw, _ := json.Marshal(input.Parameters)
	_ = json.Unmarshal(raw, &cloned.Parameters)
	return cloned
}

func snapshotDigest(capability Capability, modelSKU string, input []byte) string {
	encoded := make([]byte, 0, len(contractVersion)+len(capability)+len(modelSKU)+len(input)+64)
	for _, field := range [][]byte{[]byte(contractVersion), []byte(capability), []byte(modelSKU), input} {
		encoded = strconv.AppendInt(encoded, int64(len(field)), 10)
		encoded = append(encoded, ':')
		encoded = append(encoded, field...)
		encoded = append(encoded, ';')
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func isAllowedModelSKU(capability Capability, modelSKU string) bool {
	models, ok := allowedModelSKUs[capability]
	if !ok {
		return false
	}
	_, ok = models[modelSKU]
	return ok
}

// IsSupportedModelSKU 供回调等适配层复用执行快照的精确能力与模型 SKU 对应关系。
func IsSupportedModelSKU(capability Capability, modelSKU string) bool {
	return isAllowedModelSKU(capability, modelSKU)
}
