package adminview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/transport/sessionauth"
)

func TestHandler拒绝非管理员会话访问管理接口(t *testing.T) {
	handler := NewHandler(&fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1", Role: "user"}}, &fakeReader{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/analytics/overview", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestHandler管理员读取总览不回退上游(t *testing.T) {
	reader := &fakeReader{overview: adminview.Overview{TotalUsers: 3, TotalImages: 5, TotalVideos: 2}}
	handler := NewHandler(&fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: "admin"}}, reader)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/analytics/overview", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if reader.overviewCalls != 1 {
		t.Fatalf("overview calls = %d, want 1", reader.overviewCalls)
	}
}

// /admin/users/duplicates 与 /admin/users/overview 是尚未迁移的**子资源**，
// 不能被当成用户 ID 去查用户 —— 那会返回 404「用户不存在」，
// 把「没实现」伪装成「资源缺失」。fakeReader 未实现 User 方法，
// 一旦代码走到查询就会 panic，因此本测试同时守住回归。
func TestHandler未迁移的用户子资源返回501(t *testing.T) {
	handler := NewHandler(&fakeAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: "admin"}}, &fakeReader{})
	for _, path := range []string{"/api/admin/users/duplicates", "/api/admin/users/overview"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotImplemented {
			t.Errorf("%s: status = %d, want %d", path, recorder.Code, http.StatusNotImplemented)
		}
	}
}

type fakeAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (fake *fakeAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return fake.identity, fake.err
}

type fakeReader struct {
	adminview.Repository
	overview      adminview.Overview
	overviewCalls int
}

func (fake *fakeReader) Overview(context.Context) (adminview.Overview, error) {
	fake.overviewCalls++
	return fake.overview, nil
}
