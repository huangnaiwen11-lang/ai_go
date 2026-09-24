package adminapps

import (
	"net/http"
	"strings"

	biz "ai-business-service/internal/biz/adminapps"
)

// dispatchPlatforms owns the AppsPage platform-config contract. This remains
// in the Apps transport because its client identifiers are derived from the
// App registry and native review mode is deliberately routed through Apps.
func (h *Handler) dispatchPlatforms(w http.ResponseWriter, r *http.Request, actor biz.Actor, suffix string) {
	suffix = strings.TrimPrefix(suffix, "/")
	if suffix == "" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		configs, err := h.repository.ListPlatformConfigs(r.Context(), r.URL.Query().Get("includeDisabled") == "true")
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, map[string]any{"platforms": configs})
		return
	}
	parts := strings.Split(suffix, "/")
	platform := parts[0]
	if !biz.IsPlatformName(platform) {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid platform")
		return
	}
	clientID := strings.TrimSpace(r.URL.Query().Get("clientId"))
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			config, err := h.repository.GetPlatformConfig(r.Context(), platform, clientID)
			if err != nil {
				writeRepositoryError(w, err)
				return
			}
			success(w, http.StatusOK, map[string]any{"config": config})
		case http.MethodPut:
			payload, ok := decodeWritePayload(w, r)
			if !ok {
				return
			}
			config, err := h.repository.UpdatePlatformConfig(r.Context(), actor, platform, clientID, payload)
			if err != nil {
				writeRepositoryError(w, err)
				return
			}
			success(w, http.StatusOK, map[string]any{"config": config})
		case http.MethodDelete:
			if err := h.repository.DeletePlatformConfig(r.Context(), actor, platform, clientID); err != nil {
				writeRepositoryError(w, err)
				return
			}
			success(w, http.StatusOK, map[string]any{"success": true})
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "clone" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		payload, ok := decodeWritePayload(w, r)
		if !ok {
			return
		}
		config, err := h.repository.ClonePlatformConfig(r.Context(), actor, platform, payload)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, map[string]any{"config": config})
		return
	}
	if len(parts) == 2 && parts[1] == "preview" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		preview, err := h.repository.PlatformPreview(r.Context(), platform, clientID, r.URL.Query().Get("userTier"))
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, map[string]any{"preview": preview})
		return
	}
	if len(parts) == 2 && (parts[1] == "content-policy" || parts[1] == "providers" || parts[1] == "features") {
		if r.Method != http.MethodPatch {
			methodNotAllowed(w, http.MethodPatch)
			return
		}
		payload, ok := decodeWritePayload(w, r)
		if !ok {
			return
		}
		if clientID == "" {
			clientID = strings.TrimSpace(stringValueForRoute(payload["clientId"]))
		}
		config, err := h.repository.PatchPlatformConfig(r.Context(), actor, platform, clientID, parts[1], payload)
		if err != nil {
			writeRepositoryError(w, err)
			return
		}
		success(w, http.StatusOK, map[string]any{"config": config})
		return
	}
	notMigrated(w)
}

func stringValueForRoute(value any) string { text, _ := value.(string); return text }
