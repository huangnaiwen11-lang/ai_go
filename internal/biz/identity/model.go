// Package identity 管理用户、游客绑定、会话与账号状态。
package identity

import "time"

const (
	// ContentAccessStandard 表示普通用户，保持现网自由 T2I 的默认内容语义。
	ContentAccessStandard = "standard"
	// ContentAccessReviewRestricted 表示审核受限用户，提交器必须先完成内容审核。
	ContentAccessReviewRestricted = "review_restricted"
)

// AccountStatus 表示账户能否继续使用主站能力。
type AccountStatus string

const (
	// AccountStatusNormal 表示账户可正常使用。
	AccountStatusNormal AccountStatus = "normal"
	// AccountStatusBanned 表示账户被封禁，已有会话必须立即失效。
	AccountStatusBanned AccountStatus = "banned"
	// AccountStatusDeleted 表示账户已删除，已有会话必须立即失效。
	AccountStatusDeleted AccountStatus = "deleted"
)

// BindingState 表示用户是否已经从游客升级为已绑定账户。
type BindingState string

const (
	// BindingStateGuest 表示仍是游客，只能在原用户上完成绑定。
	BindingStateGuest BindingState = "guest"
	// BindingStateBound 表示用户已有可识别的外部身份。
	BindingStateBound BindingState = "bound"
)

// User 是身份模块拥有的用户领域对象。
// Timezone 只在首次创建用户时写入，后续绑定和状态变更不得修改。
type User struct {
	ID          string
	DisplayName string
	// Bio 是用户主动填写的公开简介，最大 200 个字符。
	Bio string
	// AvatarImageID 仅保存当前用户拥有的 Go 素材 ID，不保存外部 URL。
	AvatarImageID  string
	AccountStatus  AccountStatus
	BindingState   BindingState
	Timezone       string
	GuestPlatform  string
	GuestDeviceID  string
	SessionVersion int64
	ContentAccess  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ExternalIdentity 是归属于用户的外部身份。
// Provider 与 Subject 的组合在全局范围内唯一。
type ExternalIdentity struct {
	ID        string
	Provider  string
	Subject   string
	UserID    string
	CreatedAt time.Time
}

// Session 表示已签发的登录会话。
// SessionVersion 固化签发时用户版本，用于在封禁或删除后立即拒绝旧会话。
type Session struct {
	ID             string
	UserID         string
	SessionVersion int64
	// ContentAccess 是会话校验时从已验证用户读取的运行时内容访问级别，不持久化到 sessions。
	ContentAccess string
	RevokedAt     *time.Time
	ExpiresAt     time.Time
}

// BindGuestInput 是游客绑定已有用户的输入。
// 它不包含注册奖励、余额或任何现网登录协议字段。
type BindGuestInput struct {
	UserID   string
	Provider string
	Subject  string
}

// RegisterInput 是邮箱密码注册的领域输入。密码仅在用例调用期间使用，不会保存到 User。
type RegisterInput struct {
	Email       string
	Password    string
	Timezone    string
	DisplayName string
}

// PasswordLoginInput 是邮箱密码登录的领域输入。
type PasswordLoginInput struct {
	Email    string
	Password string
}

// GuestLoginInput 是移动端设备游客登录的领域输入。
type GuestLoginInput struct {
	Platform string
	DeviceID string
	Timezone string
}

// LoginResult 把新签发的会话与安全用户投影返回给 transport。
// AccountBalance 在注册/游客创建时固定为 0，不承担通用钱包读取职责。
type LoginResult struct {
	User           *User
	Session        *Session
	AccountBalance int64
}
