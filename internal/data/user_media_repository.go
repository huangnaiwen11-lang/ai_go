package data

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai-business-service/internal/biz/media"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const (
	userMediaOwnerType = "user"
	userMediaAssetKind = "source_image"
	userMediaAvailable = "available"
	localMediaHost     = "local-media.invalid"
)

// mongoLocalUserMediaRepository 把用户图片元数据写入 Go 自有 Mongo，并将二进制
// 放到可配置的本地运行目录。目录实现仅用于本机联调；替换为 R2/S3 时保持媒体领域接口不变。
type mongoLocalUserMediaRepository struct {
	assets     *mongo.Collection
	storageDir string
}

// NewLocalUserMediaRepository 仅装配本地运行目录，不读取 Node 上传、R2 或生产配置。
func NewLocalUserMediaRepository(data *Data, storageDir string) *mongoLocalUserMediaRepository {
	if data == nil || data.database == nil {
		return &mongoLocalUserMediaRepository{}
	}
	return &mongoLocalUserMediaRepository{
		assets:     data.database.Collection(schema.CollectionAssets),
		storageDir: strings.TrimSpace(storageDir),
	}
}

func (repository *mongoLocalUserMediaRepository) SaveImage(ctx context.Context, command media.SaveImageCommand) (*media.Image, error) {
	if repository == nil || repository.assets == nil || repository.storageDir == "" {
		return nil, errors.New("local user media repository is not configured")
	}
	if err := os.MkdirAll(repository.storageDir, 0o700); err != nil {
		return nil, fmt.Errorf("create local media directory: %w", err)
	}

	imageID := uuid.NewString()
	filePath := filepath.Join(repository.storageDir, imageID+imageExtension(command.ContentType))
	// 临时文件与最终文件在同一目录，rename 对本地文件系统是原子操作，避免读取到半张图片。
	temporaryPath := filePath + ".uploading-" + uuid.NewString()
	if err := os.WriteFile(temporaryPath, command.Content, 0o600); err != nil {
		return nil, fmt.Errorf("write local media content: %w", err)
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("commit local media content: %w", err)
	}

	document := model.AssetDocument{
		ID: "user_media:" + imageID, OwnerType: userMediaOwnerType, OwnerID: command.OwnerID,
		AssetKind: userMediaAssetKind, StorageKey: filePath, Status: userMediaAvailable,
		ContentType: command.ContentType, ByteSize: int64(len(command.Content)), CreatedAt: time.Now().UTC(),
	}
	if _, err := repository.assets.InsertOne(ctx, document); err != nil {
		_ = os.Remove(filePath)
		return nil, fmt.Errorf("insert user media metadata: %w", err)
	}
	return toUserMedia(document, imageID), nil
}

func (repository *mongoLocalUserMediaRepository) ResolveOwnedImage(ctx context.Context, ownerID, reference string) (string, error) {
	imageID, ok := parseUserMediaReference(reference)
	if !ok {
		return "", media.ErrImageAccessDenied
	}
	document, err := repository.findOwnedImage(ctx, ownerID, imageID)
	if err != nil || document.StorageKey == "" {
		return "", media.ErrImageAccessDenied
	}
	return userMediaReference(imageID), nil
}

func (repository *mongoLocalUserMediaRepository) OpenOwnedImage(ctx context.Context, ownerID, imageID string) (*media.Image, []byte, error) {
	parsed, err := uuid.Parse(imageID)
	if err != nil || parsed.String() != imageID {
		return nil, nil, media.ErrImageAccessDenied
	}
	document, err := repository.findOwnedImage(ctx, ownerID, imageID)
	if err != nil {
		return nil, nil, media.ErrImageAccessDenied
	}
	content, err := os.ReadFile(document.StorageKey)
	if err != nil || len(content) == 0 {
		return nil, nil, media.ErrImageAccessDenied
	}
	return toUserMedia(document, imageID), content, nil
}

func (repository *mongoLocalUserMediaRepository) findOwnedImage(ctx context.Context, ownerID, imageID string) (model.AssetDocument, error) {
	if repository == nil || repository.assets == nil || strings.TrimSpace(ownerID) == "" {
		return model.AssetDocument{}, media.ErrImageAccessDenied
	}
	var document model.AssetDocument
	err := repository.assets.FindOne(ctx, bson.D{
		{Key: "_id", Value: "user_media:" + imageID}, {Key: "owner_type", Value: userMediaOwnerType},
		{Key: "owner_id", Value: ownerID}, {Key: "asset_kind", Value: userMediaAssetKind}, {Key: "status", Value: userMediaAvailable},
	}).Decode(&document)
	if err != nil {
		return model.AssetDocument{}, err
	}
	return document, nil
}

func toUserMedia(document model.AssetDocument, imageID string) *media.Image {
	return &media.Image{ID: imageID, OwnerID: document.OwnerID, Reference: userMediaReference(imageID), ContentType: document.ContentType, SizeBytes: document.ByteSize}
}

func userMediaReference(imageID string) string {
	return "https://" + localMediaHost + "/assets/" + imageID
}

func parseUserMediaReference(reference string) (string, bool) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "https" || parsed.Host != localMediaHost || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	identifier := strings.TrimPrefix(parsed.EscapedPath(), "/assets/")
	if identifier == parsed.EscapedPath() {
		return "", false
	}
	parsedID, err := uuid.Parse(identifier)
	return identifier, err == nil && parsedID.String() == identifier
}

func imageExtension(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

var _ media.Repository = (*mongoLocalUserMediaRepository)(nil)
