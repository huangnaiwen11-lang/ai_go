package generation

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-business-service/internal/executionv2"
)

const requestSigningKey = "request-signing-key-at-least-thirty-two-characters"

func TestBuildExecution固定合同字段且不含禁止字段(t *testing.T) {
	execution, err := BuildExecution(Step{
		ID:         "step-42",
		Capability: CapabilityTextToImage,
		ModelSKU:   "ps-image-v1",
		Input: Input{
			Prompt:         "一只在月光下奔跑的狐狸",
			NegativePrompt: "模糊",
			Parameters: map[string]any{
				"width": 1024,
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildExecution() error = %v", err)
	}

	raw, err := json.Marshal(execution)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	assertExactKeys(t, body, "contractVersion", "idempotencyKey", "externalRef", "capability", "modelSku", "input", "delivery", "priorityClass", "metadata")
	if got := body["contractVersion"]; got != "execution.v2" {
		t.Fatalf("contractVersion = %v, want execution.v2", got)
	}
	if got := body["idempotencyKey"]; got != "cling-step:step-42" {
		t.Fatalf("idempotencyKey = %v, want cling-step:step-42", got)
	}
	if got := body["externalRef"]; got != "step-42" {
		t.Fatalf("externalRef = %v, want step-42", got)
	}
	if got := body["priorityClass"]; got != "standard" {
		t.Fatalf("priorityClass = %v, want standard", got)
	}
	input, ok := body["input"].(map[string]any)
	if !ok {
		t.Fatalf("input = %#v, want object", body["input"])
	}
	assertExactKeys(t, input, "assets", "prompt", "negativePrompt", "parameters")
	delivery, ok := body["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("delivery = %#v, want object", body["delivery"])
	}
	if got := delivery["callback"]; got != "tenant" {
		t.Fatalf("delivery.callback = %v, want tenant", got)
	}
	if got := delivery["resultUrlPolicy"]; got != "permanent" {
		t.Fatalf("delivery.resultUrlPolicy = %v, want permanent", got)
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want object", body["metadata"])
	}
	if got := metadata["site"]; got != "cling-go-main" {
		t.Fatalf("metadata.site = %v, want cling-go-main", got)
	}
	assertNoForbiddenKeys(t, body)
}

func TestSubmit成功HTTP中的非接受状态不得伪装为成功(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		wantErr error
	}{
		{name: "明确拒绝", status: "rejected", wantErr: ErrRejected},
		{name: "未知状态", status: "unrecognized", wantErr: ErrOutcomeUnknown},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusCreated)
				_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-not-accepted","status":"` + testCase.status + `"}}`))
			}))
			defer server.Close()
			client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-status-"+testCase.status)

			result, err := client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage)))
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("Submit() result/error = %#v / %v, want %v", result, err, testCase.wantErr)
			}
		})
	}
}

// TestSubmit与Lookup都携带独立APIKey 冻结生成中台的双重准入合同：
// API Key 用于租户识别，V2 签名用于请求完整性，二者不能相互替代。
func TestSubmit与Lookup都携带独立APIKey(t *testing.T) {
	const expectedAPIKey = "local-generation-api-key-for-contract-test"
	client, err := NewClientWithCallbackOriginAndAPIKey(
		"http://127.0.0.1:19081",
		requestSigningKey,
		"http://127.0.0.1:18000",
		expectedAPIKey,
		&http.Client{},
		fixedNow,
		fixedNonce,
	)
	if err != nil {
		t.Fatalf("NewClientWithCallbackOriginAndAPIKey() error = %v", err)
	}

	for _, target := range []string{executionPath, lookupPath + "?externalRef=step-contract&idempotencyKey=cling-step%3Astep-contract"} {
		request, err := client.newRequest(context.Background(), http.MethodPost, target, []byte(`{}`))
		if target != executionPath {
			request, err = client.newRequest(context.Background(), http.MethodGet, target, nil)
		}
		if err != nil {
			t.Fatalf("newRequest(%q) error = %v", target, err)
		}
		if got := request.Header.Get("X-API-Key"); got != expectedAPIKey {
			t.Fatalf("X-API-Key = %q, want %q", got, expectedAPIKey)
		}
		if request.Header.Get("X-Signature-V2") == "" {
			t.Fatal("X-Signature-V2 must be present alongside X-API-Key")
		}
	}
}

// TestSubmit已创建的中台状态应视为受理，避免 Go 把实际已存在的任务误判为未知，
// 继而进入无意义的重复对账。明确终态失败仍必须保留为失败语义。
func TestSubmit已创建的中台状态应视为受理(t *testing.T) {
	for _, status := range []string{"queued", "dispatching", "processing", "completed"} {
		t.Run(status, func(t *testing.T) {
			if err := classifyExecutionStatus(status); err != nil {
				t.Fatalf("classifyExecutionStatus(%q) error = %v", status, err)
			}
		})
	}
}

