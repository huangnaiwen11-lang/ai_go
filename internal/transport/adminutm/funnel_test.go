package adminutm

import (
	"net/http"
	"testing"

	"ai-business-service/internal/transport/sessionauth"
)

func TestFunnelUnavailableHandlerAllowsAdminToSeeDataReadiness(t *testing.T) {
	for _, role := range []string{"admin", "super_admin"} {
		handler := NewFunnelUnavailableHandler(stubAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: role}})
		recorder := call(t, handler, http.MethodGet, "/api/admin/analytics/utm-funnel", "")
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("role=%q status=%d body=%s", role, recorder.Code, recorder.Body.String())
		}
	}
}
