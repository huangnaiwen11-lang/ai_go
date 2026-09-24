package r2

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadConfig未配置时保持禁用(t *testing.T) {
	config, enabled, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if enabled {
		t.Fatalf("enabled = true, want false; config = %#v", config)
	}
}

func TestLoadConfig拒绝局部配置且不回显凭据(t *testing.T) {
	const secret = "secret-must-never-appear-in-an-error"
	_, enabled, err := LoadConfig(environment(map[string]string{
		"R2_ACCOUNT_ID":        "account1",
		"R2_ACCESS_KEY_ID":     "access-1",
		"R2_SECRET_ACCESS_KEY": secret,
	}))
	if enabled || !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("LoadConfig() enabled/error = %v/%v, want false/ErrIncompleteConfig", enabled, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("configuration error must not expose credentials: %v", err)
	}
}

func TestLoadConfig使用冻结的R2Endpoint与公开桶默认值(t *testing.T) {
	config, enabled, err := LoadConfig(environment(map[string]string{
		"R2_ACCOUNT_ID":        "d8464084f6d843a3a5a4469be534ae4d",
		"R2_ACCESS_KEY_ID":     "access-1",
		"R2_SECRET_ACCESS_KEY": "secret-1",
		"R2_PUBLIC_URL":        "https://pub-d8464084f6d843a3a5a4469be534ae4d.r2.dev",
	}))
	if err != nil || !enabled {
		t.Fatalf("LoadConfig() enabled/error = %v/%v", enabled, err)
	}
	if config.Region != "auto" {
		t.Fatalf("Region = %q, want auto", config.Region)
	}
	if config.PublicBucket != "ai-host" {
		t.Fatalf("PublicBucket = %q, want ai-host", config.PublicBucket)
	}
	if config.Endpoint() != "https://d8464084f6d843a3a5a4469be534ae4d.r2.cloudflarestorage.com" {
		t.Fatalf("Endpoint() = %q", config.Endpoint())
	}
	if got := config.PublicObjectURL("images/result.png"); got != "https://pub-d8464084f6d843a3a5a4469be534ae4d.r2.dev/images/result.png" {
		t.Fatalf("PublicObjectURL() = %q", got)
	}
}

func TestLoadConfig拒绝不安全的账号和公开基址(t *testing.T) {
	base := map[string]string{
		"R2_ACCOUNT_ID":        "account1",
		"R2_ACCESS_KEY_ID":     "access-1",
		"R2_SECRET_ACCESS_KEY": "secret-1",
		"R2_BUCKET_NAME":       "cling-ai",
		"R2_PUBLIC_URL":        "https://media.example.test",
	}
	for name, mutate := range map[string]func(map[string]string){
		"account contains host delimiter":         func(values map[string]string) { values["R2_ACCOUNT_ID"] = "account1/path" },
		"public url must be HTTPS origin":         func(values map[string]string) { values["R2_PUBLIC_URL"] = "http://media.example.test" },
		"public url must not contain path":        func(values map[string]string) { values["R2_PUBLIC_URL"] = "https://media.example.test/path" },
		"public url must not have trailing slash": func(values map[string]string) { values["R2_PUBLIC_URL"] = "https://media.example.test/" },
	} {
		t.Run(name, func(t *testing.T) {
			values := make(map[string]string, len(base))
			for key, value := range base {
				values[key] = value
			}
			mutate(values)
			_, enabled, err := LoadConfig(environment(values))
			if enabled || !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("LoadConfig() enabled/error = %v/%v, want false/ErrInvalidConfig", enabled, err)
			}
		})
	}
}

func TestLoadConfig私有桶是显式可选能力(t *testing.T) {
	config, enabled, err := LoadConfig(environment(map[string]string{
		"R2_ACCOUNT_ID":             "account1",
		"R2_ACCESS_KEY_ID":          "access-1",
		"R2_SECRET_ACCESS_KEY":      "secret-1",
		"R2_BUCKET_NAME":            "cling-ai",
		"R2_PUBLIC_URL":             "https://media.example.test",
		"R2_PRIVATE_BUCKET_NAME":    "cling-ai-private",
		"R2_PRIVATE_PUBLIC_URL":     "https://private.example.test",
		"LEGAL_PAGE_R2_BUCKET_NAME": "cling-legal-private",
	}))
	if err != nil || !enabled {
		t.Fatalf("LoadConfig() enabled/error = %v/%v", enabled, err)
	}
	if config.PrivateBucket != "cling-ai-private" || config.PrivatePublicURL != "https://private.example.test" || config.LegalPageBucket != "cling-legal-private" {
		t.Fatalf("private config = %#v", config)
	}
	if config.LegalBucket() != "cling-legal-private" {
		t.Fatalf("LegalBucket() = %q", config.LegalBucket())
	}
}

func TestLoadConfig法律页桶按冻结规则回退私有桶(t *testing.T) {
	config, enabled, err := LoadConfig(environment(map[string]string{
		"R2_ACCOUNT_ID":          "account1",
		"R2_ACCESS_KEY_ID":       "access-1",
		"R2_SECRET_ACCESS_KEY":   "secret-1",
		"R2_BUCKET_NAME":         "cling-ai",
		"R2_PUBLIC_URL":          "https://media.example.test",
		"R2_PRIVATE_BUCKET_NAME": "cling-ai-private",
	}))
	if err != nil || !enabled {
		t.Fatalf("LoadConfig() enabled/error = %v/%v", enabled, err)
	}
	if config.LegalBucket() != "cling-ai-private" {
		t.Fatalf("LegalBucket() = %q, want private fallback", config.LegalBucket())
	}
}

func environment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
