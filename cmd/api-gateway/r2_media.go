package main

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ai-business-service/internal/integrations/r2"
	transport "ai-business-service/internal/transport/r2media"
)

const (
	defaultMediaSignedURLExpiry = time.Hour
	defaultMediaRedirectCache   = "public, max-age=300, s-maxage=900, stale-while-revalidate=600"
)

// newOptionalR2MediaHandler installs the three frozen media routes only when
// the complete R2 topology is configured. An entirely absent R2 configuration
// leaves the routes on the fail-closed Node proxy path; a partial configuration
// is an operator error rather than a reason to expose another storage backend.
func newOptionalR2MediaHandler() (http.Handler, func(), error) {
	storageConfig, enabled, err := r2.LoadConfig(os.Getenv)
	if err != nil {
		return nil, nil, err
	}
	if !enabled {
		// The route may be explicitly enabled in a pure-Cling deployment before
		// credentials are installed. Return a local fail-closed handler instead
		// of proxying to an absent Node service (502), while never pretending a
		// private/public media operation succeeded.
		return unavailableR2MediaHandler{}, func() {}, nil
	}
	secret := strings.TrimSpace(os.Getenv("MEDIA_PROXY_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("JWT_SECRET"))
		if secret != "" {
			secret += ":media-proxy"
		}
	}
	if secret == "" {
		return nil, nil, errors.New("MEDIA_PROXY_SECRET or JWT_SECRET is required for R2 media proxy")
	}
	client, err := r2.NewClient(storageConfig)
	if err != nil {
		return nil, nil, err
	}
	handler, err := transport.NewHandler(client, transport.Config{
		ProxySecret:          secret,
		PublicURL:            storageConfig.PublicURL,
		Redirect:             mediaRedirectEnabled(),
		AttachmentRedirect:   mediaAttachmentRedirectEnabled(),
		SharedRedirectCache:  mediaSharedRedirectCacheEnabled(),
		RedirectCacheControl: mediaRedirectCacheControl(),
		Expires:              mediaSignedURLExpiry(),
	})
	if err != nil {
		client.CloseIdleConnections()
		return nil, nil, err
	}
	return handler, client.CloseIdleConnections, nil
}

type unavailableR2MediaHandler struct{}

func (unavailableR2MediaHandler) ServeHTTP(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusServiceUnavailable)
	_, _ = writer.Write([]byte(`{"success":false,"code":"SERVICE_UNAVAILABLE","message":"Service unavailable","details":null}`))
}

func mediaSignedURLExpiry() time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(os.Getenv("MEDIA_PROXY_SIGNED_URL_EXPIRES_SECONDS")))
	if err != nil || seconds <= 0 {
		return defaultMediaSignedURLExpiry
	}
	if seconds > int((7*24*time.Hour)/time.Second) {
		return 7 * 24 * time.Hour
	}
	return time.Duration(seconds) * time.Second
}

func mediaRedirectEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(getenv("MEDIA_PROXY_REDIRECT", "true")), "true")
}
func mediaAttachmentRedirectEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(getenv("MEDIA_PROXY_ATTACHMENT_REDIRECT_ENABLED", "true")), "false")
}
func mediaSharedRedirectCacheEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(getenv("MEDIA_PROXY_SHARED_CACHE_ENABLED", "true")), "false")
}
func mediaRedirectCacheControl() string {
	value := strings.TrimSpace(os.Getenv("MEDIA_PROXY_REDIRECT_CACHE_CONTROL"))
	if value == "" {
		return defaultMediaRedirectCache
	}
	return value
}
