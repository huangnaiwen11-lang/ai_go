package data

import (
	"encoding/json"
	"errors"
	"testing"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoB2BProductRecipeReadsExactPublishedTemplateVersion(t *testing.T) {
	f := newSubmissionMongoFixture(t)
	document := bson.M{
		"_id": "recipe:" + f.stepID, "status": "published",
		"template_id": "template-image", "template_version": int64(7), "atom": "text_to_image",
		"product_key": "image-standard", "input": bson.M{"prompt": "base", "aspectRatio": "1:1"},
		"allowed_user_inputs":    bson.A{"prompt", "aspectRatio"},
		"prompt_user_input_mode": "append",
	}
	if _, err := f.database.Collection(schema.CollectionGenerationProductRecipes).InsertOne(f.ctx, document); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		_, _ = f.database.Collection(schema.CollectionGenerationProductRecipes).DeleteOne(ctx, bson.M{"_id": document["_id"]})
	})
	reader := NewB2BProductRecipeRepository(f.data)
	got, err := reader.LoadB2BProductRecipe(f.ctx, "template-image", 7, creations.AtomTextToImage)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := got.CompileB2BProductRecipe(map[string]json.RawMessage{"prompt": json.RawMessage(`"a lighthouse"`)}, nil)
	if err != nil || compiled.ProductKey != "image-standard" || string(compiled.Input) != `{"aspectRatio":"1:1","prompt":"base，a lighthouse"}` {
		t.Fatalf("compiled recipe = %#v / %v", compiled, err)
	}
	if _, err := reader.LoadB2BProductRecipe(f.ctx, "template-image", 8, creations.AtomTextToImage); !errors.Is(err, creations.ErrB2BProductRecipeUnavailable) {
		t.Fatalf("other version error = %v", err)
	}
}
