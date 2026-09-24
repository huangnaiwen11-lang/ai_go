package adminutm

import (
	"net/http"
	"strings"

	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/adminauth"
)

// NewFunnelUnavailableHandler keeps the analytics route's admin read access
// independent from UTM-link mutations, which correctly remain super-admin
// only. The old Node route also accepts partner operators with a scoped
// analytics permission; Go sessions do not yet carry that permission/scope,
// so those identities still fail closed until the analytics projection exists.
func NewFunnelUnavailableHandler(authenticator authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticator == nil {
			failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "UTM funnel handler unavailable")
			return
		}
		identity, err := authenticator.Authenticate(r)
		if err != nil {
			adminauth.WriteDenial(w, err)
			return
		}
		if identity == nil || strings.TrimSpace(identity.UserID) == "" {
			adminauth.WriteDenial(w, shared.ErrUnauthenticated)
			return
		}
		if identity.Role != "admin" && identity.Role != "super_admin" {
			adminauth.WriteDenial(w, adminauth.ErrForbidden)
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "UTM funnel analytics data source is not ready")
	})
}
