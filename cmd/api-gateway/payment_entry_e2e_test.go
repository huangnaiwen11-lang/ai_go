package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"ai-business-service/internal/gateway"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestGateway本地支付与IAP闭环 验证真实本机 rs0 中的账号、订单、回执、余额和账本闭环。
// 回执使用唯一的 local:v1 测试合同，不会对 PayCores、Apple 或 Google 发出网络请求。
func TestGateway本地支付与IAP闭环(t *testing.T) {
	uri := os.Getenv("CLING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("未配置本机 rs0 测试连接")
	}
	bootstrap, err := loadGatewayBootstrap("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.Data.Mongo.Uri = uri

	authenticator, cleanupAuth, err := newConfiguredSessionAuthenticator(bootstrap.GetData())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupAuth)
	authEntry, cleanupEntry, err := newConfiguredAuthEntryHandler(bootstrap.GetData(), bootstrap.GetSecurity(), authenticator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupEntry)
	paymentEntry, cleanupPayment, err := newConfiguredPaymentEntryHandler(bootstrap.GetData(), authenticator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupPayment)

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	database := client.Database("cling_main")
	trackedAccount := newAuthE2ECleanup(t, database)

	productID := "local-iap-product-" + uuid.NewString()
	transactionID := "local-iap-transaction-" + uuid.NewString()
	storeProductID := "coins100"
	checkoutOrderID := ""
	storage, cleanupStorage, err := data.NewData(bootstrap.GetData())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupStorage)
	repository, ok := data.NewPaymentRepository(storage).(payments.CheckoutRepository)
	if !ok || repository == nil {
		t.Fatal("本地支付仓储未实现 CheckoutRepository")
	}
	if err := repository.CreateProduct(context.Background(), payments.PaymentProduct{ID: productID, Version: 1, DiamondAmount: 120, PublishStatus: payments.ProductPublishStatusPublished}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupPaymentE2ERecords(database, productID, transactionID, checkoutOrderID) })

	routes := t.TempDir() + "/routes.json"
	if err := os.WriteFile(routes, []byte(`{"routes":{"POST /api/auth/register":true,"POST /api/wallet/create-external-checkout":true,"POST /api/wallet/verify-purchase":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 探针不模拟 Node 业务，防止测试在误回退后仍然被判为 Go 支付闭环通过。
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("Go 支付请求不应回退 Node：%s %s", request.Method, request.URL.Path)
		writer.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	entry := newGatewayWithPaymentEntry(upstreamURL, gateway.NewFileRouteSwitch(routes), 0, nil, nil, nil, nil, nil, paymentEntry, authEntry, nil).Handler()

	email := "payment-e2e-" + uuid.NewString() + "@example.test"
	registered := authE2ECall(t, entry, http.MethodPost, "/api/auth/register", `{"email":"`+email+`","password":"correct-horse","timezone":"Asia/Shanghai"}`, "")
	if registered.Code != http.StatusCreated || registered.UserID == "" || registered.Token == "" {
		t.Fatalf("注册本地支付测试用户失败：%d %s", registered.Code, registered.Raw)
	}
	trackedAccount.add(registered.UserID, registered.Token)

	checkoutResponse := paymentE2ECall(t, entry, registered.Token, "/api/wallet/create-external-checkout", `{"productId":"`+productID+`"}`)
	if checkoutResponse.Code != http.StatusOK || checkoutResponse.OrderID == "" || checkoutResponse.IntegrationMode != "local_only" {
		t.Fatalf("创建本地收银台订单失败：%d %s", checkoutResponse.Code, checkoutResponse.Raw)
	}
	checkoutOrderID = checkoutResponse.OrderID

	receipt := "local:v1:google:" + transactionID + ":" + storeProductID
	first := paymentE2ECall(t, entry, registered.Token, "/api/wallet/verify-purchase", `{"provider":"google","receipt":"`+receipt+`","productId":"`+productID+`","storeProductId":"`+storeProductID+`"}`)
	if first.Code != http.StatusOK || first.Balance != 120 || first.Duplicate {
		t.Fatalf("首次 IAP 入账失败：%d %s", first.Code, first.Raw)
	}
	replay := paymentE2ECall(t, entry, registered.Token, "/api/wallet/verify-purchase", `{"provider":"google","receipt":"`+receipt+`","productId":"`+productID+`","storeProductId":"`+storeProductID+`"}`)
	if replay.Code != http.StatusOK || replay.Balance != 120 || !replay.Duplicate {
		t.Fatalf("IAP 重放未收敛：%d %s", replay.Code, replay.Raw)
	}

	ledgerCount, err := database.Collection(schema.CollectionLedgerEntries).CountDocuments(context.Background(), bson.D{{Key: "account_id", Value: registered.UserID}, {Key: "reason", Value: "payment_credit"}})
	if err != nil || ledgerCount != 1 {
		t.Fatalf("本地支付账本数 = %d，error=%v，期望恰好 1", ledgerCount, err)
	}
}

// TestGateway本地PayCoresMock建单回调闭环 覆盖 Go 自有支付链路：冻结商品、双签名建单、
// 绑定渠道订单、回调入账和 nonce 防重放。PayCores 仅为 httptest 本地 mock。
func TestGateway本地PayCoresMock建单回调闭环(t *testing.T) {
	uri := os.Getenv("CLING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("未配置本机 rs0 测试连接")
	}
	bootstrap, err := loadGatewayBootstrap("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.Data.Mongo.Uri = uri
	const providerOrderID = "pco-local-e2e"
	// 回调 nonce 必须每次测试独立生成。支付领域会在两分钟 TTL 内拒绝重复 nonce，
	// 固定值会让同一用例的连续回归把首次回调误判为重放。
	callbackNonce := uuid.NewString()
	duplicateNonce := uuid.NewString()
	productID, transactionID := "paycores-product-"+uuid.NewString(), "paycores-txn-"+uuid.NewString()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Signature-V2") == "" {
			t.Fatalf("PayCores mock 请求不符合合同")
		}
		if r.URL.Path == "/internal/order-status" {
			if r.Header.Get("X-Timestamp") == "" || r.Header.Get("X-Request-Nonce") == "" {
				t.Fatalf("PayCores 状态回查缺少 V2 防重放字段")
			}
			var query struct{ OrderID, UserID string }
			if err := json.NewDecoder(r.Body).Decode(&query); err != nil || query.OrderID != providerOrderID || query.UserID == "" {
				t.Fatalf("PayCores 状态回查参数: %#v %v", query, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "orderId": providerOrderID, "productId": productID, "amountUsd": 9.99, "status": "paid", "paymentReceived": true, "backendReady": true})
			return
		}
		if r.URL.Path != "/internal/create-order" || r.Header.Get("X-Signature") == "" {
			t.Fatalf("PayCores mock 建单请求不符合合同")
		}
		var payload struct {
			AmountCents int64  `json:"amountCents"`
			Credits     int64  `json:"credits"`
			Currency    string `json:"currency"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.AmountCents != 999 || payload.Credits != 100 {
			t.Fatalf("PayCores 收到非冻结金额: %#v", payload)
		}
		_, _ = w.Write([]byte(`{"success":true,"orderId":"` + providerOrderID + `","checkoutUrl":"http://127.0.0.1:19082/checkout/` + providerOrderID + `"}`))
	}))
	t.Cleanup(mock.Close)
	bootstrap.Integrations.Paycores.BaseUrl = mock.URL

	authenticator, cleanAuth, err := newConfiguredSessionAuthenticator(bootstrap.GetData())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanAuth)
	authEntry, cleanEntry, err := newConfiguredAuthEntryHandler(bootstrap.GetData(), bootstrap.GetSecurity(), authenticator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanEntry)
	paymentEntry, cleanPayment, err := newConfiguredPayCoresPaymentEntryHandler(bootstrap, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanPayment)
	paymentCallback, cleanCallback, err := newConfiguredPaymentCallback(bootstrap.GetData(), bootstrap.GetSecurity())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanCallback)

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	database := client.Database("cling_main")
	storage, cleanStorage, err := data.NewData(bootstrap.GetData())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanStorage)
	repository := data.NewPaymentRepository(storage)
	if err := repository.CreateProduct(context.Background(), payments.PaymentProduct{ID: productID, Version: 1, DiamondAmount: 100, AmountCents: 999, Currency: "USD", Label: "100 Diamonds", PublishStatus: payments.ProductPublishStatusPublished}); err != nil {
		t.Fatal(err)
	}
	var orderID, userID string
	t.Cleanup(func() {
		cleanupPayCoresMockE2E(database, productID, providerOrderID, transactionID, orderID, userID, callbackNonce, duplicateNonce)
	})
	routes := t.TempDir() + "/routes.json"
	if err := os.WriteFile(routes, []byte(`{"routes":{"POST /api/auth/register":true,"POST /api/wallet/create-external-checkout":true,"POST /api/v1/internal/payment-confirmed":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		t.Errorf("Go PayCores 测试请求不应回退 Node：%s %s", request.Method, request.URL.Path)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	entry := newGatewayWithPaymentEntry(upstreamURL, gateway.NewFileRouteSwitch(routes), 0, nil, nil, nil, nil, paymentCallback, paymentEntry, authEntry, nil).Handler()
	registered := authE2ECall(t, entry, http.MethodPost, "/api/auth/register", `{"email":"paycores-`+uuid.NewString()+`@example.test","password":"correct-horse","timezone":"Asia/Shanghai"}`, "")
	if registered.Code != http.StatusCreated {
		t.Fatalf("注册失败: %s", registered.Raw)
	}
	userID = registered.UserID
	checkout := paymentE2ECall(t, entry, registered.Token, "/api/wallet/create-external-checkout", `{"productId":"`+productID+`"}`)
	if checkout.Code != http.StatusOK || checkout.OrderID == "" || checkout.IntegrationMode != "paycores" {
		t.Fatalf("建单失败: %d %s", checkout.Code, checkout.Raw)
	}
	orderID = checkout.OrderID
	readStatus := func() struct {
		PaymentReceived, BackendReady bool
		Status                        string
	} {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/payments/order-status/"+orderID, nil)
		request.Header.Set("Authorization", "Bearer "+registered.Token)
		recorder := httptest.NewRecorder()
		paymentEntry.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("状态回查失败: %d %s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Data struct {
				PaymentReceived, BackendReady bool
				Status                        string
			} `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Data
	}
	beforeCallback := readStatus()
	if !beforeCallback.PaymentReceived || beforeCallback.BackendReady || beforeCallback.Status != "pending" {
		t.Fatalf("渠道已收款、本地尚未结算的状态不符: %#v", beforeCallback)
	}
	body := []byte(`{"orderId":"` + providerOrderID + `","userId":"` + userID + `","providerTxnId":"` + transactionID + `"}`)
	callback := signedGatewayPaymentCallback(t, bootstrap.Security.PaycoresCallbackHmacKey, "/api/v1/internal/payment-confirmed", body, callbackNonce)
	recorder := httptest.NewRecorder()
	entry.ServeHTTP(recorder, callback)
	if recorder.Code != http.StatusOK {
		t.Fatalf("支付回调失败: %d %s", recorder.Code, recorder.Body.String())
	}
	afterCallback := readStatus()
	if !afterCallback.PaymentReceived || !afterCallback.BackendReady || afterCallback.Status != "paid" {
		t.Fatalf("回调入账后状态不符: %#v", afterCallback)
	}
	duplicate := signedGatewayPaymentCallback(t, bootstrap.Security.PaycoresCallbackHmacKey, "/api/v1/internal/payment-confirmed", body, duplicateNonce)
	duplicateRecorder := httptest.NewRecorder()
	entry.ServeHTTP(duplicateRecorder, duplicate)
	if duplicateRecorder.Code != http.StatusOK {
		t.Fatalf("新 nonce 重复通知应幂等 ACK: %d %s", duplicateRecorder.Code, duplicateRecorder.Body.String())
	}
	ledgerCount, err := database.Collection(schema.CollectionLedgerEntries).CountDocuments(context.Background(), bson.D{{Key: "account_id", Value: userID}, {Key: "reason", Value: "payment_credit"}})
	if err != nil || ledgerCount != 1 {
		t.Fatalf("重复通知后支付账本数 = %d, error = %v，期望 1", ledgerCount, err)
	}
	replay := signedGatewayPaymentCallback(t, bootstrap.Security.PaycoresCallbackHmacKey, "/api/v1/internal/payment-confirmed", body, callbackNonce)
	replayRecorder := httptest.NewRecorder()
	entry.ServeHTTP(replayRecorder, replay)
	if replayRecorder.Code != http.StatusConflict {
		t.Fatalf("回调重放 = %d，期望 409", replayRecorder.Code)
	}
}

