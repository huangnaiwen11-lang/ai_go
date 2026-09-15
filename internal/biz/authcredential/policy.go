package authcredential

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	argon2IDAlgorithm = "argon2id"
	argon2Version     = 19
	phcPartCount      = 6
)

// Policy 是不可变的密码哈希策略。
// random 默认使用 crypto/rand.Reader；仅测试可以通过未导出的构造器注入确定性来源。
type Policy struct {
	params Params
	random io.Reader
}

// NewPolicy 校验安全下限后构造密码策略。
func NewPolicy(params Params) (*Policy, error) {
	if !validParams(params) {
		return nil, ErrInvalidPolicy
	}
	return newPolicy(params, rand.Reader), nil
}

// MustNewPolicy 是测试和受已验证配置调用方的便捷构造器。
func MustNewPolicy(params Params) *Policy {
	policy, err := NewPolicy(params)
	if err != nil {
		panic(err)
	}
	return policy
}

func newPolicy(params Params, random io.Reader) *Policy {
	return &Policy{params: params, random: random}
}

// Hash 以 Argon2id 生成可独立验证的 PHC 编码字符串。密码明文仅存在于本函数栈中。
func (policy *Policy) Hash(password string) (string, error) {
	if policy == nil || !validParams(policy.params) || policy.random == nil {
		return "", ErrInvalidPolicy
	}
	if utf8.RuneCountInString(password) < MinimumPasswordLength {
		return "", ErrWeakPassword
	}

	salt := make([]byte, policy.params.SaltBytes)
	if _, err := io.ReadFull(policy.random, salt); err != nil {
		return "", fmt.Errorf("read password salt: %w", err)
	}
	key := argon2.IDKey(
		[]byte(password),
		salt,
		policy.params.TimeCost,
		policy.params.MemoryKiB,
		policy.params.Parallelism,
		policy.params.KeyBytes,
	)
	return encodePHC(policy.params, salt, key), nil
}

// Verify 使用常量时间比较校验密码。
// 返回的 needsRehash 只会在匹配成功时为 true，调用方可安全地替换为当前参数的新哈希。
func (policy *Policy) Verify(encoded, password string) (matched bool, needsRehash bool, err error) {
	if policy == nil || !validParams(policy.params) {
		return false, false, ErrInvalidPolicy
	}
	parsed, err := decodePHC(encoded)
	if err != nil {
		return false, false, ErrInvalidCredential
	}
	candidate := argon2.IDKey(
		[]byte(password),
		parsed.salt,
		parsed.params.TimeCost,
		parsed.params.MemoryKiB,
		parsed.params.Parallelism,
		uint32(len(parsed.key)),
	)
	if subtle.ConstantTimeCompare(candidate, parsed.key) != 1 {
		return false, false, nil
	}
	return true, parsed.params != policy.params, nil
}

func validParams(params Params) bool {
	return params.MemoryKiB >= MinimumMemoryKiB &&
		params.TimeCost >= MinimumTimeCost &&
		params.Parallelism >= MinimumParallelism &&
		params.SaltBytes >= MinimumSaltBytes &&
		params.KeyBytes >= MinimumKeyBytes
}

func encodePHC(params Params, salt []byte, key []byte) string {
	return fmt.Sprintf(
		"$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2IDAlgorithm,
		argon2Version,
		params.MemoryKiB,
		params.TimeCost,
		params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

type parsedCredential struct {
	params Params
	salt   []byte
	key    []byte
}

func decodePHC(encoded string) (parsedCredential, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != phcPartCount || parts[0] != "" || parts[1] != argon2IDAlgorithm || parts[2] != "v=19" {
		return parsedCredential{}, ErrInvalidCredential
	}

	params, err := decodeParams(parts[3])
	if err != nil || !validDerivedParams(params) {
		return parsedCredential{}, ErrInvalidCredential
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < int(MinimumSaltBytes) {
		return parsedCredential{}, ErrInvalidCredential
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(key) < int(MinimumKeyBytes) {
		return parsedCredential{}, ErrInvalidCredential
	}
	params.SaltBytes = uint32(len(salt))
	params.KeyBytes = uint32(len(key))
	return parsedCredential{params: params, salt: salt, key: key}, nil
}

// validDerivedParams 校验 PHC 中直接编码的 Argon2id 计算参数。盐和输出长度由
// base64 解码结果决定，不能从此处不存在的字段推断。
func validDerivedParams(params Params) bool {
	return params.MemoryKiB >= MinimumMemoryKiB &&
		params.TimeCost >= MinimumTimeCost &&
		params.Parallelism >= MinimumParallelism
}

func decodeParams(raw string) (Params, error) {
	values := strings.Split(raw, ",")
	if len(values) != 3 {
		return Params{}, ErrInvalidCredential
	}
	var params Params
	for _, value := range values {
		name, number, found := strings.Cut(value, "=")
		if !found || number == "" {
			return Params{}, ErrInvalidCredential
		}
		parsed, err := strconv.ParseUint(number, 10, 32)
		if err != nil {
			return Params{}, ErrInvalidCredential
		}
		switch name {
		case "m":
			params.MemoryKiB = uint32(parsed)
		case "t":
			params.TimeCost = uint32(parsed)
		case "p":
			if parsed > 255 {
				return Params{}, ErrInvalidCredential
			}
			params.Parallelism = uint8(parsed)
		default:
			return Params{}, ErrInvalidCredential
		}
	}
	return params, nil
}
