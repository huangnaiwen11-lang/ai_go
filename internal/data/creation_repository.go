package data

import (
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoCreationRepository struct {
	creations *mongo.Collection
	steps     *mongo.Collection
	recipes   *mongo.Collection
}

type mongoDeferredRecipeWriter struct {
	recipes *mongo.Collection
}

// NewCreationRepository 返回创作占位及步骤仓储，不向业务层暴露 MongoDB 细节。
func NewCreationRepository(data *Data) creations.Repository {
	if data == nil || data.database == nil {
		return &mongoCreationRepository{}
	}
	return &mongoCreationRepository{
		creations: data.database.Collection(schema.CollectionCreations),
		steps:     data.database.Collection(schema.CollectionCreationSteps),
		recipes:   data.database.Collection(schema.CollectionGenerationStepRecipes),
	}
}

// DeferredRecipeWriter 返回与创作仓储共享数据源的窄配方写入器。
func (repository *mongoCreationRepository) DeferredRecipeWriter() creations.DeferredRecipeWriter {
	return &mongoDeferredRecipeWriter{recipes: repository.recipes}
}

// Create 在调用方事务中写入第二步图生视频冻结配方。
func (writer *mongoDeferredRecipeWriter) Create(ctx context.Context, recipe *creations.DeferredRecipe) error {
	if writer == nil || writer.recipes == nil {
		return errors.New("deferred recipe collection is required")
	}
	if recipe == nil || recipe.StepID == "" || recipe.CreationID == "" || recipe.Atom != creations.AtomImageToVideo || recipe.Digest == "" || recipe.Status == "" || recipe.CreatedAt.IsZero() || recipe.UpdatedAt.IsZero() {
		return errors.New("deferred recipe is invalid")
	}
	document := model.DeferredRecipeDocument{
		ID:         recipe.StepID,
		StepID:     recipe.StepID,
		CreationID: recipe.CreationID,
		Atom:       string(recipe.Atom),
		Digest:     recipe.Digest,
		Status:     string(recipe.Status),
		CreatedAt:  recipe.CreatedAt.UTC(),
		UpdatedAt:  recipe.UpdatedAt.UTC(),
	}
	switch recipe.Protocol {
	case creations.DeferredRecipeProtocolExecutionV2:
		if recipe.B2B != nil || recipe.ModelSKU == "" || len(recipe.InputTemplate) == 0 {
			return errors.New("deferred execution.v2 recipe is invalid")
		}
		document.Protocol = string(creations.DeferredRecipeProtocolExecutionV2)
		document.ModelSKU = recipe.ModelSKU
		document.InputTemplate = append([]byte(nil), recipe.InputTemplate...)
	case creations.DeferredRecipeProtocolB2B:
		if recipe.ModelSKU != "" || len(recipe.InputTemplate) != 0 || recipe.B2B == nil {
			return errors.New("deferred B2B recipe is invalid")
		}
		normalized, err := recipe.B2B.Normalize()
		if err != nil || normalized.Digest != recipe.Digest {
			return errors.New("deferred B2B recipe is invalid")
		}
		payload, err := normalized.Recipe.Marshal()
		if err != nil {
			return errors.New("deferred B2B recipe is invalid")
		}
		document.Protocol = string(creations.DeferredRecipeProtocolB2B)
		document.B2BRecipe = payload
	default:
		return errors.New("deferred recipe protocol is invalid")
	}
	_, err := writer.recipes.InsertOne(ctx, document)
	if err != nil {
		return fmt.Errorf("create deferred recipe: %w", err)
	}
	return nil
}

// FindByIdempotencyKey 按全局幂等键查找创作；缺失记录是正常的未命中。
func (repository *mongoCreationRepository) FindByIdempotencyKey(ctx context.Context, key string) (*creations.Creation, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	var document model.CreationDocument
	err := repository.creations.FindOne(ctx, bson.D{{Key: "idempotency_key", Value: key}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find creation by idempotency key %q: %w", key, err)
	}
	return toBizCreation(document), nil
}

// ListSteps 按服务端计划序号升序读取创作内部步骤。
func (repository *mongoCreationRepository) ListSteps(ctx context.Context, creationID string) ([]creations.CreationStep, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	cursor, err := repository.steps.Find(
		ctx,
		bson.D{{Key: "creation_id", Value: creationID}},
		options.Find().SetSort(bson.D{{Key: "sequence", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("list creation steps for creation %q: %w", creationID, err)
	}
	defer cursor.Close(ctx)

	var documents []model.CreationStepDocument
	if err := cursor.All(ctx, &documents); err != nil {
		return nil, fmt.Errorf("decode creation steps for creation %q: %w", creationID, err)
	}
	steps := make([]creations.CreationStep, 0, len(documents))
	for _, document := range documents {
		step, err := toBizCreationStep(document)
		if err != nil {
			return nil, fmt.Errorf("decode execution route for creation step %q: %w", document.ID, err)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// Create 在调用方提供的上下文中依次写入创作占位和全部步骤。
// 调用方负责传入同一 MongoDB 事务上下文，使其与额度、余额、预留和账本分录原子提交。
func (repository *mongoCreationRepository) Create(ctx context.Context, creation *creations.Creation, steps []creations.CreationStep) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if creation == nil {
		return errors.New("creation is required")
	}
	if len(steps) == 0 {
		return errors.New("creation steps are required")
	}
	if _, err := repository.creations.InsertOne(ctx, newCreationDocument(creation)); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("insert creation %q: %w; %w", creation.ID, creations.ErrCreationAlreadyExists, err)
		}
		return fmt.Errorf("insert creation %q: %w", creation.ID, err)
	}

	documents := make([]any, 0, len(steps))
	for _, step := range steps {
		document, err := newCreationStepDocument(step)
		if err != nil {
			return fmt.Errorf("invalid execution route for creation step %q: %w", step.ID, err)
		}
		documents = append(documents, document)
	}
	if _, err := repository.steps.InsertMany(ctx, documents); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("insert creation steps for creation %q: %w; %w", creation.ID, creations.ErrCreationAlreadyExists, err)
		}
		return fmt.Errorf("insert creation steps for creation %q: %w", creation.ID, err)
	}
	return nil
}

func (repository *mongoCreationRepository) ready() error {
	if repository == nil || repository.creations == nil || repository.steps == nil {
		return errors.New("creation repository is not configured")
	}
	return nil
}

func newCreationDocument(creation *creations.Creation) model.CreationDocument {
	return model.CreationDocument{
		ID:                   creation.ID,
		IdempotencyKey:       creation.IdempotencyKey,
		UserID:               creation.UserID,
		TemplateID:           creation.TemplateID,
		TemplateVersion:      creation.TemplateVersion,
		ProductOutput:        string(creation.Output),
		VideoDurationSeconds: creation.VideoDurationSeconds,
		RequestFingerprint:   creation.RequestFingerprint,
		Status:               string(creation.Status),
		Version:              creation.Version,
		CreatedAt:            creation.CreatedAt,
		UpdatedAt:            creation.UpdatedAt,
	}
}

func newCreationStepDocument(step creations.CreationStep) (model.CreationStepDocument, error) {
	route, err := creations.NormalizeExecutionRoute(step.Route)
	if err != nil {
		return model.CreationStepDocument{}, err
	}
	return model.CreationStepDocument{
		ID:              step.ID,
		CreationID:      step.CreationID,
		Sequence:        step.Sequence,
		Atom:            string(step.Atom),
		Provider:        route.Provider,
		AccountRef:      route.AccountRef,
		ContractVersion: route.ContractVersion,
		MappingVersion:  route.MappingVersion,
		SubmitStatus:    string(step.SubmitStatus),
		CallbackVersion: step.CallbackVersion,
		CreatedAt:       step.CreatedAt,
	}, nil
}

func toBizCreation(document model.CreationDocument) *creations.Creation {
	return &creations.Creation{
		ID:                   document.ID,
		IdempotencyKey:       document.IdempotencyKey,
		UserID:               document.UserID,
		TemplateID:           document.TemplateID,
		TemplateVersion:      document.TemplateVersion,
		Output:               entitlement.ProductOutput(document.ProductOutput),
		VideoDurationSeconds: document.VideoDurationSeconds,
		RequestFingerprint:   document.RequestFingerprint,
		Status:               creations.CreationStatus(document.Status),
		Version:              document.Version,
		CreatedAt:            document.CreatedAt,
		UpdatedAt:            document.UpdatedAt,
	}
}

func toBizCreationStep(document model.CreationStepDocument) (creations.CreationStep, error) {
	route, err := creations.NormalizeExecutionRoute(creations.ExecutionRoute{
		Provider: document.Provider, AccountRef: document.AccountRef,
		ContractVersion: document.ContractVersion, MappingVersion: document.MappingVersion,
	})
	if err != nil {
		return creations.CreationStep{}, err
	}
	return creations.CreationStep{
		ID:              document.ID,
		CreationID:      document.CreationID,
		Sequence:        document.Sequence,
		Atom:            creations.StepAtom(document.Atom),
		Route:           route,
		SubmitStatus:    creations.StepSubmitStatus(document.SubmitStatus),
		CallbackVersion: document.CallbackVersion,
		CreatedAt:       document.CreatedAt,
	}, nil
}

var _ creations.Repository = (*mongoCreationRepository)(nil)