// TestGateway本地PayCoresMock建单失败与超时 验证明确定义的渠道拒绝和请求取消都不会
// 被误报为建单成功，也不会重试或绑定一个真实渠道订单。PayCores 仅为 httptest 本地 mock。
func TestGateway本地PayCoresMock建单失败与超时(t *testing.T) {
	uri := os.Getenv("CLING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("未配置本机 rs0 测试连接")
	}
	testCases := []struct {
		name           string
		requestTimeout time.Duration
	}{
		{name: "明确失败"},
		{name: "请求超时", requestTimeout: 250 * time.Millisecond},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int32
			var invalidRequest atomic.Bool
			releaseMock := make(chan struct{})
			mock := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if request.Method != http.MethodPost || request.URL.Path != "/internal/create-order" || request.Header.Get("X-Signature") == "" || request.Header.Get("X-Signature-V2") == "" {
					invalidRequest.Store(true)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if testCase.requestTimeout == 0 {
					writer.WriteHeader(http.StatusBadGateway)
					_, _ = writer.Write([]byte(`{"success":false}`))
					return
				}
				select {
				case <-request.Context().Done():
				case <-releaseMock:
					writer.WriteHeader(http.StatusGatewayTimeout)
				}
			}))
			t.Cleanup(mock.Close)
			t.Cleanup(func() { close(releaseMock) })

			bootstrap, err := loadGatewayBootstrap("../../configs/config.yaml")
			if err != nil {
				t.Fatal(err)
			}
			bootstrap.Data.Mongo.Uri = uri
			bootstrap.Integrations.Paycores.BaseUrl = mock.URL

			authenticator, cleanAuth, err := newConfiguredSessionAuthenticator(bootstrap.GetData())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanAuth)
			authEntry, cleanEntry, err := newConfiguredAuthEntryHandler(bootstrap.GetData(), bootstrap.GetSecurity(), authenticator)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanEntry)
			paymentEntry, cleanPayment, err := newConfiguredPayCoresPaymentEntryHandler(bootstrap, authenticator)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanPayment)

			client, err := mongo.Connect(options.Client().ApplyURI(uri))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
			database := client.Database("cling_main")
			trackedAccount := newAuthE2ECleanup(t, database)
			storage, cleanStorage, err := data.NewData(bootstrap.GetData())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanStorage)
			repository := data.NewPaymentRepository(storage)
			productID := "paycores-failure-product-" + uuid.NewString()
			if err := repository.CreateProduct(context.Background(), payments.PaymentProduct{ID: productID, Version: 1, DiamondAmount: 5, AmountCents: 101, Currency: "USD", Label: "5 Diamonds", PublishStatus: payments.ProductPublishStatusPublished}); err != nil {
				t.Fatal(err)
			}
			userID := ""
			t.Cleanup(func() { cleanupFailedPayCoresMockE2E(database, productID, userID) })

			routes := t.TempDir() + "/routes.json"
			if err := os.WriteFile(routes, []byte(`{"routes":{"POST /api/auth/register":true,"POST /api/wallet/create-external-checkout":true}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				t.Errorf("Go PayCores 失败测试请求不应回退 Node：%s %s", request.Method, request.URL.Path)
				writer.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(upstream.Close)
			upstreamURL, _ := url.Parse(upstream.URL)
			entry := newGatewayWithPaymentEntry(upstreamURL, gateway.NewFileRouteSwitch(routes), 0, nil, nil, nil, nil, nil, paymentEntry, authEntry, nil).Handler()
			registered := authE2ECall(t, entry, http.MethodPost, "/api/auth/register", `{"email":"paycores-failure-`+uuid.NewString()+`@example.test","password":"correct-horse","timezone":"Asia/Shanghai"}`, "")
			if registered.Code != http.StatusCreated || registered.UserID == "" || registered.Token == "" {
				t.Fatalf("注册 PayCores 失败测试用户失败：%d %s", registered.Code, registered.Raw)
			}
			userID = registered.UserID
			trackedAccount.add(registered.UserID, registered.Token)

			started := time.Now()
			var response paymentE2EResponse
			if testCase.requestTimeout == 0 {
				response = paymentE2ECall(t, entry, registered.Token, "/api/wallet/create-external-checkout", `{"productId":"`+productID+`"}`)
			} else {
				request := httptest.NewRequest(http.MethodPost, "/api/wallet/create-external-checkout", bytes.NewBufferString(`{"productId":"`+productID+`"}`))
				request.Header.Set("Authorization", "Bearer "+registered.Token)
				ctx, cancel := context.WithTimeout(request.Context(), testCase.requestTimeout)
				defer cancel()
				recorder := httptest.NewRecorder()
				entry.ServeHTTP(recorder, request.WithContext(ctx))
				response = decodePaymentE2EResponse(recorder)
			}
			elapsed := time.Since(started)
			if response.Code != http.StatusServiceUnavailable || response.ErrorCode != "SERVICE_UNAVAILABLE" || response.OrderID != "" {
				t.Fatalf("PayCores %s返回 = %d %s，期望 503 SERVICE_UNAVAILABLE 且无 orderId", testCase.name, response.Code, response.Raw)
			}
			if calls.Load() != 1 {
				t.Fatalf("PayCores %s调用次数 = %d，期望 1", testCase.name, calls.Load())
			}
			if invalidRequest.Load() {
				t.Fatalf("PayCores %s请求缺少建单路径、方法或签名", testCase.name)
			}
			if testCase.requestTimeout > 0 {
				if elapsed >= 2*time.Second {
					t.Fatalf("PayCores 超时耗时 %s，期望不等待服务端兜底", elapsed)
				}
			}

			filter := bson.D{{Key: "user_id", Value: userID}, {Key: "product_id", Value: productID}, {Key: "provider", Value: string(payments.ProviderPayCores)}}
			var order model.PaymentOrderDocument
			if err := database.Collection(schema.CollectionPaymentOrders).FindOne(context.Background(), filter).Decode(&order); err != nil {
				t.Fatalf("读取 PayCores %s后的冻结订单失败：%v", testCase.name, err)
			}
			if order.ID == "" || order.ProviderOrderID != "local-paycores-"+order.ID || order.Status != string(payments.PaymentOrderStatusPending) {
				t.Fatalf("PayCores %s后不应绑定真实渠道订单：%#v", testCase.name, order)
			}
			count, err := database.Collection(schema.CollectionPaymentOrders).CountDocuments(context.Background(), filter)
			if err != nil || count != 1 {
				t.Fatalf("PayCores %s后的冻结订单数 = %d，error=%v，期望 1", testCase.name, count, err)
			}
		})
	}
}

func signedGatewayPaymentCallback(t *testing.T, key, path string, body []byte, nonce string) *http.Request {
	t.Helper()
	timestamp := time.Now().UTC().UnixMilli()
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{http.MethodPost, path, strconv.FormatInt(timestamp, 10), nonce, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(canonical))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("X-Timestamp", strconv.FormatInt(timestamp, 10))
	req.Header.Set("X-Request-Nonce", nonce)
	req.Header.Set("X-Signature-V2", hex.EncodeToString(mac.Sum(nil)))
	return req
}

// cleanupPayCoresMockE2E 仅按本测试生成的精确标识删除数据，避免影响任何其他本地记录。
func cleanupPayCoresMockE2E(database *mongo.Database, productID, providerOrderID, transactionID, orderID, userID string, callbackNonces ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var receipt model.PaymentReceiptDocument
	_ = database.Collection(schema.CollectionPaymentReceipts).FindOne(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}}).Decode(&receipt)
	if receipt.LedgerEntryID != "" {
		_, _ = database.Collection(schema.CollectionLedgerEntries).DeleteOne(ctx, bson.D{{Key: "_id", Value: receipt.LedgerEntryID}})
	}
	_, _ = database.Collection(schema.CollectionPaymentReceipts).DeleteOne(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}})
	if orderID != "" {
		_, _ = database.Collection(schema.CollectionPaymentOrders).DeleteOne(ctx, bson.D{{Key: "_id", Value: orderID}})
	} else {
		_, _ = database.Collection(schema.CollectionPaymentOrders).DeleteOne(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "provider_order_id", Value: providerOrderID}})
	}
	_, _ = database.Collection(schema.CollectionPaymentProducts).DeleteOne(ctx, bson.D{{Key: "product_id", Value: productID}, {Key: "version", Value: int64(1)}})
	if userID != "" {
		_, _ = database.Collection(schema.CollectionAccounts).DeleteOne(ctx, bson.D{{Key: "_id", Value: userID}})
	}
	// 防重放记录没有业务外键，按本次 nonce 的摘要精确删除；禁止用 TTL 或集合级清理
	// 掩盖测试间的状态泄漏。
	for _, nonce := range callbackNonces {
		if nonceHash, err := payments.NewPaymentCallbackNonceHash(nonce); err == nil {
			_, _ = database.Collection(schema.CollectionPaymentCallbackNonces).DeleteOne(ctx, bson.D{{Key: "nonce_hash", Value: nonceHash.Hex()}})
		}
	}
}

// cleanupFailedPayCoresMockE2E 只删除失败场景唯一用户与唯一商品对应的冻结订单和商品。
func cleanupFailedPayCoresMockE2E(database *mongo.Database, productID, userID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if productID != "" && userID != "" {
		_, _ = database.Collection(schema.CollectionPaymentOrders).DeleteMany(ctx, bson.D{{Key: "user_id", Value: userID}, {Key: "product_id", Value: productID}, {Key: "provider", Value: string(payments.ProviderPayCores)}})
	}
	if productID != "" {
		_, _ = database.Collection(schema.CollectionPaymentProducts).DeleteOne(ctx, bson.D{{Key: "product_id", Value: productID}, {Key: "version", Value: int64(1)}})
	}
}

type paymentE2EResponse struct {
	Code            int
	ErrorCode       string
	OrderID         string
	IntegrationMode string
	Balance         int64
	Duplicate       bool
	Raw             string
}

func paymentE2ECall(t *testing.T, handler http.Handler, token, path, body string) paymentE2EResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return decodePaymentE2EResponse(recorder)
}

func decodePaymentE2EResponse(recorder *httptest.ResponseRecorder) paymentE2EResponse {
	response := paymentE2EResponse{Code: recorder.Code, Raw: recorder.Body.String()}
	var envelope struct {
		Code string `json:"code"`
		Data struct {
			OrderID         string `json:"orderId"`
			IntegrationMode string `json:"integrationMode"`
			Balance         int64  `json:"balance"`
			Duplicate       bool   `json:"duplicate"`
		} `json:"data"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &envelope)
	response.ErrorCode = envelope.Code
	response.OrderID, response.IntegrationMode, response.Balance, response.Duplicate = envelope.Data.OrderID, envelope.Data.IntegrationMode, envelope.Data.Balance, envelope.Data.Duplicate
	return response
}

