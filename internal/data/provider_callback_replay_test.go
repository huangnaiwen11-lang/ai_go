package data

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-business-service/internal/biz/generation"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
	"ai-business-service/internal/transport/providercallback"
)

// 验证真实验签、HTTP ACK、Mongo inbox、终态 CAS 和消费状态之间的组合契约。
// 所有 URL 仅为报文内容，不发出任何供应商请求。
func TestProviderCallbackHTTPMongoConsumedReplay(t *testing.T) {
	f, terminal := seedProviderTerminal(t)
	const secret = "test-only-provider-callback-secret-32-bytes"
	const tenant = "test-tenant"
	verifier, err := polarstarb2b.NewCallbackVerifier(secret, tenant, terminal.AccountRef)
	if err != nil {
		t.Fatal(err)
	}
	ack := generation.NewProviderCallbackUsecaseWithClock(NewGenerationProviderInboxRepository(f.data), f.runner, func() time.Time { return f.now })
	handler := providercallback.NewHandler(verifier, ack)
	inbox := NewGenerationProviderInboxConsumerStore(f.data)
	consumer := generation.NewProviderInboxConsumerUsecaseWithClock(inbox, providerIntentStore(t, f), NewGenerationCallbackRepository(f.data), f.runner, func() time.Time { return f.now })
	parser, err := platform.NewB2BObservationParser(tenant, terminal.AccountRef)
	if err != nil {
		t.Fatal(err)
	}
	for index, suffix := range []string{"first", "second"} {
		deliveryID := suffix + "-" + f.stepID
		body, err := json.Marshal(map[string]any{
			"contractVersion": "b2b.callback.v2", "tenantId": tenant, "jobId": terminal.ExternalExecutionID,
			"externalId": f.stepID, "capability": "text_to_image", "status": "completed", "deliveryId": deliveryID,
			"output": map[string]string{"resultUrl": "https://cdn.example.com/image.png"},
		})
		if err != nil {
			t.Fatal(err)
		}
		post := func(payload []byte) int {
			req := httptest.NewRequest(http.MethodPost, platform.B2BCallbackPath, bytes.NewReader(payload)).WithContext(f.ctx)
			mac := hmac.New(sha256.New, []byte(secret))
			_, _ = mac.Write(payload)
			req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
			req.Header.Set("X-Job-Id", terminal.ExternalExecutionID)
			req.Header.Set("X-Delivery-Id", deliveryID)
			req.Header.Set("X-Callback-Attempt", "1")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			return response.Code
		}
		if code := post(body); code != http.StatusOK {
			t.Fatalf("initial ACK: %d", code)
		}
		key := generation.ProviderInboxKey{Source: generation.ProviderInboxSource, AccountRef: terminal.AccountRef, DeliveryID: deliveryID}
		record, err := inbox.ReadProviderInbox(f.ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := parser.Parse(record)
		if err != nil {
			t.Fatal(err)
		}
		result, err := consumer.Consume(f.ctx, key, observation)
		want := generation.ProviderTerminalApplied
		if index > 0 {
			want = generation.ProviderTerminalNoop
		}
		if err != nil || result != want {
			t.Fatalf("consume %s: %s / %v", suffix, result, err)
		}
		if code := post(body); code != http.StatusOK {
			t.Fatalf("consumed replay: %d", code)
		}
		record, err = inbox.ReadProviderInbox(f.ctx, key)
		if err != nil || record.Status != generation.ProviderInboxApplied {
			t.Fatalf("delivery not applied: %#v / %v", record, err)
		}
		// 相同 delivery 更换原文字节仍须冲突；签名正确也无权覆盖已接受事实。
		if code := post(append(body, ' ')); code != http.StatusConflict {
			t.Fatalf("changed raw bytes: %d", code)
		}
		record, err = inbox.ReadProviderInbox(f.ctx, key)
		if err != nil || record.Status != generation.ProviderInboxQuarantined {
			t.Fatalf("conflict not durable: %#v / %v", record, err)
		}
	}
}
