package data

import (
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/works"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoWorksRepository 只读取 Go 自有创作、步骤和结果资产集合。
// 它不读取旧 Node 作品、技术配方、外部任务号或支付集合。
type mongoWorksRepository struct {
	creations *mongo.Collection
	steps     *mongo.Collection
	assets    *mongo.Collection
}

// NewWorksRepository 创建用户作品历史的 MongoDB 只读仓储。
func NewWorksRepository(data *Data) works.Repository {
	if data == nil || data.database == nil {
		return &mongoWorksRepository{}
	}
	return &mongoWorksRepository{
		creations: data.database.Collection(schema.CollectionCreations),
		steps:     data.database.Collection(schema.CollectionCreationSteps),
		assets:    data.database.Collection(schema.CollectionAssets),
	}
}

// List 先按当前用户过滤；Kind 非空时再按产品类型过滤，随后以创建时间与作品 ID 复合倒序分页。
// user_id 必须是首个过滤条件，防止游标条件或类型条件扩大到其他用户的作品。
func (repository *mongoWorksRepository) List(ctx context.Context, query works.ListQuery) (*works.Page, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}

	filter := bson.D{{Key: "user_id", Value: query.UserID}}
	if query.Kind != "" {
		filter = append(filter, bson.E{Key: "product_output", Value: string(query.Kind)})
	}
	findOptions := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(query.Limit + 1))
	if query.Cursor != "" {
		position, err := works.ParseCursor(query.Cursor)
		if err != nil {
			return nil, err
		}
		filter = append(filter, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "created_at", Value: bson.D{{Key: "$lt", Value: position.CreatedAt}}}},
			bson.D{{Key: "created_at", Value: position.CreatedAt}, {Key: "_id", Value: bson.D{{Key: "$lt", Value: position.WorkID}}}},
		}})
	} else if query.Skip > 0 {
		findOptions.SetSkip(int64(query.Skip))
	}

	cursor, err := repository.creations.Find(ctx, filter, findOptions)
	if err != nil {
		return nil, fmt.Errorf("find owned works for user %q: %w", query.UserID, err)
	}
	defer cursor.Close(ctx)

	documents := make([]model.CreationDocument, 0, query.Limit+1)
	for cursor.Next(ctx) {
		var document model.CreationDocument
		if err := cursor.Decode(&document); err != nil {
			return nil, fmt.Errorf("decode owned work for user %q: %w", query.UserID, err)
		}
		documents = append(documents, document)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate owned works for user %q: %w", query.UserID, err)
	}

	hasMore := len(documents) > query.Limit
	if hasMore {
		documents = documents[:query.Limit]
	}
	page := &works.Page{Items: make([]works.Work, 0, len(documents))}
	for _, document := range documents {
		work, err := repository.toWork(ctx, document)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *work)
	}
	if hasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		nextCursor, err := works.EncodeCursor(last.CreatedAt, last.ID)
		if err != nil {
			return nil, fmt.Errorf("encode owned works cursor for user %q: %w", query.UserID, err)
		}
		page.NextCursor = nextCursor
	}
	return page, nil
}

// FindByID 以当前用户和作品 ID 同时查询。任何未命中都统一为 404 语义，
// 从而不会让用户通过详情接口探测其他用户作品是否存在。
func (repository *mongoWorksRepository) FindByID(ctx context.Context, userID, id string) (*works.Work, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	var document model.CreationDocument
	err := repository.creations.FindOne(ctx, bson.D{{Key: "user_id", Value: userID}, {Key: "_id", Value: id}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, works.ErrWorkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find owned work %q for user %q: %w", id, userID, err)
	}
	return repository.toWork(ctx, document)
}

func (repository *mongoWorksRepository) toWork(ctx context.Context, document model.CreationDocument) (*works.Work, error) {
	kind := works.Kind(document.ProductOutput)
	if kind != works.KindImage && kind != works.KindVideo {
		return nil, fmt.Errorf("owned work %q has unsupported product output", document.ID)
	}
	work := &works.Work{
		ID:              document.ID,
		Kind:            kind,
		Status:          document.Status,
		TemplateID:      document.TemplateID,
		TemplateVersion: document.TemplateVersion,
		DurationSeconds: document.VideoDurationSeconds,
		CreatedAt:       document.CreatedAt.UTC(),
		UpdatedAt:       document.UpdatedAt.UTC(),
		Error:           safeUserError(creations.CreationStatus(document.Status)),
	}
	if creations.CreationStatus(document.Status) != creations.CreationStatusSucceeded {
		return work, nil
	}
	resultURL, err := repository.finalResultURL(ctx, document.ID)
	if err != nil {
		return nil, err
	}
	work.ResultURL = resultURL
	return work, nil
}

// finalResultURL 只能从最终步骤关联的可用 result 资产读取，禁止拼接、猜测或回退到中间资产。
func (repository *mongoWorksRepository) finalResultURL(ctx context.Context, creationID string) (string, error) {
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{{Key: "creation_id", Value: creationID}}, options.FindOne().SetSort(bson.D{{Key: "sequence", Value: -1}})).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", works.ErrResultUnavailable
	}
	if err != nil {
		return "", fmt.Errorf("find final work step: %w", err)
	}
	var asset model.AssetDocument
	err = repository.assets.FindOne(ctx, bson.D{
		{Key: "owner_type", Value: creationStepAssetOwnerType},
		{Key: "owner_id", Value: step.ID},
		{Key: "asset_kind", Value: creationStepAssetKind},
		{Key: "status", Value: creationStepAssetAvailable},
	}, options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})).Decode(&asset)
	if errors.Is(err, mongo.ErrNoDocuments) || asset.StorageKey == "" {
		return "", works.ErrResultUnavailable
	}
	if err != nil {
		return "", fmt.Errorf("find final work result asset: %w", err)
	}
	return asset.StorageKey, nil
}

func safeUserError(status creations.CreationStatus) string {
	switch status {
	case creations.CreationStatusSubmissionFailed:
		return "提交生成失败，已自动退还本次预扣"
	case creations.CreationStatusGenerationFailed:
		return "生成失败，已自动退还本次预扣"
	case creations.CreationStatusConfiscated:
		return "作品未通过审核"
	default:
		return ""
	}
}

func (repository *mongoWorksRepository) ready() error {
	if repository == nil || repository.creations == nil || repository.steps == nil || repository.assets == nil {
		return errors.New("works repository is not configured")
	}
	return nil
}

var _ works.Repository = (*mongoWorksRepository)(nil)
