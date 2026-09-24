package adminpaycores

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/integrations/paycores"
	"ai-business-service/internal/transport/adminauth"
	"ai-business-service/internal/transport/sessionauth"
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type channelReader interface {
	ChannelsOverview(context.Context) (json.RawMessage, error)
}

type handler struct {
	authenticator authenticator
	channels      channelReader
}

func NewHandler(authenticator authenticator, channels channelReader) http.Handler {
	return &handler{authenticator: authenticator, channels: channels}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.authenticator == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Admin session unavailable")
		return
	}
	identity, err := h.authenticator.Authenticate(r)
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
	if r.URL.Path != "/api/admin/paycores/channels-overview" {
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This PayCores admin API has not been migrated")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		failure(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if h.channels == nil {
		failure(w, http.StatusServiceUnavailable, "PAYCORES_NOT_CONFIGURED", "PayCores admin integration is not configured")
		return
	}
	data, err := h.channels.ChannelsOverview(r.Context())
	if err != nil {
		if errors.Is(err, paycores.ErrAdminNotConfigured) {
			failure(w, http.StatusServiceUnavailable, "PAYCORES_NOT_CONFIGURED", "PayCores admin integration is not configured")
		} else {
			failure(w, http.StatusBadGateway, "PAYCORES_UNAVAILABLE", "PayCores channels overview unavailable")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func failure(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}
