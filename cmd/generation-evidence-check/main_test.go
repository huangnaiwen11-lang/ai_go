package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun接受完整的脱敏证据清单(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	readFile := func(path string) ([]byte, error) {
		if path != "evidence.json" {
			t.Fatalf("read path = %q, want evidence.json", path)
		}
		return completeManifest(), nil
	}

	code := run([]string{"--manifest", "evidence.json"}, readFile, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "证据门禁通过：仅表示本地清单完整，不代表迁移或切流许可。\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRun拒绝未知字段且不回显脱敏材料引用(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	manifest := append([]byte{}, completeManifest()...)
	manifest = []byte(strings.Replace(string(manifest), "}", ",\"unexpected\":true}", 1))
	readFile := func(string) ([]byte, error) { return manifest, nil }

	code := run([]string{"--manifest", "evidence.json"}, readFile, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "证据清单无效。\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if strings.Contains(stderr.String(), "sha256:example") {
		t.Fatalf("stderr must not reveal artifact reference: %q", stderr.String())
	}
}

func TestRun拒绝缺少清单参数与无法读取的文件(t *testing.T) {
	t.Run("缺少参数", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		code := run(nil, func(string) ([]byte, error) { return nil, nil }, &stdout, &stderr)

		if code != 2 {
			t.Fatalf("run exit code = %d, want 2", code)
		}
		if got, want := stderr.String(), "用法：generation-evidence-check --manifest <脱敏清单文件>\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("读取失败", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		readFile := func(string) ([]byte, error) { return nil, errors.New("permission denied") }

		code := run([]string{"--manifest", "evidence.json"}, readFile, &stdout, &stderr)

		if code != 2 {
			t.Fatalf("run exit code = %d, want 2", code)
		}
		if got, want := stderr.String(), "无法读取证据清单。\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
	})
}

func Test示例脱敏证据清单可通过本地校验(t *testing.T) {
	examplePath := filepath.Join("..", "..", "docs", "audit", "generation-evidence-manifest.example.json")
	content, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("读取示例清单失败：%v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		[]string{"--manifest", "example.json"},
		func(string) ([]byte, error) { return content, nil },
		&stdout,
		&stderr,
	)

	if code != 0 {
		t.Fatalf("示例清单 exit code = %d, stderr = %q", code, stderr.String())
	}
}

func completeManifest() []byte {
	return []byte(`{
  "mode": "text_to_image",
  "route": "POST /api/chat/image/async",
  "scope": "inputImages=absent",
  "evidence": [
    {"kind": "request_validation", "artifactRef": "sha256:example"},
    {"kind": "authentication", "artifactRef": "sha256:example"},
    {"kind": "account_eligibility", "artifactRef": "sha256:example"},
    {"kind": "sensitive_rate_limit", "artifactRef": "sha256:example"},
    {"kind": "content_policy", "artifactRef": "sha256:example"},
    {"kind": "billing_settlement", "artifactRef": "sha256:example"},
    {"kind": "task_persistence", "artifactRef": "sha256:example"},
    {"kind": "idempotent_submission", "artifactRef": "sha256:example"},
    {"kind": "callback_convergence", "artifactRef": "sha256:example"},
    {"kind": "refund_convergence", "artifactRef": "sha256:example"}
  ]
}`)
}
