package model

import "time"

// UserDocument 表示用户持久化对象。
type UserDocument struct {
	ID             string    `bson:"_id"`
	DisplayName    string    `bson:"display_name,omitempty"`
	Bio            string    `bson:"bio,omitempty"`
	AvatarImageID  string    `bson:"avatar_image_id,omitempty"`
	AccountStatus  string    `bson:"account_status"`
	BindingState   string    `bson:"binding_state"`
	Role           string    `bson:"role,omitempty"`
	Timezone       string    `bson:"timezone"`
	GuestPlatform  string    `bson:"guest_platform,omitempty"`
	GuestDeviceID  string    `bson:"guest_device_id,omitempty"`
	SessionVersion int64     `bson:"session_version"`
	ContentAccess  string    `bson:"content_access"`
	CreatedAt      time.Time `bson:"created_at"`
	UpdatedAt      time.Time `bson:"updated_at"`
}

// CredentialDocument 表示 Go 自有邮箱密码凭据。
// password_hash 只能保存 Argon2id 编码结果，绝不保存或派生密码明文。
type CredentialDocument struct {
	ID              string    `bson:"_id"`
	UserID          string    `bson:"user_id"`
	EmailNormalized string    `bson:"email_normalized"`
	PasswordHash    string    `bson:"password_hash"`
	Active          bool      `bson:"active"`
	CreatedAt       time.Time `bson:"created_at"`
	UpdatedAt       time.Time `bson:"updated_at"`
}

// IdentityDocument 表示身份绑定持久化对象。
type IdentityDocument struct {
	ID        string    `bson:"_id"`
	Provider  string    `bson:"provider"`
	Subject   string    `bson:"subject"`
	UserID    string    `bson:"user_id"`
	CreatedAt time.Time `bson:"created_at"`
}

// SessionDocument 表示会话持久化对象。
type SessionDocument struct {
	ID             string     `bson:"_id"`
	UserID         string     `bson:"user_id"`
	SessionVersion int64      `bson:"session_version"`
	RevokedAt      *time.Time `bson:"revoked_at,omitempty"`
	ExpiresAt      time.Time  `bson:"expires_at"`
}
