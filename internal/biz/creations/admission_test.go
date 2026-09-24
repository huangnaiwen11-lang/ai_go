package creations

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"ai-business-service/internal/biz/entitlement"
)

func validB2BRecipe() B2BProductRecipe {
	return B2BProductRecipe{
		ProductKey:  "ps-image-v1",
		TemplateKey: "portrait",
		Input:       json.RawMessage(`{"prompt":"海边的灯塔","aspectRatio":"1:1"}`),
		Assets:      []B2BAsset{{Role: "source_image", URL: "https://cdn.example.com/source.png"}},
	}
}

func b2bExecutionRoute() ExecutionRoute {
	return ExecutionRoute{Provider: PolarStarB2BProvider, AccountRef: "acct-main", ContractVersion: B2BContractVersion, MappingVersion: "polarstar.image.v1"}
}

// 未装配 B2B admission 时，任何已按公开合同编译的配方都必须被拒绝，
// 否则它会被静默丢弃并把 B2B 请求跑成本地模拟器。
func TestLocalAdmissionResolverRejectsPublicRecipe(t *testing.T) {
	resolver := admissionResolverOrDefault(nil)
	recipe := validB2BRecipe()
	if _, err := resolver.ResolveAdmission(context.Background(), AdmissionRequest{Atom: AtomTextToImage, Sequence: 1, B2B: &recipe}); !errors.Is(err, ErrAdmissionConfigurationUnavailable) {
		t.Fatalf("ResolveAdmission() error = %v, want ErrAdmissionConfigurationUnavailable", err)
	}
}

func TestLocalAdmissionResolverFreezesLocalRoute(t *testing.T) {
	resolver := admissionResolverOrDefault(nil)
	admission, err := resolver.ResolveAdmission(context.Background(), AdmissionRequest{Atom: AtomTextToImage, Sequence: 1})
	if err != nil {
		t.Fatalf("ResolveAdmission() error = %v", err)
	}
	if admission.Route != LocalExecutionRoute() || admission.B2B != nil {
		t.Fatalf("admission = %#v, want bare local route", admission)
	}
}

func TestAdmissionResolverOrDefaultKeepsSuppliedResolver(t *testing.T) {
	supplied := &stubLocalResolver{}
	if admissionResolverOrDefault(supplied) != supplied {
		t.Fatal("已装配的解析器不得被默认值覆盖")
	}
}

type stubLocalResolver struct{}

func (*stubLocalResolver) ResolveAdmission(context.Context, AdmissionRequest) (StepAdmission, error) {
	return StepAdmission{Route: LocalExecutionRoute()}, nil
}

// 新步骤不接受零值归属：历史文档的全空归属兼容本地，但新任务必须显式选择 provider。
func TestStepAdmissionRejectsZeroRoute(t *testing.T) {
	if _, err := (StepAdmission{}).Normalize(); !errors.Is(err, ErrInvalidAdmission) {
		t.Fatalf("Normalize() error = %v, want ErrInvalidAdmission", err)
	}
}

func TestStepAdmissionRequiresRecipeForB2BRoute(t *testing.T) {
	if _, err := (StepAdmission{Route: b2bExecutionRoute()}).Normalize(); !errors.Is(err, ErrInvalidAdmission) {
		t.Fatalf("Normalize() error = %v, want ErrInvalidAdmission", err)
	}
}

// 本地归属不得携带公开配方：那正是「selector 选 B2B、admission 仍冻本地」的静默降级形状。
func TestStepAdmissionRejectsRecipeOnLocalRoute(t *testing.T) {
	recipe := validB2BRecipe()
	if _, err := (StepAdmission{Route: LocalExecutionRoute(), B2B: &recipe}).Normalize(); !errors.Is(err, ErrInvalidAdmission) {
		t.Fatalf("Normalize() error = %v, want ErrInvalidAdmission", err)
	}
}

func TestStepAdmissionAcceptsB2BRouteWithRecipe(t *testing.T) {
	recipe := validB2BRecipe()
	admission, err := (StepAdmission{Route: b2bExecutionRoute(), B2B: &recipe}).Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if admission.Route != b2bExecutionRoute() || admission.B2B == nil || admission.B2B.ProductKey != recipe.ProductKey {
		t.Fatalf("admission = %#v, want frozen B2B route with recipe", admission)
	}
}

