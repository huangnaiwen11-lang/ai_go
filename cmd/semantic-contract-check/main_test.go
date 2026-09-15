package main

import (
	"strings"
	"testing"
)

func TestValidateCaseRejectsMissingDatabaseAfterFacts(t *testing.T) {
	raw := []byte(`{
		"case_id":"guest-generation-rejected",
		"consumer":{"method":"POST","path":"/api/v1/creations"},
		"actor":{"account_status":"normal","binding_state":"guest"},
		"expected":{"http":{"status":403,"error_code":"ACCOUNT_BINDING_REQUIRED"}},
		"source":{"kind":"confirmed_business_baseline"}
	}`)

	err := ValidateCase(raw)
	if err == nil || !strings.Contains(err.Error(), "database_after") {
		t.Fatalf("err = %v, want missing database_after", err)
	}
}

func TestValidateCaseRejectsSensitiveFixtureField(t *testing.T) {
	raw := []byte(`{
		"case_id":"invalid-sensitive-fixture",
		"consumer":{"method":"POST","path":"/api/v1/creations"},
		"actor":{"account_status":"normal","binding_state":"bound"},
		"expected":{
			"http":{"status":200},
			"database_before":{"creations":0},
			"database_after":{"creations":1},
			"side_effects":["creation.placeholder.created"]
		},
		"fixture":{"authorization":"secret-value"},
		"source":{"kind":"confirmed_business_baseline"}
	}`)

	err := ValidateCase(raw)
	if err == nil || !strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("err = %v, want sensitive fixture error", err)
	}
}

func TestValidateCaseRejectsMissingTemplateContext(t *testing.T) {
	raw := []byte(`{
		"case_id":"image-generation-missing-template",
		"consumer":{"method":"POST","path":"/api/v1/chat/image/async"},
		"actor":{"account_status":"normal","binding_state":"bound"},
		"fixture":{"product_intent":"template_image"},
		"expected":{
			"http":{"status":200,"error_code":"OK"},
			"database_before":{"creations":0,"reservations":0,"ledger_entries":0},
			"database_after":{"creations":1,"reservations":1,"ledger_entries":1},
			"side_effects":["creation.placeholder.created"]
		},
		"source":{"kind":"confirmed_business_baseline"}
	}`)

	err := ValidateCase(raw)
	if err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("err = %v, want missing template context", err)
	}
}

func TestValidateCaseAcceptsCompleteCase(t *testing.T) {
	raw := []byte(`{
		"case_id":"guest-generation-rejected",
		"consumer":{"method":"POST","path":"/api/v1/creations"},
		"actor":{"account_status":"normal","binding_state":"guest"},
		"template":{"id":"portrait-template","version":"v1","content_surface":"sfw","product_mode":"image"},
		"fixture":{"template_id":"portrait-template"},
		"expected":{
			"http":{"status":403,"error_code":"ACCOUNT_BINDING_REQUIRED"},
			"database_before":{"creations":0,"reservations":0,"ledger_entries":0},
			"database_after":{"creations":0,"reservations":0,"ledger_entries":0},
			"side_effects":[],
			"technical_calls":[]
		},
		"source":{"kind":"confirmed_business_baseline"}
	}`)

	if err := ValidateCase(raw); err != nil {
		t.Fatalf("ValidateCase() error = %v", err)
	}
}
