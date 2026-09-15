package works

import "errors"

var (
	// ErrInvalidQuery 表示作品读取参数不符合受控合同。
	ErrInvalidQuery = errors.New("works: invalid query")
	// ErrWorkNotFound 统一表示缺失或不属于当前用户的作品，避免泄露其他用户的作品存在性。
	ErrWorkNotFound = errors.New("works: not found")
	// ErrResultUnavailable 表示数据标记成功但最终可用结果资产缺失，不能伪造成功作品。
	ErrResultUnavailable = errors.New("works: succeeded result unavailable")
	// ErrDependenciesUnavailable 表示读取用例未获得仓储依赖。
	ErrDependenciesUnavailable = errors.New("works: dependencies unavailable")
)
