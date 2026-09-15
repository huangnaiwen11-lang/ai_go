// Package media 定义用户上传图片的领域规则。
// 文件如何落盘或迁移至对象存储由 data 层实现；业务层只关心素材归属与可用性。
package media

import (
	"context"
	"errors"
	"strings"
)

const MaxImageBytes = 10 << 20

var (
	// ErrInvalidImage 表示文件不符合公开图片素材合同。
	ErrInvalidImage = errors.New("media: invalid image")
	// ErrImageAccessDenied 表示素材不存在、不可用或不属于当前用户。
	// 对外统一处理，避免通过错误细节枚举其他用户的素材。
	ErrImageAccessDenied = errors.New("media: image access denied")
)

// Image 是已确认可用于创作的用户图片领域对象。
type Image struct {
	ID          string
	OwnerID     string
	Reference   string
	ContentType string
	SizeBytes   int64
}

// SaveImageCommand 是传输层完成 MIME 检测后的受控图片内容。
type SaveImageCommand struct {
	OwnerID     string
	Filename    string
	ContentType string
	Content     []byte
}

// Repository 是媒体持久化与引用解析的反转边界。
// 实现可以从本地运行目录无缝替换为 R2/S3，不影响 I2I 的领域合同。
type Repository interface {
	SaveImage(context.Context, SaveImageCommand) (*Image, error)
	ResolveOwnedImage(context.Context, string, string) (string, error)
	OpenOwnedImage(context.Context, string, string) (*Image, []byte, error)
}

// Usecase 统一收口上传后入库与 I2I 前的素材归属确认。
type Usecase struct{ repository Repository }

func NewUsecase(repository Repository) *Usecase {
	return &Usecase{repository: repository}
}

func (usecase *Usecase) SaveImage(ctx context.Context, command SaveImageCommand) (*Image, error) {
	if usecase == nil || usecase.repository == nil || strings.TrimSpace(command.OwnerID) == "" || strings.TrimSpace(command.Filename) == "" || !isAllowedImageContentType(command.ContentType) || len(command.Content) == 0 || len(command.Content) > MaxImageBytes {
		return nil, ErrInvalidImage
	}
	image, err := usecase.repository.SaveImage(ctx, command)
	if err != nil || image == nil || image.ID == "" || image.OwnerID != command.OwnerID || image.Reference == "" {
		return nil, ErrInvalidImage
	}
	return image, nil
}

// ResolveOwnedImage 实现 t2i.OwnedImageReader，确保模板编辑在预扣前完成授权判断。
func (usecase *Usecase) ResolveOwnedImage(ctx context.Context, ownerID, reference string) (string, error) {
	if usecase == nil || usecase.repository == nil || strings.TrimSpace(ownerID) == "" || strings.TrimSpace(reference) == "" {
		return "", ErrImageAccessDenied
	}
	resolved, err := usecase.repository.ResolveOwnedImage(ctx, ownerID, reference)
	if err != nil || strings.TrimSpace(resolved) == "" {
		return "", ErrImageAccessDenied
	}
	return resolved, nil
}

// OpenOwnedImage 仅供上传后的当前用户预览或后续受控读取使用，不能作为公开静态资源。
func (usecase *Usecase) OpenOwnedImage(ctx context.Context, ownerID, imageID string) (*Image, []byte, error) {
	if usecase == nil || usecase.repository == nil || strings.TrimSpace(ownerID) == "" || strings.TrimSpace(imageID) == "" {
		return nil, nil, ErrImageAccessDenied
	}
	image, content, err := usecase.repository.OpenOwnedImage(ctx, ownerID, imageID)
	if err != nil || image == nil || image.OwnerID != ownerID || len(content) == 0 {
		return nil, nil, ErrImageAccessDenied
	}
	return image, content, nil
}

func isAllowedImageContentType(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}
