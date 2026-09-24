package generation

import (
	"encoding/json"
	"strings"
	"testing"

	"ai-business-service/internal/conf"
)

func TestConfiguredLocalClientRejectsAnotherProvider(t *testing.T) {
	for _, provider := range []string{"polarstar_b2b_v2", "unknown_provider"} {
		t.Run(provider, func(t *testing.T) {
			security, integrations := localProviderFixture()
			// JSON keeps this regression executable before the config schema grows.
			if err := json.Unmarshal([]byte(`{"provider":"`+provider+`"}`), integrations.Generation); err != nil {
				t.Fatal(err)
			}
			client, err := NewConfiguredClient(security, integrations)
			if err == nil || client != nil {
				t.Fatal("local client must not be constructed for another provider")
			}
			if strings.Contains(err.Error(), integrations.Generation.ApiKey) {
				t.Fatal("provider error exposed a credential")
			}
		})
	}
}

func TestConfiguredLocalClientPreservesDefaultAndExplicitLocal(t *testing.T) {
	for _, provider := range []string{"", "local_execution_v2"} {
		security, integrations := localProviderFixture()
		if err := json.Unmarshal([]byte(`{"provider":"`+provider+`"}`), integrations.Generation); err != nil {
			t.Fatal(err)
		}
		client, err := NewConfiguredClient(security, integrations)
		if err != nil || client == nil {
			t.Fatalf("local provider construction: %v", err)
		}
	}
}

func localProviderFixture() (*conf.Security, *conf.Integrations) {
	return &conf.Security{GenerationRequestHmacKey: strings.Repeat("r", 32)}, &conf.Integrations{
		Generation: &conf.Integrations_Generation{
			BaseUrl: "http://127.0.0.1:19081", CallbackBaseUrl: "http://127.0.0.1:18000", ApiKey: "test-only-local-key",
		},
	}
}
