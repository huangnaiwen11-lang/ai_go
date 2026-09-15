package t2i

import (
	"context"
	"errors"

	"ai-business-service/internal/biz/creations"

	"github.com/google/uuid"
)

const maxImageStatusIDs = 20

var (
	// ErrImageNotFound 统一表示创作不存在或不属于当前用户。
	ErrImageNotFound = errors.New("t2i: image not found")
	// ErrInvalidImageStatusRequest 表示批量图片状态请求不满足 UUID、去重或数量限制。
	ErrInvalidImageStatusRequest = errors.New("t2i: invalid image status request")
)

// GenerationStatus 是面向现有前端图片轮询的稳定状态枚举。
type GenerationStatus string

const (
	// GenerationStatusGenerating 表示任务尚未产生最终图片。
	GenerationStatusGenerating GenerationStatus = "generating"
	// GenerationStatusCompleted 表示已收到可信回调且最终图片资产可用。
	GenerationStatusCompleted GenerationStatus = "completed"
	// GenerationStatusFailed 表示提交、生成或审核终态失败。
	GenerationStatusFailed GenerationStatus = "failed"
)

// OwnedImageCreation 是数据层已完成归属约束后返回的最小状态事实。
// ResultURL 只能来自最终步骤的已验证可用结果资产。
type OwnedImageCreation struct {
	ID        string
	Status    creations.CreationStatus
	ResultURL string
}

// ImageStatusReader 是创作图片状态的只读边界。
// 实现必须以 userID 参与查询，不能先按 ID 查出记录后再由业务层过滤归属。
type ImageStatusReader interface {
	FindOwnedImages(context.Context, string, []string) ([]OwnedImageCreation, error)
}

// ImageView 是 transport 层编码现网图片状态字段所需的安全投影。
type ImageView struct {
	ID                     string
	ImageURL               string
	GenerationStatus       GenerationStatus
	GenerationErrorCode    string
	GenerationErrorMessage string
	GenerationErrorDetail  any
}

// StatusUsecase 负责把 Go 创作状态投影为图片轮询视图。
type StatusUsecase struct {
	images ImageStatusReader
}

// NewStatusUsecase 创建图片状态读取用例。
func NewStatusUsecase(images ImageStatusReader) *StatusUsecase {
	return &StatusUsecase{images: images}
}

// List 按请求顺序返回当前用户拥有的图片状态；不存在和非本人的 ID 不返回条目。
func (usecase *StatusUsecase) List(ctx context.Context, userID string, ids []string) ([]ImageView, error) {
	if usecase == nil || usecase.images == nil || userID == "" || !validImageStatusIDs(ids) {
		return nil, ErrInvalidImageStatusRequest
	}
	records, err := usecase.images.FindOwnedImages(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]OwnedImageCreation, len(records))
	for _, record := range records {
		// 数据层若意外返回非请求 ID，必须忽略，不能扩展用户可见集合。
		byID[record.ID] = record
	}
	views := make([]ImageView, 0, len(records))
	for _, id := range ids {
		record, found := byID[id]
		if !found {
			continue
		}
		view, err := projectImageStatus(record)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// Get 返回单条图片状态；不区分不存在和非本人，统一返回 ErrImageNotFound。
func (usecase *StatusUsecase) Get(ctx context.Context, userID, id string) (*ImageView, error) {
	views, err := usecase.List(ctx, userID, []string{id})
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, ErrImageNotFound
	}
	return &views[0], nil
}

func projectImageStatus(record OwnedImageCreation) (ImageView, error) {
	view := ImageView{ID: record.ID}
	switch record.Status {
	case creations.CreationStatusPendingSubmission:
		view.GenerationStatus = GenerationStatusGenerating
	case creations.CreationStatusSucceeded:
		if record.ResultURL == "" {
			// 成功但没有可信最终资产是数据不一致，不能伪造一个 completed 空结果。
			return ImageView{}, errors.New("t2i: succeeded creation has no result asset")
		}
		view.GenerationStatus = GenerationStatusCompleted
		view.ImageURL = record.ResultURL
	case creations.CreationStatusSubmissionFailed:
		view.GenerationStatus = GenerationStatusFailed
		view.GenerationErrorCode = "GENERATION_SUBMISSION_FAILED"
		view.GenerationErrorMessage = "生成服务暂不可用，请稍后重试"
	case creations.CreationStatusGenerationFailed:
		view.GenerationStatus = GenerationStatusFailed
		view.GenerationErrorCode = "GENERATION_FAILED"
		view.GenerationErrorMessage = "生成失败，请重试"
	case creations.CreationStatusConfiscated:
		view.GenerationStatus = GenerationStatusFailed
		view.GenerationErrorCode = "CONTENT_CONFISCATED"
		view.GenerationErrorMessage = "内容审核未通过"
	default:
		return ImageView{}, errors.New("t2i: unsupported creation status")
	}
	return view, nil
}

func validImageStatusIDs(ids []string) bool {
	if len(ids) == 0 || len(ids) > maxImageStatusIDs {
		return false
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id {
			return false
		}
		if _, exists := seen[id]; exists {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}
