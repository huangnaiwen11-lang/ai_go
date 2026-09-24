package gateway

import (
	"context"
	"net/http"
	"strings"

	"ai-business-service/internal/biz/runtimeapp"
)

// ClientAppResolver is the sole App-instance dependency admitted into the
// Gateway. It deliberately exposes no App configuration or secrets.
type ClientAppResolver interface {
	Resolve(context.Context, string, string) (runtimeapp.App, error)
}

func (gateway *Gateway) allowClientApp(writer http.ResponseWriter, request *http.Request) bool {
	if gateway.clientAppResolver == nil {
		return true
	}
	if request == nil {
		writeClientAppScopeDenied(writer)
		return false
	}
	platform, ok := singleHeaderValue(request.Header, "X-Client-Platform")
	if !ok {
		writeClientAppScopeDenied(writer)
		return false
	}
	identifier, ok := singleHeaderValue(request.Header, "X-Client-App-Id")
	if !ok {
		writeClientAppScopeDenied(writer)
		return false
	}
	app, err := gateway.clientAppResolver.Resolve(request.Context(), platform, identifier)
	if err != nil || app.ID != gateway.clientAppID {
		writeClientAppScopeDenied(writer)
		return false
	}
	return true
}

func singleHeaderValue(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) != 1 {
		return "", false
	}
	value := strings.TrimSpace(values[0])
	return value, value != ""
}

func writeClientAppScopeDenied(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusForbidden)
	_, _ = writer.Write([]byte(`{"success":false,"code":"APP_SCOPE_DENIED","message":"Client app is not authorized","details":null}`))
}
