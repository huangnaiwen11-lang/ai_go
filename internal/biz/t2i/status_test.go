package t2i

import (
	"context"
	"testing"

	"ai-business-service/internal/biz/creations"
)

// 图片状态投影必须保持现网轮询字段，同时只从可信结果资产返回成功图片地址。
func TestStatusUsecase投影创作生命周期(t *testing.T) {
	ids := []string{
		"550e8400-e29b-41d4-a716-446655440001",
		"550e8400-e29b-41d4-a716-446655440002",
		"550e8400-e29b-41d4-a716-446655440003",
		"550e8400-e29b-41d4-a716-446655440004",
		"550e8400-e29b-41d4-a716-446655440005",
	}
	usecase := NewStatusUsecase(staticImageStatusReader{records: []OwnedImageCreation{
		{ID: ids[0], Status: creations.CreationStatusPendingSubmission},
		{ID: ids[1], Status: creations.CreationStatusSucceeded, ResultURL: "https://assets.example.test/final.png"},
		{ID: ids[2], Status: creations.CreationStatusSubmissionFailed},
		{ID: ids[3], Status: creations.CreationStatusGenerationFailed},
		{ID: ids[4], Status: creations.CreationStatusConfiscated},
	}})

	views, err := usecase.List(context.Background(), "user-1", ids)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(views) != len(ids) {
		t.Fatalf("状态数 = %d, want %d", len(views), len(ids))
	}
	if views[0].GenerationStatus != GenerationStatusGenerating || views[1].GenerationStatus != GenerationStatusCompleted || views[1].ImageURL != "https://assets.example.test/final.png" {
		t.Fatalf("成功与生成中投影 = %#v", views[:2])
	}
	if views[2].GenerationErrorCode != "GENERATION_SUBMISSION_FAILED" || views[2].GenerationErrorMessage != "生成服务暂不可用，请稍后重试" {
		t.Fatalf("提交失败投影 = %#v", views[2])
	}
	if views[3].GenerationErrorCode != "GENERATION_FAILED" || views[4].GenerationErrorCode != "CONTENT_CONFISCATED" {
		t.Fatalf("失败与没收投影 = %#v / %#v", views[3], views[4])
	}
}

// 单条读取对非本人或不存在创作统一返回未找到，不能借由错误区别探测他人任务。
func TestStatusUsecase单条未命中返回统一未找到(t *testing.T) {
	usecase := NewStatusUsecase(staticImageStatusReader{})
	_, err := usecase.Get(context.Background(), "user-1", "550e8400-e29b-41d4-a716-446655440006")
	if err != ErrImageNotFound {
		t.Fatalf("Get() error = %v, want ErrImageNotFound", err)
	}
}

type staticImageStatusReader struct {
	records []OwnedImageCreation
	err     error
}

func (reader staticImageStatusReader) FindOwnedImages(context.Context, string, []string) ([]OwnedImageCreation, error) {
	return reader.records, reader.err
}