// cleanupPaymentE2ERecords 只按此测试生成的精确业务标识删除文档，禁止使用空条件或集合级删除。
func cleanupPaymentE2ERecords(database *mongo.Database, productID, transactionID, checkoutOrderID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var receipt model.PaymentReceiptDocument
	_ = database.Collection(schema.CollectionPaymentReceipts).FindOne(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderGoogle)}, {Key: "external_transaction_id", Value: transactionID}}).Decode(&receipt)
	if receipt.LedgerEntryID != "" {
		_, _ = database.Collection(schema.CollectionLedgerEntries).DeleteOne(ctx, bson.D{{Key: "_id", Value: receipt.LedgerEntryID}})
	}
	_, _ = database.Collection(schema.CollectionPaymentReceipts).DeleteOne(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderGoogle)}, {Key: "external_transaction_id", Value: transactionID}})
	_, _ = database.Collection(schema.CollectionPaymentOrders).DeleteOne(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderGoogle)}, {Key: "provider_order_id", Value: transactionID}})
	if checkoutOrderID != "" {
		_, _ = database.Collection(schema.CollectionPaymentOrders).DeleteOne(ctx, bson.D{{Key: "_id", Value: checkoutOrderID}})
	}
	_, _ = database.Collection(schema.CollectionPaymentProducts).DeleteOne(ctx, bson.D{{Key: "product_id", Value: productID}, {Key: "version", Value: int64(1)}})
}
