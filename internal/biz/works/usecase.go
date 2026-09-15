package works

import (
	"context"
	"strings"
)

// Usecase 提供用户本人作品的只读查询，不参与任务创建、预扣或任何生成调用。
type Usecase struct {
	repository Repository
}

// NewUsecase 创建作品历史读取用例。
func NewUsecase(repository Repository) *Usecase {
	return &Usecase{repository: repository}
}

// List 返回当前会话用户按创建时间倒序的作品历史。
func (usecase *Usecase) List(ctx context.Context, query ListQuery) (*Page, error) {
	if err := usecase.ready(); err != nil {
		return nil, err
	}
	if !validListQuery(query) {
		return nil, ErrInvalidQuery
	}
	if query.Cursor != "" {
		query.Skip = 0
	}
	page, err := usecase.repository.List(ctx, query)
	if err != nil {
		return nil, err
	}
	if page == nil {
		return &Page{Items: []Work{}}, nil
	}
	return page, nil
}

// Get 返回当前会话用户拥有的一件作品。跨用户查询必须由仓储统一映射为 ErrWorkNotFound。
func (usecase *Usecase) Get(ctx context.Context, userID, id string) (*Work, error) {
	if err := usecase.ready(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrInvalidQuery
	}
	work, err := usecase.repository.FindByID(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if work == nil {
		return nil, ErrWorkNotFound
	}
	return work, nil
}

func (usecase *Usecase) ready() error {
	if usecase == nil || usecase.repository == nil {
		return ErrDependenciesUnavailable
	}
	return nil
}

func validListQuery(query ListQuery) bool {
	if strings.TrimSpace(query.UserID) == "" || (query.Kind != KindImage && query.Kind != KindVideo) || query.Limit < 1 || query.Limit > 100 || query.Skip < 0 {
		return false
	}
	if query.Cursor == "" {
		return true
	}
	_, err := ParseCursor(query.Cursor)
	return err == nil
}
