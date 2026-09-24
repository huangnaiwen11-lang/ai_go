package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	bizidentity "ai-business-service/internal/biz/identity"
	"ai-business-service/internal/data"
	"ai-business-service/internal/gateway"
	transportadminapps "ai-business-service/internal/transport/adminapps"
	transportadminmux "ai-business-service/internal/transport/adminmux"
	transportadmin "ai-business-service/internal/transport/adminview"
	"ai-business-service/internal/transport/sessionauth"
)

type rejectingAdminSessionValidator struct{}

func (rejectingAdminSessionValidator) ValidateSession(context.Context, string, time.Time) (*bizidentity.Session, error) {
	return nil, bizidentity.ErrSessionNotFound
}

func TestAdminAppsGatewayAssemblyDoesNotProxyUnauthenticatedRequest(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	authenticator := sessionauth.NewAuthenticator(rejectingAdminSessionValidator{})
	adminHandler := transportadminmux.New(transportadminmux.Options{
		Apps:     transportadminapps.NewHandler(data.NewAdminAppsRepository(&data.Data{}), authenticator),
		Fallback: transportadmin.NewHandler(authenticator, data.NewAdminViewRepository(&data.Data{})),
	})
	instance := newGatewayWithPaymentEntry(
		upstreamURL, gateway.NewFileRouteSwitch(""), 0,
		nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, adminHandler,
	)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil)
	request.Header.Set("Authorization", "Bearer expired-admin-session")
	instance.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("/api/admin/apps unexpectedly proxied to Node %d time(s)", upstreamCalls)
	}
}

func TestAdminStagingMongoEnabled仅接受显式True(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: "false", want: false},
		{value: "1", want: false},
		{value: "true", want: true},
		{value: "TRUE", want: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("GATEWAY_ADMIN_MONGO_ENABLED", tc.value)
			if got := adminStagingMongoEnabled(); got != tc.want {
				t.Fatalf("adminStagingMongoEnabled() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestAdminStagingMongoConfigFromEnvironment要求完整配置(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_MONGO_URI", "")
	t.Setenv("GATEWAY_ADMIN_MONGO_DATABASE", "")
	if _, err := adminStagingMongoConfigFromEnvironment(); err == nil || !strings.Contains(err.Error(), "GATEWAY_ADMIN_MONGO_URI") {
		t.Fatalf("missing URI error = %v", err)
	}

	t.Setenv("GATEWAY_ADMIN_MONGO_URI", "mongodb://staging-user:staging-pass@host.docker.internal:27019/ai-host-v2-staging?authSource=admin&directConnection=true")
	if _, err := adminStagingMongoConfigFromEnvironment(); err == nil || !strings.Contains(err.Error(), "GATEWAY_ADMIN_MONGO_DATABASE") {
		t.Fatalf("missing database error = %v", err)
	}

	t.Setenv("GATEWAY_ADMIN_MONGO_DATABASE", "ai-host-v2-staging")
	config, err := adminStagingMongoConfigFromEnvironment()
	if err != nil {
		t.Fatalf("valid staging config error = %v", err)
	}
	if got := config.GetMongo().GetDatabase(); got != "ai-host-v2-staging" {
		t.Fatalf("database = %q", got)
	}
	if got := config.GetMongo().GetReplicaSet(); got != "" {
		t.Fatalf("replica set = %q, want empty directConnection profile", got)
	}
}

func TestAdminStagingMongoConfigFromEnvironment拒绝公网URI(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_MONGO_URI", "mongodb://staging-user:staging-pass@mongo.example.test:27019/ai-host-v2-staging?authSource=admin&directConnection=true")
	t.Setenv("GATEWAY_ADMIN_MONGO_DATABASE", "ai-host-v2-staging")
	if _, err := adminStagingMongoConfigFromEnvironment(); err == nil || !strings.Contains(err.Error(), "mongo host") {
		t.Fatalf("public URI error = %v", err)
	}
}