func TestStepAdmissionRejectsInvalidRecipe(t *testing.T) {
	recipe := validB2BRecipe()
	recipe.Input = json.RawMessage(`[]`)
	if _, err := (StepAdmission{Route: b2bExecutionRoute(), B2B: &recipe}).Normalize(); !errors.Is(err, ErrInvalidAdmission) {
		t.Fatalf("Normalize() error = %v, want ErrInvalidAdmission", err)
	}
}

// 归一化必须返回全新切片，调用方之后修改原配方不得影响冻结结果。
func TestStepAdmissionNormalizeDoesNotShareRecipeSlices(t *testing.T) {
	recipe := validB2BRecipe()
	admission, err := (StepAdmission{Route: b2bExecutionRoute(), B2B: &recipe}).Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	recipe.Input[0] = '['
	recipe.Assets[0].URL = "https://cdn.example.com/other.png"
	if string(admission.B2B.Input) != `{"prompt":"海边的灯塔","aspectRatio":"1:1"}` {
		t.Fatalf("归一化后的输入被调用方改写：%s", admission.B2B.Input)
	}
	if admission.B2B.Assets[0].URL != "https://cdn.example.com/source.png" {
		t.Fatalf("归一化后的素材被调用方改写：%#v", admission.B2B.Assets)
	}
}

func TestB2BProductRecipeRejectsInvalidShapes(t *testing.T) {
	cases := map[string]func(*B2BProductRecipe){
		"空产品键":     func(recipe *B2BProductRecipe) { recipe.ProductKey = "" },
		"产品键含空白":   func(recipe *B2BProductRecipe) { recipe.ProductKey = "ps image" },
		"产品键首尾空白":  func(recipe *B2BProductRecipe) { recipe.ProductKey = " ps-image " },
		"产品键过长":    func(recipe *B2BProductRecipe) { recipe.ProductKey = strings.Repeat("a", 513) },
		"模板键非法":    func(recipe *B2BProductRecipe) { recipe.TemplateKey = "bad\tkey" },
		"输入为空":     func(recipe *B2BProductRecipe) { recipe.Input = nil },
		"输入非对象":    func(recipe *B2BProductRecipe) { recipe.Input = json.RawMessage(`"prompt"`) },
		"输入为 null": func(recipe *B2BProductRecipe) { recipe.Input = json.RawMessage(`null`) },
		"输入超限": func(recipe *B2BProductRecipe) {
			recipe.Input = json.RawMessage(`{"p":"` + strings.Repeat("a", maxAdmissionRecipeBytes) + `"}`)
		},
		"素材过多": func(recipe *B2BProductRecipe) { recipe.Assets = make([]B2BAsset, maxAdmissionAssets+1) },
		"素材角色为空": func(recipe *B2BProductRecipe) {
			recipe.Assets = []B2BAsset{{Role: "", URL: "https://cdn.example.com/a.png"}}
		},
		"素材地址为空": func(recipe *B2BProductRecipe) { recipe.Assets = []B2BAsset{{Role: "source_image"}} },
		"素材地址换行": func(recipe *B2BProductRecipe) {
			recipe.Assets = []B2BAsset{{Role: "source_image", URL: "https://cdn.example.com/a.png\n"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			recipe := validB2BRecipe()
			mutate(&recipe)
			if _, err := recipe.Normalize(); !errors.Is(err, ErrInvalidAdmission) {
				t.Fatalf("Normalize() error = %v, want ErrInvalidAdmission", err)
			}
		})
	}
}

func TestB2BProductRecipeAcceptsMinimalShape(t *testing.T) {
	recipe := B2BProductRecipe{ProductKey: "ps-image-v1", Input: json.RawMessage(`{"prompt":"x"}`)}
	normalized, err := recipe.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if normalized.TemplateKey != "" || len(normalized.Assets) != 0 {
		t.Fatalf("normalized = %#v, want empty optional fields", normalized)
	}
}

// 发件箱载荷与幂等指纹都依赖同一份字节，因此重复编码必须完全一致。
func TestB2BProductRecipeMarshalIsStable(t *testing.T) {
	recipe := validB2BRecipe()
	first, err := recipe.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	second, err := recipe.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("重复编码不一致：%s vs %s", first, second)
	}
	firstDigest, err := recipe.Digest()
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	secondDigest, err := recipe.Digest()
	if err != nil || firstDigest != secondDigest {
		t.Fatalf("重复摘要不一致：%q vs %q (err=%v)", firstDigest, secondDigest, err)
	}
}

