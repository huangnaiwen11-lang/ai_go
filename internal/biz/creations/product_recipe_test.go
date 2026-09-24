package creations

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestPublishedB2BProductRecipeCompilesOnlyApprovedUserFields(t *testing.T) {
	source := PublishedB2BProductRecipe{
		TemplateID: "template-image", TemplateVersion: 7, Atom: AtomTextToImage,
		ProductKey: "image-standard", Input: json.RawMessage(`{"prompt":"base","aspectRatio":"1:1"}`),
		AllowedUserInputs: []string{"prompt", "aspectRatio"},
	}
	compiled, err := source.CompileB2BProductRecipe(map[string]json.RawMessage{
		"prompt": json.RawMessage(`"a lighthouse"`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(compiled.Input) != `{"aspectRatio":"1:1","prompt":"a lighthouse"}` || compiled.ProductKey != "image-standard" {
		t.Fatalf("compiled product recipe = %#v", compiled)
	}
	if _, err := source.CompileB2BProductRecipe(map[string]json.RawMessage{
		"privateWorkflow": json.RawMessage(`"never"`),
	}, nil); !errors.Is(err, ErrB2BProductRecipeUnavailable) {
		t.Fatalf("unapproved field error = %v", err)
	}
}

func TestPublishedB2BProductRecipeRejectsDuplicateAssetRole(t *testing.T) {
	source := PublishedB2BProductRecipe{
		TemplateID: "template-edit", TemplateVersion: 2, Atom: AtomImageEdit,
		ProductKey: "edit-standard", Input: json.RawMessage(`{"prompt":"base"}`),
		Assets: []B2BAsset{{Role: "guide_image", URL: "https://static.example.com/guide.png"}},
	}
	_, err := source.CompileB2BProductRecipe(nil, []B2BAsset{{Role: "guide_image", URL: "https://media.example.com/user.png"}})
	if !errors.Is(err, ErrB2BProductRecipeUnavailable) {
		t.Fatalf("duplicate asset role error = %v", err)
	}
}

// 模板图编辑的用户文案在旧业务中是附加到受审模板 prompt，而不是替换模板
// 语义。这个拼接规则必须成为发布配方的一部分，不能由调用方猜测。
func TestPublishedB2BProductRecipeCanAppendUserPromptWhenPublished(t *testing.T) {
	source := PublishedB2BProductRecipe{
		TemplateID: "template-edit", TemplateVersion: 2, Atom: AtomImageEdit,
		ProductKey: "edit-standard", Input: json.RawMessage(`{"prompt":"editorial fashion"}`),
		AllowedUserInputs: []string{"prompt"}, PromptUserInputMode: PromptUserInputModeAppend,
	}
	compiled, err := source.CompileB2BProductRecipe(map[string]json.RawMessage{
		"prompt": json.RawMessage(`"red evening dress"`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(compiled.Input) != `{"prompt":"editorial fashion，red evening dress"}` {
		t.Fatalf("compiled input = %s", compiled.Input)
	}
}
