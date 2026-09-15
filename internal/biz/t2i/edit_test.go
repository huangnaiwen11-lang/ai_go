package t2i

import (
	"encoding/json"
	"testing"

	"ai-business-service/internal/biz/creations"
)

func TestCompileImageEdit冻结模板配方与用户素材(t *testing.T) {
	compiled, err := CompileImageEdit(ImageEditRecipe{
		TemplateID: "dress-up", Version: 2, ModelSKU: "ps-edit-apparel-v1",
		Prompt: "换装模板提示词", NegativePrompt: "模糊", Parameters: json.RawMessage(`{"steps":28}`),
		ReferenceAssets: []ImageEditAsset{{Role: "guide_image", URL: "https://assets.example/guide.png"}},
	}, ImageEditInput{UserImageURL: "https://uploads.example/source.png", UserPrompt: "红色礼服", AspectRatio: "9:16"})
	if err != nil {
		t.Fatalf("CompileImageEdit() error = %v", err)
	}
	if compiled.TemplateID != "dress-up" || compiled.TemplateVersion != 2 || len(compiled.Plan) != 1 || compiled.Plan[0] != (creations.StepPlan{Sequence: 1, Atom: creations.AtomImageEdit}) {
		t.Fatalf("编译结果 = %#v", compiled)
	}
	var input map[string]any
	if err := json.Unmarshal(compiled.InitialSubmission.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input["prompt"] != "换装模板提示词，红色礼服" || input["negativePrompt"] != "模糊" {
		t.Fatalf("冻结提示词 = %#v", input)
	}
	parameters := input["parameters"].(map[string]any)
	if parameters["aspectRatio"] != "9:16" {
		t.Fatalf("用户选择的画幅没有写入冻结技术快照：%#v", parameters)
	}
	assets := input["assets"].([]any)
	if len(assets) != 2 || assets[0].(map[string]any)["role"] != "source_image" || assets[1].(map[string]any)["role"] != "guide_image" {
		t.Fatalf("冻结素材 = %#v", assets)
	}
}

func TestCompileImageEdit拒绝非法模板或用户素材(t *testing.T) {
	valid := ImageEditRecipe{TemplateID: "dress-up", Version: 1, ModelSKU: "ps-edit-apparel-v1", Prompt: "模板提示词", Parameters: json.RawMessage(`{"steps":28}`)}
	for _, testCase := range []struct {
		name   string
		recipe ImageEditRecipe
		input  ImageEditInput
	}{
		{name: "空用户图", recipe: valid, input: ImageEditInput{}},
		{name: "非HTTPS用户图", recipe: valid, input: ImageEditInput{UserImageURL: "http://uploads.example/source.png"}},
		{name: "客户端模型不能替换", recipe: ImageEditRecipe{TemplateID: "dress-up", Version: 1, ModelSKU: "client-model", Prompt: "模板提示词", Parameters: json.RawMessage(`{}`)}, input: ImageEditInput{UserImageURL: "https://uploads.example/source.png"}},
		{name: "非法回调参数", recipe: ImageEditRecipe{TemplateID: "dress-up", Version: 1, ModelSKU: "ps-edit-apparel-v1", Prompt: "模板提示词", Parameters: json.RawMessage(`{"callback":"https://bad.example"}`)}, input: ImageEditInput{UserImageURL: "https://uploads.example/source.png"}},
		{name: "非法参考图", recipe: ImageEditRecipe{TemplateID: "dress-up", Version: 1, ModelSKU: "ps-edit-apparel-v1", Prompt: "模板提示词", Parameters: json.RawMessage(`{}`), ReferenceAssets: []ImageEditAsset{{Role: "guide_image", URL: "http://assets.example/guide.png"}}}, input: ImageEditInput{UserImageURL: "https://uploads.example/source.png"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := CompileImageEdit(testCase.recipe, testCase.input); err == nil {
				t.Fatal("非法模板图编辑配方被接受")
			}
		})
	}
}
