package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRun接受完整的Identity脱敏证据清单(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	readFile := func(path string) ([]byte, error) {
		if path != "identity.json" {
			t.Fatalf("read path = %q, want identity.json", path)
		}
		return completeIdentityManifest(), nil
	}

	code := run([]string{"--manifest", "identity.json"}, readFile, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "迁移证据门禁通过：仅表示本地清单完整，不代表迁移或切流许可。\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRun拒绝敏感字段且不回显其内容(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	readFile := func(string) ([]byte, error) {
		return []byte(`{"stage":"identity","authorization":"Bearer private-token","evidence":[]}`), nil
	}

	code := run([]string{"--manifest", "identity.json"}, readFile, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "迁移证据清单无效。\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if bytes.Contains(stderr.Bytes(), []byte("private-token")) {
		t.Fatalf("stderr must not reveal sensitive content: %q", stderr.String())
	}
}

func TestRun拒绝读取失败(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run(
		[]string{"--manifest", "identity.json"},
		func(string) ([]byte, error) { return nil, errors.New("permission denied") },
		&stdout,
		&stderr,
	)

	if code != 2 {
		t.Fatalf("run exit code = %d, want 2", code)
	}
	if got, want := stderr.String(), "无法读取迁移证据清单。\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func Test三个阶段的示例清单可通过本地校验(t *testing.T) {
	for _, filename := range []string{
		"identity-migration-evidence.example.json",
		"wallet-billing-migration-evidence.example.json",
		"admin-migration-evidence.example.json",
	} {
		t.Run(filename, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join("..", "..", "docs", "audit", filename))
			if err != nil {
				t.Fatalf("读取示例清单失败：%v", err)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run(
				[]string{"--manifest", filename},
				func(string) ([]byte, error) { return content, nil },
				&stdout,
				&stderr,
			)

			if code != 0 {
				t.Fatalf("示例清单 exit code = %d, stderr = %q", code, stderr.String())
			}
		})
	}
}

func completeIdentityManifest() []byte {
	return []byte(`{
  "stage": "identity",
  "evidence": [
    {"kind": "credential_compatibility", "artifactRef": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
    {"kind": "account_scope_rejection", "artifactRef": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
    {"kind": "oauth_and_magic_link", "artifactRef": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
    {"kind": "login_lock_and_audit", "artifactRef": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
    {"kind": "unauthorized_envelope", "artifactRef": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
    {"kind": "registration_idempotency", "artifactRef": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
  ]
}`)
}
