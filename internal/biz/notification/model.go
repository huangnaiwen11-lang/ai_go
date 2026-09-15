// Package notification 定义用户端通知读写合同，不包含推送供应商和管理后台投递。
package notification

import (
	"context"
	"errors"
	"time"
)

var ErrInvalidInput = errors.New("notification: invalid input")

type Item struct {
	ID, UserID, Type, Title, Body string
	Data                          map[string]any
	Read                          bool
	ReadAt                        *time.Time
	CreatedAt                     time.Time
}
type ListQuery struct {
	UserID     string
	Limit      int
	UnreadOnly bool
}

type Repository interface {
	List(context.Context, ListQuery) ([]Item, int, error)
	MarkRead(context.Context, string, string, time.Time) error
	MarkAllRead(context.Context, string, time.Time) (int, error)
	Delete(context.Context, string, string) error
	// DeleteRead 只能删除当前用户已经阅读的通知，不能承担“全量清空”语义。
	DeleteRead(context.Context, string) (int, error)
}

type Usecase struct {
	repository Repository
	clock      func() time.Time
}

func NewUsecase(repository Repository) *Usecase {
	return &Usecase{repository: repository, clock: time.Now}
}
func (usecase *Usecase) List(ctx context.Context, query ListQuery) ([]Item, int, error) {
	if query.UserID == "" {
		return nil, 0, ErrInvalidInput
	}
	if query.Limit <= 0 || query.Limit > 100 {
		query.Limit = 50
	}
	return usecase.repository.List(ctx, query)
}
func (usecase *Usecase) MarkRead(ctx context.Context, userID, id string) error {
	if userID == "" || id == "" {
		return ErrInvalidInput
	}
	return usecase.repository.MarkRead(ctx, userID, id, usecase.clock().UTC())
}
func (usecase *Usecase) MarkAllRead(ctx context.Context, userID string) (int, error) {
	if userID == "" {
		return 0, ErrInvalidInput
	}
	return usecase.repository.MarkAllRead(ctx, userID, usecase.clock().UTC())
}
func (usecase *Usecase) Delete(ctx context.Context, userID, id string) error {
	if userID == "" || id == "" {
		return ErrInvalidInput
	}
	return usecase.repository.Delete(ctx, userID, id)
}

// DeleteRead 清理当前会话用户已读通知。未读通知保留，避免把待处理消息误删。
func (usecase *Usecase) DeleteRead(ctx context.Context, userID string) (int, error) {
	if userID == "" {
		return 0, ErrInvalidInput
	}
	return usecase.repository.DeleteRead(ctx, userID)
}
