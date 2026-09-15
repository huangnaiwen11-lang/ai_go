package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	biznotification "ai-business-service/internal/biz/notification"
	"ai-business-service/internal/transport/sessionauth"
)

type fakeAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
}

func (fake fakeAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return fake.identity, nil
}

type fakeNotificationRepository struct {
	lastQuery         biznotification.ListQuery
	readUser          string
	readID            string
	preferencesUserID string
	preferences       biznotification.Preferences
}

func (fake *fakeNotificationRepository) List(_ context.Context, query biznotification.ListQuery) ([]biznotification.Item, int, error) {
	fake.lastQuery = query
	return []biznotification.Item{{ID: "n-1", UserID: query.UserID, Title: "标题", Body: "内容", CreatedAt: time.Unix(1, 0).UTC()}}, 1, nil
}
func (fake *fakeNotificationRepository) MarkRead(_ context.Context, userID, id string, _ time.Time) error {
	fake.readUser, fake.readID = userID, id
	return nil
}
func (fake *fakeNotificationRepository) MarkAllRead(context.Context, string, time.Time) (int, error) {
	return 1, nil
}
func (fake *fakeNotificationRepository) Delete(context.Context, string, string) error { return nil }
func (fake *fakeNotificationRepository) DeleteRead(context.Context, string) (int, error) {
	return 1, nil
}
func (fake *fakeNotificationRepository) GetPreferences(_ context.Context, userID string) (biznotification.Preferences, error) {
	fake.preferencesUserID = userID
	return fake.preferences, nil
}
func (fake *fakeNotificationRepository) SavePreferences(_ context.Context, userID string, preferences biznotification.Preferences) error {
	fake.preferencesUserID, fake.preferences = userID, preferences
	return nil
}

func TestHandlerListUsesSessionUserAndProjectsLegacyFields(t *testing.T) {
	repository := &fakeNotificationRepository{}
	handler := NewHandler(fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-a"}}, biznotification.NewUsecase(repository))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/notifications?limit=10&unreadOnly=true", nil))
	if recorder.Code != http.StatusOK || repository.lastQuery.UserID != "user-a" || repository.lastQuery.Limit != 10 || !repository.lastQuery.UnreadOnly {
		t.Fatalf("unexpected request projection: code=%d query=%+v", recorder.Code, repository.lastQuery)
	}
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || !envelope.Success || envelope.Data.Items[0]["userId"] != "user-a" {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
}

func TestHandlerMarkReadNeverAcceptsClientUserID(t *testing.T) {
	repository := &fakeNotificationRepository{}
	handler := NewHandler(fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, biznotification.NewUsecase(repository))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/notifications/n-9/read?userId=other-user", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound || repository.readUser != "" {
		t.Fatalf("query must not bypass exact route or alter repository: code=%d readUser=%q", recorder.Code, repository.readUser)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/notifications/n-9/read", nil))
	if recorder.Code != http.StatusOK || repository.readUser != "session-user" || repository.readID != "n-9" {
		t.Fatalf("session ownership not propagated: code=%d user=%q id=%q", recorder.Code, repository.readUser, repository.readID)
	}
}

func TestHandlerRejectsUnauthenticatedRequests(t *testing.T) {
	handler := NewHandler(fakeAuthenticator{}, biznotification.NewUsecase(&fakeNotificationRepository{}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/notifications", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", recorder.Code)
	}
}

func TestHandlerClearReadUsesOnlySessionUserAndRejectsQuery(t *testing.T) {
	repository := &fakeNotificationRepository{}
	handler := NewHandler(fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, biznotification.NewUsecase(repository))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/notifications", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/notifications?all=1", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("query status=%d, want 404", recorder.Code)
	}
}

// 偏好只属于当前 Go 会话，浏览器不能在路径、查询参数或 JSON 中指定目标用户。
func TestHandlerNotificationPreferencesUseOnlyCurrentSession(t *testing.T) {
	repository := &fakeNotificationRepository{preferences: biznotification.Preferences{PushEnabled: true, EmailEnabled: false, GenerationCompletedEnabled: true}}
	handler := NewHandler(fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, biznotification.NewUsecase(repository))

	getRecorder := httptest.NewRecorder()
	handler.ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, "/api/notifications/preferences", nil))
	if getRecorder.Code != http.StatusOK || repository.preferencesUserID != "session-user" {
		t.Fatalf("get status=%d user=%q", getRecorder.Code, repository.preferencesUserID)
	}

	patchRecorder := httptest.NewRecorder()
	patchRequest := httptest.NewRequest(http.MethodPatch, "/api/notifications/preferences", strings.NewReader(`{"pushEnabled":false,"emailEnabled":true,"generationCompletedEnabled":false,"userId":"other-user"}`))
	handler.ServeHTTP(patchRecorder, patchRequest)
	if patchRecorder.Code != http.StatusBadRequest || repository.preferences.EmailEnabled {
		t.Fatalf("unknown user field must be rejected: status=%d preferences=%+v", patchRecorder.Code, repository.preferences)
	}

	patchRecorder = httptest.NewRecorder()
	patchRequest = httptest.NewRequest(http.MethodPatch, "/api/notifications/preferences", strings.NewReader(`{"pushEnabled":false,"emailEnabled":true,"generationCompletedEnabled":false}`))
	handler.ServeHTTP(patchRecorder, patchRequest)
	if patchRecorder.Code != http.StatusOK || repository.preferencesUserID != "session-user" || repository.preferences.PushEnabled || !repository.preferences.EmailEnabled || repository.preferences.GenerationCompletedEnabled {
		t.Fatalf("patch status=%d user=%q preferences=%+v", patchRecorder.Code, repository.preferencesUserID, repository.preferences)
	}
}
