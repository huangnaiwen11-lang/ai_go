package identity

import (
	"context"
	"strings"

	"ai-business-service/internal/biz/authcredential"
)

type userCredentialReader interface {
	FindActiveByUser(context.Context, string) (*authcredential.Credential, error)
}

// ChangePassword 校验旧密码后更新新密码，并使已有会话版本失效。
func (usecase *Usecase) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	if strings.TrimSpace(userID) == "" || currentPassword == "" || newPassword == "" || currentPassword == newPassword || usecase == nil || usecase.passwords == nil {
		return ErrInvalidAuthEntryInput
	}
	reader, ok := usecase.credentials.(userCredentialReader)
	if !ok {
		return ErrAuthDependenciesUnavailable
	}
	credential, err := reader.FindActiveByUser(ctx, userID)
	if err != nil {
		return ErrInvalidCredentials
	}
	matched, _, err := usecase.passwords.Verify(credential.PasswordHash, currentPassword)
	if err != nil || !matched {
		return ErrInvalidCredentials
	}
	hash, err := usecase.passwords.Hash(newPassword)
	if err != nil {
		return ErrInvalidAuthEntryInput
	}
	if err := usecase.credentials.UpdatePasswordHash(ctx, userID, hash, usecase.clock()); err != nil {
		return err
	}
	// 修改密码后撤销其他活动会话，当前客户端也会在下一次请求时重新认证。
	return usecase.sessions.RevokeActiveByUser(ctx, userID, usecase.clock())
}
