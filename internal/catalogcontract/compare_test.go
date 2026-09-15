package catalogcontract

import (
	"net/http"
	"strings"
	"testing"
)

func TestCompareAcceptsEquivalentJSONWithDifferentObjectKeyOrder(t *testing.T) {
	baseline := testResponse(`{"success":true,"data":{"name":"template","tags":["dance","portrait"]}}`)
	candidate := testResponse(`{"data":{"tags":["dance","portrait"],"name":"template"},"success":true}`)

	if err := Compare(baseline, candidate); err != nil {
		t.Fatalf("Compare() error = %v, want nil", err)
	}
}

func TestCompareRejectsDifferentJSONValue(t *testing.T) {
	baseline := testResponse(`{"success":true,"data":{"name":"template"}}`)
	candidate := testResponse(`{"success":true,"data":{"name":"different-template"}}`)

	err := Compare(baseline, candidate)
	if err == nil {
		t.Fatal("Compare() error = nil, want JSON body difference")
	}
	if !strings.Contains(err.Error(), "JSON body") {
		t.Fatalf("Compare() error = %q, want JSON body difference", err)
	}
}

func TestCompareRejectsDifferentLargeIntegerJSONValues(t *testing.T) {
	baseline := testResponse(`{"success":true,"data":{"id":9007199254740992}}`)
	candidate := testResponse(`{"success":true,"data":{"id":9007199254740993}}`)

	err := Compare(baseline, candidate)
	if err == nil {
		t.Fatal("Compare() error = nil, want JSON body difference for large integer values")
	}
	if !strings.Contains(err.Error(), "JSON body") {
		t.Fatalf("Compare() error = %q, want JSON body difference", err)
	}
}

func TestCompareRejectsDifferentCacheControl(t *testing.T) {
	baseline := testResponse(`{"success":true,"data":[]}`)
	baseline.Header.Set("Cache-Control", "public, max-age=60")
	candidate := testResponse(`{"success":true,"data":[]}`)
	candidate.Header.Set("Cache-Control", "no-store")

	err := Compare(baseline, candidate)
	if err == nil {
		t.Fatal("Compare() error = nil, want Cache-Control difference")
	}
	if !strings.Contains(err.Error(), "Cache-Control") {
		t.Fatalf("Compare() error = %q, want Cache-Control difference", err)
	}
}

func TestCompareRejectsDifferentRedirectLocationWithoutLeakingTargets(t *testing.T) {
	baseline := testResponse(`{"success":false,"code":"REDIRECT"}`)
	baseline.StatusCode = http.StatusFound
	baseline.Header.Set("Location", "https://baseline.example.test/private-target")
	candidate := testResponse(`{"success":false,"code":"REDIRECT"}`)
	candidate.StatusCode = http.StatusFound
	candidate.Header.Set("Location", "https://candidate.example.test/private-target")

	err := Compare(baseline, candidate)
	if err == nil {
		t.Fatal("Compare() error = nil, want Location difference")
	}
	if err.Error() != "Location differs" {
		t.Fatalf("Compare() error = %q, want Location difference", err)
	}
	for _, target := range []string{
		"https://baseline.example.test/private-target",
		"https://candidate.example.test/private-target",
	} {
		if strings.Contains(err.Error(), target) {
			t.Fatalf("Compare() error = %q, must not reveal redirect target", err)
		}
	}
}

func TestCompareRejectsDuplicateRedirectLocationWithoutLeakingTargets(t *testing.T) {
	baseline := testResponse(`{"success":false,"code":"REDIRECT"}`)
	baseline.StatusCode = http.StatusFound
	baseline.Header["Location"] = []string{
		"https://redirect.example.test/private-target",
		"https://redirect.example.test/private-target",
	}
	candidate := testResponse(`{"success":false,"code":"REDIRECT"}`)
	candidate.StatusCode = http.StatusFound
	candidate.Header["Location"] = []string{
		"https://redirect.example.test/private-target",
		"https://redirect.example.test/private-target",
	}

	err := Compare(baseline, candidate)
	if err == nil {
		t.Fatal("Compare() error = nil, want invalid duplicate Location")
	}
	if err.Error() != "Location is missing or invalid" {
		t.Fatalf("Compare() error = %q, want invalid duplicate Location", err)
	}
	if strings.Contains(err.Error(), "https://redirect.example.test/private-target") {
		t.Fatalf("Compare() error = %q, must not reveal redirect target", err)
	}
}

func TestCompareAcceptsVaryTokensInDifferentOrder(t *testing.T) {
	baseline := testResponse(`{"success":true,"data":[]}`)
	baseline.Header.Set("Vary", "Accept-Encoding, Origin")
	candidate := testResponse(`{"success":true,"data":[]}`)
	candidate.Header.Set("Vary", "Origin, Accept-Encoding")

	if err := Compare(baseline, candidate); err != nil {
		t.Fatalf("Compare() error = %v, want nil", err)
	}
}

func TestCompareAcceptsDifferentCacheStatusValues(t *testing.T) {
	baseline := testResponse(`{"success":true,"data":[]}`)
	baseline.Header.Set("X-Cache-Status", "miss")
	candidate := testResponse(`{"success":true,"data":[]}`)
	candidate.Header.Set("X-Cache-Status", "hit")

	if err := Compare(baseline, candidate); err != nil {
		t.Fatalf("Compare() error = %v, want nil", err)
	}
}

func TestCompareRejectsMissingCacheStatus(t *testing.T) {
	baseline := testResponse(`{"success":true,"data":[]}`)
	baseline.Header.Set("X-Cache-Status", "miss")
	candidate := testResponse(`{"success":true,"data":[]}`)

	err := Compare(baseline, candidate)
	if err == nil {
		t.Fatal("Compare() error = nil, want X-Cache-Status presence difference")
	}
	if !strings.Contains(err.Error(), "X-Cache-Status") {
		t.Fatalf("Compare() error = %q, want X-Cache-Status difference", err)
	}
}

