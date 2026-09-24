package polarstarb2b

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic fixtures exercise only fields documented in the public contract;
// they are not evidence of a real supplier response or output-media schema.
const jobFixture = `{"success":true,"data":{"contractVersion":"b2b.job.v2","jobId":"job-1","tenantId":"tenant-1","externalId":"step-1","capability":"text_to_image","status":"queued","output":null}}`

func testClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	c, err := NewClient(ClientOptions{BaseURL: s.URL, APIKey: "test-secret", AccountRef: "account-a", ExpectedTenantID: "tenant-1", RootCAs: roots, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c, s
}

func TestClientOptionsUseBoundedTransportSettings(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer s.Close()
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	c, err := NewClient(ClientOptions{BaseURL: s.URL, APIKey: "test-secret", AccountRef: "account-a", ExpectedTenantID: "tenant-1", RootCAs: roots, Timeout: 20 * time.Second, ConnectTimeout: 3 * time.Second, ResponseHeaderTimeout: 7 * time.Second, MaxConnectionsPerHost: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", c.http.Transport)
	}
	if tr.MaxConnsPerHost != 7 || tr.TLSHandshakeTimeout != 3*time.Second || tr.ResponseHeaderTimeout != 7*time.Second {
		t.Fatalf("transport limits not applied: max=%d tls=%s header=%s", tr.MaxConnsPerHost, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
	}
}

func TestClientOptionsDefaultsAndTotalTimeoutBounds(t *testing.T) {
	c, s := testClient(t, func(w http.ResponseWriter, r *http.Request) {})
	defer s.Close()
	tr := c.http.Transport.(*http.Transport)
	if tr.TLSHandshakeTimeout != time.Second || tr.ResponseHeaderTimeout != time.Second || tr.MaxConnsPerHost != 32 {
		t.Fatalf("defaults changed: tls=%s header=%s max=%d", tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout, tr.MaxConnsPerHost)
	}
	for _, opts := range []ClientOptions{
		{BaseURL: s.URL, APIKey: "k", AccountRef: "a", ExpectedTenantID: "t", Timeout: 10 * time.Second, ConnectTimeout: 6 * time.Second},
		{BaseURL: s.URL, APIKey: "k", AccountRef: "a", ExpectedTenantID: "t", Timeout: 10 * time.Second, ResponseHeaderTimeout: 11 * time.Second},
		{BaseURL: s.URL, APIKey: "k", AccountRef: "a", ExpectedTenantID: "t", Timeout: 10 * time.Second, MaxConnectionsPerHost: 33},
	} {
		if _, err := NewClient(opts); !errors.Is(err, ErrNotSent) {
			t.Fatalf("unsafe options accepted: %v", err)
		}
	}
}

func lookupKey() LookupKey {
	return LookupKey{AccountRef: "account-a", IdempotencyKey: "cling-step:step-1", ExternalID: "step-1", Capability: "text_to_image", JobID: "job-1"}
}

func TestLookupContract(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/jobs/lookup" || r.URL.Query().Get("idempotencyKey") != "cling-step:step-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-secret" || r.Header.Get("Authorization") != "" {
			t.Error("wrong authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, jobFixture)
	})
	j, err := c.Lookup(context.Background(), lookupKey())
	if err != nil || j.JobID != "job-1" || j.Status != "queued" {
		t.Fatalf("job=%+v err=%v", j, err)
	}
}

func TestLookupRejectsUntrustedResponses(t *testing.T) {
	tests := []struct{ name, body string }{
		{"truncated", jobFixture[:len(jobFixture)-1]},
		{"trailing", jobFixture + `{}`},
		{"duplicate", strings.Replace(jobFixture, `"success":true`, `"success":false,"success":true`, 1)},
		{"case_alias", strings.Replace(jobFixture, `"success":true`, `"Success":true`, 1)},
		{"tenant", strings.Replace(jobFixture, "tenant-1", "tenant-2", 1)},
		{"external", strings.Replace(jobFixture, "step-1", "other-step", 1)},
		{"capability", strings.Replace(jobFixture, "text_to_image", "image_edit", 1)},
		{"job", strings.Replace(jobFixture, "job-1", "job-2", 1)},
		{"version", strings.Replace(jobFixture, "b2b.job.v2", "b2b.job.v3", 1)},
		{"status", strings.Replace(jobFixture, "queued", "succeeded", 1)},
		{"no_job", strings.Replace(jobFixture, `"jobId":"job-1",`, "", 1)},
		{"null", `{"success":true,"data":null}`},
		{"oversized", jobFixture + strings.Repeat(" ", 1<<20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tt.body)
			})
			_, err := c.Lookup(context.Background(), lookupKey())
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("want unknown, got %v", err)
			}
		})
	}
}

