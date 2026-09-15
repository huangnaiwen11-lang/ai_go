// Package feedback 定义用户反馈的最小业务合同，不包含管理后台回复能力。
package feedback

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	minMessageRunes = 10
	maxMessageRunes = 2000
	maxAttachments  = 3
)

var allowedTypes = map[string]struct{}{
	"bug": {}, "feature": {}, "content": {}, "payment": {}, "praise": {}, "other": {},
}

var (
	ErrInvalidInput          = errors.New("feedback: invalid input")
	ErrRepositoryUnavailable = errors.New("feedback: repository unavailable")
)

// Attachment 是已校验归属后的附件引用。浏览器请求只允许提供 ID，DownloadURL 由服务端生成。
type Attachment struct {
	ID          string
	DownloadURL string
}

// AttachmentVerifier 是反馈域对素材域的反转依赖；它只确认当前用户拥有指定图片，
// 不让反馈用例直接认识本地文件、Mongo 集合或对象存储实现。
type AttachmentVerifier interface {
	VerifyOwnedAttachment(context.Context, string, string) (Attachment, error)
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
	repository         Repository
	attachmentVerifier AttachmentVerifier
	clock              func() time.Time
}

func NewUsecase(repository Repository, attachmentVerifier AttachmentVerifier) *Usecase {
	return &Usecase{repository: repository, attachmentVerifier: attachmentVerifier, clock: time.Now}
}

type SubmitInput struct {
	UserID, Type, Message, Email string
	Attachments                  []Attachment
}

func (usecase *Usecase) Submit(ctx context.Context, input SubmitInput) (*Submission, error) {
	if usecase == nil || usecase.repository == nil {
		return nil, ErrRepositoryUnavailable
	}
	message := strings.TrimSpace(input.Message)
	feedbackType := strings.TrimSpace(input.Type)
	if strings.TrimSpace(input.UserID) == "" || !isAllowedType(feedbackType) || len([]rune(message)) < minMessageRunes || len([]rune(message)) > maxMessageRunes || len(input.Attachments) > maxAttachments || !isValidOptionalEmail(input.Email) {
		return nil, ErrInvalidInput
	}
	attachments := make([]Attachment, 0, len(input.Attachments))
	seenAttachmentIDs := make(map[string]struct{}, len(input.Attachments))
	for _, attachment := range input.Attachments {
		attachmentID := strings.TrimSpace(attachment.ID)
		if attachmentID == "" || attachment.DownloadURL != "" || usecase.attachmentVerifier == nil {
			return nil, ErrInvalidInput
		}
		if _, exists := seenAttachmentIDs[attachmentID]; exists {
			return nil, ErrInvalidInput
		}
		verified, err := usecase.attachmentVerifier.VerifyOwnedAttachment(ctx, strings.TrimSpace(input.UserID), attachmentID)
		if err != nil || strings.TrimSpace(verified.ID) != attachmentID || strings.TrimSpace(verified.DownloadURL) == "" {
			return nil, ErrInvalidInput
		}
		seenAttachmentIDs[attachmentID] = struct{}{}
		attachments = append(attachments, verified)
	}
	submission := Submission{ID: newID(), UserID: strings.TrimSpace(input.UserID), Type: feedbackType, Message: message, Email: strings.TrimSpace(input.Email), Attachments: attachments, CreatedAt: usecase.clock().UTC()}
	if err := usecase.repository.Create(ctx, submission); err != nil {
		return nil, err
	}
	return &submission, nil
}

func newID() string { return uuid.NewString() }

func isAllowedType(value string) bool {
	_, ok := allowedTypes[value]
	return ok
}

func isValidOptionalEmail(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Address == value
}