func TestClient提交使用自己的回调地址(t *testing.T) {
	callbackReceiver := httptest.NewServer(http.NotFoundHandler())
	defer callbackReceiver.Close()
	generationServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Delivery Delivery `json:"delivery"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		if got, want := body.Delivery.Callback, callbackReceiver.URL+generationCallbackPath; got != want {
			t.Fatalf("delivery.callback = %q, want %q", got, want)
		}
		if strings.Contains(body.Delivery.Callback, "cling-ai.com") {
			t.Fatalf("delivery.callback 不得指向中台默认域名: %q", body.Delivery.Callback)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-callback","status":"accepted"}}`))
	}))
	defer generationServer.Close()

	client, err := NewClientWithCallbackOrigin(
		generationServer.URL,
		requestSigningKey,
		callbackReceiver.URL,
		generationServer.Client(),
		fixedNow,
		fixedNonce,
	)
	if err != nil {
		t.Fatalf("NewClientWithCallbackOrigin() error = %v", err)
	}
	if _, err := client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage))); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
}

func TestNewClientWithCallbackOrigin拒绝默认域名及尾随点(t *testing.T) {
	for _, callbackOrigin := range []string{
		"https://cling-ai.com",
		"https://cling-ai.com.",
		"https://callbacks.cling-ai.com",
		"https://CALLBACKS.CLING-AI.COM..",
	} {
		t.Run(callbackOrigin, func(t *testing.T) {
			_, err := NewClientWithCallbackOrigin(
				"http://127.0.0.1:19081",
				requestSigningKey,
				callbackOrigin,
				&http.Client{},
				fixedNow,
				fixedNonce,
			)
			if err == nil {
				t.Fatal("NewClientWithCallbackOrigin() error = nil, want rejection")
			}
			if strings.Contains(err.Error(), callbackOrigin) {
				t.Fatalf("error 泄露 callback origin: %q", err)
			}
		})
	}
}

func TestBuildExecution接受共享快照合同允许的输入(t *testing.T) {
	cases := []struct {
		capability executionv2.Capability
		sku        string
		raw        []byte
	}{
		{executionv2.CapabilityTextToImage, "ps-image-v1", []byte(`{"prompt":"图","assets":[],"parameters":{"width":1024}}`)},
		{executionv2.CapabilityImageEdit, "ps-edit-v1", []byte(`{"prompt":"编辑","assets":[{"role":"source_image","url":"https://assets.example.test/a.png"}],"parameters":{}}`)},
		{executionv2.CapabilityImageToVideo, "ps-auto", []byte(`{"prompt":"动起来","assets":[{"role":"opening_frame","url":"https://assets.example.test/a.png"}],"parameters":{}}`)},
	}
	for _, tc := range cases {
		t.Run(string(tc.capability), func(t *testing.T) {
			snapshot, err := executionv2.Compile(tc.capability, tc.sku, tc.raw)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}
			if _, err := BuildExecution(Step{ID: "step-contract", Capability: Capability(snapshot.Capability), ModelSKU: snapshot.ModelSKU, Input: Input(snapshot.Input)}); err != nil {
				t.Fatalf("BuildExecution() error = %v", err)
			}
			execution := mustBuildExecution(t, Step{ID: "step-contract", Capability: Capability(snapshot.Capability), ModelSKU: snapshot.ModelSKU, Input: Input(snapshot.Input)})
			if err := validateExecution(execution); err != nil {
				t.Fatalf("validateExecution() error = %v", err)
			}
		})
	}
}

func TestBuildExecution拒绝非法原子模型输入和商业字段(t *testing.T) {
	cases := []struct {
		name string
		step Step
	}{
		{
			name: "不支持的能力",
			step: validStep(Capability("animate")),
		},
		{
			name: "模型不是安全原子",
			step: func() Step {
				step := validStep(CapabilityTextToImage)
				step.ModelSKU = "flux-1"
				return step
			}(),
		},
		{
			name: "文生图不能携带素材",
			step: func() Step {
				step := validStep(CapabilityTextToImage)
				step.Input.Assets = []Asset{{Role: "source_image", URL: "https://assets.local/example.png"}}
				return step
			}(),
		},
		{
			name: "图像编辑必须携带素材",
			step: func() Step {
				step := validStep(CapabilityImageEdit)
				step.ModelSKU = "ps-edit-v1"
				return step
			}(),
		},
		{
			name: "嵌套商业字段",
			step: func() Step {
				step := validStep(CapabilityTextToImage)
				step.Input.Parameters = map[string]any{"render": map[string]any{"wallet": true}}
				return step
			}(),
		},
		{
			name: "嵌套来源字段",
			step: func() Step {
				step := validStep(CapabilityTextToImage)
				step.Input.Parameters = map[string]any{"render": []any{map[string]any{"provider": "gpu-1"}}}
				return step
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildExecution(tc.step); err == nil {
				t.Fatal("BuildExecution() error = nil, want rejection")
			}
		})
	}
}

