package payments

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"ai-business-service/internal/biz/shared"
)

type readRepositoryStub struct {
	products   []PaymentProduct
	order      *PaymentOrder
	err        error
	orderCalls int
}

type paycoresStatusStub struct {
	result          PayCoresOrderStatusSnapshot
	err             error
	calls           int
	orderID, userID string
}

func (stub *paycoresStatusStub) GetPayCoresOrderStatus(_ context.Context, orderID, userID string) (PayCoresOrderStatusSnapshot, error) {
	stub.calls++
	stub.orderID, stub.userID = orderID, userID
	return stub.result, stub.err
}

func (repo *readRepositoryStub) ListPublishedProductSnapshots(context.Context) ([]PaymentProduct, error) {
	return repo.products, repo.err
}
func (repo *readRepositoryStub) FindOrder(context.Context, string) (*PaymentOrder, error) {
	repo.orderCalls++
	return repo.order, repo.err
}

func listedProduct(id string, version, cents int64) PaymentProduct {
	return PaymentProduct{ID: id, Version: version, Label: id, DiamondAmount: 100, AmountCents: cents, Currency: "USD", PublishStatus: ProductPublishStatusPublished}
}

func TestReadProductsLatestPublishedAndStableOrder(t *testing.T) {
	draft := listedProduct("a", 9, 999)
	draft.PublishStatus = ProductPublishStatusDraft
	foreign := listedProduct("eur", 1, 999)
	foreign.Currency = "EUR"
	blankLabel := listedProduct("blank", 1, 999)
	blankLabel.Label = " "
	repo := &readRepositoryStub{products: []PaymentProduct{listedProduct("z", 1, 500), listedProduct("a", 1, 200), listedProduct("a", 2, 300), draft, listedProduct("invalid-latest", 1, 100), listedProduct("invalid-latest", 2, 0), foreign, blankLabel}}
	got, err := NewReadUsecase(repo).ListProducts(context.Background())
	want := []PaymentProduct{listedProduct("a", 2, 300), listedProduct("z", 1, 500)}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("products = %#v, error = %v, want %#v", got, err, want)
	}
	if repo.products[0].ID != "z" {
		t.Fatal("read mutated repository snapshots")
	}
}

func TestReadProductsEmptyAndRepositoryError(t *testing.T) {
	got, err := NewReadUsecase(&readRepositoryStub{}).ListProducts(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty = %#v %v", got, err)
	}
	broken := errors.New("database offline")
	if _, err := NewReadUsecase(&readRepositoryStub{err: broken}).ListProducts(context.Background()); !errors.Is(err, broken) {
		t.Fatalf("error = %v", err)
	}
	if _, err := NewReadUsecase(nil).ListProducts(context.Background()); !errors.Is(err, ErrDependenciesUnavailable) {
		t.Fatalf("nil repo = %v", err)
	}
}

func readOrder(status PaymentOrderStatus) *PaymentOrder {
	return &PaymentOrder{ID: "order-1", UserID: "user-1", Provider: ProviderPayCores, ProviderOrderID: "external-1", ProductID: "coins_100", ProductVersion: 1, DiamondAmount: 100, AmountCents: 999, Currency: "USD", Status: status, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}
}

func TestReadOrderUsesFrozenCommittedFacts(t *testing.T) {
	for _, status := range []PaymentOrderStatus{PaymentOrderStatusPending, PaymentOrderStatusPaid} {
		t.Run(string(status), func(t *testing.T) {
			repo := &readRepositoryStub{order: readOrder(status), products: []PaymentProduct{listedProduct("coins_100", 99, 999999)}}
			got, err := NewReadUsecase(repo).OrderStatus(context.Background(), "user-1", "order-1")
			paid := status == PaymentOrderStatusPaid
			if err != nil || got.OrderID != "order-1" || got.ProductID != "coins_100" || got.Status != status || got.Provider != ProviderPayCores || got.AmountCents != 999 || got.Currency != "USD" || got.Credits != 100 || got.PaymentReceived != paid || got.BackendReady != paid {
				t.Fatalf("status = %#v, err = %v", got, err)
			}
			if repo.order.Status != status || repo.orderCalls != 1 {
				t.Fatal("query modified order or read more than once")
			}
		})
	}
}

