package walletview

import (
	"context"
	"strings"
)

// Usecase 提供钱包页所需的纯读取查询。
// 它不承担生成门禁，绝不根据余额创建任务、扣钻或调用支付。
type Usecase struct {
	repository Repository
}

// NewUsecase 创建钱包只读查询用例。
func NewUsecase(repository Repository) *Usecase {
	return &Usecase{repository: repository}
}

// GetSnapshot 查询用户自己的钱包快照。
// 缺失钱包投影按现网语义展示为 0 钻石，而不是创建账户或阻止后续生成流程。
func (usecase *Usecase) GetSnapshot(ctx context.Context, userID string) (*Snapshot, error) {
	if err := usecase.ready(); err != nil {
		return nil, err
	}
	if !validUserID(userID) {
		return nil, ErrInvalidWalletViewQuery
	}

	snapshot, err := usecase.repository.FindSnapshot(ctx, userID)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return &Snapshot{UserID: userID}, nil
	}
	return snapshot, nil
}

// ListLedgerEntries 查询用户自己的倒序账本。
// 旧客户端同时携带 cursor 和 skip 时，cursor 优先，避免两种分页基准被叠加。
func (usecase *Usecase) ListLedgerEntries(ctx context.Context, query LedgerPageQuery) (*LedgerPage, error) {
	if err := usecase.ready(); err != nil {
		return nil, err
	}
	if !validLedgerPageQuery(query) {
		return nil, ErrInvalidWalletViewQuery
	}
	if query.Cursor != "" {
		query.Skip = 0
	}

	page, err := usecase.repository.ListLedgerEntries(ctx, query)
	if err != nil {
		return nil, err
	}
	if page == nil {
		return &LedgerPage{}, nil
	}
	return page, nil
}

func (usecase *Usecase) ready() error {
	if usecase == nil || usecase.repository == nil {
		return ErrWalletViewDependenciesUnavailable
	}
	return nil
}

func validUserID(userID string) bool {
	return strings.TrimSpace(userID) != ""
}

func validLedgerPageQuery(query LedgerPageQuery) bool {
	if !validUserID(query.UserID) || query.Limit <= 0 || query.Skip < 0 {
		return false
	}
	if query.Cursor == "" {
		return true
	}
	_, err := ParseLedgerCursor(query.Cursor)
	return err == nil
}
