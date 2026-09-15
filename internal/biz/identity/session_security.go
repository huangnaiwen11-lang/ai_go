package identity

import (
	"context"
)

// RevokeAllSessions 撤销用户全部活动会话，供用户主动退出其他设备使用。
// 它只操作 Go 自有会话，不读取或修改旧 Node 登录态。
func (usecase *Usecase) RevokeAllSessions(ctx context.Context, userID string) error {
	if userID == "" || usecase == nil || usecase.sessions == nil {
		return ErrInvalidAuthEntryInput
	}
	return usecase.sessions.RevokeActiveByUser(ctx, userID, usecase.clock())
}
