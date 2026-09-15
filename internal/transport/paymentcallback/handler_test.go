package paymentcallback

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	paycores "ai-business-service/internal/integrations/paycores"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	paymentHandlerSigningKey = "payment-handler-signing-key-at-least-thirty-two-characters"
	paymentHandlerBody       = `{"orderId":"provider-order-1","userId":"user-1","providerTxnId":"txn-1"}`
	paymentHandlerNonce      = "12345678-1234-4123-8123-123456789abc"
)

var paymentHandlerNow = func() time.Time { return time.UnixMilli(1_788_768_550_000).UTC() }

func TestHandler验签后映射受控确认并保持Node成功响应(t *testing.T) {
	for _, path := range []string{"/api/internal/payment-confirmed", "/api/v1/internal/payment-confirmed"} {
		t.Run(path, func(t *testing.T) {
			usecase := &recordingConfirmedCallbackUsecase{result: payments.ApplyResult{Applied: true, DiamondBalance: 42}}
			handler := NewHandler(newPaymentHandlerVerifier(t), usecase)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, signedPaymentHandlerRequest(t, http.MethodPost, path, []byte(paymentHandlerBody), paymentHandlerNonce))

			if recorder.Code != http.StatusOK || recorder.Body.String() != `{"success":true,"data":{"newBalance":42,"duplicate":false}}` {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", recorder.Header().Get("Content-Type"))
			}
			if usecase.handleCalls != 1 || usecase.successCalls != 1 {
				t.Fatalf("Handle()/结算调用 = %d/%d, want 1/1", usecase.handleCalls, usecase.successCalls)
			}
			assertControlledConfirmation(t, usecase.confirmation)
		})
	}
}

func TestHandler拒绝不可信HTTP输入(t *testing.T) {
	validBody := []byte(paymentHandlerBody)
	cases := []struct {
		name      string
		request   *http.Request
		wantCode  int
		wantAllow string
	}{
		{name: "错误方法", request: signedPaymentHandlerRequest(t, http.MethodGet, "/api/v1/internal/payment-confirmed", validBody, paymentHandlerNonce), wantCode: http.StatusMethodNotAllowed, wantAllow: http.MethodPost},
		{name: "未知路径", request: signedPaymentHandlerRequest(t, http.MethodPost, "/api/v1/internal/other", validBody, paymentHandlerNonce), wantCode: http.StatusNotFound},
		{name: "编码路径", request: signedPaymentHandlerRequest(t, http.MethodPost, "/api%2Fv1/internal/payment-confirmed", validBody, paymentHandlerNonce), wantCode: http.StatusNotFound},
		{name: "重复斜杠", request: signedPaymentHandlerRequest(t, http.MethodPost, "/api//v1/internal/payment-confirmed", validBody, paymentHandlerNonce), wantCode: http.StatusNotFound},
		{name: "查询参数", request: signedPaymentHandlerRequest(t, http.MethodPost, "/api/v1/internal/payment-confirmed?x=1", validBody, paymentHandlerNonce), wantCode: http.StatusBadRequest},
		{name: "空查询", request: signedPaymentHandlerRequest(t, http.MethodPost, "/api/v1/internal/payment-confirmed?", validBody, paymentHandlerNonce), wantCode: http.StatusBadRequest},
		{name: "正文超过上限", request: signedPaymentHandlerRequest(t, http.MethodPost, "/api/v1/internal/payment-confirmed", []byte(strings.Repeat("a", 1024*1024+1)), paymentHandlerNonce), wantCode: http.StatusRequestEntityTooLarge},
		{name: "签名错误", request: invalidPaymentHandlerSignatureRequest(t), wantCode: http.StatusUnauthorized},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			usecase := &recordingConfirmedCallbackUsecase{}
			handler := NewHandler(newPaymentHandlerVerifier(t), usecase)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, testCase.request)

			if recorder.Code != testCase.wantCode || recorder.Body.String() != `{"success":false,"error":"Invalid signature"}` {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("Allow") != testCase.wantAllow {
				t.Fatalf("Allow = %q, want %q", recorder.Header().Get("Allow"), testCase.wantAllow)
			}
			if usecase.handleCalls != 0 || usecase.successCalls != 0 {
				t.Fatalf("Handle()/结算调用 = %d/%d, want 0/0", usecase.handleCalls, usecase.successCalls)
			}
		})
	}
}

