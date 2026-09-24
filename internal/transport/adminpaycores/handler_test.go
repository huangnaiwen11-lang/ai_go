package adminpaycores

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-business-service/internal/integrations/paycores"
	"ai-business-service/internal/transport/sessionauth"
)

type authStub struct {
	identity *sessionauth.AuthenticatedIdentity
}

func (s authStub) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return s.identity, nil
}

type channelsStub struct {
	data json.RawMessage
	err  error
}

func (s channelsStub) ChannelsOverview(context.Context) (json.RawMessage, error) {
	return s.data, s.err
}

func TestHandlerSessionRoleAndPaycoresFailureModes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		role   string
		reader channelReader
		want   int
		code   string
	}{
		{"anonymous", "", channelsStub{}, 401, "UNAUTHENTICATED"},
		{"non-admin", "user", channelsStub{}, 403, "FORBIDDEN"},
		{"unconfigured", "admin", nil, 503, "PAYCORES_NOT_CONFIGURED"},
		{"upstream-unavailable", "admin", channelsStub{err: errors.New("network down")}, 502, "PAYCORES_UNAVAILABLE"},
		{"client-unconfigured", "admin", channelsStub{err: paycores.ErrAdminNotConfigured}, 503, "PAYCORES_NOT_CONFIGURED"},
		{"success", "super_admin", channelsStub{data: json.RawMessage(`{"channels":[{"id":"stripe:default"}],"totalWeight":1}`)}, 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var identity *sessionauth.AuthenticatedIdentity
			if tc.role != "" {
				identity = &sessionauth.AuthenticatedIdentity{UserID: "admin-1", Role: tc.role}
			}
			recorder := httptest.NewRecorder()
			NewHandler(authStub{identity}, tc.reader).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/paycores/channels-overview", nil))
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var body struct {
				Code string          `json:"code"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Code != tc.code {
				t.Fatalf("body = %+v, error = %v", body, err)
			}
			if tc.want == 200 && string(body.Data) == "null" {
				t.Fatalf("lost upstream data: %s", recorder.Body.String())
			}
		})
	}
}