func TestBuildExecution参数只允许安全技术白名单(t *testing.T) {
	cases := []map[string]any{
		{"product": "premium"},
		{"productId": "product-secret"},
		{"plan": "vip"},
		{"credits": 100},
		{"quota": 10},
		{"render": map[string]any{"product": "premium"}},
		{"render": []any{map[string]any{"credits": 100}}},
	}
	for _, parameters := range cases {
		step := validStep(CapabilityTextToImage)
		step.Input.Parameters = parameters
		if _, err := BuildExecution(step); err == nil {
			t.Fatalf("BuildExecution(%#v) error = nil, want parameter rejection", parameters)
		}
	}

	step := validStep(CapabilityTextToImage)
	step.Input.Parameters = map[string]any{"width": 1024, "render": map[string]any{"height": 1024}}
	if _, err := BuildExecution(step); err != nil {
		t.Fatalf("BuildExecution() error = %v, want safe technical parameters accepted", err)
	}
}

func TestBuildExecution拒绝重复参数键(t *testing.T) {
	step := validStep(CapabilityTextToImage)
	step.Input.Parameters = map[string]any{
		"render": &duplicateKeyMarshaler{},
	}

	if _, err := BuildExecution(step); err == nil {
		t.Fatal("BuildExecution() error = nil, want duplicate parameter rejection")
	}
}

func TestValidateRawExecution拒绝深层重复参数键(t *testing.T) {
	execution := mustBuildExecution(t, validStep(CapabilityTextToImage))
	raw, err := json.Marshal(execution)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	cases := map[string]string{
		"嵌套对象": `{"render":{"width":{"product":"x"},"width":1024}}`,
		"数组对象": `{"render":[{"height":{"product":"x"},"height":1024}]}`,
	}
	for name, parameters := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := []byte(strings.Replace(string(raw), `"parameters":{}`, `"parameters":`+parameters, 1))
			if err := validateRawExecution(mutated); err == nil {
				t.Fatal("validateRawExecution() error = nil, want duplicate parameter rejection")
			}
		})
	}
}

func TestValidateRawExecution拒绝超过发件箱上限的合法信封(t *testing.T) {
	largeSite := strings.Repeat("x", 1<<20)
	raw := []byte(`{"contractVersion":"execution.v2","idempotencyKey":"cling-step:step-42","externalRef":"step-42","capability":"text_to_image","modelSku":"ps-image-v1","input":{"prompt":"图","assets":[],"parameters":{}},"delivery":{"callback":"tenant","resultUrlPolicy":"permanent"},"priorityClass":"standard","metadata":{"site":"` + largeSite + `"}}`)
	if len(raw) <= 1<<20 {
		t.Fatalf("test envelope length = %d, want > 1MiB", len(raw))
	}
	if err := validateRawExecution(raw); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("validateRawExecution() error = %v, want ErrInvalidExecution", err)
	}
}

func TestContractErrors不回显不可信输入(t *testing.T) {
	cases := []struct {
		name   string
		step   Step
		secret string
	}{
		{
			name: "能力值",
			step: func() Step {
				step := validStep(Capability("capability-secret"))
				return step
			}(),
			secret: "capability-secret",
		},
		{
			name: "参数名",
			step: func() Step {
				step := validStep(CapabilityTextToImage)
				step.Input.Parameters = map[string]any{"product-secret-key": "value"}
				return step
			}(),
			secret: "product-secret-key",
		},
		{
			name: "序列化错误",
			step: func() Step {
				step := validStep(CapabilityTextToImage)
				step.Input.Parameters = map[string]any{"width": failingMarshaler{message: "marshal-secret-prompt-url"}}
				return step
			}(),
			secret: "marshal-secret-prompt-url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildExecution(tc.step)
			if err == nil {
				t.Fatal("BuildExecution() error = nil, want rejection")
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("BuildExecution() error = %q, must not expose untrusted input", err)
			}
		})
	}
}

