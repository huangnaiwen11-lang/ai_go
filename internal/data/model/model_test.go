package model

import (
	"bytes"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// emptyBSONDocument 是合法的空 BSON 文档：前四字节表示长度 5，最后一个字节是终止符。
var emptyBSONDocument = bson.Raw{5, 0, 0, 0, 0}

func Test关键持久化对象的BSON字段名(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		document any
		fields   []string
	}{
		{
			name: "账户",
			document: AccountDocument{
				ID:             "user-1",
				DiamondBalance: 20,
				CreatedAt:      now,
				UpdatedAt:      now,
			},
			fields: []string{
				"_id",
				"diamond_balance",
				"created_at",
				"updated_at",
			},
		},
		{
			name: "用户",
			document: UserDocument{
				ID:             "user-1",
				Timezone:       "Asia/Shanghai",
				AccountStatus:  "active",
				BindingState:   "bound",
				SessionVersion: 1,
				ContentAccess:  "standard",
				CreatedAt:      now,
				UpdatedAt:      now,
			},
			fields: []string{
				"_id",
				"timezone",
				"account_status",
				"binding_state",
				"session_version",
				"content_access",
			},
		},
		{
			name: "创作",
			document: CreationDocument{
				ID:                 "creation-1",
				IdempotencyKey:     "idem-1",
				UserID:             "user-1",
				TemplateID:         "template-1",
				TemplateVersion:    2,
				ProductOutput:      "image",
				RequestFingerprint: "fingerprint-1",
				ParentID:           "parent-1",
				Status:             "pending",
				Version:            1,
				CreatedAt:          now,
				UpdatedAt:          now,
			},
			fields: []string{
				"_id",
				"idempotency_key",
				"user_id",
				"template_id",
				"template_version",
				"product_output",
				"request_fingerprint",
				"parent_id",
				"status",
				"version",
			},
		},
		{
			name: "订阅快照",
			document: SubscriptionDocument{
				UserID:        "user-1",
				Status:        "active",
				BillingPeriod: "monthly",
				StartsAt:      now,
				ExpiresAt:     now.Add(30 * 24 * time.Hour),
				CreatedAt:     now,
				UpdatedAt:     now,
			},
			fields: []string{
				"_id",
				"status",
				"billing_period",
				"starts_at",
				"expires_at",
				"created_at",
				"updated_at",
			},
		},
		{
			name: "预留",
			document: ReservationDocument{
				ID:               "reservation-1",
				CreationID:       "creation-1",
				UserID:           "user-1",
				PriceDiamonds:    20,
				ReservedDiamonds: 20,
				BenefitSource:    "diamonds",
				QuotaKind:        "vip_daily_image",
				LocalDate:        "2026-09-05",
				QuotaLimit:       1,
				QuotaUnits:       1,
				Status:           "reserved",
				CreatedAt:        now,
				UpdatedAt:        now,
			},
			fields: []string{
				"_id",
				"creation_id",
				"user_id",
				"price_diamonds",
				"reserved_diamonds",
				"benefit_source",
				"quota_kind",
				"local_date",
				"quota_limit",
				"quota_units",
				"status",
				"created_at",
				"updated_at",
			},
		},
		{
			name: "账本分录",
			document: LedgerEntryDocument{
				ID:             "reserve:creation-1",
				IdempotencyKey: "reserve:creation-1",
				AccountID:      "user-1",
				CreationID:     "creation-1",
				DeltaDiamonds:  -20,
				Reason:         "generation_reserved",
				ReservationID:  "reservation:creation-1",
				CreatedAt:      now,
			},
			fields: []string{
				"_id",
				"idempotency_key",
				"account_id",
				"creation_id",
				"delta_diamonds",
				"reason",
				"reservation_id",
				"created_at",
			},
		},
		{
			name: "支付商品",
			document: PaymentProductDocument{
				ProductID:     "coins-100",
				Version:       1,
				DiamondAmount: 100,
				PublishStatus: "published",
			},
			fields: []string{
				"product_id",
				"version",
				"diamond_amount",
				"publish_status",
			},
		},
		{
			name: "支付订单",
			document: PaymentOrderDocument{
				ID:              "order-1",
				UserID:          "user-1",
				Provider:        "provider-a",
				ProviderOrderID: "provider-order-1",
				ProductID:       "coins-100",
				ProductVersion:  1,
				DiamondAmount:   100,
				Status:          "pending",
				CreatedAt:       now,
				UpdatedAt:       now,
			},
			fields: []string{
				"_id",
				"user_id",
				"provider",
				"provider_order_id",
				"product_id",
				"product_version",
				"diamond_amount",
				"status",
				"created_at",
				"updated_at",
			},
		},
		{
			name: "支付回执",
			document: PaymentReceiptDocument{
				ID:                    "receipt-1",
				Provider:              "provider-a",
				ExternalTransactionID: "transaction-1",
				PaymentOrderID:        "order-1",
				LedgerEntryID:         "ledger-1",
				ReceivedAt:            now,
			},
			fields: []string{
				"_id",
				"provider",
				"external_transaction_id",
				"payment_order_id",
				"ledger_entry_id",
			},
		},
		{
			name: "回调回执",
			document: CallbackReceiptDocument{
				ID:              "callback-1",
				Source:          "generator",
				NonceHash:       "nonce-hash-1",
				ReceivedAt:      now,
				PayloadDigest:   "payload-digest-1",
				CreationID:      "creation-1",
				StepID:          "step-1",
				ExternalRef:     "step-1",
				JobID:           "job-1",
				Capability:      "text_to_image",
				Terminal:        "completed",
				MediaType:       "image",
				ResultURL:       "https://assets.example.test/result.png",
				CallbackVersion: "2",
			},
			fields: []string{
				"_id",
				"source",
				"nonce_hash",
				"received_at",
				"payload_digest",
				"creation_id",
				"step_id",
				"external_ref",
				"job_id",
				"capability",
				"terminal",
				"media_type",
				"result_url",
				"callback_version",
			},
		},
		{
			name: "模板",
			document: TemplateDocument{
				ID:             "template-doc-1",
				TemplateID:     "template-1",
				Version:        1,
				ContentSurface: "creation",
				Mode:           "image",
				SortOrder:      1,
				Enabled:        true,
				Parameters:     emptyBSONDocument,
				CreatedAt:      now,
				UpdatedAt:      now,
			},
			fields: []string{
				"_id",
				"template_id",
				"version",
				"content_surface",
				"mode",
				"sort_order",
				"enabled",
				"parameters",
			},
		},
		{
			name: "发件箱事件",
			document: OutboxEventDocument{
				ID:             "outbox-1",
				AggregateID:    "creation-1",
				EventType:      "generation.submission",
				Payload:        []byte(`{"prompt":"x","assets":[]}`),
				DeliveryStatus: "dispatching",
				AttemptCount:   0,
				NextAttemptAt:  now,
				LeaseToken:     "lease-token",
				LeaseUntil:     now.Add(time.Minute),
				LeaseOwner:     "worker-1",
				LastError:      "safe error summary",
				// 关注原因与 last_error 分开持久化：Requeue 每轮都会覆盖
				// last_error，把原因塞进去会让它在重新入队之后消失。
				AttentionReason: "provider_result_material_upload_budget_exhausted",
				CreatedAt:       now,
				UpdatedAt:       now,
			},
			fields: []string{
				"_id",
				"aggregate_id",
				"event_type",
				"payload",
				"delivery_status",
				"attempt_count",
				"next_attempt_at",
				"lease_token",
				"lease_until",
				"lease_owner",
				"last_error",
				"attention_reason",
				"created_at",
				"updated_at",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := bson.Marshal(tt.document)
			if err != nil {
				t.Fatalf("序列化 BSON: %v", err)
			}

			var decoded bson.M
			if err := bson.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("反序列化 BSON: %v", err)
			}

			for _, field := range tt.fields {
				if _, ok := decoded[field]; !ok {
					t.Errorf("BSON 文档缺少字段 %q: %#v", field, decoded)
				}
			}
		})
	}
}

