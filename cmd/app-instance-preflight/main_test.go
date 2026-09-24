package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunAcceptsExplicitFlagsAndRedactsMongoURI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	secretURI := "mongodb://operator:super-secret@db.example.test:27017/cling_production?replicaSet=rs0"
	code := run([]string{
		"--app-id", "cling-production-cn",
		"--public-origin", "https://app.example.test",
		"--mongo-uri", secretURI,
		"--mongo-database", "cling_production",
		"--r2-public-bucket", "cling-production-public",
		"--r2-private-bucket", "cling-production-private",
		"--polarstar-account-ref", "account-production-cn",
		"--polarstar-tenant-id", "tenant-production-cn",
		"--callback-origin", "https://app.example.test",
	}, func(name string) string {
		if name == "GATEWAY_APP_SCOPE_ENABLED" {
			return "true"
		}
		if name == "CLING_MONGO_PROFILE" {
			return "production"
		}
		return ""
	}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `"status":"ready"`) || !strings.Contains(got, `"app_id":"cling-production-cn"`) || strings.Contains(got, secretURI) {
		t.Fatalf("stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunUsesExistingEnvironmentNames(t *testing.T) {
	values := completeEnvironment()
	var stdout, stderr bytes.Buffer
	if code := run(nil, func(name string) string { return values[name] }, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
}

func TestRunRequiresEnabledAppScope(t *testing.T) {
	values := completeEnvironment()
	values["GATEWAY_APP_SCOPE_ENABLED"] = "false"
	var stdout, stderr bytes.Buffer
	if code := run(nil, func(name string) string { return values[name] }, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "GATEWAY_APP_SCOPE_ENABLED") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunRequiresProductionMongoProfile(t *testing.T) {
	values := completeEnvironment()
	values["CLING_MONGO_PROFILE"] = "staging"
	var stdout, stderr bytes.Buffer
	if code := run(nil, func(name string) string { return values[name] }, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "CLING_MONGO_PROFILE") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunRejectsUnsafeConfigurationWithoutLeakingMongoURI(t *testing.T) {
	values := completeEnvironment()
	values["CLING_MONGO_URI"] = "mongodb://operator:super-secret@127.0.0.1:27017/cling_production"
	var stdout, stderr bytes.Buffer
	if code := run(nil, func(name string) string { return values[name] }, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1; stderr = %s", code, stderr.String())
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), values["CLING_MONGO_URI"]) || !strings.Contains(stderr.String(), "MongoURI") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunHelpDoesNotEchoEnvironmentMongoURI(t *testing.T) {
	values := completeEnvironment()
	values["CLING_MONGO_URI"] = "mongodb://operator:super-secret@db.example.test:27017/cling_production"
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, func(name string) string { return values[name] }, &stdout, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if strings.Contains(stderr.String(), values["CLING_MONGO_URI"]) {
		t.Fatalf("help leaks Mongo URI: %q", stderr.String())
	}
}

func completeEnvironment() map[string]string {
	return map[string]string{
		"CLING_APP_ID":                  "cling-production-cn",
		"CLING_MONGO_PROFILE":           "production",
		"CLING_PUBLIC_ORIGIN":           "https://app.example.test",
		"CLING_MONGO_URI":               "mongodb://db.example.test:27017/cling_production?replicaSet=rs0",
		"CLING_MONGO_DATABASE":          "cling_production",
		"R2_BUCKET_NAME":                "cling-production-public",
		"R2_PRIVATE_BUCKET_NAME":        "cling-production-private",
		"POLARSTAR_B2B_ACCOUNT_REF":     "account-production-cn",
		"POLARSTAR_B2B_TENANT_ID":       "tenant-production-cn",
		"POLARSTAR_B2B_CALLBACK_ORIGIN": "https://app.example.test",
		"GATEWAY_APP_SCOPE_ENABLED":     "true",
	}
}