func TestSignatureV2使用现网签名载荷(t *testing.T) {
	rawBody := []byte(`{"contractVersion":"execution.v2"}`)
	const timestamp = "1788768550000"
	const nonce = "nonce-7"
	const target = "/api/v2/executions"
	const key = requestSigningKey

	wantPayload := "main-backend-generation-request-v2\nmain-backend\ngeneration-service\nPOST\n/api/v2/executions\n1788768550000\nnonce-7\n" + testSHA256Hex(rawBody)
	if got := SignaturePayloadV2(http.MethodPost, target, timestamp, nonce, rawBody); got != wantPayload {
		t.Fatalf("SignaturePayloadV2() = %q, want %q", got, wantPayload)
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(wantPayload))
	wantSignature := hex.EncodeToString(mac.Sum(nil))
	if got := SignV2(key, http.MethodPost, target, timestamp, nonce, rawBody); got != wantSignature {
		t.Fatalf("SignV2() = %q, want %q", got, wantSignature)
	}
}

func TestBuildExecution使用对象资产结构和能力约束(t *testing.T) {
	step := validStep(CapabilityImageEdit)
	step.ModelSKU = "ps-edit-v1"
	step.Input.Assets = []Asset{{Role: "source_image", URL: "https://assets.example.test/source.png", MediaType: "image/png"}}
	execution := mustBuildExecution(t, step)
	raw, err := json.Marshal(execution)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var body struct {
		Input struct {
			Assets []map[string]any `json:"assets"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(body.Input.Assets) != 1 {
		t.Fatalf("assets = %#v, want one object", body.Input.Assets)
	}
	assertExactKeys(t, body.Input.Assets[0], "role", "url", "mediaType")
	if got := body.Input.Assets[0]["role"]; got != "source_image" {
		t.Fatalf("asset.role = %v, want source_image", got)
	}

	cases := []struct {
		name string
		step Step
	}{
		{
			name: "HTTP 素材地址",
			step: withAsset(step, Asset{Role: "source_image", URL: "http://assets.example.test/source.png"}),
		},
		{
			name: "带用户信息的素材地址",
			step: withAsset(step, Asset{Role: "source_image", URL: "https://user@assets.example.test/source.png"}),
		},
		{
			name: "带片段的素材地址",
			step: withAsset(step, Asset{Role: "source_image", URL: "https://assets.example.test/source.png#fragment"}),
		},
		{
			name: "未知素材角色",
			step: withAsset(step, Asset{Role: "unknown", URL: "https://assets.example.test/source.png"}),
		},
		{
			name: "视频只能有一张起始或源图",
			step: Step{
				ID:         "step-video",
				Capability: CapabilityImageToVideo,
				ModelSKU:   "ps-auto",
				Input: Input{
					Prompt: "镜头缓慢推进",
					Assets: []Asset{
						{Role: "opening_frame", URL: "https://assets.example.test/opening.png"},
						{Role: "source_image", URL: "https://assets.example.test/source.png"},
					},
				},
			},
		},
		{
			name: "引用图只能配参考模型",
			step: Step{
				ID:         "step-reference",
				Capability: CapabilityImageToVideo,
				ModelSKU:   "ps-auto",
				Input: Input{
					Prompt: "镜头缓慢推进",
					Assets: []Asset{
						{Role: "opening_frame", URL: "https://assets.example.test/opening.png"},
						{Role: "reference_image", URL: "https://assets.example.test/reference.png"},
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildExecution(tc.step); err == nil {
				t.Fatal("BuildExecution() error = nil, want rejection")
			}
		})
	}

	referenceStep := Step{
		ID:         "step-reference-ok",
		Capability: CapabilityImageToVideo,
		ModelSKU:   "ps-reference-v1",
		Input: Input{
			Prompt: "镜头缓慢推进",
			Assets: []Asset{
				{Role: "opening_frame", URL: "https://assets.example.test/opening.png"},
				{Role: "reference_image", URL: "https://assets.example.test/reference.png"},
			},
		},
	}
	if _, err := BuildExecution(referenceStep); err != nil {
		t.Fatalf("BuildExecution(referenceStep) error = %v", err)
	}
}

func TestBuildExecution只接受能力对应的现网模型(t *testing.T) {
	models := map[Capability][]string{
		CapabilityTextToImage: {"ps-image-v1", "ps-anime-v1"},
		CapabilityImageEdit: {
			"ps-edit-v1", "ps-edit-body-v1", "ps-edit-pose-v1", "ps-edit-identity-v1", "ps-edit-apparel-v1", "ps-edit-compose-v1", "ps-edit-perspective-v1", "ps-edit-reshape-v1", "ps-edit-reshape-detail-v1", "ps-upscale-image-v1",
		},
		CapabilityImageToVideo: {"ps-auto", "ps-anchor-v1", "ps-rush-v1", "ps-apex-v1", "ps-reference-v1"},
	}
	for capability, skus := range models {
		for _, modelSKU := range skus {
			t.Run(string(capability)+"/"+modelSKU, func(t *testing.T) {
				step := validStep(capability)
				step.ModelSKU = modelSKU
				if capability == CapabilityImageEdit {
					step.Input.Assets = []Asset{{Role: "source_image", URL: "https://assets.example.test/source.png"}}
				}
				if capability == CapabilityImageToVideo {
					step.Input.Assets = []Asset{{Role: "opening_frame", URL: "https://assets.example.test/opening.png"}}
				}
				if _, err := BuildExecution(step); err != nil {
					t.Fatalf("BuildExecution() error = %v", err)
				}
			})
		}
	}

	for _, step := range []Step{
		func() Step {
			step := validStep(CapabilityTextToImage)
			step.ModelSKU = "ps-edit-v1"
			return step
		}(),
		func() Step {
			step := validStep(CapabilityImageEdit)
			step.ModelSKU = "ps-image-v1"
			step.Input.Assets = []Asset{{Role: "source_image", URL: "https://assets.example.test/source.png"}}
			return step
		}(),
	} {
		if _, err := BuildExecution(step); err == nil {
			t.Fatalf("BuildExecution(%s, %s) error = nil, want model rejection", step.Capability, step.ModelSKU)
		}
	}
}

func TestClient提交请求的路径请求头与主体精确(t *testing.T) {
	execution := mustBuildExecution(t, validStep(CapabilityTextToImage))
	const timestamp = "1788768550000"
	const nonce = "nonce-8"
	const key = requestSigningKey
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", request.Method)
		}
		if request.URL.Path != "/api/v2/executions" || request.URL.RawQuery != "" {
			t.Fatalf("target = %s?%s, want /api/v2/executions", request.URL.Path, request.URL.RawQuery)
		}
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if got := request.Header.Get("X-Service-Id"); got != "main-backend" {
			t.Fatalf("X-Service-Id = %q, want main-backend", got)
		}
		if got := request.Header.Get("X-Timestamp"); got != timestamp {
			t.Fatalf("X-Timestamp = %q, want %q", got, timestamp)
		}
		if got := request.Header.Get("X-Request-Nonce"); got != nonce {
			t.Fatalf("X-Request-Nonce = %q, want %q", got, nonce)
		}
		if got := request.Header.Get("X-Content-SHA256"); got != testSHA256Hex(rawBody) {
			t.Fatalf("X-Content-SHA256 = %q, want %q", got, testSHA256Hex(rawBody))
		}
		if got := request.Header.Get("X-Signature-V2"); got != SignV2(key, http.MethodPost, "/api/v2/executions", timestamp, nonce, rawBody) {
			t.Fatalf("X-Signature-V2 = %q, want signed request", got)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-42","status":"accepted"}}`))
	}))
	defer server.Close()

	client := mustClient(t, server, key, timestamp, nonce)
	result, err := client.Submit(context.Background(), execution)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if result.JobID != "job-42" {
		t.Fatalf("Submit().JobID = %q, want job-42", result.JobID)
	}
}

