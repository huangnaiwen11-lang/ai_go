package localpayment

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/integrations/appstore"
)

const productsPath = "/api/wallet/products"
const externalPaymentMethodsPath = "/api/wallet/external-payment-methods"
const orderStatusPrefix = "/api/payments/order-status/"

var orderStatusPath = regexp.MustCompile(`^/api/payments/order-status/[A-Za-z0-9_-]{1,128}$`)

type queryUsecase interface {
	ListProducts(context.Context) ([]payments.PaymentProduct, error)
	OrderStatus(context.Context, string, string) (payments.OrderStatus, error)
}

type externalMethodsUsecase interface {
	ListPaymentMethods(context.Context, payments.PaymentMethodsRequest) ([]payments.PaymentMethod, error)
}

// NewHandlerWithQueries 在既有支付开关下装配只读用例，不改变 POST 或渠道创建器行为。
func NewHandlerWithQueries(auth authenticator, checkout checkoutUsecase, verifier appstore.Verifier, queries queryUsecase, paycores ...paycoresCheckoutUsecase) http.Handler {
	return NewHandlerWithQueriesAndMethods(auth, checkout, verifier, queries, nil, paycores...)
}

func NewHandlerWithQueriesAndMethods(auth authenticator, checkout checkoutUsecase, verifier appstore.Verifier, queries queryUsecase, methods externalMethodsUsecase, paycores ...paycoresCheckoutUsecase) http.Handler {
	instance := NewHandler(auth, checkout, verifier, paycores...).(*handler)
	instance.queries = queries
	instance.methods = methods
	return instance
}

// serveQuery 只处理规范 GET；商品延续公开目录语义，订单必须使用 Go 登录身份。
func (handler *handler) serveQuery(writer http.ResponseWriter, request *http.Request) bool {
	if request == nil || request.URL == nil || request.Method != http.MethodGet || request.URL.ForceQuery || request.URL.EscapedPath() != request.URL.Path {
		return false
	}
	if request.URL.Path != productsPath && request.URL.Path != externalPaymentMethodsPath && !orderStatusPath.MatchString(request.URL.Path) {
		return false
	}
	if request.URL.Path != externalPaymentMethodsPath && strings.Contains(request.RequestURI, "?") {
		return false
	}
	if request.URL.Path == externalPaymentMethodsPath {
		if handler == nil || handler.methods == nil {
			writeError(writer, shared.ErrServiceUnavailable)
			return true
		}
		identity, err := handler.authenticate(request)
		if err != nil {
			writeError(writer, err)
			return true
		}
		ctx, cancel := context.WithTimeout(request.Context(), paycoresTimeout)
		defer cancel()
		methods, err := handler.methods.ListPaymentMethods(ctx, payments.PaymentMethodsRequest{
			UserID:               identity.UserID,
			ProductID:            strings.TrimSpace(request.URL.Query().Get("productId")),
			Country:              strings.TrimSpace(request.URL.Query().Get("country")),
			ClientDevicePlatform: "web",
		})
		if err != nil {
			writeError(writer, shared.ErrServiceUnavailable)
			return true
		}
		result := make([]map[string]any, 0, len(methods))
		for _, method := range methods {
			result = append(result, map[string]any{
				"provider": method.Provider, "account": method.Account, "label": method.Label,
				"methodType": method.MethodType, "icon": method.Icon, "checkoutType": method.CheckoutType,
				"discountRules": method.DiscountRules,
			})
		}
		writeSuccess(writer, http.StatusOK, map[string]any{"methods": result})
		return true
	}
	if request.URL.Path == productsPath && request.URL.RawQuery != "" {
		return false
	}
	if handler == nil || handler.queries == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return true
	}
	if request.URL.Path == productsPath {
		handler.listProducts(writer, request)
		return true
	}
	identity, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return true
	}
	status, err := handler.queries.OrderStatus(request.Context(), identity.UserID, strings.TrimPrefix(request.URL.Path, orderStatusPrefix))
	if err != nil {
		if errors.Is(err, payments.ErrPaymentOrderNotFound) {
			writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Order not found")
		} else {
			writeError(writer, shared.ErrServiceUnavailable)
		}
		return true
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"orderId": status.OrderID, "productId": status.ProductID, "status": status.Status, "provider": status.Provider, "amountCents": status.AmountCents, "currency": status.Currency, "credits": status.Credits, "paymentReceived": status.PaymentReceived, "backendReady": status.BackendReady})
	return true
}

// productResponse 是公开商品 DTO，明确排除发布态等持久化内部字段。
type productResponse struct {
	ID            string `json:"id"`
	Version       int64  `json:"version"`
	Label         string `json:"label"`
	DiamondAmount int64  `json:"diamondAmount"`
	AmountCents   int64  `json:"amountCents"`
	Currency      string `json:"currency"`
}

func (handler *handler) listProducts(writer http.ResponseWriter, request *http.Request) {
	products, err := handler.queries.ListProducts(request.Context())
	if err != nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	result := make([]productResponse, 0, len(products))
	for _, product := range products {
		result = append(result, productResponse{ID: product.ID, Version: product.Version, Label: product.Label, DiamondAmount: product.DiamondAmount, AmountCents: product.AmountCents, Currency: product.Currency})
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"products": result})
}
