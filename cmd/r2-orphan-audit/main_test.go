package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsInvalidMaxKeysBeforeReadingConfig(t *testing.T) {
	err := run("does-not-exist.yaml", "", 0, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "max-keys") {
		t.Fatalf("run() error = %v, want max-keys validation error", err)
	}
}

func TestRunValidatesConfigBeforeOpeningR2(t *testing.T) {
	err := run("does-not-exist.yaml", "", 100, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "本地配置") {
		t.Fatalf("run() error = %v, want config error", err)
	}
}
