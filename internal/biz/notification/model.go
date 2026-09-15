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

// Preferences 是 AI 工作室用户可自行控制的通知投递偏好。
// 它不包含 PWA 订阅、邮件地址或任何第三方供应商凭据；这些运输细节必须留在后续投递适配器中。
type Preferences struct {
	PushEnabled                bool
	EmailEnabled               bool
	GenerationCompletedEnabled bool
}

// DefaultPreferences 让首次进入设置页的用户明确看到当前默认策略。
// 用户选择关闭后由自有 Mongo 文档持久化，永不读取旧 Node 用户设置。
func DefaultPreferences() Preferences {
	return Preferences{PushEnabled: true, EmailEnabled: true, GenerationCompletedEnabled: true}
}

type Repository interface {
	List(context.Context, ListQuery) ([]Item, int, error)
	MarkRead(context.Context, string, string, time.Time) error
	MarkAllRead(context.Context, string, time.Time) (int, error)
	Delete(context.Context, string, string) error
	// DeleteRead 只能删除当前用户已经阅读的通知，不能承担“全量清空”语义。
	DeleteRead(context.Context, string) (int, error)
	GetPreferences(context.Context, string) (Preferences, error)
	SavePreferences(context.Context, string, Preferences) error
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

func (usecase *Usecase) GetPreferences(ctx context.Context, userID string) (Preferences, error) {
	if userID == "" {
		return Preferences{}, ErrInvalidInput
	}
	return usecase.repository.GetPreferences(ctx, userID)
}

// SavePreferences 以完整快照保存三项开关，避免多个独立开关在并发点击时互相覆盖。
func (usecase *Usecase) SavePreferences(ctx context.Context, userID string, preferences Preferences) error {
	if userID == "" {
		return ErrInvalidInput
	}
	return usecase.repository.SavePreferences(ctx, userID, preferences)
}
