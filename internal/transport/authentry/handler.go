// Package authentry 提供 Gateway 已精确分流后的本地账号 HTTP 入口。
package authentry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"ai-business-service/internal/biz/identity"
	bizmedia "ai-business-service/internal/biz/media"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

const (
	registerPath      = "/api/auth/register"
	loginPath         = "/api/auth/login"
	guestPath         = "/api/auth/guest"
	bindPath          = "/api/auth/bind"
	mePath            = "/api/auth/me"
	profilePath       = "/api/auth/me/profile"
	sessionsPath      = "/api/auth/me/sessions"
	passwordPath      = "/api/auth/me/password"
	localGooglePath   = "/api/auth/local-google"
	deleteAccountPath = "/api/auth/me"
	maxRequestBytes   = 64 << 10
)

type sessionAuthenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type entryUsecase interface {
	Register(context.Context, identity.RegisterInput) (*identity.LoginResult, error)
	LoginWithPassword(context.Context, identity.PasswordLoginInput) (*identity.LoginResult, error)
	LoginGuest(context.Context, identity.GuestLoginInput) (*identity.LoginResult, error)
	CurrentUser(context.Context, string) (*identity.User, error)
	BindGuest(context.Context, identity.BindGuestInput) (*identity.User, error)
}

type localDevelopmentLoginUsecase interface {
	LoginLocalDevelopment(context.Context, identity.LocalDevelopmentLoginInput) (*identity.LoginResult, error)
}

// bindingVerifier 负责把外部登录/验证凭据兑换成已验证的 provider + subject。
// HTTP 层绝不直接信任浏览器提交的 subject，具体实现由 OAuth 或短信验证适配器提供。
type bindingVerifier interface {
	Verify(context.Context, string, string) (identity.ExternalIdentity, error)
}

type handler struct {
	authenticator sessionAuthenticator
	usecase       entryUsecase
	verifier      bindingVerifier
	profileImages interface {
		OpenOwnedImage(context.Context, string, string) (*bizmedia.Image, []byte, error)
	}
	localGoogle *localGoogleConfig
}

type localGoogleConfig struct {
	email       string
	timezone    string
	displayName string
}

type profileUsecase interface {
	UpdateDisplayName(context.Context, string, string) (*identity.User, error)
	UpdateProfile(context.Context, string, identity.ProfileUpdate) (*identity.User, error)
}

type sessionSecurityUsecase interface {
	RevokeAllSessions(context.Context, string) error
}
type passwordSecurityUsecase interface {
	ChangePassword(context.Context, string, string, string) error
}
type accountDeletionUsecase interface {
	ChangeAccountStatus(context.Context, string, identity.AccountStatus) (*identity.User, error)
}

// NewHandler 构造账号入口 Handler；路由注册与本地开关由 Gateway 负责。
func NewHandler(authenticator sessionAuthenticator, usecase entryUsecase) http.Handler {
	return &handler{authenticator: authenticator, usecase: usecase}
}

// NewHandlerWithBinding 在账号入口上装配安全的游客绑定能力。
// verifier 为空时仍可提供登录、注册和游客入口，但绑定路由会返回服务不可用，避免降级为不安全的裸 subject。
func NewHandlerWithBinding(authenticator sessionAuthenticator, usecase entryUsecase, verifier bindingVerifier) http.Handler {
	return &handler{authenticator: authenticator, usecase: usecase, verifier: verifier}
}

// NewHandlerWithProfileImages 在资料页显式提交头像时验证素材归属。
func NewHandlerWithProfileImages(authenticator sessionAuthenticator, usecase entryUsecase, verifier bindingVerifier, images interface {
	OpenOwnedImage(context.Context, string, string) (*bizmedia.Image, []byte, error)
}) http.Handler {
	return &handler{authenticator: authenticator, usecase: usecase, verifier: verifier, profileImages: images}
}

