package video_test

import (
	"context"
	"errors"
	"testing"

	"ai-business-service/internal/biz/creations"
	bizvideo "ai-business-service/internal/biz/video"
)

// 视频状态只投影用户可见字段，并且必须从最终可信视频资产返回完成结果。
func TestVideoStatusUsecase投影创作生命周期(t *testing.T) {
	ids := []string{
		"550e8400-e29b-41d4-a716-446655440101",
		"550e8400-e29b-41d4-a716-446655440102",
		"550e8400-e29b-41d4-a716-446655440103",
		"550e8400-e29b-41d4-a716-446655440104",
		"550e8400-e29b-41d4-a716-446655440105",
	}
	usecase := bizvideo.NewStatusUsecase(staticVideoStatusReader{records: []bizvideo.OwnedVideoCreation{
		{ID: ids[0], Status: creations.CreationStatusPendingSubmission, DurationSeconds: 5},
		{ID: ids[1], Status: creations.CreationStatusSucceeded, DurationSeconds: 10, ResultURL: "https://assets.example.test/final.mp4"},
		{ID: ids[2], Status: creations.CreationStatusSubmissionFailed, DurationSeconds: 15},
		{ID: ids[3], Status: creations.CreationStatusGenerationFailed, DurationSeconds: 5},
		{ID: ids[4], Status: creations.CreationStatusConfiscated, DurationSeconds: 10},
	}})

	views, err := usecase.List(context.Background(), "user-1", ids)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(views) != len(ids) {
		t.Fatalf("状态数 = %d, want %d", len(views), len(ids))
	}
	if views[0].Status != bizvideo.GenerationStatusGenerating || views[0].DurationSeconds != 5 {
		t.Fatalf("生成中投影 = %#v", views[0])
	}
	if views[1].Status != bizvideo.GenerationStatusCompleted || views[1].VideoURL != "https://assets.example.test/final.mp4" || views[1].DurationSeconds != 10 {
		t.Fatalf("成功投影 = %#v", views[1])
	}
	if views[2].ErrorCode != "GENERATION_SUBMISSION_FAILED" || views[3].ErrorCode != "GENERATION_FAILED" || views[4].ErrorCode != "CONTENT_CONFISCATED" {
		t.Fatalf("失败投影 = %#v / %#v / %#v", views[2], views[3], views[4])
	}
}

// 成功但缺少最终可信资产是数据不一致，绝不能对外伪造 completed。
func TestVideoStatusUsecase拒绝缺最终资产的成功任务(t *testing.T) {
	usecase := bizvideo.NewStatusUsecase(staticVideoStatusReader{records: []bizvideo.OwnedVideoCreation{{
		ID: "550e8400-e29b-41d4-a716-446655440106", Status: creations.CreationStatusSucceeded, DurationSeconds: 5,
	}}})
	_, err := usecase.Get(context.Background(), "user-1", "550e8400-e29b-41d4-a716-446655440106")
	if err == nil || errors.Is(err, bizvideo.ErrVideoNotFound) {
		t.Fatalf("Get() error = %v，期望成功任务缺资产的数据不一致错误", err)
	}
}

// 单条未命中不区分不存在和非本人，防止任务枚举。
func TestVideoStatusUsecase单条未命中返回统一未找到(t *testing.T) {
	usecase := bizvideo.NewStatusUsecase(staticVideoStatusReader{})
	_, err := usecase.Get(context.Background(), "user-1", "550e8400-e29b-41d4-a716-446655440107")
	if !errors.Is(err, bizvideo.ErrVideoNotFound) {
		t.Fatalf("Get() error = %v，期望 ErrVideoNotFound", err)
	}
}

type staticVideoStatusReader struct {
	records []bizvideo.OwnedVideoCreation
	err     error
}

func (reader staticVideoStatusReader) FindOwnedVideos(context.Context, string, []string) ([]bizvideo.OwnedVideoCreation, error) {
	return reader.records, reader.err
}
