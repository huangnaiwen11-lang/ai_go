// Package feedback 定义用户反馈的最小业务合同，不包含管理后台回复能力。
package feedback

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput          = errors.New("feedback: invalid input")
	ErrRepositoryUnavailable = errors.New("feedback: repository unavailable")
)

// Attachment 是已通过 Go 素材合同的附件引用，不接受浏览器本地路径或任意 URL。
type Attachment struct {
	ID          string
	DownloadURL string
}

type Submission struct {
	ID          string
	UserID      string
	Type        string
	Message     string
	Email       string
	Attachments []Attachment
	CreatedAt   time.Time
}

type Repository interface {
	Create(context.Context, Submission) error
}

type Usecase struct {
	repository Repository
	clock      func() time.Time
}

func NewUsecase(repository Repository) *Usecase {
	return &Usecase{repository: repository, clock: time.Now}
}

type SubmitInput struct {
	UserID, Type, Message, Email string
	Attachments                  []Attachment
}

func (usecase *Usecase) Submit(ctx context.Context, input SubmitInput) (*Submission, error) {
	if usecase == nil || usecase.repository == nil {
		return nil, ErrRepositoryUnavailable
	}
	if strings.TrimSpace(input.UserID) == "" || strings.TrimSpace(input.Type) == "" || len([]rune(strings.TrimSpace(input.Message))) < 10 || len([]rune(input.Message)) > 5000 || len(input.Attachments) > 5 {
		return nil, ErrInvalidInput
	}
	for _, attachment := range input.Attachments {
		if strings.TrimSpace(attachment.ID) == "" || strings.TrimSpace(attachment.DownloadURL) == "" {
			return nil, ErrInvalidInput
		}
	}
	submission := Submission{ID: newID(), UserID: strings.TrimSpace(input.UserID), Type: strings.TrimSpace(input.Type), Message: strings.TrimSpace(input.Message), Email: strings.TrimSpace(input.Email), Attachments: append([]Attachment(nil), input.Attachments...), CreatedAt: usecase.clock().UTC()}
	if err := usecase.repository.Create(ctx, submission); err != nil {
		return nil, err
	}
	return &submission, nil
}

func newID() string { return uuid.NewString() }
