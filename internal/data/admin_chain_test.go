package data

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	a "ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/biz/authcredential"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	transportadmin "ai-business-service/internal/transport/adminview"
	transportauth "ai-business-service/internal/transport/authentry"
	"ai-business-service/internal/transport/sessionauth"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type adminChainAuth struct{ role string }

func (a adminChainAuth) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return &sessionauth.AuthenticatedIdentity{UserID: "admin-chain", Role: a.role}, nil
}

func TestAdminUserWalletFeedbackChain(t *testing.T) {
	client := newLocalMongoClient(t)
	db := client.Database("admin_chain_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	ctx := context.Background()
	t.Cleanup(func() { _ = db.Drop(context.Background()) })
	now := time.Now().UTC()
	for _, row := range []struct {
		collection string
		doc        any
	}{
		{schema.CollectionUsers, model.UserDocument{ID: "admin-chain", Role: "admin", AccountStatus: "normal", SessionVersion: 1, CreatedAt: now}},
		{schema.CollectionUsers, model.UserDocument{ID: "target", DisplayName: "Test Customer", AccountStatus: "normal", BindingState: "bound", SessionVersion: 1, CreatedAt: now}},
		{schema.CollectionCredentials, model.CredentialDocument{ID: "credential", UserID: "target", EmailNormalized: "chain@example.test", PasswordHash: "must-not-leak", Active: true}},
		{schema.CollectionAccounts, model.AccountDocument{ID: "target", DiamondBalance: 100}},
		{schema.CollectionSessions, model.SessionDocument{ID: "target-session", UserID: "target", SessionVersion: 1, ExpiresAt: now.Add(time.Hour)}},
		{schema.CollectionFeedbacks, model.FeedbackDocument{ID: "feedback", UserID: "target", Message: "Please fix my test issue", Type: "bug", CreatedAt: now}},
	} {
		if _, err := db.Collection(row.collection).InsertOne(ctx, row.doc); err != nil {
			t.Fatal(err)
		}
	}
	h := transportadmin.NewHandler(adminChainAuth{"admin"}, NewAdminViewRepository(&Data{client: client, database: db}))
	call := func(t *testing.T, method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Idempotency-Key", "chain-operation")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "must-not-leak") {
			t.Fatal("credential leaked")
		}
		var env struct{ Data map[string]any }
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		return env.Data
	}
	t.Run("user search and revoke", func(t *testing.T) {
		d := call(t, "GET", "/api/admin/users?search=chain%40example.test&page=1&limit=20", "", 200)
		if len(d["users"].([]any)) != 1 {
			t.Fatalf("search=%+v", d)
		}
		call(t, "PATCH", "/api/admin/users/target/status", `{"status":"suspended"}`, 200)
		var user model.UserDocument
		_ = db.Collection(schema.CollectionUsers).FindOne(ctx, bson.M{"_id": "target"}).Decode(&user)
		if user.AccountStatus != "banned" || user.SessionVersion != 2 {
			t.Fatalf("user=%+v", user)
		}
		call(t, "PATCH", "/api/admin/users/target/status", `{"status":"active"}`, 200)
		call(t, "PATCH", "/api/admin/users/admin-chain/status", `{"status":"suspended"}`, 403)
		call(t, "PATCH", "/api/admin/users/target/role", `{"role":"super_admin"}`, 403)
	})
	t.Run("wallet mutation idempotency and negative guard", func(t *testing.T) {
		body := `{"userId":"target","delta":50,"reason":"acceptance test"}`
		for i := 0; i < 2; i++ {
			d := call(t, "POST", "/api/admin/wallet/adjust", body, 200)
			if d["newBalance"] != float64(150) {
				t.Fatalf("balance=%+v", d)
			}
		}
		call(t, "POST", "/api/admin/wallet/adjust", `{"userId":"target","delta":60,"reason":"acceptance test"}`, 409)
		d := call(t, "GET", "/api/admin/wallet/ledger?userId=target", "", 200)
		if len(d["entries"].([]any)) != 1 {
			t.Fatalf("duplicate ledger=%+v", d)
		}
		call(t, "GET", "/api/admin/wallet/user/target", "", 200)
		call(t, "GET", "/api/admin/wallet/stats", "", 200)
		call(t, "GET", "/api/admin/wallet/purchases?page=1&limit=50", "", 200)
		call(t, "GET", "/api/admin/wallet/purchase-countries", "", 200)
	})
	t.Run("feedback response and notification atomic", func(t *testing.T) {
		call(t, "GET", "/api/admin/reports/feedback?status=pending&type=bug", "", 200)
		call(t, "GET", "/api/admin/reports/feedback/stats", "", 200)
		for i := 0; i < 2; i++ {
			call(t, "POST", "/api/admin/reports/feedback/feedback/respond", `{"response":"We have resolved your issue","note":"test"}`, 200)
		}
		count, err := db.Collection(schema.CollectionNotifications).CountDocuments(ctx, bson.M{"user_id": "target"})
		if err != nil || count != 1 {
			t.Fatalf("notifications=%d err=%v", count, err)
		}
		d := call(t, "GET", "/api/admin/reports/feedback?status=resolved", "", 200)
		if len(d["feedbacks"].([]any)) != 1 {
			t.Fatalf("feedback=%+v", d)
		}
	})
}

