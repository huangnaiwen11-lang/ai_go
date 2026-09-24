package gateway

import (
	"net/http"
	"strings"
)

const creationCancelRoutePrefix = "/api/creations/"

// creationCancelRoute uses the repository-wide :id placeholder convention.
// It must remain byte-for-byte identical to the deployment route-switch key;
// an unrecognized key invalidates the entire fail-closed switch document.
var creationCancelRoute = exactRouteKey{method: http.MethodPost, path: "/api/creations/:id/cancel"}

// matchCreationCancelRoute accepts one unescaped ID path component and no
// query. The domain handler owns the ID's semantic validation; this layer only
// protects the exact Gateway routing boundary.
func matchCreationCancelRoute(request *http.Request) (exactRouteKey, bool) {
	if request == nil || request.URL == nil || request.Method != creationCancelRoute.method || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.EscapedPath() != request.URL.Path || !strings.HasPrefix(request.URL.Path, creationCancelRoutePrefix) || !strings.HasSuffix(request.URL.Path, "/cancel") {
		return exactRouteKey{}, false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, creationCancelRoutePrefix), "/cancel")
	if id == "" || strings.Contains(id, "/") {
		return exactRouteKey{}, false
	}
	return creationCancelRoute, true
}
