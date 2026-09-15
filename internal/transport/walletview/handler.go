// Package walletview 提供由 Gateway 精确接管的钱包只读 HTTP 接口。
//
// 它只读取 Go 自有钱包投影。处理器绝不创建账户、扣钻、查询支付订单或调用 PayCores。
package walletview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-business-service/internal/biz/shared"
	bizwalletview "ai-business-service/internal/biz/walletview"
	"ai-business-service/internal/transport/sessionauth"
)

const (
	summaryPath        = "/api/wallet/summary"
	ledgerPath         = "/api/wallet/ledger"
	defaultLedgerLimit = 50
	maxLedgerLimit     = 100
	maxLedgerSkip      = 100000
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type walletReader interface {
	GetSnapshot(context.Context, string) (*bizwalletview.Snapshot, error)
	ListLedgerEntries(context.Context, bizwalletview.LedgerPageQuery) (*bizwalletview.LedgerPage, error)
}

type handler struct {
	authenticator authenticator
	reader        walletReader
}

// NewHandler 创建钱包只读处理器。实际路由接管由 Gateway 的本地开关和精确 route-switch 决定。
func NewHandler(authenticator authenticator, reader walletReader) http.Handler {
	return &handler{authenticator: authenticator, reader: reader}
}

// ServeHTTP 只服务钱包概览和账本两条路径。接管后所有响应都来自 Go 自有域，
// 不能因错误回退 Node，以免前端混用两个钱包事实源。
func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if request == nil || request.URL == nil {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == summaryPath:
		handler.summary(writer, request, identity.UserID)
	case request.Method == http.MethodGet && request.URL.Path == ledgerPath:
		handler.ledger(writer, request, identity.UserID)
	default:
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
	}
}

func (handler *handler) authenticate(request *http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if handler == nil || handler.authenticator == nil || handler.reader == nil {
		return nil, shared.ErrServiceUnavailable
	}
	identity, err := handler.authenticator.Authenticate(request)
	if err != nil {
		return nil, err
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		return nil, shared.ErrUnauthenticated
	}
	return identity, nil
}

func (handler *handler) summary(writer http.ResponseWriter, request *http.Request, userID string) {
	// 用户标识只来自已验证的 Go 会话；刻意不读取 query/body 中的 userId。
	snapshot, err := handler.reader.GetSnapshot(request.Context(), userID)
	if err != nil {
		writeError(writer, err)
		return
	}
	if snapshot == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	writeSuccess(writer, http.StatusOK, summaryView(*snapshot))
}

func (handler *handler) ledger(writer http.ResponseWriter, request *http.Request, userID string) {
	query, err := parseLedgerQuery(request, userID)
	if err != nil {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	page, err := handler.reader.ListLedgerEntries(request.Context(), query)
	if err != nil {
		writeError(writer, err)
		return
	}
	if page == nil {
		page = &bizwalletview.LedgerPage{}
	}
	writeSuccess(writer, http.StatusOK, ledgerPageView(*page))
}

func parseLedgerQuery(request *http.Request, userID string) (bizwalletview.LedgerPageQuery, error) {
	if request == nil || request.URL == nil {
		return bizwalletview.LedgerPageQuery{}, bizwalletview.ErrInvalidWalletViewQuery
	}
	values := request.URL.Query()
	limit, err := parseBoundedInteger(values["limit"], defaultLedgerLimit, 1, maxLedgerLimit)
	if err != nil {
		return bizwalletview.LedgerPageQuery{}, err
	}
	skip, err := parseBoundedInteger(values["skip"], 0, 0, maxLedgerSkip)
	if err != nil {
		return bizwalletview.LedgerPageQuery{}, err
	}
	cursor, err := parseSingleOptionalValue(values["cursor"])
	if err != nil {
		return bizwalletview.LedgerPageQuery{}, err
	}
	return bizwalletview.LedgerPageQuery{UserID: userID, Limit: limit, Skip: skip, Cursor: cursor}, nil
}

func parseBoundedInteger(rawValues []string, fallback, minimum, maximum int) (int, error) {
	if len(rawValues) == 0 {
		return fallback, nil
	}
	if len(rawValues) != 1 || rawValues[0] == "" {
		return 0, bizwalletview.ErrInvalidWalletViewQuery
	}
	value, err := strconv.Atoi(rawValues[0])
	if err != nil || value < minimum || value > maximum {
		return 0, bizwalletview.ErrInvalidWalletViewQuery
	}
	return value, nil
}

func parseSingleOptionalValue(rawValues []string) (string, error) {
	if len(rawValues) == 0 {
		return "", nil
	}
	if len(rawValues) != 1 {
		return "", bizwalletview.ErrInvalidWalletViewQuery
	}
	return rawValues[0], nil
}

func summaryView(snapshot bizwalletview.Snapshot) map[string]any {
	return map[string]any{
		"balance": snapshot.DiamondBalance,
		"vip": map[string]any{
			"active":    snapshot.VIP.Active,
			"expiresAt": optionalTime(snapshot.VIP.ExpiresAt),
		},
		"timezone":   snapshot.Timezone,
		"localDate":  snapshot.DailyImage.LocalDate,
		"dailyImage": dailyQuotaView(snapshot.DailyImage),
		"dailyVideo": dailyQuotaView(snapshot.DailyVideo),
	}
}

func dailyQuotaView(quota bizwalletview.DailyQuota) map[string]any {
	return map[string]any{"limit": quota.Limit, "used": quota.Used, "remaining": quota.Remaining}
}

func optionalTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func ledgerPageView(page bizwalletview.LedgerPage) map[string]any {
	entries := make([]map[string]any, 0, len(page.Entries))
	for _, entry := range page.Entries {
		entries = append(entries, map[string]any{
			"id": entry.ID, "creationId": entry.CreationID, "deltaDiamonds": entry.DeltaDiamonds,
			"reason": entry.Reason, "createdAt": entry.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	var nextCursor any
	if page.NextCursor != "" {
		nextCursor = page.NextCursor
	}
	return map[string]any{"entries": entries, "nextCursor": nextCursor}
}

func writeError(writer http.ResponseWriter, err error) {
	var apiError *shared.APIError
	if errors.As(err, &apiError) {
		writeClientError(writer, apiError.StatusCode(), apiError.Code(), apiError.Message())
		return
	}
	if errors.Is(err, bizwalletview.ErrInvalidWalletViewQuery) {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	writeClientError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
}

func writeSuccess(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
}

func writeClientError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

var _ http.Handler = (*handler)(nil)
