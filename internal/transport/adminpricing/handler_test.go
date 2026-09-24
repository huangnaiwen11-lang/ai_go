package adminpricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	biz "ai-business-service/internal/biz/adminpricing"
	"ai-business-service/internal/transport/sessionauth"
)

type pricingAuthStub struct {
	identity *sessionauth.AuthenticatedIdentity
}

func (stub pricingAuthStub) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return stub.identity, nil
}

type pricingRepositoryStub struct{ configs map[string]biz.SystemConfig }

func (stub *pricingRepositoryStub) LoadSystemConfig(_ context.Context, key, _ string) (*biz.SystemConfig, error) {
	value, ok := stub.configs[key]
	if !ok {
		return nil, biz.ErrNotFound
	}
	copy := value
	return &copy, nil
}

func (stub *pricingRepositoryStub) SaveSystemConfig(_ context.Context, _ biz.Actor, input biz.SystemConfigInput) (biz.SystemConfig, error) {
	value := biz.SystemConfig{Key: input.Key, Value: input.Value, Category: input.Category, Description: input.Description, Environment: input.Environment, Enabled: true}
	stub.configs[input.Key] = value
	return value, nil
}

func (stub *pricingRepositoryStub) DeleteSystemConfig(_ context.Context, _ biz.Actor, key, environment string) (biz.SystemConfig, error) {
	value, ok := stub.configs[key]
	if !ok {
		return biz.SystemConfig{}, biz.ErrNotFound
	}
	value.Environment = environment
	value.Enabled = false
	delete(stub.configs, key)
	return value, nil
}

func adminPricingHandlerForTest(role string) http.Handler {
	repository := &pricingRepositoryStub{configs: map[string]biz.SystemConfig{}}
	return NewHandler(biz.NewOperations(repository), pricingAuthStub{identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: role}})
}

func TestHandlerListsStaticPackages(t *testing.T) {
	handler := adminPricingHandlerForTest("admin")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/wallet/packages", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Packages []map[string]any `json:"packages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Success || len(response.Data.Packages) != 5 {
		t.Fatalf("response = %#v", response)
	}
}

func TestHandlerExposesPayCoresStrategyGate(t *testing.T) {
	handler := adminPricingHandlerForTest("admin")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/wallet/subscription-strategy", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Code != "PAYCORES_NOT_CONFIGURED" {
		t.Fatalf("code = %q", response.Code)
	}
}

func TestHandlerKeepsSystemConfigSuperAdminBoundary(t *testing.T) {
	handler := adminPricingHandlerForTest("admin")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/config/system/wallet.coinPackagesOverrides", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
