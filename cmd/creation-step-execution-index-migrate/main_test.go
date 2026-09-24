package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"ai-business-service/internal/data/migrate"
)

func TestRunDefaultsToReadOnlyPreflight(t *testing.T) {
	fake := &fakeCreationStepIndexMigration{report: migrate.CreationStepExecutionIndexPreflight{DocumentsWithJob: 4}}
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--mongo-uri", "mongodb://migration-test", "--database", "cling_main"},
		fakeCreationStepIndexOpener(fake), &stdout, &stderr,
	)
	if code != 0 || fake.preflightCalls != 1 || fake.ensureCalls != 0 || fake.retireCalls != 0 {
		t.Fatalf("run result code=%d preflight=%d ensure=%d retire=%d stderr=%q", code, fake.preflightCalls, fake.ensureCalls, fake.retireCalls, stderr.String())
	}
	if !strings.Contains(stdout.String(), "预检通过") || !strings.Contains(stdout.String(), "未创建或删除索引") {
		t.Fatalf("read-only output = %q", stdout.String())
	}
}

func TestRunRejectsLegacyRetirementWithoutWriterConfirmation(t *testing.T) {
	fake := &fakeCreationStepIndexMigration{report: migrate.CreationStepExecutionIndexPreflight{}}
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--mongo-uri", "mongodb://migration-test", "--database", "cling_main", "--mode", modeRetireLegacy},
		fakeCreationStepIndexOpener(fake), &stdout, &stderr,
	)
	if code != 2 || fake.preflightCalls != 0 || fake.ensureCalls != 0 || fake.retireCalls != 0 || !strings.Contains(stderr.String(), "--confirm-scoped-writers") {
		t.Fatalf("unsafe retire result code=%d calls=%#v stderr=%q", code, fake, stderr.String())
	}
}

func TestRunDoesNotCreateScopedIndexWhenPreflightIsBlocked(t *testing.T) {
	fake := &fakeCreationStepIndexMigration{report: migrate.CreationStepExecutionIndexPreflight{DocumentsWithJob: 1, InvalidRouteDocuments: 1}}
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--mongo-uri", "mongodb://migration-test", "--database", "cling_main", "--mode", modeCreateScoped},
		fakeCreationStepIndexOpener(fake), &stdout, &stderr,
	)
	if code != 1 || fake.preflightCalls != 1 || fake.ensureCalls != 0 || fake.retireCalls != 0 || !strings.Contains(stderr.String(), "预检未通过") {
		t.Fatalf("blocked create result code=%d calls=%#v stderr=%q", code, fake, stderr.String())
	}
}

type fakeCreationStepIndexMigration struct {
	report         migrate.CreationStepExecutionIndexPreflight
	preflightCalls int
	ensureCalls    int
	retireCalls    int
}

func (fake *fakeCreationStepIndexMigration) Preflight(context.Context) (migrate.CreationStepExecutionIndexPreflight, error) {
	fake.preflightCalls++
	return fake.report, nil
}

func (fake *fakeCreationStepIndexMigration) EnsureScopedUnique(context.Context) (migrate.CreationStepExecutionIndexPreflight, error) {
	fake.ensureCalls++
	return fake.report, nil
}

func (fake *fakeCreationStepIndexMigration) RetireLegacyGlobalUnique(_ context.Context, _ bool) (migrate.CreationStepExecutionIndexPreflight, error) {
	fake.retireCalls++
	return fake.report, nil
}

func fakeCreationStepIndexOpener(migration creationStepIndexMigration) creationStepIndexOpener {
	return func(context.Context, string, string) (creationStepIndexMigration, func(context.Context), error) {
		return migration, func(context.Context) {}, nil
	}
}
