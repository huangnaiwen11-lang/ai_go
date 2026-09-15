package feedback

import (
	"context"
	"testing"
)

type memoryRepository struct{ submission *Submission }

type staticAttachmentVerifier struct{}

func (staticAttachmentVerifier) VerifyOwnedAttachment(_ context.Context, _ string, attachmentID string) (Attachment, error) {
	return Attachment{ID: attachmentID, DownloadURL: "/api/media/images/" + attachmentID}, nil
}

func (repository *memoryRepository) Create(_ context.Context, submission Submission) error {
	repository.submission = &submission
	return nil
}

func TestSubmitPreservesUserAndRejectsShortMessage(t *testing.T) {
	repository := &memoryRepository{}
	usecase := NewUsecase(repository, nil)
	if _, err := usecase.Submit(context.Background(), SubmitInput{UserID: "user-1", Type: "bug", Message: "too short"}); err != ErrInvalidInput {
		t.Fatalf("short message error = %v", err)
	}
	submission, err := usecase.Submit(context.Background(), SubmitInput{UserID: "user-1", Type: "bug", Message: "这是一条足够长的反馈内容"})
	if err != nil || submission.UserID != "user-1" || repository.submission == nil {
		t.Fatalf("submission = %#v, err = %v", submission, err)
	}
}

// 旧站的用户端只允许这六类反馈；不得把页面内部状态或新增分类直接写入工单库。
func TestSubmitEnforcesLegacyFeedbackContract(t *testing.T) {
	repository := &memoryRepository{}
	usecase := NewUsecase(repository, staticAttachmentVerifier{})
	valid := SubmitInput{
		UserID:      "user-1",
		Type:        "praise",
		Message:     "这是一条用于验证旧站反馈合同的足够长内容。",
		Email:       "user@example.com",
		Attachments: []Attachment{{ID: "media-1"}, {ID: "media-2"}, {ID: "media-3"}},
	}
	if _, err := usecase.Submit(context.Background(), valid); err != nil {
		t.Fatalf("valid legacy feedback error = %v", err)
	}
	if repository.submission == nil || repository.submission.Attachments[0].DownloadURL != "/api/media/images/media-1" {
		t.Fatalf("stored attachment must use server generated URL, got %#v", repository.submission)
	}

	for name, input := range map[string]SubmitInput{
		"unsupported type":     {UserID: "user-1", Type: "generation", Message: valid.Message},
		"too long message":     {UserID: "user-1", Type: "bug", Message: string(make([]rune, 2001))},
		"too many attachments": {UserID: "user-1", Type: "bug", Message: valid.Message, Attachments: []Attachment{{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}}},
		"invalid email":        {UserID: "user-1", Type: "bug", Message: valid.Message, Email: "not-an-email"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := usecase.Submit(context.Background(), input); err != ErrInvalidInput {
				t.Fatalf("Submit() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}
