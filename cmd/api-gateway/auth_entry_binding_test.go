package main

import (
	"os"
	"testing"
)

func TestLocalBindingVerifierRequiresExplicitLocalConfiguration(t *testing.T) {
	t.Setenv("GO_LOCAL_BINDING_VERIFIER_ENABLED", "1")
	t.Setenv("GO_LOCAL_BINDING_SECRET", "production-looking-secret")
	if verifier := localBindingVerifier(); verifier != nil {
		t.Fatal("verifier enabled with a non-local secret")
	}

	_ = os.Unsetenv("GO_LOCAL_BINDING_VERIFIER_ENABLED")
	t.Setenv("GO_LOCAL_BINDING_SECRET", "local-test-secret")
	if verifier := localBindingVerifier(); verifier != nil {
		t.Fatal("verifier enabled without explicit switch")
	}
}