func TestAdminDashboardHTTPChain(t *testing.T) {
	client := newLocalMongoClient(t)
	db := client.Database("admin_chain_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	ctx, cancel := newMongoTestContext()
	defer cancel()
	t.Cleanup(func() { _ = db.Drop(context.Background()) })
	now := time.Now().UTC()
	for _, item := range []struct {
		collection string
		doc        any
	}{
		{schema.CollectionUsers, model.UserDocument{ID: "test-user", CreatedAt: now, AccountStatus: "normal", BindingState: "bound"}},
		{schema.CollectionCreations, model.CreationDocument{ID: "test-image", UserID: "test-user", ProductOutput: "image", Status: "pending_submission", CreatedAt: now}},
		{schema.CollectionPaymentOrders, model.PaymentOrderDocument{ID: "paid-usd", UserID: "test-user", Status: "paid", Currency: "USD", AmountCents: 1250, CreatedAt: now, UpdatedAt: now}},
		{schema.CollectionPaymentOrders, model.PaymentOrderDocument{ID: "pending", Status: "pending", Currency: "USD", AmountCents: 900, CreatedAt: now}},
		{schema.CollectionPaymentOrders, model.PaymentOrderDocument{ID: "paid-cny", Status: "paid", Currency: "CNY", AmountCents: 9000, CreatedAt: now, UpdatedAt: now}},
	} {
		if _, err := db.Collection(item.collection).InsertOne(ctx, item.doc); err != nil {
			t.Fatal(err)
		}
	}
	h := transportadmin.NewHandler(adminChainAuth{"admin"}, NewAdminViewRepository(&Data{client: client, database: db}))
	request := func(t *testing.T, path string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("%s: status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		var env struct{ Data map[string]any }
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		return env.Data
	}
	t.Run("overview uses paid USD not legacy completed", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/admin/analytics/overview", nil))
		var env struct {
			Data struct {
				Revenue struct{ Total float64 }
				Images  struct{ Pending int }
			}
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Data.Revenue.Total != 12.5 || env.Data.Images.Pending != 1 {
			t.Fatalf("overview=%s", rr.Body.String())
		}
	})
	for _, metric := range []string{"users", "images", "videos", "revenue-trends"} {
		t.Run(metric, func(t *testing.T) {
			data := request(t, "/api/admin/analytics/"+metric+"?days=3")
			if len(data["trends"].([]any)) != 3 {
				t.Fatalf("expected 3 days including zero buckets: %+v", data)
			}
			want := float64(1)
			if metric == "videos" {
				want = 0
			}
			if metric == "revenue-trends" {
				want = 12.5
			}
			if data["total"] != want {
				t.Fatalf("total=%v want=%v", data["total"], want)
			}
		})
	}
	t.Run("bad query rejected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/admin/analytics/users?days=999999", nil))
		if rr.Code != 400 {
			t.Fatalf("status=%d", rr.Code)
		}
	})
}

