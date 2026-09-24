package adminapps

import (
	"errors"
	"regexp"
	"testing"
	"time"
)

func TestBuildLegalPublishingCandidate(t *testing.T) {
	now := time.Date(2026, time.September, 24, 8, 0, 0, 0, time.UTC)
	candidate, err := BuildLegalPublishingCandidate(map[string]any{
		"approvedBrandDomain": "Legal.Example.com",
		"publisherBinding": map[string]any{
			"provider":           "aws_cloudfront",
			"distributionId":     "E1234ABC",
			"originHostname":     "origin.example.com",
			"repository":         "owner/repo",
			"workflowPath":       ".github/workflows/publish.yml",
			"environment":        "production",
			"managedIdentityRef": "ssh-alias:legal-publisher",
		},
	}, "admin-1", now)
	if err != nil {
		t.Fatalf("BuildLegalPublishingCandidate() error = %v", err)
	}
	if candidate.Status != "pending" || candidate.ApprovedDomain != "legal.example.com" {
		t.Fatalf("candidate = %#v", candidate)
	}
	binding := FieldObject(candidate.LegalPublishing, "publisherBinding")
	if token := FieldString(binding, "verificationToken"); !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(token) {
		t.Errorf("verificationToken = %q, want 64-char lower hex", token)
	}
	if binding["updatedAt"] != now || binding["updatedBy"] != "admin-1" {
		t.Errorf("server fields = %#v", binding)
	}
}

func TestBuildLegalPublishingCandidateRejectsInvalidInput(t *testing.T) {
	base := map[string]any{
		"approvedBrandDomain": "legal.example.com",
		"publisherBinding":    map[string]any{"provider": "cloudflare_r2"},
	}
	for name, mutate := range map[string]func(map[string]any){
		"IP hostname":          func(input map[string]any) { input["approvedBrandDomain"] = "127.0.0.1" },
		"reserved hostname":    func(input map[string]any) { input["approvedBrandDomain"] = "localhost.localdomain" },
		"empty hostname label": func(input map[string]any) { input["approvedBrandDomain"] = "a..com" },
		"hostname scheme":      func(input map[string]any) { input["approvedBrandDomain"] = "https://legal.example.com" },
		"hostname path":        func(input map[string]any) { input["approvedBrandDomain"] = "legal.example.com/path" },
		"hostname port":        func(input map[string]any) { input["approvedBrandDomain"] = "legal.example.com:443" },
		"client approved":      func(input map[string]any) { input["publisherBinding"].(map[string]any)["status"] = "approved" },
		"invalid distribution": func(input map[string]any) { input["publisherBinding"].(map[string]any)["distributionId"] = "bad" },
		"invalid repository": func(input map[string]any) {
			input["publisherBinding"].(map[string]any)["repository"] = "owner/repo/extra"
		},
		"invalid workflow": func(input map[string]any) {
			input["publisherBinding"].(map[string]any)["workflowPath"] = "workflow.yml"
		},
		"invalid environment": func(input map[string]any) { input["publisherBinding"].(map[string]any)["environment"] = "prod env" },
		"invalid identity reference": func(input map[string]any) {
			input["publisherBinding"].(map[string]any)["managedIdentityRef"] = "secret-value"
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := map[string]any{"approvedBrandDomain": base["approvedBrandDomain"], "publisherBinding": map[string]any{"provider": "cloudflare_r2"}}
			mutate(input)
			_, err := BuildLegalPublishingCandidate(input, "admin-1", time.Now())
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("BuildLegalPublishingCandidate() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestLegalPublishingPolicyAndActivePages(t *testing.T) {
	current := Document{"legalPublishing": Document{"approvedBrandDomain": "legal.example.com", "publisherBinding": Document{"provider": "cloudflare_r2"}}, "legalPages": Document{"privacy": Document{"currentVersionId": "v1"}}}
	equivalent := Document{"legalPublishing": Document{"approvedBrandDomain": "legal.example.com", "publisherBinding": Document{"provider": "cloudflare_r2", "verificationToken": "different"}}}
	changed := Document{"legalPublishing": Document{"approvedBrandDomain": "new.example.com", "publisherBinding": Document{"provider": "cloudflare_r2"}}}
	if !HasActiveLegalPages(current) {
		t.Fatal("HasActiveLegalPages() = false, want true")
	}
	if !SameLegalPublishingPolicy(current, equivalent) {
		t.Fatal("SameLegalPublishingPolicy() = false, want true")
	}
	if SameLegalPublishingPolicy(current, changed) {
		t.Fatal("SameLegalPublishingPolicy() = true, want false")
	}
}
