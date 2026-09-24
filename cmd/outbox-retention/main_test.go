package main

import (
	"strings"
	"testing"
)

func TestRunMutatingModesRequireExplicitConfirmationBeforeConfig(t *testing.T) {
	for _, mode := range []string{"create", "purge"} {
		err := run("does-not-exist.yaml", mode, false, 100)
		if err == nil || !strings.Contains(err.Error(), "--confirm") {
			t.Fatalf("mode %s error = %v, want confirmation error before config", mode, err)
		}
	}
}

func TestRunRejectsInvalidModeOrBatchBeforeConfig(t *testing.T) {
	if err := run("does-not-exist.yaml", "unknown", false, 100); err == nil || !strings.Contains(err.Error(), "未知 mode") {
		t.Fatalf("unknown mode error = %v", err)
	}
	if err := run("does-not-exist.yaml", "purge", true, 0); err == nil || !strings.Contains(err.Error(), "batch-size") {
		t.Fatalf("invalid batch size error = %v", err)
	}
}

func TestRunPreflightValidatesConfigAfterMode(t *testing.T) {
	err := run("does-not-exist.yaml", "preflight", false, 100)
	if err == nil || !strings.Contains(err.Error(), "本地配置") {
		t.Fatalf("preflight error = %v, want config error", err)
	}
}
