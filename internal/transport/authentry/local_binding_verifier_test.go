package authentry

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestLocalBindingVerifierAcceptsSignedCredential(t *testing.T) {
	secret := []byte("local-test-secret")
	provider, subject := "email", "user@example.test"
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("local-v1." + provider + "." + subject))
	credential := "local-v1." + provider + "." + base64.RawURLEncoding.EncodeToString([]byte(subject)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	result, err := (LocalBindingVerifier{Secret: secret}).Verify(context.Background(), provider, credential)
	if err != nil || result.Provider != provider || result.Subject != subject {
		t.Fatalf("verify result = %#v, err = %v", result, err)
	}
}

func TestLocalBindingVerifierRejectsUnsignedSubject(t *testing.T) {
	_, err := (LocalBindingVerifier{Secret: []byte("local-test-secret")}).Verify(context.Background(), "email", "local-v1.email.dXNlckBleGFtcGxlLnRlc3Q.raw")
	if err == nil {
		t.Fatal("unsigned local credential was accepted")
	}
}

func TestIssueLocalBindingCredentialCanBeVerified(t *testing.T) {
	secret := "local-binding-secret-for-test-only"
	credential, err := IssueLocalBindingCredential("google", "subject-123", secret)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := (LocalBindingVerifier{Secret: []byte(secret)}).Verify(context.Background(), "google", credential)
	if err != nil || verified.Provider != "google" || verified.Subject != "subject-123" {
		t.Fatalf("credential verify = %#v, %v", verified, err)
	}
}
