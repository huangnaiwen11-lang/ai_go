// Package adminpricing exposes the Go-owned pricing configuration contracts.
// Static catalog defaults are kept in adminpricing business code; Mongo stores
// only SystemConfig-compatible overrides and every mutation is audited.
package adminpricing

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	biz "ai-business-service/internal/biz/adminpricing"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/adminauth"
	"ai-business-service/internal/transport/sessionauth"
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type Handler struct {
	operations    *biz.Operations
	authenticator authenticator
}

func NewHandler(operations *biz.Operations, authenticator authenticator) http.Handler {
	return &Handler{operations: operations, authenticator: authenticator}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.operations == nil || h.authenticator == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Admin pricing handler unavailable")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	switch {
	case path == "wallet/packages":
		h.packages(w, r)
	case path == "wallet/vip-config":
		h.vip(w, r)
	case path == "wallet/subscription-strategy":
		h.strategy(w, r)
	case strings.HasPrefix(path, "config/system"):
		h.systemConfig(w, r, strings.TrimPrefix(path, "config/system"))
	default:
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This pricing API has not been migrated to the Go Gateway")
	}
}

func (h *Handler) packages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if _, ok := h.authorize(w, r, false); !ok {
		return
	}
	packages, err := h.operations.Packages(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(packages))
	for _, item := range packages {
		label := item.ID
		if item.Label != nil {
			if value := item.Label["en"]; value != "" {
				label = value
			}
		}
		var badge any
		if item.Badge != nil {
			badge = *item.Badge
		}
		rows = append(rows, map[string]any{
			"id": item.ID, "coins": item.Coins, "bonusCoins": item.BonusCoins,
			"priceInCents": item.PriceInCents, "label": label, "labelObj": item.Label,
			"icon": item.Icon, "popular": item.Popular, "badge": badge,
			"enabled": item.Enabled, "allowedChannels": emptyStrings(item.AllowedChannels),
			"_builtin": item.Builtin, "_custom": item.Custom,
		})
	}
	success(w, map[string]any{"packages": rows})
}

func (h *Handler) vip(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.authorize(w, r, false)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		snapshot, err := h.operations.VIP(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		success(w, snapshot)
	case http.MethodPut:
		payload, valid := decodeVIPPayload(w, r)
		if !valid {
			return
		}
		snapshot, err := h.operations.SaveVIP(r.Context(), actor, payload.Overrides, payload.ChangeReason)
		if err != nil {
			writeError(w, err)
			return
		}
		success(w, map[string]any{"saved": true, "effective": snapshot.Effective})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

func (h *Handler) strategy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
		return
	}
	if _, ok := h.authorize(w, r, false); !ok {
		return
	}
	_, err := h.operations.SubscriptionStrategy(r.Context())
	writeError(w, err)
}

func (h *Handler) systemConfig(w http.ResponseWriter, r *http.Request, suffix string) {
	if strings.HasPrefix(suffix, "/") {
		suffix = strings.TrimPrefix(suffix, "/")
	}
	if suffix == "" || strings.Contains(suffix, "/") {
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This system config API has not been migrated to the Go Gateway")
		return
	}
	key := suffix
	if !isSupportedSystemKey(key) {
		failure(w, http.StatusNotImplemented, "ADMIN_API_NOT_MIGRATED", "This system config key has not been migrated to the Go Gateway")
		return
	}
	actor, ok := h.authorize(w, r, true)
	if !ok {
		return
	}
	environment := r.URL.Query().Get("environment")
	if environment == "" {
		environment = "all"
	}
	switch r.Method {
	case http.MethodGet:
		config, err := h.operations.SystemConfig(r.Context(), key, environment)
		if err != nil {
			writeError(w, err)
			return
		}
		success(w, map[string]any{"config": configView(config)})
	case http.MethodPut:
		payload, valid := decodeSystemPayload(w, r)
		if !valid {
			return
		}
		config, err := h.operations.SaveSystemConfig(r.Context(), actor, biz.SystemConfigInput{Key: key, Value: payload.Value, Category: payload.Category, Description: payload.Description, Environment: environment, ChangeReason: payload.ChangeReason})
		if err != nil {
			writeError(w, err)
			return
		}
		success(w, map[string]any{"config": configView(config)})
	case http.MethodDelete:
		config, err := h.operations.DeleteSystemConfig(r.Context(), actor, key, environment)
		if err != nil {
			writeError(w, err)
			return
		}
		success(w, map[string]any{"config": configView(config)})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, superAdmin bool) (biz.Actor, bool) {
	identity, err := h.authenticator.Authenticate(r)
	if err != nil {
		adminauth.WriteDenial(w, err)
		return biz.Actor{}, false
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		adminauth.WriteDenial(w, shared.ErrUnauthenticated)
		return biz.Actor{}, false
	}
	if identity.Role != "admin" && identity.Role != "super_admin" {
		adminauth.WriteDenial(w, adminauth.ErrForbidden)
		return biz.Actor{}, false
	}
	if superAdmin && identity.Role != "super_admin" {
		adminauth.WriteDenial(w, adminauth.ErrForbidden)
		return biz.Actor{}, false
	}
	return biz.Actor{ID: identity.UserID, Role: identity.Role}, true
}

type vipPayload struct {
	Overrides    map[string]any `json:"overrides"`
	ChangeReason string         `json:"changeReason"`
}

func decodeVIPPayload(w http.ResponseWriter, r *http.Request) (vipPayload, bool) {
	var payload vipPayload
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.Overrides == nil || decoder.Decode(new(any)) != io.EOF {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid VIP config payload")
		return vipPayload{}, false
	}
	return payload, true
}

type systemPayload struct {
	Value        json.RawMessage `json:"value"`
	Category     string          `json:"category"`
	Description  string          `json:"description"`
	ChangeReason string          `json:"changeReason"`
}

type decodedSystemPayload struct {
	Value        any
	Category     string
	Description  string
	ChangeReason string
}

func decodeSystemPayload(w http.ResponseWriter, r *http.Request) (decodedSystemPayload, bool) {
	var payload systemPayload
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.Value == nil || decoder.Decode(new(any)) != io.EOF {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid system config payload")
		return decodedSystemPayload{}, false
	}
	var value any
	if err := json.Unmarshal(payload.Value, &value); err != nil {
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid system config value")
		return decodedSystemPayload{}, false
	}
	return decodedSystemPayload{Value: value, Category: payload.Category, Description: payload.Description, ChangeReason: payload.ChangeReason}, true
}

func configView(config biz.SystemConfig) map[string]any {
	return map[string]any{"key": config.Key, "value": config.Value, "category": config.Category, "description": config.Description, "environment": config.Environment, "enabled": config.Enabled, "updatedAt": config.UpdatedAt, "createdAt": config.CreatedAt}
}

func isSupportedSystemKey(key string) bool {
	return key == biz.CoinPackagesKey || key == "wallet.firstRechargeBonusCoins"
}
func emptyStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func success(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func failure(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	failure(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
}

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, biz.ErrInvalid):
		failure(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid pricing configuration")
	case errors.Is(err, biz.ErrNotFound):
		failure(w, http.StatusNotFound, "NOT_FOUND", "Pricing configuration not found")
	case errors.Is(err, biz.ErrPayCoresUnavailable):
		failure(w, http.StatusServiceUnavailable, "PAYCORES_NOT_CONFIGURED", "Subscription strategy is managed by PayCores and is not configured")
	case errors.Is(err, shared.ErrUnauthenticated):
		failure(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
	default:
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Pricing service unavailable")
	}
}
