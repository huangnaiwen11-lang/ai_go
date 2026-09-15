package main

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"ai-business-service/internal/catalogcontract"
)

func TestReplayConfigRequiresExplicitCandidateUpstream(t *testing.T) {
	_, err := replayConfigFromEnv(func(string) string { return "" })
	if err == nil {
		t.Fatal("replayConfigFromEnv accepted an empty candidate upstream")
	}
}

func TestReplayConfigAcceptsHTTPSCandidateAndDefaultsBaseline(t *testing.T) {
	config, err := replayConfigFromEnv(func(name string) string {
		if name == "CATALOG_CANDIDATE_UPSTREAM" {
			return "https://candidate.example"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("replayConfigFromEnv() error = %v", err)
	}
	if config.baselineUpstream != "http://127.0.0.1:4000" {
		t.Fatalf("baseline upstream = %q, want default", config.baselineUpstream)
	}
	if config.candidateUpstream != "https://candidate.example" {
		t.Fatalf("candidate upstream = %q", config.candidateUpstream)
	}
}

func TestReplayConfigRejectsNonHTTPCandidate(t *testing.T) {
	_, err := replayConfigFromEnv(func(name string) string {
		if name == "CATALOG_CANDIDATE_UPSTREAM" {
			return "ftp://candidate.example"
		}
		return ""
	})
	if err == nil {
		t.Fatal("replayConfigFromEnv accepted a non-HTTP candidate upstream")
	}
}

func TestFixedReplayCasesMatchAuditedWhitelist(t *testing.T) {
	wantCases := []struct {
		name               string
		path               string
		rawQuery           string
		platform           string
		conditional        bool
		requireCacheStatus bool
		etagPolicy         catalogcontract.ETagPolicy
	}{
		{name: "api-query", path: "/api/homepage/video-templates", rawQuery: "category=all&limit=24", platform: "web", requireCacheStatus: true, etagPolicy: catalogcontract.ETagForbidden},
		{name: "api-v1-query", path: "/api/v1/homepage/video-templates", rawQuery: "category=all&limit=24", platform: "web", requireCacheStatus: true, etagPolicy: catalogcontract.ETagForbidden},
		{name: "api-no-query", path: "/api/homepage/video-templates", rawQuery: "", platform: "web", requireCacheStatus: true, etagPolicy: catalogcontract.ETagForbidden},
		{name: "api-limit-min", path: "/api/homepage/video-templates", rawQuery: "category=all&limit=1&offset=0", platform: "web", requireCacheStatus: true, etagPolicy: catalogcontract.ETagForbidden},
		{name: "api-limit-max", path: "/api/homepage/video-templates", rawQuery: "category=all&limit=100&offset=1", platform: "web", requireCacheStatus: true, etagPolicy: catalogcontract.ETagForbidden},
		{name: "api-query-normalization", path: "/api/homepage/video-templates", rawQuery: "category=unrecognized&limit=not-a-number&offset=-1", platform: "web", requireCacheStatus: true, etagPolicy: catalogcontract.ETagForbidden},
		{name: "api-max-rating-sfw", path: "/api/homepage/video-templates", rawQuery: "category=all&limit=24&maxRating=sfw", platform: "web", requireCacheStatus: true, etagPolicy: catalogcontract.ETagForbidden},
		{name: "api-ios-platform", path: "/api/homepage/video-templates", rawQuery: "category=all&limit=24", platform: "ios", requireCacheStatus: true, etagPolicy: catalogcontract.ETagForbidden},
		{name: "api-android-platform", path: "/api/homepage/video-templates", rawQuery: "category=all&limit=24", platform: "android", requireCacheStatus: true, etagPolicy: catalogcontract.ETagForbidden},
		{name: "homepage-content-etag", path: "/api/homepage/content", rawQuery: "catalog=contract-replay", platform: "web", conditional: true, requireCacheStatus: true, etagPolicy: catalogcontract.ETagRequired},
	}
	gotCases := fixedReplayCases()
	if len(gotCases) != len(wantCases) {
		t.Fatalf("fixed replay case count = %d, want %d", len(gotCases), len(wantCases))
	}

	for index, wantCase := range wantCases {
		t.Run(wantCase.name, func(t *testing.T) {
			gotCase := gotCases[index]
			gotRequest := gotCase.requestCase
			if gotRequest.Name != wantCase.name || gotRequest.Path != wantCase.path || gotRequest.RawPath != "" || gotRequest.RawQuery != wantCase.rawQuery || gotCase.conditional != wantCase.conditional {
				t.Fatalf("fixed replay case = %+v, want name=%q path=%q empty RawPath rawQuery=%q conditional=%t", gotCase, wantCase.name, wantCase.path, wantCase.rawQuery, wantCase.conditional)
			}
			if gotRequest.CacheContract.RequireCacheStatus != wantCase.requireCacheStatus || gotRequest.CacheContract.ETagPolicy != wantCase.etagPolicy {
				t.Fatalf("fixed replay case cache contract = %+v, want requireCacheStatus=%t etagPolicy=%d", gotRequest.CacheContract, wantCase.requireCacheStatus, wantCase.etagPolicy)
			}
			if err := validateAnonymousReplayHeaderWhitelist(gotRequest.Header, wantCase.name, wantCase.platform); err != nil {
				t.Fatalf("fixed replay case headers are invalid: %v", err)
			}
		})
	}
}

func TestValidateAnonymousReplayHeaderWhitelist(t *testing.T) {
	testCases := []struct {
		name      string
		header    http.Header
		platform  string
		wantError string
	}{
		{
			name: "canonical keys pass",
			header: http.Header{
				"X-Request-Id":      {"catalog-contract-replay-api-query"},
				"X-Client-Platform": {"web"},
			},
			platform: "web",
		},
		{
			name: "iOS platform pass",
			header: http.Header{
				"x-request-id":      {"catalog-contract-replay-api-ios-platform"},
				"x-client-platform": {"ios"},
			},
			platform: "ios",
		},
		{
			name: "extra authorization fails",
			header: http.Header{
				"X-Request-Id":      {"catalog-contract-replay-api-query"},
				"X-Client-Platform": {"web"},
				"Authorization":     {"Bearer private"},
			},
			platform:  "web",
			wantError: "unexpected header",
		},
		{
			name: "duplicate request ID casing fails",
			header: http.Header{
				"X-Request-Id":      {"catalog-contract-replay-api-query"},
				"x-request-id":      {"catalog-contract-replay-api-query"},
				"X-Client-Platform": {"web"},
			},
			platform:  "web",
			wantError: "appears more than once",
		},
		{
			name: "single request ID key with two values fails",
			header: http.Header{
				"X-Request-Id":      {"catalog-contract-replay-api-query", "duplicate"},
				"X-Client-Platform": {"web"},
			},
			platform:  "web",
			wantError: "has 2 values, want exactly one",
		},
		{
			name: "platform mismatch fails",
			header: http.Header{
				"X-Request-Id":      {"catalog-contract-replay-api-ios-platform"},
				"X-Client-Platform": {"web"},
			},
			platform:  "ios",
			wantError: "header value",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			caseName := "api-query"
			if testCase.platform == "ios" {
				caseName = "api-ios-platform"
			}
			err := validateAnonymousReplayHeaderWhitelist(testCase.header, caseName, testCase.platform)
			if testCase.wantError == "" {
				if err != nil {
					t.Fatalf("validateAnonymousReplayHeaderWhitelist() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("validateAnonymousReplayHeaderWhitelist() error = %v, want %q", err, testCase.wantError)
			}
		})
	}
}

func validateAnonymousReplayHeaderWhitelist(header http.Header, caseName, platform string) error {
	expectedHeaders := []struct {
		logicalName string
		displayName string
		value       string
	}{
		{
			logicalName: "x-request-id",
			displayName: "X-Request-Id",
			value:       "catalog-contract-replay-" + caseName,
		},
		{
			logicalName: "x-client-platform",
			displayName: "X-Client-Platform",
			value:       platform,
		},
	}
	seen := make(map[string]bool, len(expectedHeaders))
	for actualName, values := range header {
		logicalName := strings.ToLower(actualName)
		expectedIndex := -1
		for index := range expectedHeaders {
			if expectedHeaders[index].logicalName == logicalName {
				expectedIndex = index
				break
			}
		}
		if expectedIndex == -1 {
			return fmt.Errorf("unexpected header %q", actualName)
		}
		expected := expectedHeaders[expectedIndex]
		if seen[logicalName] {
			return fmt.Errorf("%s header appears more than once", expected.displayName)
		}
		seen[logicalName] = true
		if len(values) != 1 {
			return fmt.Errorf("%s header has %d values, want exactly one", expected.displayName, len(values))
		}
		if values[0] != expected.value {
			return fmt.Errorf("%s header value = %q, want %q", expected.displayName, values[0], expected.value)
		}
	}
	for _, expected := range expectedHeaders {
		if !seen[expected.logicalName] {
			return fmt.Errorf("missing required %s header", expected.displayName)
		}
	}

	return nil
}

func TestReplayFixedCaseSelectsConditionalReplay(t *testing.T) {
	requestCase := newReplayCase("homepage-content-etag", "/api/homepage/content", "catalog=contract-replay", "web", catalogcontract.CacheContract{})
	caseSpec := replayCase{requestCase: requestCase, conditional: true}
	client := &http.Client{}
	const baseline = "http://baseline.test"
	const candidate = "http://candidate.test"

	var ordinaryCalls int
	var conditionalCalls int
	var receivedBaseline string
	var receivedCandidate string
	var receivedRequest catalogcontract.RequestCase
	ordinaryReplay := func(context.Context, *http.Client, string, string, catalogcontract.RequestCase) error {
		ordinaryCalls++
		return nil
	}
	conditionalReplay := func(_ context.Context, receivedClient *http.Client, gotBaseline, gotCandidate string, gotRequest catalogcontract.RequestCase) error {
		conditionalCalls++
		if receivedClient != client {
			t.Fatal("conditional replay received a different HTTP client")
		}
		receivedBaseline = gotBaseline
		receivedCandidate = gotCandidate
		receivedRequest = gotRequest
		return nil
	}

	if err := replayFixedCase(context.Background(), client, baseline, candidate, caseSpec, ordinaryReplay, conditionalReplay); err != nil {
		t.Fatalf("replayFixedCase() error = %v", err)
	}
	if ordinaryCalls != 0 || conditionalCalls != 1 {
		t.Fatalf("replay calls = ordinary %d, conditional %d, want ordinary 0 and conditional 1", ordinaryCalls, conditionalCalls)
	}
	if receivedBaseline != baseline || receivedCandidate != candidate || !reflect.DeepEqual(receivedRequest, requestCase) {
		t.Fatalf("conditional replay received baseline=%q candidate=%q request=%+v, want original inputs", receivedBaseline, receivedCandidate, receivedRequest)
	}
}

func TestReplayFixedCaseSelectsOrdinaryReplay(t *testing.T) {
	requestCase := newReplayCase("api-query", "/api/homepage/video-templates", "category=all&limit=24", "web", catalogcontract.CacheContract{})
	caseSpec := replayCase{requestCase: requestCase}
	client := &http.Client{}
	const baseline = "http://baseline.test"
	const candidate = "http://candidate.test"

	var ordinaryCalls int
	var conditionalCalls int
	var receivedBaseline string
	var receivedCandidate string
	var receivedRequest catalogcontract.RequestCase
	ordinaryReplay := func(_ context.Context, receivedClient *http.Client, gotBaseline, gotCandidate string, gotRequest catalogcontract.RequestCase) error {
		ordinaryCalls++
		if receivedClient != client {
			t.Fatal("ordinary replay received a different HTTP client")
		}
		receivedBaseline = gotBaseline
		receivedCandidate = gotCandidate
		receivedRequest = gotRequest
		return nil
	}
	conditionalReplay := func(context.Context, *http.Client, string, string, catalogcontract.RequestCase) error {
		conditionalCalls++
		return nil
	}

	if err := replayFixedCase(context.Background(), client, baseline, candidate, caseSpec, ordinaryReplay, conditionalReplay); err != nil {
		t.Fatalf("replayFixedCase() error = %v", err)
	}
	if ordinaryCalls != 1 || conditionalCalls != 0 {
		t.Fatalf("replay calls = ordinary %d, conditional %d, want ordinary 1 and conditional 0", ordinaryCalls, conditionalCalls)
	}
	if receivedBaseline != baseline || receivedCandidate != candidate || !reflect.DeepEqual(receivedRequest, requestCase) {
		t.Fatalf("ordinary replay received baseline=%q candidate=%q request=%+v, want original inputs", receivedBaseline, receivedCandidate, receivedRequest)
	}
}