func TestHTTPFailuresRemainUnknownAndSanitized(t *testing.T) {
	for _, status := range []int{200, 201, 400, 401, 402, 403, 404, 409, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "999999")
				w.WriteHeader(status)
				io.WriteString(w, `{"success":false,"code":"SECRET_test-secret","message":"https://private.example/prompt"}`)
			})
			_, err := c.Lookup(context.Background(), lookupKey())
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("expected unknown: %v", err)
			}
			var ce *ClientError
			if !errors.As(err, &ce) || ce.HTTPStatus != status {
				t.Fatalf("missing safe status: %v", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "http") || strings.Contains(err.Error(), "prompt") {
				t.Fatalf("unsafe error: %v", err)
			}
			if (status == 429 || status == 503) && (ce.RetryAfter <= 0 || ce.RetryAfter > 5*time.Minute) {
				t.Fatalf("unbounded retry: %v", ce.RetryAfter)
			}
			if calls.Load() != 1 {
				t.Fatal("automatic retry")
			}
			if status == 409 && !errors.Is(err, ErrConflict) {
				t.Fatal("missing conflict classification")
			}
		})
	}
}

func TestClientRejectsBadConfigurationAndKeys(t *testing.T) {
	for _, base := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path", "https://example.com?key=secret", "https://example.com#x", "https://example.com:bad", "https:///"} {
		_, err := NewClient(ClientOptions{BaseURL: base, APIKey: "test-secret", AccountRef: "account-a", ExpectedTenantID: "tenant-1"})
		if !errors.Is(err, ErrNotSent) {
			t.Fatalf("accepted base URL or unsafe error: %v", err)
		}
	}
	var calls atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	bad := lookupKey()
	bad.ExternalID = ""
	if _, err := c.Lookup(context.Background(), bad); !errors.Is(err, ErrNotSent) {
		t.Fatalf("bad identity accepted: %v", err)
	}
	for _, id := range []string{"", "../jobs", "a/b", "a?b", "a%2Fb"} {
		if _, err := c.Cancel(context.Background(), id); !errors.Is(err, ErrNotSent) {
			t.Fatalf("bad job accepted: %v", err)
		}
	}
	if _, err := c.Submit(context.Background(), Request{}); !errors.Is(err, ErrNotSent) {
		t.Fatalf("empty frozen request accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Lookup(ctx, lookupKey()); !errors.Is(err, ErrNotSent) || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input sent")
	}
}

func TestNoRedirectAndContextTimeout(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	if _, err := c.Lookup(context.Background(), lookupKey()); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("redirect: %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect followed")
	}
	c, _ = testClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Lookup(ctx, lookupKey()); !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
}

func TestCancelOnlyAcknowledgesRequest(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/jobs/job-1/cancel" {
			t.Error("wrong cancel route")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, jobFixture)
	})
	receipt, err := c.Cancel(context.Background(), "job-1")
	if err != nil || receipt.JobID != "job-1" || !receipt.RequestAcknowledged {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func clientRequest(t *testing.T) Request {
	t.Helper()
	s, r, p, cb := mapperFixture()
	req, err := MapRequest(s, r, p, cb)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestSubmitFrozenContractAndNoReplay(t *testing.T) {
	request := clientRequest(t)
	response := jobFixture
	for _, status := range []int{201, 200, 202, 400, 401, 402, 403, 404, 409, 429, 500, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/api/v1/jobs" || r.Header.Get("X-API-Key") != "test-secret" {
					t.Error("wrong submit contract")
				}
				if r.Header.Get("Idempotency-Key") != "" {
					t.Error("must not opt POST into transport retries")
				}
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, request.Payload()) {
					t.Error("frozen payload changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				io.WriteString(w, response)
			})
			j, err := c.Submit(context.Background(), request)
			if status == 201 {
				if err != nil || j.JobID != "job-1" {
					t.Fatalf("created job=%+v err=%v", j, err)
				}
			} else if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("non-201 accepted: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("POST replayed %d times", calls.Load())
			}
		})
	}
}

