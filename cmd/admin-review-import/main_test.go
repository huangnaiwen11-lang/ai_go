package main

import (
	"bytes"
	"testing"
)

func TestRunRequiresExplicitLegacyAndTargetConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--mode=dry-run"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, configuration error must not emit report", stdout.String())
	}
}

func TestRunRejectsReadyConfirmationInDryRun(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{
		"--legacy-uri=mongodb://legacy", "--legacy-database=legacy",
		"--target-uri=mongodb://target", "--target-database=cling_main",
		"--mode=dry-run", "--confirm-ready",
	}
	if code := run(args, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
}

func TestRunRejectsSameSourceAndTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{
		"--legacy-uri=mongodb://same", "--legacy-database=legacy",
		"--target-uri=mongodb://same", "--target-database=legacy",
		"--mode=dry-run",
	}
	if code := run(args, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
}
