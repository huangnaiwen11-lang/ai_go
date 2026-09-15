package video

import (
	"context"
	"errors"

	"ai-business-service/internal/biz/creations"

	"github.com/google/uuid"
)

const maxVideoStatusIDs = 20

var (
	// ErrVideoNotFound 统一表示视频创作不存在或不属于当前用户。
	ErrVideoNotFound = errors.New("video: task not found")
	// ErrInvalidVideoStatusRequest 表示状态查询未满足 UUID、去重或数量约束。
	ErrInvalidVideoStatusRequest = errors.New("video: invalid status request")
)

// GenerationStatus 是面向现网视频轮询的稳定状态枚举。
type GenerationStatus string

const (
	// GenerationStatusGenerating 表示视频尚未产生最终可信资产。
	GenerationStatusGenerating GenerationStatus = "generating"
	// GenerationStatusCompleted 表示最终图生视频步骤已回调成功。
	GenerationStatusCompleted GenerationStatus = "completed"
	// GenerationStatusFailed 表示提交、生成或审核已进入失败终态。
	GenerationStatusFailed GenerationStatus = "failed"
)

// OwnedVideoCreation 是数据层完成归属与产品输出隔离后返回的最小状态事实。
// ResultURL 只能来自最后一个步骤的 available/result 视频资产。
type OwnedVideoCreation struct {
	ID              string
	Status          creations.CreationStatus
	DurationSeconds int32
	ResultURL       string
}

// VideoStatusReader 是视频创作状态的只读边界。
// 实现必须让 userID 与 product_output=video 同时参与 Mongo 查询。
type VideoStatusReader interface {
	FindOwnedVideos(context.Context, string, []string) ([]OwnedVideoCreation, error)
}

// VideoView 是 transport 层编码视频轮询响应所需的安全投影。
type VideoView struct {
	TaskID          string
	Status          GenerationStatus
	DurationSeconds int32
	VideoURL        string
	ErrorCode       string
	ErrorMessage    string
}

// StatusUsecase 负责把内部创作生命周期投影成视频轮询状态。
type StatusUsecase struct {
	videos VideoStatusReader
}

// NewStatusUsecase 创建视频状态读取用例。
func NewStatusUsecase(videos VideoStatusReader) *StatusUsecase {
	return &StatusUsecase{videos: videos}
}

// List 按请求顺序返回当前用户拥有的视频状态；未命中与非本人任务均不返回。
func (usecase *StatusUsecase) List(ctx context.Context, userID string, ids []string) ([]VideoView, error) {
	if usecase == nil || usecase.videos == nil || userID == "" || !validVideoStatusIDs(ids) {
		return nil, ErrInvalidVideoStatusRequest
	}
	records, err := usecase.videos.FindOwnedVideos(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]OwnedVideoCreation, len(records))
	for _, record := range records {
		byID[record.ID] = record
	}
	views := make([]VideoView, 0, len(records))
	for _, id := range ids {
		record, found := byID[id]
		if !found {
			continue
		}
		view, err := projectVideoStatus(record)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// Get 返回单条视频状态；不存在与非本人统一返回 ErrVideoNotFound。
func (usecase *StatusUsecase) Get(ctx context.Context, userID, id string) (*VideoView, error) {
	views, err := usecase.List(ctx, userID, []string{id})
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, ErrVideoNotFound
	}
	return &views[0], nil
}

func projectVideoStatus(record OwnedVideoCreation) (VideoView, error) {
	if !validVideoDuration(record.DurationSeconds) {
		return VideoView{}, errors.New("video: creation has invalid duration")
	}
	view := VideoView{TaskID: record.ID, DurationSeconds: record.DurationSeconds}
	switch record.Status {
	case creations.CreationStatusPendingSubmission:
		view.Status = GenerationStatusGenerating
	case creations.CreationStatusSucceeded:
		if record.ResultURL == "" {
			// 成功记录没有最终可信资产是数据不一致，不能向客户端伪造 completed。
			return VideoView{}, errors.New("video: succeeded creation has no result asset")
		}
		view.Status = GenerationStatusCompleted
		view.VideoURL = record.ResultURL
	case creations.CreationStatusSubmissionFailed:
		view.Status = GenerationStatusFailed
		view.ErrorCode = "GENERATION_SUBMISSION_FAILED"
		view.ErrorMessage = "生成服务暂不可用，请稍后重试"
	case creations.CreationStatusGenerationFailed:
		view.Status = GenerationStatusFailed
		view.ErrorCode = "GENERATION_FAILED"
		view.ErrorMessage = "生成失败，请重试"
	case creations.CreationStatusConfiscated:
		view.Status = GenerationStatusFailed
		view.ErrorCode = "CONTENT_CONFISCATED"
		view.ErrorMessage = "内容审核未通过"
	default:
		return VideoView{}, errors.New("video: unsupported creation status")
	}
	return view, nil
}

func validVideoStatusIDs(ids []string) bool {
	if len(ids) == 0 || len(ids) > maxVideoStatusIDs {
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
