package main

import (
	"strings"
	"testing"
)

func TestAppScopeEnabledOnlyAcceptsExplicitTrue(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"false", false}, {"1", false}, {"true", true}, {" TRUE ", true},
	} {
		t.Run(testCase.value, func(t *testing.T) {
			t.Setenv("GATEWAY_APP_SCOPE_ENABLED", testCase.value)
			if got := appScopeEnabled(); got != testCase.want {
				t.Fatalf("appScopeEnabled(%q) = %t, want %t", testCase.value, got, testCase.want)
			}
		})
	}
}

func TestNewOptionalAppScopeResolverDisabledDoesNotReadConfig(t *testing.T) {
	resolver, appID, cleanup, err := newOptionalAppScopeResolver(false, "/private/tmp/missing-gateway-config.yaml")
	if err != nil || resolver != nil || appID != "" || cleanup != nil {
		t.Fatalf("disabled scope = resolver:%T appID:%q cleanup:%t error:%v", resolver, appID, cleanup != nil, err)
	}
}

func TestNewOptionalAppScopeResolverRequiresConfiguredAppID(t *testing.T) {
	t.Setenv("CLING_APP_ID", "")
	resolver, appID, cleanup, err := newOptionalAppScopeResolver(true, "/private/tmp/missing-gateway-config.yaml")
	if err == nil || !strings.Contains(err.Error(), "CLING_APP_ID") {
		t.Fatalf("missing CLING_APP_ID error = %v", err)
	}
	if resolver != nil || appID != "" || cleanup != nil {
		t.Fatalf("missing app ID must not construct dependencies: resolver:%T appID:%q cleanup:%t", resolver, appID, cleanup != nil)
	}
}

func TestNewInitializedAppScopeResolverFailsClosedAndCleansUpOnSchemaFailure(t *testing.T) {
	cleanupCalls := 0
	resolver, appID, cleanup, err := newInitializedAppScopeResolver(nil, "app-id", func() { cleanupCalls++ })
	if err == nil || !strings.Contains(err.Error(), "initialize runtime App MongoDB schema") {
		t.Fatalf("schema initialization error = %v", err)
	}
	if resolver != nil || appID != "" || cleanup != nil {
		t.Fatalf("schema failure must not construct resolver: resolver:%T appID:%q cleanup:%t", resolver, appID, cleanup != nil)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
}