func TestReadOrderOwnershipMissingAndErrors(t *testing.T) {
	broken := errors.New("database offline")
	for _, tc := range []struct {
		name, user    string
		order         *PaymentOrder
		repoErr, want error
	}{
		{"missing", "user-1", nil, nil, ErrPaymentOrderNotFound},
		{"other owner", "user-2", readOrder(PaymentOrderStatusPaid), nil, ErrPaymentOrderNotFound},
		{"anonymous", "", readOrder(PaymentOrderStatusPaid), nil, shared.ErrUnauthenticated},
		{"repository", "user-1", nil, broken, broken},
		{"unknown status", "user-1", readOrder("processing"), nil, ErrInvalidPaymentOrder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &readRepositoryStub{order: tc.order, err: tc.repoErr}
			_, err := NewReadUsecase(repo).OrderStatus(context.Background(), tc.user, "order-1")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if tc.user == "" && repo.orderCalls != 0 {
				t.Fatal("anonymous request reached repository")
			}
		})
	}
}

func TestReadPendingPayCoresOrderReportsReceivedButDoesNotSettle(t *testing.T) {
	repo := &readRepositoryStub{order: readOrder(PaymentOrderStatusPending)}
	remote := &paycoresStatusStub{result: PayCoresOrderStatusSnapshot{OrderID: "external-1", ProductID: "coins_100", AmountUSD: 9.99, Status: "paid", PaymentReceived: true, BackendReady: true}}
	got, err := NewReadUsecaseWithPayCoresStatus(repo, remote).OrderStatus(context.Background(), "user-1", "order-1")
	if err != nil || !got.PaymentReceived || got.BackendReady || got.Status != PaymentOrderStatusPending || repo.order.Status != PaymentOrderStatusPending {
		t.Fatalf("status = %#v, local = %#v, err = %v", got, repo.order, err)
	}
	if remote.calls != 1 || remote.orderID != "external-1" || remote.userID != "user-1" {
		t.Fatalf("remote call = %#v", remote)
	}
}

func TestReadPayCoresStatusRequiresMatchingFrozenFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*PayCoresOrderStatusSnapshot)
	}{
		{"order ID", func(s *PayCoresOrderStatusSnapshot) { s.OrderID = "different" }},
		{"product ID", func(s *PayCoresOrderStatusSnapshot) { s.ProductID = "other" }},
		{"amount", func(s *PayCoresOrderStatusSnapshot) { s.AmountUSD = 9.98 }},
		{"status", func(s *PayCoresOrderStatusSnapshot) { s.Status = "pending" }},
		{"receipt flag", func(s *PayCoresOrderStatusSnapshot) { s.PaymentReceived = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := PayCoresOrderStatusSnapshot{OrderID: "external-1", ProductID: "coins_100", AmountUSD: 9.99, Status: "paid", PaymentReceived: true}
			tc.mutate(&snapshot)
			got, err := NewReadUsecaseWithPayCoresStatus(&readRepositoryStub{order: readOrder(PaymentOrderStatusPending)}, &paycoresStatusStub{result: snapshot}).OrderStatus(context.Background(), "user-1", "order-1")
			if err != nil || got.PaymentReceived || got.BackendReady || got.Status != PaymentOrderStatusPending {
				t.Fatalf("status = %#v, err = %v", got, err)
			}
		})
	}
}

func TestReadPayCoresStatusSkipsUnknownOrUnauthorizedOrders(t *testing.T) {
	for _, tc := range []struct {
		name      string
		order     *PaymentOrder
		user      string
		wantCalls int
	}{
		{"other owner", readOrder(PaymentOrderStatusPending), "user-2", 0},
		{"already settled", readOrder(PaymentOrderStatusPaid), "user-1", 0},
		{"unbound", func() *PaymentOrder {
			o := readOrder(PaymentOrderStatusPending)
			o.ProviderOrderID = "local-paycores-order-1"
			return o
		}(), "user-1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := &paycoresStatusStub{err: errors.New("upstream offline")}
			_, _ = NewReadUsecaseWithPayCoresStatus(&readRepositoryStub{order: tc.order}, remote).OrderStatus(context.Background(), tc.user, "order-1")
			if remote.calls != tc.wantCalls {
				t.Fatalf("remote calls = %d", remote.calls)
			}
		})
	}
	remote := &paycoresStatusStub{err: errors.New("upstream offline")}
	got, err := NewReadUsecaseWithPayCoresStatus(&readRepositoryStub{order: readOrder(PaymentOrderStatusPending)}, remote).OrderStatus(context.Background(), "user-1", "order-1")
	if err != nil || got.PaymentReceived || got.BackendReady || remote.calls != 1 {
		t.Fatalf("upstream failure status = %#v, calls = %d, err = %v", got, remote.calls, err)
	}
}