// NewHandlerWithLocalGoogle 开启仅用于本地 Docker 联调的固定账号入口。配置由服务端
// 注入，浏览器请求不包含用户邮箱、密码或 OAuth token。
func NewHandlerWithLocalGoogle(authenticator sessionAuthenticator, usecase entryUsecase, verifier bindingVerifier, images interface {
	OpenOwnedImage(context.Context, string, string) (*bizmedia.Image, []byte, error)
}, email string) http.Handler {
	return &handler{
		authenticator: authenticator,
		usecase:       usecase,
		verifier:      verifier,
		profileImages: images,
		localGoogle:   &localGoogleConfig{email: strings.TrimSpace(email), timezone: "Asia/Shanghai", displayName: "winjayzwj"},
	}
}

// ServeHTTP 仅处理四条精确账号路由。Gateway 接管后不允许重放 Node，避免注册或登录
// 在两个用户域产生不一致状态。
func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || handler.usecase == nil || request == nil || request.URL == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == registerPath:
		handler.register(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == loginPath:
		handler.login(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == localGooglePath:
		handler.localGoogleLogin(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == guestPath:
		handler.guest(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == bindPath:
		handler.bind(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == mePath:
		handler.me(writer, request)
	case request.Method == http.MethodPatch && request.URL.Path == profilePath:
		handler.profile(writer, request)
	case request.Method == http.MethodDelete && request.URL.Path == sessionsPath:
		handler.revokeSessions(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == passwordPath:
		handler.changePassword(writer, request)
	case request.Method == http.MethodDelete && request.URL.Path == deleteAccountPath:
		handler.deleteAccount(writer, request)
	default:
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
	}
}

func (handler *handler) localGoogleLogin(writer http.ResponseWriter, request *http.Request) {
	if handler.localGoogle == nil || strings.TrimSpace(handler.localGoogle.email) == "" || requestBodyHasData(request) {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	usecase, ok := handler.usecase.(localDevelopmentLoginUsecase)
	if !ok {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	result, err := usecase.LoginLocalDevelopment(request.Context(), identity.LocalDevelopmentLoginInput{
		Email:       handler.localGoogle.email,
		Timezone:    handler.localGoogle.timezone,
		DisplayName: handler.localGoogle.displayName,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeLoginSuccess(writer, http.StatusOK, result)
}

// deleteAccount 只作用于当前会话用户。领域层会以事务写入 deleted 状态并撤销全部会话，
// 因此 HTTP 层既不接受用户 ID，也不执行物理删除或钱包操作。
func (handler *handler) deleteAccount(writer http.ResponseWriter, request *http.Request) {
	authenticated, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if requestBodyHasData(request) {
		writeError(writer, shared.ErrInvalidRequest)
		return
	}
	usecase, ok := handler.usecase.(accountDeletionUsecase)
	if !ok {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	if _, err := usecase.ChangeAccountStatus(request.Context(), authenticated.UserID, identity.AccountStatusDeleted); err != nil {
		writeError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"deleted": true})
}

// requestBodyHasData 同时覆盖已声明长度和 HTTP 分块传输。只读取一个字节即可判断，
// 避免为了拒绝非法注销请求而读取任意大的请求体。
func requestBodyHasData(request *http.Request) bool {
	if request == nil || request.ContentLength > 0 {
		return request != nil
	}
	if request.Body == nil || request.Body == http.NoBody {
		return false
	}
	content, err := io.ReadAll(io.LimitReader(request.Body, 1))
	return err != nil || len(content) > 0
}

func (handler *handler) changePassword(writer http.ResponseWriter, request *http.Request) {
	authenticated, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	usecase, ok := handler.usecase.(passwordSecurityUsecase)
	if !ok {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := decodeStrictJSON(request, &body); err != nil {
		writeError(writer, shared.ErrInvalidRequest)
		return
	}
	if err := usecase.ChangePassword(request.Context(), authenticated.UserID, body.CurrentPassword, body.NewPassword); err != nil {
		writeError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"changed": true})
}

func (handler *handler) revokeSessions(writer http.ResponseWriter, request *http.Request) {
	authenticated, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	usecase, ok := handler.usecase.(sessionSecurityUsecase)
	if !ok {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	if err := usecase.RevokeAllSessions(request.Context(), authenticated.UserID); err != nil {
		writeError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"revoked": true})
}

func (handler *handler) authenticate(request *http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if handler.authenticator == nil {
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

func (handler *handler) profile(writer http.ResponseWriter, request *http.Request) {
	if handler.authenticator == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	authenticated, err := handler.authenticator.Authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	usecase, ok := handler.usecase.(profileUsecase)
	if !ok || authenticated == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	var body struct {
		DisplayName   string  `json:"displayName"`
		Bio           *string `json:"bio"`
		AvatarImageID *string `json:"avatarImageId"`
	}
	if err := decodeStrictJSON(request, &body); err != nil {
		writeError(writer, shared.ErrInvalidRequest)
		return
	}
	// 旧前端只提交 displayName 时保持“只改昵称”语义；只有显式带 bio 才更新简介。
	var user *identity.User
	if body.Bio == nil {
		user, err = usecase.UpdateDisplayName(request.Context(), authenticated.UserID, body.DisplayName)
	} else {
		if body.AvatarImageID != nil {
			if handler.profileImages == nil {
				writeError(writer, shared.ErrServiceUnavailable)
				return
			}
			image, _, imageErr := handler.profileImages.OpenOwnedImage(request.Context(), authenticated.UserID, *body.AvatarImageID)
			if imageErr != nil || image == nil {
				writeError(writer, shared.ErrInvalidRequest)
				return
			}
		}
		user, err = usecase.UpdateProfile(request.Context(), authenticated.UserID, identity.ProfileUpdate{DisplayName: body.DisplayName, Bio: *body.Bio, AvatarImageID: body.AvatarImageID})
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"user": safeUser(user)})
}

func (handler *handler) bind(writer http.ResponseWriter, request *http.Request) {
	if handler.authenticator == nil || handler.verifier == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	authenticated, err := handler.authenticator.Authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if authenticated == nil || strings.TrimSpace(authenticated.UserID) == "" {
		writeError(writer, shared.ErrUnauthenticated)
		return
	}
	var body struct {
		Provider   string `json:"provider"`
		Credential string `json:"credential"`
	}
	if err := decodeStrictJSON(request, &body); err != nil {
		writeError(writer, shared.ErrInvalidRequest)
		return
	}
	verified, err := handler.verifier.Verify(request.Context(), strings.TrimSpace(body.Provider), strings.TrimSpace(body.Credential))
	if err != nil {
		writeError(writer, err)
		return
	}
	if strings.TrimSpace(verified.Provider) == "" || strings.TrimSpace(verified.Subject) == "" {
		writeError(writer, shared.ErrInvalidRequest)
		return
	}
	user, err := handler.usecase.BindGuest(request.Context(), identity.BindGuestInput{UserID: authenticated.UserID, Provider: verified.Provider, Subject: verified.Subject})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"user": safeUser(user)})
}

func (handler *handler) register(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		Timezone    string `json:"timezone"`
		DisplayName string `json:"displayName,omitempty"`
	}
	if err := decodeStrictJSON(request, &body); err != nil {
		writeError(writer, shared.ErrInvalidRequest)
		return
	}
	result, err := handler.usecase.Register(request.Context(), identity.RegisterInput{Email: body.Email, Password: body.Password, Timezone: body.Timezone, DisplayName: body.DisplayName})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeLoginSuccess(writer, http.StatusCreated, result)
}

func (handler *handler) login(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeStrictJSON(request, &body); err != nil {
		writeError(writer, shared.ErrInvalidRequest)
		return
	}
	result, err := handler.usecase.LoginWithPassword(request.Context(), identity.PasswordLoginInput{Email: body.Email, Password: body.Password})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeLoginSuccess(writer, http.StatusOK, result)
}

func (handler *handler) guest(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Platform string `json:"platform"`
		DeviceID string `json:"deviceId"`
		Timezone string `json:"timezone"`
	}
	if err := decodeStrictJSON(request, &body); err != nil {
		writeError(writer, shared.ErrInvalidRequest)
		return
	}
	result, err := handler.usecase.LoginGuest(request.Context(), identity.GuestLoginInput{Platform: body.Platform, DeviceID: body.DeviceID, Timezone: body.Timezone})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeLoginSuccess(writer, http.StatusOK, result)
}

func (handler *handler) me(writer http.ResponseWriter, request *http.Request) {
	if handler.authenticator == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	authenticated, err := handler.authenticator.Authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if authenticated == nil || strings.TrimSpace(authenticated.UserID) == "" {
		writeError(writer, shared.ErrUnauthenticated)
		return
	}
	user, err := handler.usecase.CurrentUser(request.Context(), authenticated.UserID)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"user": safeUser(user)})
}

func decodeStrictJSON(request *http.Request, target any) error {
	if request == nil || request.Body == nil {
		return errors.New("request body is required")
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxRequestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

func writeLoginSuccess(writer http.ResponseWriter, status int, result *identity.LoginResult) {
	if result == nil || result.User == nil || result.Session == nil || result.Session.ID == "" {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	writeSuccess(writer, status, map[string]any{"token": result.Session.ID, "user": safeUser(result.User)})
}

// safeUser 是账号入口唯一允许输出的用户投影，显式排除凭据、余额和会话内部事实。
func safeUser(user *identity.User) map[string]any {
	if user == nil {
		return nil
	}
	return map[string]any{"id": user.ID, "displayName": user.DisplayName, "bio": user.Bio, "avatarImageId": user.AvatarImageID, "bindingState": user.BindingState, "accountStatus": user.AccountStatus, "contentAccess": user.ContentAccess, "role": user.Role, "timezone": user.Timezone, "isGuest": user.BindingState == identity.BindingStateGuest}
}

func writeSuccess(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
}

func writeError(writer http.ResponseWriter, err error) {
	var apiError *shared.APIError
	if errors.As(err, &apiError) {
		writeClientError(writer, apiError.StatusCode(), apiError.Code(), apiError.Message())
		return
	}
	switch {
	case errors.Is(err, identity.ErrInvalidAuthEntryInput):
		writeClientError(writer, shared.ErrInvalidRequest.StatusCode(), shared.ErrInvalidRequest.Code(), shared.ErrInvalidRequest.Message())
	case errors.Is(err, identity.ErrEmailAlreadyRegistered):
		writeClientError(writer, shared.ErrEmailAlreadyRegistered.StatusCode(), shared.ErrEmailAlreadyRegistered.Code(), shared.ErrEmailAlreadyRegistered.Message())
	case errors.Is(err, identity.ErrInvalidCredentials):
		writeClientError(writer, shared.ErrInvalidCredentials.StatusCode(), shared.ErrInvalidCredentials.Code(), shared.ErrInvalidCredentials.Message())
	case errors.Is(err, identity.ErrWebGuestLoginDisabled):
		writeClientError(writer, shared.ErrWebGuestLoginDisabled.StatusCode(), shared.ErrWebGuestLoginDisabled.Code(), shared.ErrWebGuestLoginDisabled.Message())
	case errors.Is(err, identity.ErrGuestBindingNotAllowed):
		writeClientError(writer, http.StatusForbidden, "GUEST_BINDING_NOT_ALLOWED", "当前账号不可绑定")
	case errors.Is(err, identity.ErrIdentityAlreadyBound):
		writeClientError(writer, http.StatusConflict, "IDENTITY_ALREADY_BOUND", "该身份已绑定其他账号")
	case errors.Is(err, identity.ErrInvalidBindingInput):
		writeClientError(writer, shared.ErrInvalidRequest.StatusCode(), shared.ErrInvalidRequest.Code(), shared.ErrInvalidRequest.Message())
	default:
		writeClientError(writer, shared.ErrServiceUnavailable.StatusCode(), shared.ErrServiceUnavailable.Code(), shared.ErrServiceUnavailable.Message())
	}
}

func writeClientError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

var _ http.Handler = (*handler)(nil)
