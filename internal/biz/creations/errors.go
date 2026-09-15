package creations

import "errors"

var (
	// ErrInvalidCreateCommand 表示创作创建命令缺少必填信息或包含非法输入摘要。
	ErrInvalidCreateCommand = errors.New("creations: invalid create command")
	// ErrInvalidStepPlan 表示服务端解析出的内部步骤计划不属于允许的固定组合。
	ErrInvalidStepPlan = errors.New("creations: invalid step plan")
	// ErrCreationCommandConflict 表示同一幂等键对应了不同的业务命令。
	ErrCreationCommandConflict = errors.New("creations: creation command conflict")
	// ErrCreationAlreadyExists 表示创作或步骤写入命中了幂等唯一约束。
	ErrCreationAlreadyExists = errors.New("creations: creation already exists")
	// ErrAccountUnavailable 表示用户不存在，或其账号不处于可创建状态。
	ErrAccountUnavailable = errors.New("creations: account unavailable")
)
