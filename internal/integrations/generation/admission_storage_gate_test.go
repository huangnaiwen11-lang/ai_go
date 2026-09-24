package generation

import (
	"errors"
	"testing"

	"ai-business-service/internal/conf"
)

func TestB2BAdmissionStorageReadiness本地不读取R2且B2B缺失拒绝(t *testing.T) {
	clearR2Environment(t)
	local, err := NewB2BAdmissionStorageReadiness(integrationsFor(&conf.Integrations_Generation{}))
	if err != nil {
		t.Fatalf("local readiness = %v", err)
	}
	if err := local.RequireB2B(); !errors.Is(err, ErrAdmissionStorageConfig) {
		t.Fatalf("local readiness must not authorize B2B: %v", err)
	}
	if _, err := NewB2BAdmissionStorageReadiness(integrationsFor(b2bGenerationConfig())); !errors.Is(err, ErrAdmissionStorageConfig) {
		t.Fatalf("B2B without R2 was accepted: %v", err)
	}
}

func TestB2BAdmissionStorageReadiness完整R2允许安全Admission构造(t *testing.T) {
	setR2Environment(t)
	readiness, err := NewB2BAdmissionStorageReadiness(integrationsFor(b2bGenerationConfig()))
	if err != nil {
		t.Fatalf("B2B readiness = %v", err)
	}
	if err := readiness.RequireB2B(); err != nil {
		t.Fatalf("complete R2 must authorize B2B admission: %v", err)
	}
	resolver, err := NewAdmissionResolverWithStorageReadiness(integrationsFor(b2bGenerationConfig()), &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}, readiness)
	if err != nil {
		t.Fatalf("safe B2B admission constructor = %v", err)
	}
	if resolver == nil {
		t.Fatal("safe B2B admission resolver is nil")
	}
}

func clearR2Environment(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY", "R2_BUCKET_NAME", "R2_PUBLIC_URL",
		"R2_PRIVATE_BUCKET_NAME", "R2_PRIVATE_PUBLIC_URL", "LEGAL_PAGE_R2_BUCKET_NAME",
	} {
		t.Setenv(key, "")
	}
}

func setR2Environment(t *testing.T) {
	t.Helper()
	t.Setenv("R2_ACCOUNT_ID", "account1")
	t.Setenv("R2_ACCESS_KEY_ID", "access1")
	t.Setenv("R2_SECRET_ACCESS_KEY", "secret1")
	t.Setenv("R2_BUCKET_NAME", "cling-ai")
	t.Setenv("R2_PUBLIC_URL", "https://media.example.test")
	for _, key := range []string{"R2_PRIVATE_BUCKET_NAME", "R2_PRIVATE_PUBLIC_URL", "LEGAL_PAGE_R2_BUCKET_NAME"} {
		t.Setenv(key, "")
	}
}
