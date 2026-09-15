// Package authcredential 提供密码凭据的领域规则。
//
// 本包只处理密码哈希、验证和参数升级，不保存用户、会话或 MongoDB 文档，避免密码
// 明文与持久化技术细节扩散到身份、传输层和日志中。
package authcredential

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrWeakPassword 表示密码未满足已冻结的最小长度规则。
	ErrWeakPassword = errors.New("auth credential: password is too weak")
	// ErrInvalidPolicy 表示运行配置低于安全下限或内部参数自相矛盾。
	ErrInvalidPolicy = errors.New("auth credential: invalid argon2id policy")
	// ErrInvalidCredential 表示存量编码格式不可信或已经损坏；调用方必须按认证失败处理。
	ErrInvalidCredential = errors.New("auth credential: invalid password credential")
	// ErrCredentialNotFound 表示没有可用于密码登录的活动凭据。
	ErrCredentialNotFound = errors.New("auth credential: credential not found")
	// ErrEmailAlreadyRegistered 表示活动凭据已占用规范化邮箱。
	ErrEmailAlreadyRegistered = errors.New("auth credential: email already registered")
)

const (
	// MinimumPasswordLength 与现网邮箱密码入口保持最小 6 个 Unicode 字符的语义。
	MinimumPasswordLength = 6
	// MinimumMemoryKiB 是 OWASP 推荐的 Argon2id 低风险基线，禁止本地配置降级。
	MinimumMemoryKiB uint32 = 19 * 1024
	// MinimumTimeCost 表示至少执行两轮 Argon2id。
	MinimumTimeCost uint32 = 2
	// MinimumParallelism 保证至少使用一个并行通道。
	MinimumParallelism uint8 = 1
	// MinimumSaltBytes 避免短盐导致可预计算攻击。
	MinimumSaltBytes uint32 = 16
	// MinimumKeyBytes 表示输出至少 256 bit 的派生密钥。
	MinimumKeyBytes uint32 = 32
)

// Params 是 Argon2id 编码中需要冻结的计算参数。
// 参数会编码进每条凭据，使服务升级后仍可验证旧哈希并在成功登录时重哈希。
type Params struct {
	MemoryKiB   uint32
	TimeCost    uint32
	Parallelism uint8
	SaltBytes   uint32
	KeyBytes    uint32
}

// Credential 是密码凭据的领域对象。PasswordHash 只能由本包生成、校验或由 data 层
// 原样持久化，调用方绝不将其写入 HTTP 响应或日志。
type Credential struct {
	ID              string
	UserID          string
	EmailNormalized string
	PasswordHash    string
	Active          bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Repository 是密码凭据的持久化反转边界。
// 所有方法接收上层传入的事务 context，仓储不得自行开启事务。
type Repository interface {
	FindActiveByEmail(context.Context, string) (*Credential, error)
	Create(context.Context, Credential) error
	UpdatePasswordHash(context.Context, string, string, time.Time) error
	DeactivateByUser(context.Context, string, time.Time) error
}

// PasswordPolicy 是身份用例依赖的最小密码安全能力。
type PasswordPolicy interface {
	Hash(string) (string, error)
	Verify(encoded, password string) (matched bool, needsRehash bool, err error)
}
