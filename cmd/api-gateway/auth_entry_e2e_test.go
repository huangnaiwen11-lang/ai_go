package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"ai-business-service/internal/biz/authcredential"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/data"
	"ai-business-service/internal/gateway"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestGateway本地账号入口闭环 验证 Gateway 接管后仍只操作 Go 自有身份域。
func TestGateway本地账号入口闭环(t *testing.T) {
	uri := os.Getenv("CLING_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("未配置本机 rs0 测试连接")
	}
	bootstrap, err := loadGatewayBootstrap("../../configs/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.Data.Mongo.Uri = uri
	authenticator, cleanupAuth, err := newConfiguredSessionAuthenticator(bootstrap.GetData())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupAuth)
	handler, cleanupEntry, err := newConfiguredAuthEntryHandler(bootstrap.GetData(), bootstrap.GetSecurity(), authenticator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupEntry)

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	database := client.Database("cling_main")
	tracked := newAuthE2ECleanup(t, database)

	routes := t.TempDir() + "/routes.json"
	if err := os.WriteFile(routes, []byte(`{"routes":{"POST /api/auth/register":true,"POST /api/auth/login":true,"POST /api/auth/guest":true,"GET /api/auth/me":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 上游只是回退探针，不提供业务实现；任意误回退都必须使 Go 独立验收失败。
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("Go 账号请求不应回退 Node：%s %s", request.Method, request.URL.Path)
		writer.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := newGateway(upstreamURL, gateway.NewFileRouteSwitch(routes), 0, nil, nil, nil, handler)
	entry := proxy.Handler()

	email := "gateway-e2e-" + uuid.NewString() + "@example.test"
	registered := authE2ECall(t, entry, http.MethodPost, "/api/auth/register", `{"email":"`+email+`","password":"correct-horse","timezone":"Asia/Shanghai"}`, "")
	if registered.Code != http.StatusCreated || registered.Token == "" || registered.UserID == "" {
		t.Fatalf("注册响应 = %d %s", registered.Code, registered.Raw)
	}
	tracked.add(registered.UserID, registered.Token)
	me := authE2ECall(t, entry, http.MethodGet, "/api/auth/me", "", registered.Token)
	if me.Code != http.StatusOK || me.UserID != registered.UserID {
		t.Fatalf("当前用户响应 = %d %s", me.Code, me.Raw)
	}
	loggedIn := authE2ECall(t, entry, http.MethodPost, "/api/auth/login", `{"email":"`+email+`","password":"correct-horse"}`, "")
	if loggedIn.Code != http.StatusOK || loggedIn.UserID != registered.UserID || loggedIn.Token == "" {
		t.Fatalf("密码登录响应 = %d %s", loggedIn.Code, loggedIn.Raw)
	}
	tracked.add(registered.UserID, loggedIn.Token)

	// 状态变更必须经身份领域用例完成，确保用户版本递增与全部活动会话撤销在同一事务中发生。
	statusData, closeStatusData, err := data.NewData(bootstrap.GetData())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeStatusData)
	statusUsecase := identity.NewUsecase(
		data.NewUserRepository(statusData), data.NewIdentityRepository(statusData),
		data.NewSessionRepository(statusData), data.NewTxRunner(statusData),
	)
	if _, err := statusUsecase.ChangeAccountStatus(context.Background(), registered.UserID, identity.AccountStatusBanned); err != nil {
		t.Fatalf("封禁本地测试账号：%v", err)
	}
	for _, token := range []string{registered.Token, loggedIn.Token} {
		invalidated := authE2ECall(t, entry, http.MethodGet, "/api/auth/me", "", token)
		if invalidated.Code != http.StatusUnauthorized {
			t.Fatalf("封禁后旧会话仍有效：%d %s", invalidated.Code, invalidated.Raw)
		}
	}

	deletedEmail := "gateway-deleted-" + uuid.NewString() + "@example.test"
	deleted := authE2ECall(t, entry, http.MethodPost, "/api/auth/register", `{"email":"`+deletedEmail+`","password":"correct-horse","timezone":"UTC"}`, "")
	if deleted.Code != http.StatusCreated || deleted.UserID == "" {
		t.Fatalf("待删除账号注册失败：%d %s", deleted.Code, deleted.Raw)
	}
	tracked.add(deleted.UserID, deleted.Token)
	policy, err := authcredential.NewPolicy(authcredential.Params{
		MemoryKiB: bootstrap.GetSecurity().GetPasswordMemoryKib(), TimeCost: bootstrap.GetSecurity().GetPasswordTimeCost(),
		Parallelism: uint8(bootstrap.GetSecurity().GetPasswordParallelism()), SaltBytes: bootstrap.GetSecurity().GetPasswordSaltBytes(), KeyBytes: bootstrap.GetSecurity().GetPasswordKeyBytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	deleteUsecase := identity.NewAuthUsecase(
		data.NewAuthUserRepository(statusData), data.NewIdentityRepository(statusData), data.NewAuthSessionRepository(statusData),
		data.NewAccountRepository(statusData), data.NewCredentialRepository(statusData), policy, data.NewTxRunner(statusData),
	)
	if _, err := deleteUsecase.ChangeAccountStatus(context.Background(), deleted.UserID, identity.AccountStatusDeleted); err != nil {
		t.Fatalf("删除本地测试账号：%v", err)
	}
	if invalidated := authE2ECall(t, entry, http.MethodGet, "/api/auth/me", "", deleted.Token); invalidated.Code != http.StatusUnauthorized {
		t.Fatalf("删除后旧会话仍有效：%d %s", invalidated.Code, invalidated.Raw)
	}
	reRegistered := authE2ECall(t, entry, http.MethodPost, "/api/auth/register", `{"email":"`+deletedEmail+`","password":"correct-horse","timezone":"UTC"}`, "")
	if reRegistered.Code != http.StatusCreated || reRegistered.UserID == deleted.UserID || reRegistered.UserID == "" {
		t.Fatalf("删除邮箱重新注册未创建新用户：%d %s", reRegistered.Code, reRegistered.Raw)
	}
	tracked.add(reRegistered.UserID, reRegistered.Token)

	deviceID := "ios-device-" + uuid.NewString()
	guestFirst := authE2ECall(t, entry, http.MethodPost, "/api/auth/guest", `{"platform":"ios","deviceId":"`+deviceID+`","timezone":"Asia/Shanghai"}`, "")
	guestSecond := authE2ECall(t, entry, http.MethodPost, "/api/auth/guest", `{"platform":"ios","deviceId":"`+deviceID+`","timezone":"UTC"}`, "")
	if guestFirst.Code != http.StatusOK || guestSecond.Code != http.StatusOK || guestFirst.UserID == "" || guestFirst.UserID != guestSecond.UserID {
		t.Fatalf("移动端游客复用失败：%s / %s", guestFirst.Raw, guestSecond.Raw)
	}
	tracked.add(guestFirst.UserID, guestFirst.Token, guestSecond.Token)
	webGuest := authE2ECall(t, entry, http.MethodPost, "/api/auth/guest", `{"platform":"web","deviceId":"web-device-`+uuid.NewString()+`","timezone":"UTC"}`, "")
	if webGuest.Code != http.StatusForbidden || !bytes.Contains([]byte(webGuest.Raw), []byte("WEB_GUEST_LOGIN_DISABLED")) {
		t.Fatalf("Web 游客入口应拒绝：%d %s", webGuest.Code, webGuest.Raw)
	}
}

type authE2EResponse struct {
	Code               int
	Token, UserID, Raw string
}

func authE2ECall(t *testing.T, handler http.Handler, method, path, body, token string) authE2EResponse {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	result := authE2EResponse{Code: recorder.Code, Raw: recorder.Body.String()}
	var payload struct {
		Data struct {
			Token string `json:"token"`
			User  struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	result.Token, result.UserID = payload.Data.Token, payload.Data.User.ID
	return result
}

type authE2ECleanup struct {
	t        *testing.T
	database *mongo.Database
	users    map[string][]string
}

func newAuthE2ECleanup(t *testing.T, database *mongo.Database) *authE2ECleanup {
	cleanup := &authE2ECleanup{t: t, database: database, users: map[string][]string{}}
	t.Cleanup(cleanup.run)
	return cleanup
}
func (cleanup *authE2ECleanup) add(userID string, tokens ...string) {
	cleanup.users[userID] = append(cleanup.users[userID], tokens...)
}
func (cleanup *authE2ECleanup) run() {
	for userID, tokens := range cleanup.users {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var credential struct {
			ID string `bson:"_id"`
		}
		_ = cleanup.database.Collection("credentials").FindOne(ctx, bson.D{{Key: "user_id", Value: userID}}).Decode(&credential)
		if credential.ID != "" {
			_, _ = cleanup.database.Collection("credentials").DeleteOne(ctx, bson.D{{Key: "_id", Value: credential.ID}})
		}
		for _, token := range tokens {
			if token != "" {
				_, _ = cleanup.database.Collection("sessions").DeleteOne(ctx, bson.D{{Key: "_id", Value: token}})
			}
		}
		_, _ = cleanup.database.Collection("accounts").DeleteOne(ctx, bson.D{{Key: "_id", Value: userID}})
		_, _ = cleanup.database.Collection("users").DeleteOne(ctx, bson.D{{Key: "_id", Value: userID}})
		cancel()
	}
}