func TestSubmitDisconnectAndTruncatedSuccess(t *testing.T) {
	request := clientRequest(t)
	for _, disconnect := range []bool{true, false} {
		t.Run(strconv.FormatBool(disconnect), func(t *testing.T) {
			var calls atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if disconnect {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "1000")
				w.WriteHeader(201)
				io.WriteString(w, `{"success":true`)
			})
			_, err := c.Submit(context.Background(), request)
			if !errors.Is(err, ErrOutcomeUnknown) || calls.Load() != 1 {
				t.Fatalf("unknown POST replayed or rejected: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestResponseHeadersAndNestedJSONBoundaries(t *testing.T) {
	tests := []struct {
		name, body string
		headers    map[string][]string
	}{
		{"encoded", jobFixture, map[string][]string{"Content-Encoding": {"gzip"}}},
		{"duplicate_type", jobFixture, map[string][]string{"Content-Type": {"application/json", "application/json"}}},
		{"wrong_type", jobFixture, map[string][]string{"Content-Type": {"text/html"}}},
		{"duplicate_identity", strings.Replace(jobFixture, `"tenantId":"tenant-1"`, `"tenantId":"tenant-2","tenantId":"tenant-1"`, 1), nil},
		{"escaped_duplicate", strings.Replace(jobFixture, `"tenantId":"tenant-1"`, `"tenantId":"tenant-2","tenant\u0049d":"tenant-1"`, 1), nil},
		{"case_duplicate", strings.Replace(jobFixture, `"tenantId":"tenant-1"`, `"TenantId":"tenant-2","tenantId":"tenant-1"`, 1), nil},
		{"deep", strings.Replace(jobFixture, `"output":null`, `"output":`+strings.Repeat("[", 40)+"0"+strings.Repeat("]", 40), 1), nil},
		{"invalid_utf8", strings.Replace(jobFixture, "queued", string([]byte{0xff}), 1), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				for key, values := range tt.headers {
					w.Header()[key] = values
				}
				io.WriteString(w, tt.body)
			})
			if _, err := c.Lookup(context.Background(), lookupKey()); !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("untrusted response accepted: %v", err)
			}
		})
	}
}

func TestRetryAfterFormats(t *testing.T) {
	for _, tt := range []struct {
		value    string
		positive bool
	}{{"2", true}, {time.Now().Add(time.Minute).UTC().Format(http.TimeFormat), true}, {"-1", false}, {"nonsense", false}, {"999999999999999999999999999999999999", false}} {
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", tt.value)
			w.WriteHeader(503)
			io.WriteString(w, `{"success":false,"code":"MODEL_BUSY"}`)
		})
		_, err := c.Lookup(context.Background(), lookupKey())
		var ce *ClientError
		if !errors.As(err, &ce) || ce.Code != "MODEL_BUSY" || (ce.RetryAfter > 0) != tt.positive || ce.RetryAfter > 5*time.Minute {
			t.Fatalf("bad retry delay/error: %+v", ce)
		}
	}
}

func TestSubmitRejectsDifferentFrozenAccount(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		io.WriteString(w, jobFixture)
	})
	s, r, p, cb := mapperFixture()
	r.AccountRef = "other-account"
	request, err := MapRequest(s, r, p, cb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Submit(context.Background(), request); !errors.Is(err, ErrNotSent) {
		t.Fatalf("different account sent: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("different account contacted")
	}
}

func TestCompletedRequiresSafeResultURL(t *testing.T) {
	completed := strings.Replace(jobFixture, `"status":"queued"`, `"status":"completed"`, 1)
	for _, output := range []string{`null`, `{}`, `{"resultUrl":"http://cdn.example.com/a.png"}`, `{"resultUrl":"https://user:secret@cdn.example.com/a.png"}`, `{"resultUrl":"https://localhost/a.png"}`, `{"resultUrl":"https://127.0.0.1/a.png"}`, `{"resultUrl":"https://10.1.2.3/a.png"}`, `{"resultUrl":"https://[::1]/a.png"}`, `{"resultUrl":"https://169.254.169.254/a.png"}`, `{"resultUrl":"https://cdn.example.com/a.png#fragment"}`, `{"resultUrl":42}`} {
		c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, strings.Replace(completed, `"output":null`, `"output":`+output, 1))
		})
		if _, err := c.Lookup(context.Background(), lookupKey()); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("unsafe completed output accepted: %v", err)
		}
	}
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, strings.Replace(completed, `"output":null`, `"output":{"resultUrl":"https://cdn.example.com/a.png?signature=fixture"}`, 1))
	})
	j, err := c.Lookup(context.Background(), lookupKey())
	if err != nil || j.ResultURL != "https://cdn.example.com/a.png?signature=fixture" {
		t.Fatalf("missing validated result: %+v %v", j, err)
	}
}

func TestLookupRejectsMismatchedFrozenScope(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, jobFixture)
	})
	for _, mismatch := range []string{"account", "key"} {
		key := lookupKey()
		if mismatch == "account" {
			key.AccountRef = "other-account"
		} else {
			key.IdempotencyKey = "cling-step:other-step"
		}
		if _, err := c.Lookup(context.Background(), key); !errors.Is(err, ErrNotSent) {
			t.Fatalf("different scope sent: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("mismatched lookup sent")
	}
}
