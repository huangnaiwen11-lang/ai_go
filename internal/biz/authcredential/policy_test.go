package authcredential_test

import (
	"errors"
	"testing"

	"ai-business-service/internal/biz/authcredential"
)

// TestPolicyRejectsPasswordShorterThanSixCharacters 固化现网邮箱密码登录的最小密码长度。
func TestPolicyRejectsPasswordShorterThanSixCharacters(t *testing.T) {
	policy := authcredential.MustNewPolicy(authcredential.Params{
		MemoryKiB:   19 * 1024,
		TimeCost:    2,
		Parallelism: 1,
		SaltBytes:   16,
		KeyBytes:    32,
	})

	_, err := policy.Hash("12345")
	if !errors.Is(err, authcredential.ErrWeakPassword) {
		t.Fatalf("Hash() error = %v, want ErrWeakPassword", err)
	}
}

// TestPolicyHashVerifyAndRehash 验证密码不以明文保存，并在参数升级后请求安全重哈希。
func TestPolicyHashVerifyAndRehash(t *testing.T) {
	oldPolicy := authcredential.MustNewPolicy(authcredential.Params{
		MemoryKiB:   19 * 1024,
		TimeCost:    2,
		Parallelism: 1,
		SaltBytes:   16,
		KeyBytes:    32,
	})
	encoded, err := oldPolicy.Hash("correct-horse")
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if encoded == "correct-horse" {
		t.Fatal("Hash() must not return plaintext password")
	}

	currentPolicy := authcredential.MustNewPolicy(authcredential.Params{
		MemoryKiB:   32 * 1024,
		TimeCost:    3,
		Parallelism: 1,
		SaltBytes:   16,
		KeyBytes:    32,
	})
	matched, needsRehash, err := currentPolicy.Verify(encoded, "correct-horse")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !matched || !needsRehash {
		t.Fatalf("Verify() = matched %t, needsRehash %t, want true, true", matched, needsRehash)
	}
}

// TestPolicyReturnsNoMatchForWrongPassword 确保校验失败是正常认证结果而非内部错误。
func TestPolicyReturnsNoMatchForWrongPassword(t *testing.T) {
	policy := authcredential.MustNewPolicy(authcredential.Params{
		MemoryKiB:   19 * 1024,
		TimeCost:    2,
		Parallelism: 1,
		SaltBytes:   16,
		KeyBytes:    32,
	})
	encoded, err := policy.Hash("correct-horse")
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}

	matched, needsRehash, err := policy.Verify(encoded, "wrong-horse")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if matched || needsRehash {
		t.Fatalf("Verify() = matched %t, needsRehash %t, want false, false", matched, needsRehash)
	}
}
