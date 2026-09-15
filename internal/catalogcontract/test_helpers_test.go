package catalogcontract

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type observedRequest struct {
	method      string
	path        string
	escapedPath string
	rawPath     string
	rawQuery    string
	header      http.Header
}

type conditionalReplayServerConfig struct {
	initialETags      []string
	initialBody       string
	conditionalStatus int
	conditionalETag   string
}

func newConditionalReplayServer(t *testing.T, requests chan<- observedRequest, config conditionalReplayServerConfig) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requests != nil {
			requests <- observedRequest{
				method:      request.Method,
				path:        request.URL.Path,
				escapedPath: request.URL.EscapedPath(),
				rawPath:     request.URL.RawPath,
				rawQuery:    request.URL.RawQuery,
				header:      request.Header.Clone(),
			}
		}
		writer.Header().Set("X-Request-Id", request.Header.Get("X-Request-Id"))
		if request.Header.Get("If-None-Match") == "" {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "public, max-age=60")
			for _, etag := range config.initialETags {
				writer.Header().Add("ETag", etag)
			}
			writer.WriteHeader(http.StatusOK)
			body := config.initialBody
			if body == "" {
				body = `{"success":true,"data":[]}`
			}
			_, _ = writer.Write([]byte(body))
			return
		}

		writer.Header().Set("ETag", config.conditionalETag)
		status := config.conditionalStatus
		if status == 0 {
			status = http.StatusNotModified
		}
		writer.WriteHeader(status)
		if status != http.StatusNotModified {
			_, _ = writer.Write([]byte(`{"success":true,"data":[]}`))
		}
	}))
}

func assertConditionalObservedRequest(t *testing.T, name string, request observedRequest, want RequestCase, wantETag string) {
	t.Helper()
	if request.method != http.MethodGet {
		t.Fatalf("%s method = %q, want GET", name, request.method)
	}
	if request.path != want.Path {
		t.Fatalf("%s path = %q, want %q", name, request.path, want.Path)
	}
	if request.escapedPath != want.RawPath {
		t.Fatalf("%s escaped path = %q, want %q", name, request.escapedPath, want.RawPath)
	}
	if request.rawQuery != want.RawQuery {
		t.Fatalf("%s raw query = %q, want %q", name, request.rawQuery, want.RawQuery)
	}
	if got := request.header.Get("X-Request-Id"); got != want.Header.Get("X-Request-Id") {
		t.Fatalf("%s X-Request-Id = %q, want %q", name, got, want.Header.Get("X-Request-Id"))
	}
	if got := request.header.Values("If-None-Match"); strings.Join(got, ",") != wantETag {
		t.Fatalf("%s If-None-Match = %q, want %q", name, got, wantETag)
	}
}

func assertReplayConditionHeaders(t *testing.T, name string, header http.Header, wantETag string) {
	t.Helper()
	for _, headerName := range []string{"If-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range"} {
		if got := header.Values(headerName); len(got) != 0 {
			t.Fatalf("%s %s = %q, want absent", name, headerName, got)
		}
	}
	if got := header.Values("If-None-Match"); strings.Join(got, ",") != wantETag {
		t.Fatalf("%s If-None-Match = %q, want %q", name, got, wantETag)
	}
}

func assertConditionalReplayError(t *testing.T, err error, caseName, detail string) {
	t.Helper()
	if err == nil {
		t.Fatal("ReplayConditional() error = nil, want conditional comparison failure")
	}
	if !strings.Contains(err.Error(), caseName) || !strings.Contains(err.Error(), "conditional compare") || !strings.Contains(err.Error(), detail) {
		t.Fatalf("ReplayConditional() error = %q, want conditional compare failure", err)
	}
}

type replayRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip replayRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type observedReadCloser struct {
	reader  io.Reader
	readErr error
	closed  bool
}

func (body *observedReadCloser) Read(buffer []byte) (int, error) {
	if body.readErr != nil {
		return 0, body.readErr
	}
	return body.reader.Read(buffer)
}

func (body *observedReadCloser) Close() error {
	body.closed = true
	return nil
}

func newConditionalHTTPResponse(status int, etag, body, requestID string) *http.Response {
	header := make(http.Header)
	header.Set("ETag", etag)
	if status == http.StatusOK {
		header.Set("Content-Type", "application/json")
		header.Set("Cache-Control", "public, max-age=60")
		header.Set("X-Request-Id", requestID)
	}

	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newReplayServer(t *testing.T, requests chan<- observedRequest, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests != nil {
			requests <- observedRequest{
				method:      r.Method,
				path:        r.URL.Path,
				escapedPath: r.URL.EscapedPath(),
				rawPath:     r.URL.RawPath,
				rawQuery:    r.URL.RawQuery,
				header:      r.Header.Clone(),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("X-Request-Id", r.Header.Get("X-Request-Id"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func assertObservedRequest(t *testing.T, request observedRequest, want RequestCase) {
	t.Helper()
	if request.method != http.MethodGet {
		t.Fatalf("method = %q, want GET", request.method)
	}
	if request.path != want.Path {
		t.Fatalf("path = %q, want %q", request.path, want.Path)
	}
	if request.rawQuery != want.RawQuery {
		t.Fatalf("raw query = %q, want %q", request.rawQuery, want.RawQuery)
	}
	for key, values := range want.Header {
		if got := request.header.Values(key); strings.Join(got, ",") != strings.Join(values, ",") {
			t.Fatalf("header %s = %q, want %q", key, got, values)
		}
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", raw, err)
	}
	return parsed
}