func TestCompareRejectsCandidateWithDuplicateRequestID(t *testing.T) {
	baseline := testResponse(`{"success":true,"data":[]}`)
	candidate := testResponse(`{"success":true,"data":[]}`)
	candidate.Header.Add("X-Request-Id", "request-123")

	err := Compare(baseline, candidate)
	if err == nil {
		t.Fatal("Compare() error = nil, want duplicate X-Request-Id rejection")
	}
	if !strings.Contains(err.Error(), "candidate X-Request-Id") {
		t.Fatalf("Compare() error = %q, want candidate X-Request-Id difference", err)
	}
}

func TestCompareAcceptsEquivalentNotModifiedResponses(t *testing.T) {
	baseline := notModifiedResponse("Etag", `"catalog-v1"`)
	baseline.Header.Set("Date", "Mon, 01 Sep 2026 08:00:00 GMT")
	baseline.Header.Set("Server", "node")
	baseline.Header.Set("Content-Length", "0")
	candidate := notModifiedResponse("ETag", `"catalog-v1"`)
	candidate.Header.Set("Date", "Mon, 01 Sep 2026 08:01:00 GMT")
	candidate.Header.Set("Server", "go")

	if err := Compare(baseline, candidate); err != nil {
		t.Fatalf("Compare() error = %v, want nil", err)
	}
}

func TestCompareRejectsNotModifiedResponseWithBody(t *testing.T) {
	testCases := []struct {
		name          string
		baselineBody  string
		candidateBody string
	}{
		{name: "baseline", baselineBody: "unexpected baseline body"},
		{name: "candidate", candidateBody: "unexpected candidate body"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			baseline := notModifiedResponse("ETag", `"catalog-v1"`)
			baseline.Body = []byte(testCase.baselineBody)
			candidate := notModifiedResponse("ETag", `"catalog-v1"`)
			candidate.Body = []byte(testCase.candidateBody)

			err := Compare(baseline, candidate)
			if err == nil {
				t.Fatal("Compare() error = nil, want 304 response body difference")
			}
			if err.Error() != "304 response body differs" {
				t.Fatalf("Compare() error = %q, want 304 response body difference", err)
			}
			if strings.Contains(err.Error(), "unexpected") {
				t.Fatalf("Compare() error = %q, must not reveal response body", err)
			}
		})
	}
}

func TestCompareRejectsNotModifiedResponseWithDifferentETag(t *testing.T) {
	baseline := notModifiedResponse("ETag", `"catalog-v1"`)
	candidate := notModifiedResponse("ETag", `"catalog-v2"`)

	err := Compare(baseline, candidate)
	if err == nil {
		t.Fatal("Compare() error = nil, want ETag difference")
	}
	if err.Error() != "304 ETag differs" {
		t.Fatalf("Compare() error = %q, want 304 ETag difference", err)
	}
	if strings.Contains(err.Error(), "catalog-v1") || strings.Contains(err.Error(), "catalog-v2") {
		t.Fatalf("Compare() error = %q, must not reveal ETag values", err)
	}
}

func TestCompareRejectsNotModifiedResponseWithMissingETag(t *testing.T) {
	testCases := []struct {
		name                string
		removeBaselineETag  bool
		removeCandidateETag bool
	}{
		{name: "baseline", removeBaselineETag: true},
		{name: "candidate", removeCandidateETag: true},
		{name: "both", removeBaselineETag: true, removeCandidateETag: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			baseline := notModifiedResponse("ETag", `"catalog-v1"`)
			candidate := notModifiedResponse("ETag", `"catalog-v1"`)
			if testCase.removeBaselineETag {
				delete(baseline.Header, "ETag")
			}
			if testCase.removeCandidateETag {
				delete(candidate.Header, "ETag")
			}

			requireSingleETagError(t, Compare(baseline, candidate))
		})
	}
}

func TestCompareRejectsNotModifiedResponseWithDuplicateETag(t *testing.T) {
	testCases := []struct {
		name                   string
		duplicateBaselineETag  bool
		duplicateCandidateETag bool
	}{
		{name: "baseline", duplicateBaselineETag: true},
		{name: "candidate", duplicateCandidateETag: true},
		{name: "both", duplicateBaselineETag: true, duplicateCandidateETag: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			baseline := notModifiedResponse("ETag", `"catalog-v1"`)
			candidate := notModifiedResponse("ETag", `"catalog-v1"`)
			if testCase.duplicateBaselineETag {
				baseline.Header["ETag"] = []string{`"catalog-v1"`, `"catalog-v1"`}
			}
			if testCase.duplicateCandidateETag {
				candidate.Header["ETag"] = []string{`"catalog-v1"`, `"catalog-v1"`}
			}

			requireSingleETagError(t, Compare(baseline, candidate))
		})
	}
}

func requireSingleETagError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Compare() error = nil, want invalid 304 ETag")
	}
	if err.Error() != "304 ETag must appear exactly once" {
		t.Fatalf("Compare() error = %q, want invalid 304 ETag", err)
	}
	if strings.Contains(err.Error(), "catalog-v1") {
		t.Fatalf("Compare() error = %q, must not reveal ETag values", err)
	}
}

func notModifiedResponse(etagName, etag string) Response {
	header := make(http.Header)
	header[etagName] = []string{etag}

	return Response{
		StatusCode: http.StatusNotModified,
		Header:     header,
	}
}

func testResponse(body string) Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("Cache-Control", "public, max-age=60")
	header.Set("X-Request-Id", "request-123")

	return Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       []byte(body),
	}
}
