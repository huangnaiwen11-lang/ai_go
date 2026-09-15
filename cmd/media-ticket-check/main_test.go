package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun接受完整的脱敏视频票据(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	readFile := func(path string) ([]byte, error) {
		if path != "ticket.json" {
			t.Fatalf("read path = %q, want ticket.json", path)
		}
		return validTicketManifest(), nil
	}

	code := run([]string{"--manifest", "ticket.json"}, readFile, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "媒体票据门禁通过：仅表示本地票据字段完整，不代表上传或迁移许可。\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRun拒绝未知字段且不回显签名地址(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	manifest := strings.Replace(string(validTicketManifest()), "}", ",\"unexpected\":true}", 1)
	readFile := func(string) ([]byte, error) { return []byte(manifest), nil }

	code := run([]string{"--manifest", "ticket.json"}, readFile, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "媒体票据无效。\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if strings.Contains(stderr.String(), "private-signature") {
		t.Fatalf("stderr must not reveal ticket data: %q", stderr.String())
	}
}

func TestRun拒绝缺少参数与读取失败(t *testing.T) {
	t.Run("缺少参数", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer

		code := run(nil, func(string) ([]byte, error) { return nil, nil }, &stdout, &stderr)

		if code != 2 {
			t.Fatalf("run exit code = %d, want 2", code)
		}
		if got, want := stderr.String(), "用法：media-ticket-check --manifest <脱敏票据文件>\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("读取失败", func(t *testing.T) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		readFile := func(string) ([]byte, error) { return nil, errors.New("permission denied") }

		code := run([]string{"--manifest", "ticket.json"}, readFile, &stdout, &stderr)

		if code != 2 {
			t.Fatalf("run exit code = %d, want 2", code)
		}
		if got, want := stderr.String(), "无法读取媒体票据。\n"; got != want {
			t.Fatalf("stderr = %q, want %q", got, want)
		}
	})
}

func Test示例脱敏票据可通过本地校验(t *testing.T) {
	examplePath := filepath.Join("..", "..", "docs", "audit", "media-ticket-manifest.example.json")
	content, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("读取示例票据失败：%v", err)
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
		t.Fatalf("示例票据 exit code = %d, stderr = %q", code, stderr.String())
	}
}

func validTicketManifest() []byte {
	return []byte(`{
  "request": {
    "kind": "video",
    "contentType": "video/mp4",
    "fileSizeBytes": 1024
  },
  "ticket": {
    "uploadURL": "https://upload.example.test/ugc/example/videos/clip.mp4?X-Amz-Signature=private-signature",
    "publicURL": "",
    "key": "ugc/example/videos/clip.mp4",
    "filename": "ugc/example/videos/clip.mp4",
    "contentType": "video/mp4",
    "maxBytes": 52428800,
    "headers": {
      "Content-Type": ["video/mp4"]
    }
  }
}`)
}
