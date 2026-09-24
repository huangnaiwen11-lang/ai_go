package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPaymentReadExactRoutes(t *testing.T) {
	for _, tc := range []struct {
		method, path, key string
		want              bool
	}{
		{"GET", "/api/wallet/products", "/api/wallet/products", true},
		{"GET", "/api/wallet/external-payment-methods", "/api/wallet/external-payment-methods", true},
		{"GET", "/api/payments/order-status/order-1", "/api/payments/order-status/:orderId", true},
		{"GET", "/api/payments/order-status/local_payment_1", "/api/payments/order-status/:orderId", true},
		{"POST", "/api/wallet/products", "", false},
		{"HEAD", "/api/wallet/products", "", false},
		{"GET", "/api/wallet/products/", "", false},
		{"GET", "/api/wallet/products?", "", false},
		{"GET", "/api/wallet/products?userId=x", "", false},
		{"GET", "/api/wallet%2fproducts", "", false},
		{"GET", "/api/v1/wallet/products", "", false},
		{"GET", "/api/payments/order-status/", "", false},
		{"GET", "/api/payments/order-status/order-1/extra", "", false},
		{"GET", "/api/payments/order-status/order-1/", "", false},
		{"GET", "/api/payments/order-status/order%2d1", "", false},
		{"GET", "/api/payments/order-status/order%2f1", "", false},
		{"GET", "/api/payments/order-status/..", "", false},
		{"GET", "/api/payments/order-status/order-1?poll=1", "", false},
		{"GET", "/api/payments/order-status/order-1?", "", false},
		{"POST", "/api/payments/order-status/order-1", "", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if got := matchesPaymentEntry(req); got != tc.want {
				t.Fatalf("matches = %v, want %v", got, tc.want)
			}
			if tc.want {
				key := paymentEntryRouteKey(req.URL.Path)
				if key != (exactRouteKey{method: http.MethodGet, path: tc.key}) || !isConfirmedExactRoute(key) {
					t.Fatalf("key = %#v", key)
				}
			}
		})
	}
}

func TestPaymentReadSwitchAndFailureNeverFallback(t *testing.T) {
	for _, path := range []string{"/api/wallet/products", "/api/payments/order-status/order-1"} {
		t.Run(path, func(t *testing.T) {
			nodeHits := 0
			transport := paymentReadTransport(func(r *http.Request) (*http.Response, error) {
				nodeHits++
				return &http.Response{StatusCode: 202, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("node")), Request: r}, nil
			})
			filename := filepath.Join(t.TempDir(), "routes.json")
			if err := os.WriteFile(filename, []byte(`{"routes":{"GET /api/wallet/products":true,"GET /api/payments/order-status/:orderId":true}}`), 0600); err != nil {
				t.Fatal(err)
			}
			localHits := 0
			local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { localHits++; w.WriteHeader(503) })
			gateway := New(Config{DefaultUpstream: mustURL(t, "http://node.invalid"), RouteSwitch: NewFileRouteSwitch(filename), PaymentEntryHandler: local})
			gateway.defaultProxy.Transport = transport
			entry := gateway.Handler()
			recorder := httptest.NewRecorder()
			entry.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
			if recorder.Code != 503 || localHits != 1 || nodeHits != 0 {
				t.Fatalf("code=%d local=%d node=%d", recorder.Code, localHits, nodeHits)
			}
			gateway = New(Config{DefaultUpstream: mustURL(t, "http://node.invalid"), PaymentEntryHandler: local})
			gateway.defaultProxy.Transport = transport
			entry = gateway.Handler()
			recorder = httptest.NewRecorder()
			entry.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
			if recorder.Code != 202 || localHits != 1 || nodeHits != 1 {
				t.Fatalf("disabled code=%d local=%d node=%d", recorder.Code, localHits, nodeHits)
			}
		})
	}
}

// 内存 RoundTripper 只记录代理分支，不监听端口也不访问任何 Node 服务。
type paymentReadTransport func(*http.Request) (*http.Response, error)

func (transport paymentReadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestPaymentReadKeepsLegacyPostMatching(t *testing.T) {
	for _, path := range []string{localCheckoutPath, localVerifyPurchasePath} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		if !matchesPaymentEntry(request) || paymentEntryRouteKey(path) != (exactRouteKey{method: http.MethodPost, path: path}) {
			t.Fatalf("existing POST no longer matches: %s", path)
		}
		for _, target := range []string{path + "?", path + "?retry=1", path + "/"} {
			if matchesPaymentEntry(httptest.NewRequest(http.MethodPost, target, nil)) {
				t.Fatalf("POST route widened: %s", target)
			}
		}
		if matchesPaymentEntry(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Fatalf("POST route accepted GET: %s", path)
		}
	}
}
