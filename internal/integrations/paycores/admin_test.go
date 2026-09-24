package paycores

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdminClientChannelsOverviewUsesLegacyGetSignatureAndPreservesPayload(t *testing.T) {
	const key = "paycores-internal-admin-secret-at-least-32-characters"
	clock := time.UnixMilli(1730000000123)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != adminChannelsPath {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Timestamp"); got != "1730000000123" {
			t.Errorf("timestamp = %q", got)
		}
		mac := hmac.New(sha256.New, []byte(key))
		_, _ = mac.Write([]byte("/channels-overview:1730000000123"))
		if got, want := r.Header.Get("X-Signature"), hex.EncodeToString(mac.Sum(nil)); got != want {
			t.Errorf("signature = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"channels":[{"id":"stripe:default","enabled":true}],"totalWeight":4,"channelOrder":["stripe:default"]}}`))
	}))
	defer server.Close()
	client, err := NewAdminClient(server.URL, key, server.Client(), func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	data, err := client.ChannelsOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `{"channels":[{"id":"stripe:default","enabled":true}],"totalWeight":4,"channelOrder":["stripe:default"]}` {
		t.Fatalf("data = %s", got)
	}
}

func TestAdminClientRejectsUnavailableOrFabricatedEmptyData(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		body   string
	}{
		{"http-error", 401, `{"success":false}`},
		{"unverified", 200, `{"success":false,"data":{"channels":[]}}`},
		{"missing-channels", 200, `{"success":true,"data":{}}`},
		{"null-channels", 200, `{"success":true,"data":{"channels":null}}`},
		{"invalid-json", 200, `{"success":true,`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()
			client, err := NewAdminClient(server.URL, "paycores-internal-admin-secret-at-least-32-characters", server.Client(), time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ChannelsOverview(context.Background()); !errors.Is(err, ErrAdminUnavailable) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := NewAdminClient("https://paycores.cc", "short", nil, time.Now); !errors.Is(err, ErrAdminNotConfigured) {
		t.Fatalf("missing key error = %v", err)
	}
}