func TestClient明确拒绝与未知结果分类(t *testing.T) {
	execution := mustBuildExecution(t, validStep(CapabilityTextToImage))
	cases := []struct {
		name       string
		handler    http.HandlerFunc
		wantTarget error
	}{
		{
			name: "400 是明确拒绝",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = writer.Write([]byte("internal rejection detail"))
			},
			wantTarget: ErrRejected,
		},
		{
			name: "500 是未知结果",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(http.StatusInternalServerError)
			},
			wantTarget: ErrOutcomeUnknown,
		},
		{
			name: "409 是未知结果",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(http.StatusConflict)
			},
			wantTarget: ErrOutcomeUnknown,
		},
		{
			name: "403 是明确拒绝",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(http.StatusForbidden)
			},
			wantTarget: ErrRejected,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-9")

			_, err := client.Submit(context.Background(), execution)
			if !errors.Is(err, tc.wantTarget) {
				t.Fatalf("Submit() error = %v, want %v", err, tc.wantTarget)
			}
			if errors.Is(tc.wantTarget, ErrRejected) && strings.Contains(err.Error(), "internal rejection detail") {
				t.Fatalf("Submit() error = %v, must not expose rejection response body", err)
			}
		})
	}
}

func TestClient2xx异常Envelope是未知结果(t *testing.T) {
	bodies := []string{
		`{"success":false}`,
		`{"success":true,"data":{}}`,
		`{"success":true,"data":{"jobId":123}}`,
	}
	for _, statusCode := range []int{http.StatusOK, http.StatusCreated} {
		for _, body := range bodies {
			t.Run(http.StatusText(statusCode)+"/"+body, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					writer.WriteHeader(statusCode)
					_, _ = writer.Write([]byte(body))
				}))
				defer server.Close()
				client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-abnormal-envelope")

				_, err := client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage)))
				if !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatalf("Submit() error = %v, want ErrOutcomeUnknown", err)
				}
				if errors.Is(err, ErrRejected) {
					t.Fatalf("Submit() error = %v, must not classify 2xx envelope as rejected", err)
				}
			})
		}
	}
}

