package data

import (
	"context"
	"encoding/json"
	"fmt"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const b2bProductRecipeStatusPublished = "published"

// mongoB2BProductRecipeRepository is runtime read-only. Publishing recipes is
// deliberately a separate control-plane concern: a running request must never
// choose a different product because an operator edited a document in place.
type mongoB2BProductRecipeRepository struct{ recipes *mongo.Collection }

func NewB2BProductRecipeRepository(data *Data) creations.B2BProductRecipeReader {
	if data == nil || data.database == nil {
		return &mongoB2BProductRecipeRepository{}
	}
	return &mongoB2BProductRecipeRepository{recipes: data.database.Collection(schema.CollectionGenerationProductRecipes)}
}

func (repository *mongoB2BProductRecipeRepository) LoadB2BProductRecipe(ctx context.Context, templateID string, templateVersion int64, atom creations.StepAtom) (creations.PublishedB2BProductRecipe, error) {
	if repository == nil || repository.recipes == nil || templateID == "" || templateVersion <= 0 {
		return creations.PublishedB2BProductRecipe{}, creations.ErrB2BProductRecipeUnavailable
	}
	cursor, err := repository.recipes.Find(ctx, bson.D{
		{Key: "status", Value: b2bProductRecipeStatusPublished},
		{Key: "template_id", Value: templateID},
		{Key: "template_version", Value: templateVersion},
		{Key: "atom", Value: string(atom)},
	}, options.Find().SetLimit(2))
	if err != nil {
		return creations.PublishedB2BProductRecipe{}, fmt.Errorf("find B2B product recipe: %w", err)
	}
	defer cursor.Close(ctx)
	var documents []model.GenerationProductRecipeDocument
	for cursor.Next(ctx) {
		var document model.GenerationProductRecipeDocument
		if err := cursor.Decode(&document); err != nil {
			return creations.PublishedB2BProductRecipe{}, fmt.Errorf("decode B2B product recipe: %w", err)
		}
		documents = append(documents, document)
	}
	if err := cursor.Err(); err != nil {
		return creations.PublishedB2BProductRecipe{}, fmt.Errorf("iterate B2B product recipes: %w", err)
	}
	if len(documents) != 1 {
		return creations.PublishedB2BProductRecipe{}, creations.ErrB2BProductRecipeUnavailable
	}
	return b2bProductRecipeFromDocument(documents[0])
}

func b2bProductRecipeFromDocument(document model.GenerationProductRecipeDocument) (creations.PublishedB2BProductRecipe, error) {
	if document.Status != b2bProductRecipeStatusPublished || document.Input == nil {
		return creations.PublishedB2BProductRecipe{}, creations.ErrB2BProductRecipeUnavailable
	}
	input, err := json.Marshal(document.Input)
	if err != nil {
		return creations.PublishedB2BProductRecipe{}, creations.ErrB2BProductRecipeUnavailable
	}
	assets := make([]creations.B2BAsset, 0, len(document.Assets))
	for _, asset := range document.Assets {
		assets = append(assets, creations.B2BAsset{Role: asset.Role, URL: asset.URL})
	}
	recipe, err := (creations.PublishedB2BProductRecipe{
		TemplateID: document.TemplateID, TemplateVersion: document.TemplateVersion,
		Atom: creations.StepAtom(document.Atom), ProductKey: document.ProductKey,
		TemplateKey: document.TemplateKey, Input: input, Assets: assets,
		AllowedUserInputs:   append([]string(nil), document.AllowedUserInputs...),
		PromptUserInputMode: creations.PromptUserInputMode(document.PromptUserInputMode),
	}).Normalize()
	if err != nil {
		return creations.PublishedB2BProductRecipe{}, creations.ErrB2BProductRecipeUnavailable
	}
	return recipe, nil
}

var _ creations.B2BProductRecipeReader = (*mongoB2BProductRecipeRepository)(nil)