func TestAdminAuthenticatedHTTPChain(t *testing.T) {
	client := newLocalMongoClient(t)
	db := client.Database("admin_auth_chain_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	ctx := context.Background()
	t.Cleanup(func() { _ = db.Drop(ctx) })
	if err := migrate.NewInitializer(db).Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	storage := &Data{client: client, database: db}
	policy := authcredential.MustNewPolicy(authcredential.Params{MemoryKiB: 19456, TimeCost: 2, Parallelism: 1, SaltBytes: 16, KeyBytes: 32})
	password := "Isolated-" + uuid.NewString()
	hash, err := policy.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"admin", "customer"} {
		role := "user"
		if id == "admin" {
			role = "admin"
		}
		for _, row := range []struct {
			collection string
			doc        any
		}{
			{schema.CollectionUsers, model.UserDocument{ID: id, Role: role, AccountStatus: "normal", BindingState: "bound", Timezone: "Asia/Shanghai", SessionVersion: 1, CreatedAt: now, UpdatedAt: now}},
			{schema.CollectionCredentials, model.CredentialDocument{ID: id, UserID: id, EmailNormalized: id + "@example.test", PasswordHash: hash, Active: true, CreatedAt: now, UpdatedAt: now}},
			{schema.CollectionAccounts, model.AccountDocument{ID: id, DiamondBalance: 100}},
		} {
			if _, err := db.Collection(row.collection).InsertOne(ctx, row.doc); err != nil {
				t.Fatal(err)
			}
		}
	}
	usecase := identity.NewAuthUsecase(NewAuthUserRepository(storage), NewIdentityRepository(storage), NewAuthSessionRepository(storage), NewAccountRepository(storage), NewCredentialRepository(storage), policy, NewTxRunner(storage))
	auth := sessionauth.NewAuthenticator(usecase)
	mux := http.NewServeMux()
	mux.Handle("/api/auth/", transportauth.NewHandler(auth, usecase))
	mux.Handle("/api/admin/", transportadmin.NewHandler(auth, NewAdminViewRepository(storage)))
	call := func(method, path, body, token, key string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Fatalf("%s %s got %d want %d: %s", method, path, rr.Code, want, rr.Body.String())
		}
		var envelope struct{ Data map[string]any }
		if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	login := func(id string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"email": id + "@example.test", "password": password})
		d := call("POST", "/api/auth/login", string(body), "", "", 200)
		token, ok := d["token"].(string)
		if !ok || token == "" {
			t.Fatal("login returned no token")
		}
		return token
	}
	adminToken, customerToken := login("admin"), login("customer")
	call("GET", "/api/admin/users", "", "", "", 401)
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/admin/users", ""},
		{"POST", "/api/admin/wallet/adjust", `{"userId":"customer","delta":10,"reason":"test adjustment"}`},
		{"PATCH", "/api/admin/users/admin/role", `{"role":"super_admin"}`},
		{"POST", "/api/admin/reports/feedback/missing/respond", `{"response":"test"}`},
	} {
		call(route.method, route.path, route.body, customerToken, "unauthorized-op", 403)
	}
	call("GET", "/api/auth/me", "", adminToken, "", 200)
	call("GET", "/api/admin/users", "", adminToken, "", 200)
	call("POST", "/api/admin/wallet/adjust", `{"userId":"customer","delta":10,"reason":"test adjustment"}`, adminToken, "", 400)
	call("POST", "/api/admin/wallet/adjust", `{"userId":"customer","delta":-101,"reason":"test negative guard"}`, adminToken, "low-balance-key", 409)
	call("POST", "/api/admin/wallet/adjust", `{"userId":"customer","delta":10,"reason":"test adjustment"}`, adminToken, "credit-key", 200)
	call("POST", "/api/admin/wallet/adjust", `{"userId":"customer","delta":10,"reason":"test adjustment"}`, adminToken, "credit-key", 200)
	d := call("GET", "/api/admin/wallet/user/customer", "", adminToken, "", 200)
	if d["wallet"].(map[string]any)["balance"] != float64(110) {
		t.Fatalf("wallet=%v", d)
	}
	call("PATCH", "/api/admin/users/customer/status", `{"status":"suspended"}`, adminToken, "", 200)
	call("GET", "/api/auth/me", "", customerToken, "", 401)
	call("PATCH", "/api/admin/users/customer/status", `{"status":"active"}`, adminToken, "", 200)
	call("GET", "/api/auth/me", "", customerToken, "", 401)
	call("GET", "/api/auth/me", "", login("customer"), "", 200)
	// A pre-existing ledger key without a matching audit is a conflict, never success.
	digest := sha256.Sum256([]byte("admin\x00orphan-ledger-key"))
	orphanID := "adjust-" + hex.EncodeToString(digest[:])
	if _, err := db.Collection(schema.CollectionLedgerEntries).InsertOne(ctx, bson.M{"_id": orphanID, "idempotency_key": orphanID, "account_id": "other", "delta_diamonds": 1}); err != nil {
		t.Fatal(err)
	}
	call("POST", "/api/admin/wallet/adjust", `{"userId":"customer","delta":10,"reason":"orphan ledger test"}`, adminToken, "orphan-ledger-key", 409)
	// Failed transaction must leave neither an account mutation nor a ledger entry.
	if n, err := db.Collection(schema.CollectionLedgerEntries).CountDocuments(ctx, bson.M{"account_id": "customer"}); err != nil || n != 1 {
		t.Fatalf("ledger count=%d err=%v", n, err)
	}
	// Role is checked from the current user, not trusted from an old session.
	if _, err := db.Collection(schema.CollectionUsers).UpdateOne(ctx, bson.M{"_id": "admin"}, bson.M{"$set": bson.M{"role": "user"}}); err != nil {
		t.Fatal(err)
	}
	call("GET", "/api/admin/users", "", adminToken, "", 403)
}

