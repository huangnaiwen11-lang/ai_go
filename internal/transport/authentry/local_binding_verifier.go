package authentry

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/shared"
)

// LocalBindingVerifier 是本地联调用 verifier。
// 它模拟 OAuth/短信服务端兑换结果：浏览器只能提交完整签名凭据，不能直接提交 subject。
// 该实现只允许本地显式装配，生产环境不得把它当作真实身份供应商。
type LocalBindingVerifier struct {
	Secret []byte
}

// IssueLocalBindingCredential 生成本地联调凭据，格式与 Verify 严格对称。
// 该函数只用于测试和本地开发，不能生成真实 OAuth 或短信凭据。
func IssueLocalBindingCredential(provider, subject, secret string) (string, error) {
	provider = strings.TrimSpace(provider)
	subject = strings.TrimSpace(subject)
	secret = strings.TrimSpace(secret)
	if provider == "" || subject == "" || secret == "" || len(subject) > 256 {
		return "", shared.ErrInvalidRequest
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("local-v1." + provider + "." + subject))
	return "local-v1." + provider + "." + base64.RawURLEncoding.EncodeToString([]byte(subject)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (verifier LocalBindingVerifier) Verify(_ context.Context, provider, credential string) (identity.ExternalIdentity, error) {
	if len(verifier.Secret) == 0 {
		return identity.ExternalIdentity{}, shared.ErrServiceUnavailable
	}
	parts := strings.Split(credential, ".")
	if len(parts) != 4 || parts[0] != "local-v1" || parts[1] != provider || parts[2] == "" || parts[3] == "" {
		return identity.ExternalIdentity{}, shared.ErrInvalidRequest
	}
	subject, signature, err := decodeLocalToken(parts[2], parts[3])
	if err != nil {
		return identity.ExternalIdentity{}, shared.ErrInvalidRequest
	}
	message := "local-v1." + provider + "." + subject
	mac := hmac.New(sha256.New, verifier.Secret)
	_, _ = mac.Write([]byte(message))
	if !hmac.Equal(mac.Sum(nil), signature) {
		return identity.ExternalIdentity{}, shared.ErrUnauthenticated
	}
	return identity.ExternalIdentity{Provider: provider, Subject: subject}, nil
}

func decodeLocalToken(subjectPart, signaturePart string) (string, []byte, error) {
	subjectBytes, err := base64.RawURLEncoding.DecodeString(subjectPart)
	if err != nil || len(subjectBytes) == 0 || len(subjectBytes) > 256 {
		return "", nil, errors.New("invalid local binding subject")
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil || len(signature) != sha256.Size {
		return "", nil, errors.New("invalid local binding signature")
	}
	return string(subjectBytes), signature, nil
}
