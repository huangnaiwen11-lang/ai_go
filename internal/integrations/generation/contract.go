package generation

import (
	"encoding/json"
	"errors"
	"regexp"

	"ai-business-service/internal/executionv2"
)

const (
	contractVersion   = "execution.v2"
	priorityClass     = "standard"
	deliveryCallback  = "tenant"
	resultURLPolicy   = "permanent"
	metadataSite      = "cling-go-main"
	idempotencyPrefix = "cling-step:"
)

var modelSKUAtom = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)

var ErrInvalidExecution = errors.New("invalid generation execution")

var ErrInvalidParameters = errors.New("invalid generation parameters")

type Capability = executionv2.Capability

const (
	CapabilityTextToImage  = executionv2.CapabilityTextToImage
	CapabilityImageEdit    = executionv2.CapabilityImageEdit
	CapabilityImageToVideo = executionv2.CapabilityImageToVideo
)

type Input = executionv2.Input

type Asset = executionv2.Asset

type Step struct {
	ID         string
	Capability Capability
	ModelSKU   string
	Input      Input
}

type Delivery struct {
	Callback        string `json:"callback"`
	ResultURLPolicy string `json:"resultUrlPolicy"`
}

type Metadata struct {
	Site string `json:"site"`
}

type Execution struct {
	ContractVersion string     `json:"contractVersion"`
	IdempotencyKey  string     `json:"idempotencyKey"`
	ExternalRef     string     `json:"externalRef"`
	Capability      Capability `json:"capability"`
	ModelSKU        string     `json:"modelSku"`
	Input           Input      `json:"input"`
	Delivery        Delivery   `json:"delivery"`
	PriorityClass   string     `json:"priorityClass"`
	Metadata        Metadata   `json:"metadata"`
}

func BuildExecution(step Step) (Execution, error) {
	if !isSafeAtom(step.ID) {
		return Execution{}, ErrInvalidExecution
	}
	rawInput, err := marshalStepInput(step.Input)
	if err != nil {
		return Execution{}, ErrInvalidParameters
	}
	snapshot, err := executionv2.Compile(step.Capability, step.ModelSKU, rawInput)
	if err != nil {
		if errors.Is(err, executionv2.ErrInvalidParameters) {
			return Execution{}, ErrInvalidParameters
		}
		return Execution{}, ErrInvalidExecution
	}
	return Execution{
		ContractVersion: contractVersion,
		IdempotencyKey:  idempotencyPrefix + step.ID,
		ExternalRef:     step.ID,
		Capability:      snapshot.Capability,
		ModelSKU:        snapshot.ModelSKU,
		Input:           snapshot.Input,
		Delivery: Delivery{
			Callback:        deliveryCallback,
			ResultURLPolicy: resultURLPolicy,
		},
		PriorityClass: priorityClass,
		Metadata: Metadata{
			Site: metadataSite,
		},
	}, nil
}

// ExecutionFromSubmissionPayload 将创建侧冻结的 executionv2 快照适配为中台提交合同。
// Outbox 仅保存能力、模型和输入；幂等键、外部引用和受控回调等投递字段只能由稳定步骤 ID 派生。
func ExecutionFromSubmissionPayload(stepID string, payload []byte) (Execution, error) {
	snapshot, err := executionv2.ParseSubmissionPayload(payload)
	if err != nil {
		if errors.Is(err, executionv2.ErrInvalidParameters) {
			return Execution{}, ErrInvalidParameters
		}
		return Execution{}, ErrInvalidExecution
	}
	return BuildExecution(Step{
		ID:         stepID,
		Capability: snapshot.Capability,
		ModelSKU:   snapshot.ModelSKU,
		Input:      snapshot.Input,
	})
}

func marshalStepInput(input Input) ([]byte, error) {
	if input.Assets == nil {
		input.Assets = []Asset{}
	}
	if input.Parameters == nil {
		input.Parameters = map[string]any{}
	}
	return json.Marshal(input)
}

func isSafeAtom(value string) bool {
	return modelSKUAtom.MatchString(value)
}

func validateRawExecution(raw []byte) error {
	return validateRawExecutionForCallback(raw, deliveryCallback)
}

func validateRawExecutionForCallback(raw []byte, expectedCallback string) error {
	if err := executionv2.ValidateJSON(raw); err != nil {
		return ErrInvalidExecution
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return ErrInvalidExecution
	}
	if err := validateRawObjectKeys(body, []string{"contractVersion", "idempotencyKey", "externalRef", "capability", "modelSku", "input", "delivery", "priorityClass", "metadata"}); err != nil {
		return err
	}
	var capability Capability
	var modelSKU string
	if err := json.Unmarshal(body["capability"], &capability); err != nil {
		return ErrInvalidExecution
	}
	if err := json.Unmarshal(body["modelSku"], &modelSKU); err != nil {
		return ErrInvalidExecution
	}
	if _, err := executionv2.Compile(capability, modelSKU, body["input"]); err != nil {
		if errors.Is(err, executionv2.ErrInvalidParameters) {
			return ErrInvalidParameters
		}
		return ErrInvalidExecution
	}
	if err := validateRawNestedObject(body["delivery"], []string{"callback", "resultUrlPolicy"}); err != nil {
		return err
	}
	if err := validateRawNestedObject(body["metadata"], []string{"site"}); err != nil {
		return err
	}
	var delivery Delivery
	if err := json.Unmarshal(body["delivery"], &delivery); err != nil || delivery.Callback != expectedCallback || delivery.ResultURLPolicy != resultURLPolicy {
		return ErrInvalidExecution
	}
	return nil
}

func validateRawNestedObject(raw json.RawMessage, required []string) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return ErrInvalidExecution
	}
	return validateRawObjectKeys(value, required)
}

func validateRawObjectKeys(object map[string]json.RawMessage, allowed []string) error {
	if len(object) != len(allowed) {
		return ErrInvalidExecution
	}
	for _, key := range allowed {
		if _, found := object[key]; !found {
			return ErrInvalidExecution
		}
	}
	return nil
}
