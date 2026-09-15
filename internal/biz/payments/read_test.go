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