func Test回调回执BSON字段精确等于持久化契约(t *testing.T) {
	now := time.Date(2026, time.September, 7, 5, 0, 0, 0, time.UTC)
	document := CallbackReceiptDocument{
		ID:              "callback-1",
		Source:          "generation.execution.v2",
		NonceHash:       "nonce-hash-1",
		ReceivedAt:      now,
		PayloadDigest:   "payload-digest-1",
		CreationID:      "creation-1",
		StepID:          "step-1",
		ExternalRef:     "step-1",
		JobID:           "job-1",
		Capability:      "text_to_image",
		Terminal:        "completed",
		MediaType:       "image",
		ResultURL:       "https://assets.example.test/result.png",
		CallbackVersion: "2",
	}
	encoded, err := bson.Marshal(document)
	if err != nil {
		t.Fatalf("序列化回调回执 BSON: %v", err)
	}
	var decoded bson.M
	if err := bson.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化回调回执 BSON: %v", err)
	}
	wantFields := map[string]struct{}{
		"_id": {}, "source": {}, "nonce_hash": {}, "received_at": {}, "payload_digest": {},
		"creation_id": {}, "step_id": {}, "external_ref": {}, "job_id": {}, "capability": {},
		"terminal": {}, "media_type": {}, "result_url": {}, "callback_version": {},
	}
	if len(decoded) != len(wantFields) {
		t.Fatalf("回调回执 BSON 字段数 = %d，期望 %d: %#v", len(decoded), len(wantFields), decoded)
	}
	for field := range wantFields {
		if _, ok := decoded[field]; !ok {
			t.Errorf("回调回执 BSON 缺少字段 %q: %#v", field, decoded)
		}
	}
	for field := range decoded {
		if _, ok := wantFields[field]; !ok {
			t.Errorf("回调回执 BSON 包含越界字段 %q: %#v", field, decoded)
		}
	}
}