func TestAdminConcurrentAdjustmentChain(t *testing.T) {
	client := newLocalMongoClient(t)
	db := client.Database("admin_concurrency_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	ctx := context.Background()
	t.Cleanup(func() { _ = db.Drop(ctx) })
	if err := migrate.NewInitializer(db).Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection(schema.CollectionUsers).InsertOne(ctx, model.UserDocument{ID: "customer", AccountStatus: "normal", BindingState: "bound"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection(schema.CollectionAccounts).InsertOne(ctx, model.AccountDocument{ID: "customer", DiamondBalance: 100}); err != nil {
		t.Fatal(err)
	}
	h := transportadmin.NewHandler(adminChainAuth{"admin"}, NewAdminViewRepository(&Data{client: client, database: db}))
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/api/admin/wallet/adjust", strings.NewReader(`{"userId":"customer","delta":10,"reason":"concurrency test"}`))
			req.Header.Set("Idempotency-Key", "same-operation")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			results <- rr
		}()
	}
	wg.Wait()
	close(results)
	for rr := range results {
		if rr.Code != 200 {
			t.Errorf("adjust=%d %s", rr.Code, rr.Body.String())
		}
	}
	var account model.AccountDocument
	if err := db.Collection(schema.CollectionAccounts).FindOne(ctx, bson.M{"_id": "customer"}).Decode(&account); err != nil {
		t.Fatal(err)
	}
	if account.DiamondBalance != 110 {
		t.Errorf("balance=%d", account.DiamondBalance)
	}
	if n, err := db.Collection(schema.CollectionLedgerEntries).CountDocuments(ctx, bson.M{}); err != nil || n != 1 {
		t.Fatalf("ledger=%d err=%v", n, err)
	}
}

func TestAdminRevenueShanghaiWindowAndUnknownBucket(t *testing.T) {
	client := newLocalMongoClient(t)
	db := client.Database("admin_revenue_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	ctx := context.Background()
	t.Cleanup(func() { _ = db.Drop(ctx) })
	start := time.Date(2026, 9, 16, 16, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		collection string
		doc        any
	}{
		{schema.CollectionUsers, bson.M{"_id": "empty", "created_at": start, "geo": bson.M{"country": ""}, "acquisition": bson.M{"channel": ""}, "guest_platform": ""}},
		{schema.CollectionUsers, bson.M{"_id": "missing", "created_at": start.Add(-time.Hour)}},
		{schema.CollectionPaymentOrders, model.PaymentOrderDocument{ID: "d0", UserID: "empty", Status: "paid", Currency: "USD", AmountCents: 500, UpdatedAt: start}},
		{schema.CollectionPaymentOrders, model.PaymentOrderDocument{ID: "old-user", UserID: "missing", Status: "paid", Currency: "USD", AmountCents: 1000, UpdatedAt: start.Add(time.Hour)}},
		{schema.CollectionPaymentOrders, model.PaymentOrderDocument{ID: "outside", UserID: "empty", Status: "paid", Currency: "USD", AmountCents: 9000, UpdatedAt: start.Add(24 * time.Hour)}},
	} {
		if _, err := db.Collection(row.collection).InsertOne(ctx, row.doc); err != nil {
			t.Fatal(err)
		}
	}
	result, err := NewAdminViewRepository(&Data{client: client, database: db}).RevenueBreakdowns(ctx, a.Window{From: start, Until: start.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for name, rows := range map[string][]a.RevenueBucket{"country": result.Country, "source": result.Source, "client": result.Client} {
		if len(rows) != 1 || rows[0].Key != "unknown" || rows[0].Cents != 1500 || rows[0].D0Cents != 500 {
			t.Errorf("%s=%+v", name, rows)
		}
	}
}
