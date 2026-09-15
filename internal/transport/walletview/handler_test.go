package walletview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-business-service/internal/biz/shared"
	bizwalletview "ai-business-service/internal/biz/walletview"
	"ai-business-service/internal/transport/sessionauth"
)

func TestWalletSummary仅信任Go会话身份并返回零余额快照(t *testing.T) {
	usecase := &recordingUsecase{snapshot: &bizwalletview.Snapshot{
		DiamondBalance: 0,
		VIP:            bizwalletview.VIP{Active: false},
		Timezone:       "Asia/Shanghai",
		DailyImage:     bizwalletview.DailyQuota{Limit: 10, Used: 0, Remaining: 10, LocalDate: "2026-09-11"},
		DailyVideo:     bizwalletview.DailyQuota{Limit: 3, Used: 0, Remaining: 3, LocalDate: "2026-09-11"},
	}}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, usecase)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/wallet/summary?userId=attacker-user", nil))

	if recorder.Code != http.StatusOK || usecase.snapshotUserID != "session-user" {
		t.Fatalf("summary status=%d userID=%q", recorder.Code, usecase.snapshotUserID)
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Balance int64 `json:"balance"`
			VIP     struct {
				Active bool `json:"active"`
			} `json:"vip"`
			Timezone   string `json:"timezone"`
			LocalDate  string `json:"localDate"`
			DailyImage struct {
				Limit     int32 `json:"limit"`
				Used      int32 `json:"used"`
				Remaining int32 `json:"remaining"`
			} `json:"dailyImage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if !response.Success || response.Data.Balance != 0 || response.Data.VIP.Active || response.Data.Timezone != "Asia/Shanghai" || response.Data.LocalDate != "2026-09-11" || response.Data.DailyImage.Remaining != 10 {
		t.Fatalf("unexpected summary response: %s", recorder.Body.String())
	}
}

func TestWalletView未认证返回统一401信封(t *testing.T) {
	handler := NewHandler(staticAuthenticator{err: shared.ErrUnauthenticated}, &recordingUsecase{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/wallet/summary", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", recorder.Code)
	}
	assertErrorEnvelope(t, recorder, "UNAUTHORIZED")
}

func TestWalletLedger保留Cursor并由领域层忽略Skip(t *testing.T) {
	cursor, err := bizwalletview.EncodeLedgerCursor(time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC), "entry-2")
	if err != nil {
		t.Fatal(err)
	}
	usecase := &recordingUsecase{ledger: &bizwalletview.LedgerPage{Entries: []bizwalletview.LedgerEntry{{ID: "entry-1", DeltaDiamonds: 20, Reason: "充值", CreatedAt: time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)}}, NextCursor: "next-cursor"}}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, usecase)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/wallet/ledger?limit=20&skip=9&cursor="+cursor, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", recorder.Code, recorder.Body.String())
	}
	if usecase.ledgerQuery.UserID != "session-user" || usecase.ledgerQuery.Limit != 20 || usecase.ledgerQuery.Skip != 9 || usecase.ledgerQuery.Cursor != cursor {
		t.Fatalf("ledger query=%#v", usecase.ledgerQuery)
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Entries []struct {
				ID string `json:"id"`
			} `json:"entries"`
			NextCursor string `json:"nextCursor"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || !response.Success || len(response.Data.Entries) != 1 || response.Data.Entries[0].ID != "entry-1" || response.Data.NextCursor != "next-cursor" {
		t.Fatalf("ledger response=%s, err=%v", recorder.Body.String(), err)
	}
}

func TestWalletLedger非法分页或Cursor返回400(t *testing.T) {
	for _, target := range []string{
		"/api/wallet/ledger?limit=0",
		"/api/wallet/ledger?limit=51x",
		"/api/wallet/ledger?limit=1001",
		"/api/wallet/ledger?skip=-1",
		"/api/wallet/ledger?cursor=invalid",
	} {
		t.Run(target, func(t *testing.T) {
			usecase := &recordingUsecase{ledgerErr: bizwalletview.ErrInvalidWalletViewQuery}
			handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, usecase)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, body=%s", recorder.Code, recorder.Body.String())
			}
			assertErrorEnvelope(t, recorder, "INVALID_REQUEST")
		})
	}
}

type walletViewUsecase interface {
	GetSnapshot(context.Context, string) (*bizwalletview.Snapshot, error)
	ListLedgerEntries(context.Context, bizwalletview.LedgerPageQuery) (*bizwalletview.LedgerPage, error)
}

type recordingUsecase struct {
	snapshot       *bizwalletview.Snapshot
	snapshotErr    error
	snapshotUserID string
	ledger         *bizwalletview.LedgerPage
	ledgerErr      error
	ledgerQuery    bizwalletview.LedgerPageQuery
}

func (usecase *recordingUsecase) GetSnapshot(_ context.Context, userID string) (*bizwalletview.Snapshot, error) {
	usecase.snapshotUserID = userID
	return usecase.snapshot, usecase.snapshotErr
}

func (usecase *recordingUsecase) ListLedgerEntries(_ context.Context, query bizwalletview.LedgerPageQuery) (*bizwalletview.LedgerPage, error) {
	usecase.ledgerQuery = query
	return usecase.ledger, usecase.ledgerErr
}

type staticAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (authenticator staticAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return authenticator.identity, authenticator.err
}

func assertErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var response struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Details any    `json:"details"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Success || response.Code != wantCode || response.Message == "" || response.Details != nil {
		t.Fatalf("error response=%s, err=%v", recorder.Body.String(), err)
	}
}