func TestPayCores回调NonceBSON字段精确等于防重放契约(t *testing.T) {
	now := time.Date(2026, time.September, 9, 12, 2, 0, 0, time.UTC)
	document := PayCoresCallbackNonceDocument{
		NonceHash: "7ee9415c3b2698efe1e604495353d12df606464911331580b224e1cd5b252c8a",
		Method:    "POST",
		Path:      "/api/v1/internal/payment-confirmed",
		ExpiresAt: now,
	}
	encoded, err := bson.Marshal(document)
	if err != nil {
		t.Fatalf("序列化 PayCores nonce BSON: %v", err)
	}
	var decoded bson.M
	if err := bson.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化 PayCores nonce BSON: %v", err)
	}
	wantFields := map[string]struct{}{
		"nonce_hash": {},
		"method":     {},
		"path":       {},
		"expires_at": {},
	}
	if len(decoded) != len(wantFields) {
		t.Fatalf("PayCores nonce BSON 字段数 = %d，期望 %d: %#v", len(decoded), len(wantFields), decoded)
	}
	for field := range wantFields {
		if _, ok := decoded[field]; !ok {
			t.Errorf("PayCores nonce BSON 缺少字段 %q: %#v", field, decoded)
		}
	}
	for field := range decoded {
		if _, ok := wantFields[field]; !ok {
			t.Errorf("PayCores nonce BSON 包含越界字段 %q: %#v", field, decoded)
		}
	}
}

