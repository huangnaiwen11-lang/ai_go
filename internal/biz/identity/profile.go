package identity

import (
	"context"
	"strings"
	"time"
)

// profileUserWriter 是资料写入的最小边界，避免把 UserRepository 扩展成任意字段更新接口。
type profileUserWriter interface {
	UpdateDisplayName(context.Context, string, string, time.Time) (*User, error)
}

// UpdateDisplayName 更新当前用户昵称。昵称为空表示清除昵称，时区等首次写入字段不可修改。
func (usecase *Usecase) UpdateDisplayName(ctx context.Context, userID, displayName string) (*User, error) {
	if strings.TrimSpace(userID) == "" || len([]rune(displayName)) > 64 {
		return nil, ErrInvalidAuthEntryInput
	}
	writer, ok := usecase.users.(profileUserWriter)
	if !ok {
		return nil, ErrAuthDependenciesUnavailable
	}
	return writer.UpdateDisplayName(ctx, userID, strings.TrimSpace(displayName), usecase.clock())
}