func TestClient拒绝成功Envelope后的尾随垃圾(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-42"}}not-json`))
	}))
	defer server.Close()
	client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-trailing-garbage")

	_, err := client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage)))
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Submit() error = %v, want ErrOutcomeUnknown", err)
	}
}

func TestClient拒绝两个合法JSON文档(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-42"}}{"success":true,"data":{"jobId":"job-43"}}`))
	}))
	defer server.Close()
	client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-two-json-documents")

	_, err := client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage)))
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Submit() error = %v, want ErrOutcomeUnknown", err)
	}
}

func TestClient超时是未知结果(t *testing.T) {
	execution := mustBuildExecution(t, validStep(CapabilityTextToImage))
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	httpClient := server.Client()
	httpClient.Timeout = 25 * time.Millisecond
	client, err := NewClient(server.URL, requestSigningKey, httpClient, func() time.Time {
		return time.Date(2026, time.September, 7, 8, 9, 10, 0, time.UTC)
	}, func() string {
		return "nonce-timeout"
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	_, err = client.Submit(context.Background(), execution)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Submit() error = %v, want ErrOutcomeUnknown", err)
	}
}

func TestClient直接网络错误是未知结果(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	client, err := NewClient("http://127.0.0.1:19081", requestSigningKey, httpClient, fixedNow, fixedNonce)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	_, err = client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage)))
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Submit() error = %v, want ErrOutcomeUnknown", err)
	}
}

func TestClient查询使用稳定外部引用参数(t *testing.T) {
	const timestamp = "1788768550000"
	const nonce = "nonce-lookup"
	const key = requestSigningKey
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", request.Method)
		}
		if request.URL.Path != "/api/v2/executions/lookup" {
			t.Fatalf("path = %q, want /api/v2/executions/lookup", request.URL.Path)
		}
		if request.URL.RawQuery != "externalRef=step-42&idempotencyKey=cling-step%3Astep-42" {
			t.Fatalf("query = %q, want stable externalRef and idempotencyKey", request.URL.RawQuery)
		}
		if got := request.Header.Get("X-Service-Id"); got != "main-backend" {
			t.Fatalf("X-Service-Id = %q, want main-backend", got)
		}
		if got := request.Header.Get("X-Content-SHA256"); got != testSHA256Hex(nil) {
			t.Fatalf("X-Content-SHA256 = %q, want empty body digest", got)
		}
		target := "/api/v2/executions/lookup?externalRef=step-42&idempotencyKey=cling-step%3Astep-42"
		if got := request.Header.Get("X-Signature-V2"); got != SignV2(key, http.MethodGet, target, timestamp, nonce, nil) {
			t.Fatalf("X-Signature-V2 = %q, want signed lookup", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-lookup","status":"accepted"}}`))
	}))
	defer server.Close()

	client := mustClient(t, server, key, timestamp, nonce)
	result, err := client.Lookup(context.Background(), "step-42")
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if result.Status != "accepted" {
		t.Fatalf("Lookup().Status = %q, want accepted", result.Status)
	}
	if result.JobID != "job-lookup" {
		t.Fatalf("Lookup().JobID = %q, want job-lookup", result.JobID)
	}
}

func TestClientDo只接受精确V2请求组合(t *testing.T) {
	var calls atomic.Int32
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body:       io.NopCloser(strings.NewReader(`{"success":true,"data":{"jobId":"job"}}`)),
			Header:     make(http.Header),
		}, nil
	})}
	client, err := NewClient("http://127.0.0.1:19081", requestSigningKey, httpClient, fixedNow, fixedNonce)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	cases := []struct {
		method string
		target string
	}{
		{method: http.MethodGet, target: executionPath},
		{method: http.MethodPost, target: lookupPath + "?externalRef=step-42&idempotencyKey=cling-step%3Astep-42"},
		{method: http.MethodDelete, target: executionPath},
		{method: http.MethodPost, target: executionPath + "?unexpected=value"},
		{method: http.MethodPost, target: executionPath + "?"},
		{method: http.MethodPost, target: executionPath + "#"},
		{method: http.MethodGet, target: lookupPath + "?externalRef=step-42"},
		{method: http.MethodGet, target: lookupPath + "?externalRef=step-42&idempotencyKey=cling-step%3Astep-42&unexpected=value"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			if _, err := client.do(context.Background(), tc.method, tc.target, nil); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("do() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0", got)
	}
}