func Test发件箱事件将JSON载荷编码为BSONBinary(t *testing.T) {
	payload := []byte(`{"prompt":"x","assets":[]}`)
	document := OutboxEventDocument{Payload: payload}
	encoded, err := bson.Marshal(document)
	if err != nil {
		t.Fatalf("序列化 BSON: %v", err)
	}
	var decoded bson.M
	if err := bson.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化 BSON: %v", err)
	}
	binary, ok := decoded["payload"].(bson.Binary)
	if !ok {
		t.Fatalf("payload BSON 类型 = %T, want bson.Binary", decoded["payload"])
	}
	if !bytes.Equal(binary.Data, payload) {
		t.Fatal("payload BSON Binary 未保留原始 JSON 载荷字节")
	}
}

func Test订阅快照BSON字段仅包含权益读取所需字段(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	document := SubscriptionDocument{
		UserID:        "user-1",
		Status:        "active",
		BillingPeriod: "monthly",
		StartsAt:      now,
		ExpiresAt:     now.Add(30 * 24 * time.Hour),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	encoded, err := bson.Marshal(document)
	if err != nil {
		t.Fatalf("序列化订阅快照 BSON: %v", err)
	}
	var decoded bson.M
	if err := bson.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化订阅快照 BSON: %v", err)
	}
	wantFields := map[string]struct{}{
		"_id":            {},
		"status":         {},
		"billing_period": {},
		"starts_at":      {},
		"expires_at":     {},
		"created_at":     {},
		"updated_at":     {},
	}
	if len(decoded) != len(wantFields) {
		t.Fatalf("订阅快照 BSON 字段数 = %d, want %d: %#v", len(decoded), len(wantFields), decoded)
	}
	for field := range wantFields {
		if _, ok := decoded[field]; !ok {
			t.Errorf("订阅快照 BSON 缺少字段 %q: %#v", field, decoded)
		}
	}
	for field := range decoded {
		if _, ok := wantFields[field]; !ok {
			t.Errorf("订阅快照 BSON 包含越界字段 %q: %#v", field, decoded)
		}
	}
}

func Test可选时间字段为空时省略BSON字段(t *testing.T) {
	document := SessionDocument{
		ID:             "session-1",
		UserID:         "user-1",
		SessionVersion: 1,
		ExpiresAt:      time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC),
	}

	encoded, err := bson.Marshal(document)
	if err != nil {
		t.Fatalf("序列化 BSON: %v", err)
	}

	var decoded bson.M
	if err := bson.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化 BSON: %v", err)
	}

	if _, ok := decoded["revoked_at"]; ok {
		t.Fatalf("nil 的 revoked_at 不应被写入 BSON 文档: %#v", decoded)
	}

}

func TestCreationStepProviderRejectionCause的BSON往返与省略语义(t *testing.T) {
	withCause := CreationStepDocument{
		ID: "step-1", CreationID: "creation-1", Sequence: 1, Atom: "text_to_image",
		SubmitStatus: "submission_failed", ProviderRejectionCause: "provider_payment_required",
		CreatedAt: time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC),
	}
	encoded, err := bson.Marshal(withCause)
	if err != nil {
		t.Fatalf("序列化带 provider rejection cause 的步骤: %v", err)
	}
	var decoded CreationStepDocument
	if err := bson.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化带 provider rejection cause 的步骤: %v", err)
	}
	if decoded.ProviderRejectionCause != withCause.ProviderRejectionCause {
		t.Fatalf("provider rejection cause = %q, want %q", decoded.ProviderRejectionCause, withCause.ProviderRejectionCause)
	}

	withoutCause := withCause
	withoutCause.ProviderRejectionCause = ""
	encoded, err = bson.Marshal(withoutCause)
	if err != nil {
		t.Fatalf("序列化无 provider rejection cause 的步骤: %v", err)
	}
	var raw bson.M
	if err := bson.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("反序列化无 provider rejection cause 的步骤: %v", err)
	}
	if _, exists := raw["provider_rejection_cause"]; exists {
		t.Fatalf("空 provider rejection cause 不应写入 BSON: %#v", raw)
	}
}
