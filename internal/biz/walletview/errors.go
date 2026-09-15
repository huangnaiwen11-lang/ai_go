package walletview

import "errors"

var (
	// ErrInvalidWalletViewQuery 表示只读钱包查询缺少用户标识或携带非法分页参数。
	ErrInvalidWalletViewQuery = errors.New("wallet view: invalid query")
	// ErrWalletViewDependenciesUnavailable 表示查询用例尚未获得只读仓储。
	ErrWalletViewDependenciesUnavailable = errors.New("wallet view: dependencies unavailable")
)