func TestClient不跟随重定向并将其归类为未知(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		redirected.Add(1)
		writer.WriteHeader(http.StatusCreated)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer origin.Close()

	client := mustClient(t, origin, requestSigningKey, "1788768550000", "nonce-redirect")
	_, err := client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage)))
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Submit() error = %v, want ErrOutcomeUnknown", err)
	}
	if got := redirected.Load(); got != 0 {
		t.Fatalf("redirect destination calls = %d, want 0", got)
	}
}

func TestClient拒绝最终序列化中出现的禁止字段且不发送(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"must-not-exist"}}`))
	}))
	defer server.Close()

	execution := validExecution(map[string]any{"style": &changingMarshaler{}})
	client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-changing-json")
	if _, err := client.Submit(context.Background(), execution); err == nil {
		t.Fatal("Submit() error = nil, want final JSON rejection")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0", got)
	}
}

func TestClient拒绝直构执行中的重复参数键且不发送(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"must-not-exist"}}`))
	}))
	defer server.Close()

	execution := validExecution(map[string]any{"render": &duplicateKeyMarshaler{}})
	client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-duplicate-parameters")
	if _, err := client.Submit(context.Background(), execution); err == nil {
		t.Fatal("Submit() error = nil, want duplicate parameter rejection")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0", got)
	}
}

func TestClient提交接受201和200Envelope(t *testing.T) {
	for _, statusCode := range []int{http.StatusCreated, http.StatusOK} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(statusCode)
				_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-accepted","status":"accepted"}}`))
			}))
			defer server.Close()
			client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-accepted")

			result, err := client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage)))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if result.JobID != "job-accepted" || result.Status != "accepted" {
				t.Fatalf("Submit() result = %#v, want accepted job", result)
			}
		})
	}
}

func TestClient查询HTTP失败始终结果未知(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(status)
			}))
			defer server.Close()
			client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-lookup-http-failure")

			_, err := client.Lookup(context.Background(), "step-42")
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("Lookup() error = %v, want ErrOutcomeUnknown", err)
			}
			if errors.Is(err, ErrRejected) {
				t.Fatalf("Lookup() error = %v, must not classify HTTP %d as rejected", err, status)
			}
		})
	}
}

func TestNewClient拒绝非Origin地址和短密钥(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		key     string
	}{
		{name: "路径", baseURL: "http://127.0.0.1:19081/path", key: requestSigningKey},
		{name: "根路径", baseURL: "https://127.0.0.1:19081/", key: requestSigningKey},
		{name: "查询参数", baseURL: "http://127.0.0.1:19081?next=value", key: requestSigningKey},
		{name: "裸查询符", baseURL: "http://127.0.0.1:19081?", key: requestSigningKey},
		{name: "裸片段符", baseURL: "http://127.0.0.1:19081#", key: requestSigningKey},
		{name: "用户信息", baseURL: "http://user@127.0.0.1:19081", key: requestSigningKey},
		{name: "短密钥", baseURL: "http://127.0.0.1:19081", key: "too-short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.baseURL, tc.key, &http.Client{}, fixedNow, fixedNonce); err == nil {
				t.Fatal("NewClient() error = nil, want rejection")
			}
		})
	}
}

func TestClient签名使用去除空白后的HMAC密钥(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		timestamp := request.Header.Get("X-Timestamp")
		nonce := request.Header.Get("X-Request-Nonce")
		want := SignV2(requestSigningKey, request.Method, request.URL.RequestURI(), timestamp, nonce, rawBody)
		if got := request.Header.Get("X-Signature-V2"); got != want {
			t.Fatalf("X-Signature-V2 = %q, want signature with trimmed key", got)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-trimmed-key","status":"accepted"}}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, " \t"+requestSigningKey+"\n", server.Client(), fixedNow, fixedNonce)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if _, err := client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage))); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
}

func TestClient发送请求时清除裸查询符(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.RequestURI != executionPath || request.URL.ForceQuery {
			t.Fatalf("request target = %q forceQuery=%t, want %q without force query", request.RequestURI, request.URL.ForceQuery, executionPath)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-clean-target","status":"accepted"}}`))
	}))
	defer server.Close()
	client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-clean-target")
	client.baseURL.ForceQuery = true

	if _, err := client.Submit(context.Background(), mustBuildExecution(t, validStep(CapabilityTextToImage))); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
}

