package feedback

import (
	"context"
	"testing"
)

type memoryRepository struct{ submission *Submission }

func (repository *memoryRepository) Create(_ context.Context, submission Submission) error {
	repository.submission = &submission
	return nil
}

func TestSubmitPreservesUserAndRejectsShortMessage(t *testing.T) {
	repository := &memoryRepository{}
	usecase := NewUsecase(repository)
	if _, err := usecase.Submit(context.Background(), SubmitInput{UserID: "user-1", Type: "bug", Message: "too short"}); err != ErrInvalidInput {
		t.Fatalf("short message error = %v", err)
	}
	submission, err := usecase.Submit(context.Background(), SubmitInput{UserID: "user-1", Type: "bug", Message: "这是一条足够长的反馈内容"})
	if err != nil || submission.UserID != "user-1" || repository.submission == nil {
		t.Fatalf("submission = %#v, err = %v", submission, err)
	}
}
