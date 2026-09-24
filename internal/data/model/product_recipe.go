package model

// GenerationProductRecipeDocument stores one immutable, published mapping from
// an old visible template version to a public B2B product recipe. Input is a
// BSON object (rather than opaque bytes) so the control-plane import can
// validate it structurally before publication; runtime converts it to stable
// JSON before freezing it in a creation.
type GenerationProductRecipeDocument struct {
	ID                  string                           `bson:"_id"`
	Status              string                           `bson:"status"`
	TemplateID          string                           `bson:"template_id"`
	TemplateVersion     int64                            `bson:"template_version"`
	Atom                string                           `bson:"atom"`
	ProductKey          string                           `bson:"product_key"`
	TemplateKey         string                           `bson:"template_key,omitempty"`
	Input               map[string]any                   `bson:"input"`
	Assets              []GenerationProductAssetDocument `bson:"assets,omitempty"`
	AllowedUserInputs   []string                         `bson:"allowed_user_inputs,omitempty"`
	PromptUserInputMode string                           `bson:"prompt_user_input_mode,omitempty"`
}

type GenerationProductAssetDocument struct {
	Role string `bson:"role"`
	URL  string `bson:"url"`
}
