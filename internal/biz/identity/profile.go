package identity

import (
	"context"
	"strings"
	"time"
)

// profileUserWriter 是资料写入的最小边界，避免把 UserRepository 扩展成任意字段更新接口。
type profileUserWriter interface {
	UpdateDisplayName(context.Context, string, string, time.Time) (*User, error)
	UpdateProfile(context.Context, string, string, string, *string, time.Time) (*User, error)
}

// ProfileUpdate 是资料页可修改字段的受控集合。
type ProfileUpdate struct {
	DisplayName   string
	Bio           string
	AvatarImageID *string
}

// UpdateProfile 保存昵称和简介。账户、时区和身份绑定不属于资料编辑能力。
func (usecase *Usecase) UpdateProfile(ctx context.Context, userID string, input ProfileUpdate) (*User, error) {
	if strings.TrimSpace(userID) == "" || len([]rune(input.DisplayName)) > 50 || len([]rune(input.Bio)) > 200 {
		return nil, ErrInvalidAuthEntryInput
	}
	writer, ok := usecase.users.(profileUserWriter)
	if !ok {
		return nil, ErrAuthDependenciesUnavailable
	}
	return writer.UpdateProfile(ctx, userID, strings.TrimSpace(input.DisplayName), strings.TrimSpace(input.Bio), input.AvatarImageID, usecase.clock())
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
