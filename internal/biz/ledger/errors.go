package ledger

import "errors"

var (
	// ErrReservationCommandConflict 表示相同创建任务携带了不一致的预留命令上下文。
	ErrReservationCommandConflict = errors.New("reservation command conflicts with existing creation")
	// ErrReservationStateConflict 表示预留已进入不允许的状态，或条件状态迁移未命中。
	ErrReservationStateConflict = errors.New("reservation state conflict")
	// ErrInvalidReservationCommand 表示预留命令缺少领域层必需字段或额度范围非法。
	ErrInvalidReservationCommand = errors.New("invalid reservation command")
	// ErrReservationNotFound 表示待冲正或没收的预留不存在。
	ErrReservationNotFound = errors.New("reservation not found")
	// ErrLedgerDependenciesUnavailable 表示账本用例尚未获得仓储或事务依赖。
	ErrLedgerDependenciesUnavailable = errors.New("ledger dependencies unavailable")
)