func TestParseB2BProductRecipeRoundTripsFrozenBytes(t *testing.T) {
	recipe := validB2BRecipe()
	payload, err := recipe.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	parsed, err := ParseB2BProductRecipe(payload)
	if err != nil {
		t.Fatalf("ParseB2BProductRecipe() error = %v", err)
	}
	normalized, err := recipe.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if parsed.ProductKey != normalized.ProductKey || parsed.TemplateKey != normalized.TemplateKey ||
		string(parsed.Input) != string(normalized.Input) || len(parsed.Assets) != len(normalized.Assets) || parsed.Assets[0] != normalized.Assets[0] {
		t.Fatalf("parsed = %#v, want %#v", parsed, normalized)
	}
}

func TestParseB2BProductRecipeRejectsUnknownOrCorruptPayload(t *testing.T) {
	for name, payload := range map[string]string{
		"未知字段":   `{"productKey":"ps-image-v1","input":{"prompt":"x"},"accountRef":"acct-main"}`,
		"非 JSON": "not-json",
		"空对象":    `{}`,
		"数组载荷":   `[{"productKey":"ps-image-v1"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseB2BProductRecipe([]byte(payload)); !errors.Is(err, ErrInvalidAdmission) {
				t.Fatalf("ParseB2BProductRecipe() error = %v, want ErrInvalidAdmission", err)
			}
		})
	}
}

// 公开配方属于请求本身：换配方必须换指纹，同配方必须稳定。
func TestRequestFingerprintCoversPublicRecipe(t *testing.T) {
	base := CreateReservedRequest{
		UserID:          "user-1",
		IdempotencyKey:  "idem-1",
		TemplateID:      "template-1",
		TemplateVersion: 1,
		Product:         entitlement.GenerationRequest{Output: entitlement.ProductOutputImage},
		InputDigest:     strings.Repeat("a", 64),
		Plan:            []StepPlan{{Sequence: 1, Atom: AtomTextToImage}},
	}
	recipe := validB2BRecipe()
	base.B2BSubmission = &recipe

	first, err := RequestFingerprint(base)
	if err != nil {
		t.Fatalf("RequestFingerprint() error = %v", err)
	}
	again, err := RequestFingerprint(base)
	if err != nil || again != first {
		t.Fatalf("相同配方指纹不稳定：%q vs %q (err=%v)", first, again, err)
	}

	changed := base
	other := validB2BRecipe()
	other.Input = json.RawMessage(`{"prompt":"另一个提示词","aspectRatio":"1:1"}`)
	changed.B2BSubmission = &other
	otherFingerprint, err := RequestFingerprint(changed)
	if err != nil {
		t.Fatalf("RequestFingerprint() error = %v", err)
	}
	if otherFingerprint == first {
		t.Fatal("更换公开配方后不得复用原请求指纹")
	}

	// 归属不属于请求：selector 变化不能让同一幂等键变成另一个创作。
	withoutRecipe, err := RequestFingerprint(CreateReservedRequest{
		UserID: base.UserID, IdempotencyKey: base.IdempotencyKey, TemplateID: base.TemplateID,
		TemplateVersion: base.TemplateVersion, Product: base.Product, InputDigest: base.InputDigest, Plan: base.Plan,
	})
	if err != nil {
		t.Fatalf("RequestFingerprint() error = %v", err)
	}
	if withoutRecipe == first {
		t.Fatal("配方缺失与配方存在不得得到同一指纹")
	}
}
