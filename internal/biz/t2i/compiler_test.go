package t2i

import (
	"encoding/json"
	"testing"

	"ai-business-service/internal/biz/creations"
)

// 自由 T2I 只能由固定模板配方决定模型和技术参数，用户输入只能填充产品字段。
func TestCompileFreeform冻结服务端技术配方(t *testing.T) {
	compiled, err := CompileFreeform(FreeformRecipe{
		TemplateID: "t2i-freeform",
		Version:    3,
		ModelSKU:   "ps-image-v1",
		Parameters: json.RawMessage(`{"steps":28,"width":1024,"height":1024}`),
	}, CreateInput{
		Prompt:         "portrait in studio light",
		NegativePrompt: "blur",
		AspectRatio:    "1:1",
	})
	if err != nil {
		t.Fatalf("CompileFreeform() error = %v", err)
	}
	if compiled.TemplateID != "t2i-freeform" || compiled.TemplateVersion != 3 {
		t.Fatalf("模板版本 = %q/%d", compiled.TemplateID, compiled.TemplateVersion)
	}
	if len(compiled.Plan) != 1 || compiled.Plan[0] != (creations.StepPlan{Sequence: 1, Atom: creations.AtomTextToImage}) {
		t.Fatalf("执行计划 = %#v", compiled.Plan)
	}
	if compiled.InitialSubmission == nil || compiled.InitialSubmission.ModelSKU != "ps-image-v1" {
		t.Fatalf("初始技术快照 = %#v", compiled.InitialSubmission)
	}
	var input map[string]any
	if err := json.Unmarshal(compiled.InitialSubmission.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input["prompt"] != "portrait in studio light" || input["negativePrompt"] != "blur" {
		t.Fatalf("提示词输入 = %#v", input)
	}
	parameters := input["parameters"].(map[string]any)
	if parameters["steps"] != float64(28) || parameters["width"] != float64(1024) || parameters["aspectRatio"] != "1:1" {
		t.Fatalf("技术参数 = %#v", parameters)
	}
}

// 未经认可的模板、模型或参数形状不能进入创作占位与预扣流程。
func TestCompileFreeform拒绝非固定模板配方(t *testing.T) {
	for _, recipe := range []FreeformRecipe{
		{TemplateID: "other-template", Version: 1, ModelSKU: "ps-image-v1", Parameters: json.RawMessage(`{}`)},
		{TemplateID: "t2i-freeform", Version: 0, ModelSKU: "ps-image-v1", Parameters: json.RawMessage(`{}`)},
		{TemplateID: "t2i-freeform", Version: 1, ModelSKU: "client-model", Parameters: json.RawMessage(`{}`)},
		{TemplateID: "t2i-freeform", Version: 1, ModelSKU: "ps-image-v1", Parameters: json.RawMessage(`{"callback":"https://example.test"}`)},
	} {
		if _, err := CompileFreeform(recipe, CreateInput{Prompt: "portrait"}); err == nil {
			t.Fatalf("非法配方被接受：%#v", recipe)
		}
	}
}