func TestClient拒绝最终JSON中的NullAssets且不发送(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"must-not-exist"}}`))
	}))
	defer server.Close()
	client := mustClient(t, server, requestSigningKey, "1788768550000", "nonce-null-assets")

	if _, err := client.Submit(context.Background(), validExecution(map[string]any{})); err == nil {
		t.Fatal("Submit() error = nil, want assets null rejection")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0", got)
	}
}

func validStep(capability Capability) Step {
	return Step{
		ID:         "step-42",
		Capability: capability,
		ModelSKU:   "ps-image-v1",
		Input: Input{
			Prompt: "一只在月光下奔跑的狐狸",
		},
	}
}

func withAsset(step Step, asset Asset) Step {
	step.Input.Assets = []Asset{asset}
	return step
}

func validExecution(parameters map[string]any) Execution {
	return Execution{
		ContractVersion: contractVersion,
		IdempotencyKey:  "cling-step:step-42",
		ExternalRef:     "step-42",
		Capability:      CapabilityTextToImage,
		ModelSKU:        "ps-image-v1",
		Input: Input{
			Prompt:     "一只在月光下奔跑的狐狸",
			Parameters: parameters,
		},
		Delivery:      Delivery{Callback: deliveryCallback, ResultURLPolicy: resultURLPolicy},
		PriorityClass: priorityClass,
		Metadata:      Metadata{Site: metadataSite},
	}
}

func fixedNow() time.Time {
	return time.Date(2026, time.September, 7, 8, 9, 10, 0, time.UTC)
}

func fixedNonce() string {
	return "nonce-fixed"
}

type changingMarshaler struct {
	calls int
}

type duplicateKeyMarshaler struct {
	calls     int
	safeCalls int
}

type failingMarshaler struct {
	message string
}

func (marshaler failingMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New(marshaler.message)
}

func (marshaler *changingMarshaler) MarshalJSON() ([]byte, error) {
	marshaler.calls++
	if marshaler.calls == 1 {
		return []byte(`"safe"`), nil
	}
	return []byte(`{"wallet":true}`), nil
}

func (marshaler *duplicateKeyMarshaler) MarshalJSON() ([]byte, error) {
	marshaler.calls++
	if marshaler.calls <= marshaler.safeCalls {
		return []byte(`{"width":1024}`), nil
	}
	return []byte(`{"width":{"product":"x"},"width":1024}`), nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func mustBuildExecution(t *testing.T, step Step) Execution {
	t.Helper()
	execution, err := BuildExecution(step)
	if err != nil {
		t.Fatalf("BuildExecution() error = %v", err)
	}
	return execution
}

func mustClient(t *testing.T, server *httptest.Server, key, timestamp, nonce string) *Client {
	t.Helper()
	client, err := NewClient(server.URL, key, server.Client(), func() time.Time {
		return time.Date(2026, time.September, 7, 8, 9, 10, 0, time.UTC)
	}, func() string {
		return nonce
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func assertExactKeys(t *testing.T, values map[string]any, want ...string) {
	t.Helper()
	if len(values) != len(want) {
		t.Fatalf("keys = %#v, want %#v", values, want)
	}
	for _, key := range want {
		if _, ok := values[key]; !ok {
			t.Fatalf("keys = %#v, want key %q", values, key)
		}
	}
}

func assertNoForbiddenKeys(t *testing.T, value any) {
	t.Helper()
	forbidden := map[string]struct{}{
		"userid": {}, "templateid": {}, "diamonds": {}, "balance": {}, "vip": {}, "dailyquota": {}, "ledger": {}, "wallet": {}, "price": {}, "pricing": {}, "entitlement": {}, "billing": {}, "payment": {},
		"callbackurl": {}, "callbackpolicy": {}, "source": {}, "origin": {}, "workflow": {}, "provider": {}, "gpu": {},
	}
	assertValueHasNoForbiddenKeys(t, value, forbidden)
}

func assertValueHasNoForbiddenKeys(t *testing.T, value any, forbidden map[string]struct{}) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if _, found := forbidden[strings.ToLower(key)]; found {
				t.Fatalf("body contains forbidden key %q", key)
			}
			assertValueHasNoForbiddenKeys(t, nested, forbidden)
		}
	case []any:
		for _, nested := range typed {
			assertValueHasNoForbiddenKeys(t, nested, forbidden)
		}
	}
}

func testSHA256Hex(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
