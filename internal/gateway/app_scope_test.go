package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-business-service/internal/biz/runtimeapp"
)

type appScopeTestResolver struct {
	app   runtimeapp.App
	err   error
	calls int
}

func (resolver *appScopeTestResolver) Resolve(context.Context, string, string) (runtimeapp.App, error) {
	resolver.calls++
	return resolver.app, resolver.err
}

type allRoutesEnabled struct{}

func (allRoutesEnabled) Enabled(exactRouteKey) bool { return true }

func TestGatewayClientAppScopeGuardsLocalClientRoutes(t *testing.T) {
	tests := []struct {
		name          string
		platform      string
		identifier    string
		resolved      runtimeapp.App
		resolveErr    error
		wantStatus    int
		wantResolver  int
		wantLocalCall int
	}{
		{
			name:          "matching active app",
			platform:      "web",
			identifier:    "app.example.com",
			resolved:      runtimeapp.App{ID: "app-expected"},
			wantStatus:    http.StatusNoContent,
			wantResolver:  1,
			wantLocalCall: 1,
		},
		{name: "missing platform", identifier: "app.example.com", wantStatus: http.StatusForbidden},
		{name: "missing app id", platform: "web", wantStatus: http.StatusForbidden},
		{
			name:         "unknown app",
			platform:     "web",
			identifier:   "unknown.example.com",
			resolveErr:   runtimeapp.ErrUnresolved,
			wantStatus:   http.StatusForbidden,
			wantResolver: 1,
		},
		{
			name:         "different app instance",
			platform:     "web",
			identifier:   "other.example.com",
			resolved:     runtimeapp.App{ID: "app-other"},
			wantStatus:   http.StatusForbidden,
			wantResolver: 1,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			upstreamCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				upstreamCalls++
			}))
			t.Cleanup(upstream.Close)
			resolver := &appScopeTestResolver{app: testCase.resolved, err: testCase.resolveErr}
			localCalls := 0
			instance := New(Config{
				DefaultUpstream:   mustURL(t, upstream.URL),
				RouteSwitch:       enabledRouteSwitch{enabled: exactRouteKey{method: http.MethodPost, path: "/api/auth/register"}},
				ClientAppResolver: resolver,
				ClientAppID:       "app-expected",
				AuthEntryHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					localCalls++
					w.WriteHeader(http.StatusNoContent)
				}),
			})
			request := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
			if testCase.platform != "" {
				request.Header.Set("X-Client-Platform", testCase.platform)
			}
			if testCase.identifier != "" {
				request.Header.Set("X-Client-App-Id", testCase.identifier)
			}
			recorder := httptest.NewRecorder()
			instance.Handler().ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if resolver.calls != testCase.wantResolver {
				t.Fatalf("resolver calls = %d, want %d", resolver.calls, testCase.wantResolver)
			}
			if localCalls != testCase.wantLocalCall {
				t.Fatalf("local calls = %d, want %d", localCalls, testCase.wantLocalCall)
			}
			if upstreamCalls != 0 {
				t.Fatalf("rejected local request unexpectedly proxied to Node %d time(s)", upstreamCalls)
			}
		})
	}
}

func TestGatewayClientAppScopeDisabledPreservesLocalRoute(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	localCalls := 0
	instance := New(Config{
		DefaultUpstream: mustURL(t, upstream.URL),
		RouteSwitch:     enabledRouteSwitch{enabled: exactRouteKey{method: http.MethodPost, path: "/api/auth/register"}},
		AuthEntryHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			localCalls++
			w.WriteHeader(http.StatusNoContent)
		}),
	})
	recorder := httptest.NewRecorder()
	instance.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/auth/register", nil))
	if recorder.Code != http.StatusNoContent || localCalls != 1 {
		t.Fatalf("scope disabled response = %d, local calls = %d; want 204/1", recorder.Code, localCalls)
	}
}

func TestGatewayClientAppScopeBypassesCallbacksAdminAndNode(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusResetContent)
	}))
	t.Cleanup(upstream.Close)
	resolver := &appScopeTestResolver{err: errors.New("resolver must not be called")}
	instance := New(Config{
		DefaultUpstream:    mustURL(t, upstream.URL),
		RouteSwitch:        allRoutesEnabled{},
		ClientAppResolver:  resolver,
		ClientAppID:        "app-expected",
		GenerationCallback: http.HandlerFunc(statusHandler(http.StatusCreated)),
		ProviderCallback:   http.HandlerFunc(statusHandler(http.StatusAccepted)),
		PaymentCallback:    http.HandlerFunc(statusHandler(http.StatusNonAuthoritativeInfo)),
		AdminHandler:       http.HandlerFunc(statusHandler(http.StatusNoContent)),
	})

	for _, testCase := range []struct {
		name, method, path string
		wantStatus         int
		wantUpstreamCalls  int
	}{
		{"generation callback", http.MethodPost, generationCallbackPathV1, http.StatusCreated, 0},
		{"provider callback", http.MethodPost, providerCallbackPath, http.StatusAccepted, 0},
		{"payment callback", http.MethodPost, paymentCallbackPathV1, http.StatusNonAuthoritativeInfo, 0},
		{"admin", http.MethodGet, "/api/admin/apps", http.StatusNoContent, 0},
		{"node proxy", http.MethodGet, "/api/unmigrated", http.StatusResetContent, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			instance.Handler().ServeHTTP(recorder, httptest.NewRequest(testCase.method, testCase.path, nil))
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if upstreamCalls != testCase.wantUpstreamCalls {
				t.Fatalf("upstream calls = %d, want %d", upstreamCalls, testCase.wantUpstreamCalls)
			}
			if resolver.calls != 0 {
				t.Fatalf("bypass request unexpectedly called resolver %d time(s)", resolver.calls)
			}
		})
	}
}

func statusHandler(status int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }
}
