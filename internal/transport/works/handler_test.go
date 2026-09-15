package works

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-business-service/internal/biz/shared"
	bizworks "ai-business-service/internal/biz/works"
	"ai-business-service/internal/transport/sessionauth"
)

func Test作品列表仅信任Go会话身份并且不泄露内部字段(t *testing.T) {
	usecase := &recordingUsecase{page: &bizworks.Page{Items: []bizworks.Work{{
		ID: "work-1", Kind: bizworks.KindImage, Status: "succeeded", TemplateID: "template-1", TemplateVersion: 2,
		CreatedAt: time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 11, 8, 1, 0, 0, time.UTC), ResultURL: "https://assets.example.test/final.png",
	}}, NextCursor: "next"}}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, usecase)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/works?kind=image&limit=1&userId=attacker-user", nil))

	if recorder.Code != http.StatusOK || usecase.listQuery.UserID != "session-user" || usecase.listQuery.Kind != bizworks.KindImage {
		t.Fatalf("status=%d query=%#v body=%s", recorder.Code, usecase.listQuery, recorder.Body.String())
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Items []struct {
				ID        string `json:"id"`
				Kind      string `json:"kind"`
				ResultURL string `json:"resultUrl"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || !response.Success || len(response.Data.Items) != 1 || response.Data.Items[0].ID != "work-1" || response.Data.Items[0].Kind != "image" || response.Data.Items[0].ResultURL == "" {
		t.Fatalf("response=%s err=%v", recorder.Body.String(), err)
	}
	for _, forbidden := range []string{"userId", "requestFingerprint", "externalExecution", "modelSku", "diamond", "payment"} {
		if containsJSONField(recorder.Body.Bytes(), forbidden) {
			t.Fatalf("响应不应包含内部字段 %q：%s", forbidden, recorder.Body.String())
		}
	}
}

func Test作品详情跨用户与未命中返回404(t *testing.T) {
	usecase := &recordingUsecase{getErr: bizworks.ErrWorkNotFound}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, usecase)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/works/other-user-work", nil))
	if recorder.Code != http.StatusNotFound || usecase.getUserID != "session-user" || usecase.getID != "other-user-work" {
		t.Fatalf("status=%d user=%q id=%q body=%s", recorder.Code, usecase.getUserID, usecase.getID, recorder.Body.String())
	}
	assertErrorEnvelope(t, recorder, "NOT_FOUND")
}

func Test作品历史未认证返回401(t *testing.T) {
	handler := NewHandler(staticAuthenticator{err: shared.ErrUnauthenticated}, &recordingUsecase{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/works?kind=image", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	assertErrorEnvelope(t, recorder, "UNAUTHORIZED")
}

func Test作品列表拒绝非法参数(t *testing.T) {
	for _, target := range []string{
		"/api/works", "/api/works?kind=animate", "/api/works?kind=image&limit=0", "/api/works?kind=video&limit=101", "/api/works?kind=image&cursor=invalid", "/api/works?kind=image&kind=video",
	} {
		t.Run(target, func(t *testing.T) {
			handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, &recordingUsecase{})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			assertErrorEnvelope(t, recorder, "INVALID_REQUEST")
		})
	}
}

type recordingUsecase struct {
	page      *bizworks.Page
	listErr   error
	listQuery bizworks.ListQuery
	work      *bizworks.Work
	getErr    error
	getUserID string
	getID     string
}

func (usecase *recordingUsecase) List(_ context.Context, query bizworks.ListQuery) (*bizworks.Page, error) {
	usecase.listQuery = query
	return usecase.page, usecase.listErr
}

func (usecase *recordingUsecase) Get(_ context.Context, userID, id string) (*bizworks.Work, error) {
	usecase.getUserID, usecase.getID = userID, id
	return usecase.work, usecase.getErr
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
		Details any    `json:"details"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Success || response.Code != wantCode || response.Details != nil {
		t.Fatalf("response=%s err=%v", recorder.Body.String(), err)
	}
}

func containsJSONField(body []byte, field string) bool {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return false
	}
	return containsField(value, field)
}

func containsField(value any, field string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == field || containsField(child, field) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsField(child, field) {
				return true
			}
		}
	}
	return false
}

var _ worksReader = (*recordingUsecase)(nil)
