package localpayment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

type queryRepo struct {
	products []payments.PaymentProduct
	order    *payments.PaymentOrder
	err      error
	calls    int
}

func (repo *queryRepo) ListPublishedProductSnapshots(context.Context) ([]payments.PaymentProduct, error) {
	repo.calls++
	return repo.products, repo.err
}
func (repo *queryRepo) FindOrder(context.Context, string) (*payments.PaymentOrder, error) {
	repo.calls++
	return repo.order, repo.err
}

type queryAuth struct {
	userID string
	err    error
	calls  int
}

func (auth *queryAuth) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	auth.calls++
	return &sessionauth.AuthenticatedIdentity{UserID: auth.userID}, auth.err
}

func TestQueryPublicProductsContract(t *testing.T) {
	auth := &queryAuth{err: shared.ErrUnauthenticated}
	repo := &queryRepo{products: []payments.PaymentProduct{{ID: "coins_100", Version: 2, Label: "100 Diamonds", DiamondAmount: 100, AmountCents: 999, Currency: "USD", PublishStatus: payments.ProductPublishStatusPublished}}}
	handler := NewHandlerWithQueries(auth, &recordingCheckout{}, nil, payments.NewReadUsecase(repo))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/wallet/products", nil))
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"success": true, "data": map[string]any{"products": []any{map[string]any{"id": "coins_100", "version": float64(2), "label": "100 Diamonds", "diamondAmount": float64(100), "amountCents": float64(999), "currency": "USD"}}}}
	if recorder.Code != 200 || !reflect.DeepEqual(got, want) || auth.calls != 0 {
		t.Fatalf("public catalog = %d %s authCalls=%d", recorder.Code, recorder.Body.String(), auth.calls)
	}
	repo.products = nil
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/wallet/products", nil))
	if recorder.Body.String() != "{\"data\":{\"products\":[]},\"success\":true}\n" {
		t.Fatalf("empty catalog = %s", recorder.Body.String())
	}
}

func TestQueryOrderContractAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, user string
		status     payments.PaymentOrderStatus
		missing    bool
		repoErr    error
		want       int
	}{
		{"pending", "user-1", payments.PaymentOrderStatusPending, false, nil, 200},
		{"paid", "user-1", payments.PaymentOrderStatusPaid, false, nil, 200},
		{"other", "user-2", payments.PaymentOrderStatusPaid, false, nil, 404},
		{"missing", "user-1", payments.PaymentOrderStatusPending, true, nil, 404},
		{"anonymous", "", payments.PaymentOrderStatusPaid, false, nil, 401},
		{"storage failure", "user-1", payments.PaymentOrderStatusPaid, false, errors.New("private database detail"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &queryRepo{order: &payments.PaymentOrder{ID: "order-1", UserID: "user-1", Provider: payments.ProviderPayCores, ProviderOrderID: "private-provider-order", ProductID: "coins_100", ProductVersion: 1, DiamondAmount: 100, AmountCents: 999, Currency: "USD", Status: tc.status, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}, err: tc.repoErr}
			if tc.missing {
				repo.order = nil
			}
			handler := NewHandlerWithQueries(&queryAuth{userID: tc.user}, &recordingCheckout{}, nil, payments.NewReadUsecase(repo))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/payments/order-status/order-1", nil))
			if recorder.Code != tc.want {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if tc.want == 200 {
				paid := tc.status == payments.PaymentOrderStatusPaid
				want := map[string]any{"success": true, "data": map[string]any{"orderId": "order-1", "productId": "coins_100", "status": string(tc.status), "provider": "paycores", "amountCents": float64(999), "currency": "USD", "credits": float64(100), "paymentReceived": paid, "backendReady": paid}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("status body = %#v", got)
				}
			} else if tc.want == 404 && recorder.Body.String() != "{\"code\":\"NOT_FOUND\",\"details\":null,\"message\":\"Order not found\",\"success\":false}\n" {
				t.Fatalf("404 differs = %s", recorder.Body.String())
			}
			if tc.want == 401 && repo.calls != 0 {
				t.Fatal("unauthenticated request accessed orders")
			}
			if tc.want == 503 && got["message"] != "Service unavailable" {
				t.Fatal("storage detail leaked")
			}
		})
	}
}

func TestQueryRejectsNonCanonicalRequests(t *testing.T) {
	repo := &queryRepo{}
	handler := NewHandlerWithQueries(&queryAuth{userID: "u"}, &recordingCheckout{}, nil, payments.NewReadUsecase(repo))
	for _, path := range []string{"/api/wallet/products?x=1", "/api/wallet/products?", "/api/wallet%2fproducts", "/api/payments/order-status/a/b", "/api/payments/order-status/%61", "/api/payments/order-status/..", "/api/payments/order-status/a?userId=u"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
		if recorder.Code != 404 {
			t.Fatalf("%s = %d", path, recorder.Code)
		}
	}
	if repo.calls != 0 {
		t.Fatal("noncanonical route reached repository")
	}
}

func TestQueryProductsRepositoryFailure(t *testing.T) {
	handler := NewHandlerWithQueries(nil, &recordingCheckout{}, nil, payments.NewReadUsecase(&queryRepo{err: errors.New("database detail")}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/wallet/products", nil))
	if recorder.Code != 503 {
		t.Fatalf("error = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestQueryReadonlyAssemblyCannotExecuteCheckout(t *testing.T) {
	handler := NewHandlerWithQueries(&queryAuth{userID: "user-1"}, nil, nil, payments.NewReadUsecase(&queryRepo{}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("POST", checkoutPath, strings.NewReader(`{"productId":"coins_100"}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing checkout dependency = %d", recorder.Code)
	}
}