func TestHandler映射领域错误并报告重复结果(t *testing.T) {
	cases := []struct {
		name     string
		result   payments.ApplyResult
		err      error
		wantCode int
		wantBody string
	}{
		{name: "已入账", result: payments.ApplyResult{Applied: false, DiamondBalance: 42}, wantCode: http.StatusOK, wantBody: `{"success":true,"data":{"newBalance":42,"duplicate":true}}`},
		{name: "回放", err: payments.ErrPaymentCallbackReplayed, wantCode: http.StatusConflict, wantBody: `{"success":false,"error":"Request replayed"}`},
		{name: "依赖不可用", err: payments.ErrConfirmedCallbackDependenciesUnavailable, wantCode: http.StatusServiceUnavailable, wantBody: `{"success":false,"error":"Authentication unavailable"}`},
		{name: "未知错误", err: errors.New("settlement contains secret details"), wantCode: http.StatusInternalServerError, wantBody: `{"success":false,"error":"Failed to process payment"}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			usecase := &recordingConfirmedCallbackUsecase{result: testCase.result, err: testCase.err}
			handler := NewHandler(newPaymentHandlerVerifier(t), usecase)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, signedPaymentHandlerRequest(t, http.MethodPost, "/api/v1/internal/payment-confirmed", []byte(paymentHandlerBody), paymentHandlerNonce))

			if recorder.Code != testCase.wantCode || recorder.Body.String() != testCase.wantBody {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHandler回放由用例拒绝且不再次结算(t *testing.T) {
	usecase := &recordingConfirmedCallbackUsecase{result: payments.ApplyResult{Applied: true, DiamondBalance: 42}, replayAfterFirst: true}
	handler := NewHandler(newPaymentHandlerVerifier(t), usecase)

	for index, wantCode := range []int{http.StatusOK, http.StatusConflict} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, signedPaymentHandlerRequest(t, http.MethodPost, "/api/v1/internal/payment-confirmed", []byte(paymentHandlerBody), paymentHandlerNonce))
		if recorder.Code != wantCode {
			t.Fatalf("request %d status = %d, want %d", index+1, recorder.Code, wantCode)
		}
	}
	if usecase.handleCalls != 2 || usecase.successCalls != 1 {
		t.Fatalf("Handle()/结算调用 = %d/%d, want 2/1", usecase.handleCalls, usecase.successCalls)
	}
}

func TestMongoHandler验签结算后拒绝同Nonce重放(t *testing.T) {
	uri := os.Getenv("CLING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set CLING_TEST_MONGO_URI to run MongoDB transaction integration tests")
	}

	testData, closeData := newPaymentCallbackMongoData(t, uri)
	defer closeData()
	database := newPaymentCallbackMongoDatabase(t, uri)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	businessAt := paymentHandlerNow()
	userID := "payment-callback-user-" + uuid.NewString()
	productID := "payment-callback-product-" + uuid.NewString()
	localOrderID := "payment-callback-order-" + uuid.NewString()
	providerOrderID := "paycores-order-" + uuid.NewString()
	providerTransactionID := "paycores-transaction-" + uuid.NewString()
	nonce := "12345678-1234-4123-8123-" + uuid.NewString()[:12]
	nonceHash, err := payments.NewPaymentCallbackNonceHash(nonce)
	if err != nil {
		t.Fatalf("构造 nonce 摘要: %v", err)
	}
	const frozenDiamonds int64 = 137
	const initialBalance int64 = 29

	cleanup := newPaymentCallbackMongoCleanup(t, database, userID, localOrderID)
	if _, err := database.Collection(schema.CollectionAccounts).InsertOne(ctx, model.AccountDocument{
		ID: userID, DiamondBalance: initialBalance, CreatedAt: businessAt, UpdatedAt: businessAt,
	}); err != nil {
		t.Fatalf("创建本地用户账户: %v", err)
	}

	repository := data.NewPaymentRepository(testData)
	if repository == nil {
		t.Fatal("NewPaymentRepository() = nil")
	}
	product := payments.PaymentProduct{
		ID: productID, Version: 1, DiamondAmount: frozenDiamonds, PublishStatus: payments.ProductPublishStatusPublished,
	}
	if err := repository.CreateProduct(ctx, product); err != nil {
		t.Fatalf("创建已发布商品: %v", err)
	}
	cleanup.productID = paymentCallbackMongoDocumentID(t, ctx, database.Collection(schema.CollectionPaymentProducts), bson.D{
		{Key: "product_id", Value: productID}, {Key: "version", Value: product.Version},
	})
	order, err := payments.FreezeOrder(payments.CreateOrderInput{
		OrderID: localOrderID, UserID: userID, Provider: payments.ProviderPayCores, ProviderOrderID: providerOrderID,
	}, product, businessAt)
	if err != nil {
		t.Fatalf("冻结本地支付订单: %v", err)
	}
	if order.ID == order.ProviderOrderID {
		t.Fatal("本地 PaymentOrderID 与 PayCores ProviderOrderID 必须不同")
	}
	if err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		return repository.CreateOrder(txCtx, *order)
	}); err != nil {
		t.Fatalf("创建 pending 本地 PayCores 订单: %v", err)
	}

	service := payments.NewService(repository, func() time.Time { return businessAt })
	usecase := payments.NewConfirmedCallbackUsecase(
		data.NewPayCoresCallbackNonceRepository(testData), repository, service, callbackPathV1, func() time.Time { return businessAt },
	)
	handler := NewHandler(newPaymentHandlerVerifier(t), usecase)
	body := []byte(`{"orderId":"` + providerOrderID + `","userId":"` + userID + `","providerTxnId":"` + providerTransactionID + `"}`)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, signedPaymentHandlerRequest(t, http.MethodPost, callbackPathV1, body, nonce))
	if first.Code != http.StatusOK {
		t.Fatalf("首次回调 status = %d, body = %s，期望 200", first.Code, first.Body.String())
	}
	cleanup.nonceID = paymentCallbackMongoDocumentID(t, ctx, database.Collection(schema.CollectionPaymentCallbackNonces), bson.D{{Key: "nonce_hash", Value: nonceHash.Hex()}})
	receipt := assertPaymentCallbackMongoFacts(t, ctx, database, localOrderID, userID, providerTransactionID, frozenDiamonds, initialBalance+frozenDiamonds, 1)
	cleanup.receiptID = receipt.ID
	cleanup.ledgerEntryID = receipt.LedgerEntryID

	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, signedPaymentHandlerRequest(t, http.MethodPost, callbackPathV1, body, nonce))
	if replay.Code != http.StatusConflict || replay.Body.String() != `{"success":false,"error":"Request replayed"}` {
		t.Fatalf("同 nonce 重放 = %d %s，期望 409 Request replayed", replay.Code, replay.Body.String())
	}
	assertPaymentCallbackMongoFacts(t, ctx, database, localOrderID, userID, providerTransactionID, frozenDiamonds, initialBalance+frozenDiamonds, 1)
}

func newPaymentCallbackMongoData(t *testing.T, uri string) (*data.Data, func()) {
	t.Helper()
	testData, closeData, err := data.NewData(&conf.Data{Mongo: &conf.Data_Mongo{
		Uri: uri, Database: "cling_main", ReplicaSet: "rs0", TransactionsRequired: true,
	}})
	if err != nil {
		t.Fatalf("连接本地 MongoDB: %v", err)
	}
	return testData, closeData
}

func newPaymentCallbackMongoDatabase(t *testing.T, uri string) *mongo.Database {
	t.Helper()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("连接本地 MongoDB 断言客户端: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := client.Disconnect(cleanupContext); err != nil {
			t.Errorf("断开本地 MongoDB 断言客户端: %v", err)
		}
	})
	return client.Database("cling_main")
}

type paymentCallbackMongoCleanup struct {
	database      *mongo.Database
	accountID     string
	orderID       string
	productID     any
	nonceID       any
	receiptID     string
	ledgerEntryID string
}

func newPaymentCallbackMongoCleanup(t *testing.T, database *mongo.Database, accountID, orderID string) *paymentCallbackMongoCleanup {
	t.Helper()
	cleanup := &paymentCallbackMongoCleanup{database: database, accountID: accountID, orderID: orderID}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		cleanup.deleteOne(t, cleanupContext, schema.CollectionLedgerEntries, cleanup.ledgerEntryID)
		cleanup.deleteOne(t, cleanupContext, schema.CollectionPaymentReceipts, cleanup.receiptID)
		cleanup.deleteOne(t, cleanupContext, schema.CollectionPaymentCallbackNonces, cleanup.nonceID)
		cleanup.deleteOne(t, cleanupContext, schema.CollectionPaymentOrders, cleanup.orderID)
		cleanup.deleteOne(t, cleanupContext, schema.CollectionPaymentProducts, cleanup.productID)
		cleanup.deleteOne(t, cleanupContext, schema.CollectionAccounts, cleanup.accountID)
	})
	return cleanup
}

func (cleanup *paymentCallbackMongoCleanup) deleteOne(t *testing.T, ctx context.Context, collection string, id any) {
	t.Helper()
	if id == nil || id == "" {
		return
	}
	if _, err := cleanup.database.Collection(collection).DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
		t.Errorf("清理 %s _id=%v: %v", collection, id, err)
	}
}

func paymentCallbackMongoDocumentID(t *testing.T, ctx context.Context, collection *mongo.Collection, filter bson.D) any {
	t.Helper()
	var document bson.M
	if err := collection.FindOne(ctx, filter).Decode(&document); err != nil {
		t.Fatalf("读取 %s 的 _id: %v", collection.Name(), err)
	}
	id, ok := document["_id"]
	if !ok || id == nil {
		t.Fatalf("%s 缺少 _id: %#v", collection.Name(), document)
	}
	return id
}

func assertPaymentCallbackMongoFacts(t *testing.T, ctx context.Context, database *mongo.Database, orderID, userID, transactionID string, diamonds, balance, wantCount int64) model.PaymentReceiptDocument {
	t.Helper()
	var order model.PaymentOrderDocument
	if err := database.Collection(schema.CollectionPaymentOrders).FindOne(ctx, bson.D{{Key: "_id", Value: orderID}}).Decode(&order); err != nil {
		t.Fatalf("读取支付订单: %v", err)
	}
	if order.Status != string(payments.PaymentOrderStatusPaid) {
		t.Fatalf("订单状态 = %q，期望 paid", order.Status)
	}
	var account model.AccountDocument
	if err := database.Collection(schema.CollectionAccounts).FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account); err != nil {
		t.Fatalf("读取用户账户: %v", err)
	}
	if account.DiamondBalance != balance {
		t.Fatalf("账户钻石余额 = %d，期望 %d", account.DiamondBalance, balance)
	}
	receipts := database.Collection(schema.CollectionPaymentReceipts)
	receiptFilter := bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}}
	if count, err := receipts.CountDocuments(ctx, receiptFilter); err != nil || count != wantCount {
		t.Fatalf("payment_receipts 数 = %d, error = %v，期望 %d", count, err, wantCount)
	}
	var receipt model.PaymentReceiptDocument
	if err := receipts.FindOne(ctx, receiptFilter).Decode(&receipt); err != nil {
		t.Fatalf("读取 payment_receipt: %v", err)
	}
	if receipt.PaymentOrderID != orderID || receipt.UserID != userID || receipt.DiamondAmount != diamonds {
		t.Fatalf("payment_receipt = %#v，期望本地订单 %q、用户 %q、冻结钻石 %d", receipt, orderID, userID, diamonds)
	}
	if receipt.ID == "" || receipt.LedgerEntryID == "" {
		t.Fatalf("payment_receipt 缺少精确清理所需的 _id 或 payment_credit 关联: %#v", receipt)
	}
	entries := database.Collection(schema.CollectionLedgerEntries)
	if count, err := entries.CountDocuments(ctx, bson.D{{Key: "account_id", Value: userID}, {Key: "reason", Value: "payment_credit"}, {Key: "delta_diamonds", Value: diamonds}}); err != nil || count != wantCount {
		t.Fatalf("payment_credit 数 = %d, error = %v，期望 %d", count, err, wantCount)
	}
	return receipt
}

type recordingConfirmedCallbackUsecase struct {
	result           payments.ApplyResult
	err              error
	replayAfterFirst bool
	handleCalls      int
	successCalls     int
	confirmation     payments.VerifiedPaymentConfirmation
}

func (usecase *recordingConfirmedCallbackUsecase) Handle(_ context.Context, confirmation payments.VerifiedPaymentConfirmation) (payments.ApplyResult, error) {
	usecase.handleCalls++
	if usecase.replayAfterFirst && usecase.successCalls > 0 {
		return payments.ApplyResult{}, payments.ErrPaymentCallbackReplayed
	}
	usecase.successCalls++
	usecase.confirmation = confirmation
	return usecase.result, usecase.err
}

func assertControlledConfirmation(t *testing.T, confirmation payments.VerifiedPaymentConfirmation) {
	t.Helper()
	if confirmation.UserID != "user-1" || confirmation.OrderID != "provider-order-1" || confirmation.ProviderTxnID != "txn-1" || !confirmation.NonceHash.Valid() {
		t.Fatalf("confirmation = %#v, want controlled verified fields", confirmation)
	}
	if confirmation.NonceHash.Hex() == paymentHandlerNonce || strings.Contains(confirmation.NonceHash.Hex(), paymentHandlerNonce) {
		t.Fatalf("nonce hash = %q, leaked raw nonce", confirmation.NonceHash.Hex())
	}
}

func newPaymentHandlerVerifier(t *testing.T) *paycores.CallbackVerifier {
	t.Helper()
	verifier, err := paycores.NewCallbackVerifier(paymentHandlerSigningKey, paymentHandlerNow)
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func signedPaymentHandlerRequest(t *testing.T, method, requestPath string, body []byte, nonce string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, requestPath, bytes.NewReader(body))
	timestamp := strconv.FormatInt(paymentHandlerNow().UnixMilli(), 10)
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{http.MethodPost, request.URL.Path, timestamp, nonce, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(paymentHandlerSigningKey))
	_, _ = mac.Write([]byte(canonical))
	request.Header.Set("X-Timestamp", timestamp)
	request.Header.Set("X-Request-Nonce", nonce)
	request.Header.Set("X-Signature-V2", hex.EncodeToString(mac.Sum(nil)))
	return request
}

func invalidPaymentHandlerSignatureRequest(t *testing.T) *http.Request {
	t.Helper()
	request := signedPaymentHandlerRequest(t, http.MethodPost, "/api/v1/internal/payment-confirmed", []byte(paymentHandlerBody), paymentHandlerNonce)
	request.Header.Set("X-Signature-V2", strings.Repeat("0", sha256.Size*2))
	return request
}
