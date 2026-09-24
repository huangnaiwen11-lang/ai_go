package t2i

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/shared"
	bizt2i "ai-business-service/internal/biz/t2i"
	"ai-business-service/internal/transport/sessionauth"
)

func TestHandler创建返回兼容图片任务信封(t *testing.T) {
	creator := &recordingCreator{result: &creations.CreateReservedResult{
		Creation:            &creations.Creation{ID: "550e8400-e29b-41d4-a716-446655440000"},
		Steps:               []creations.CreationStep{{ID: "550e8400-e29b-41d4-a716-446655440001"}},
		Reservation:         &ledger.Reservation{PriceDiamonds: 20, Source: ledger.BenefitSourceDiamonds, ChargedDiamonds: 20},
		DiamondBalanceAfter: 80,
	}}
	handler := NewHandler(staticAuthenticator{userID: "user-1"}, creator, staticStatuses{})
	request := httptest.NewRequest(http.MethodPost, "/api/chat/image/async", strings.NewReader(`{"prompt":"portrait","negativePrompt":"blur","aspectRatio":"1:1"}`))
	request.Header.Set("X-Request-Id", "550e8400-e29b-41d4-a716-446655440002")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || creator.command.UserID != "user-1" || creator.command.IdempotencyKey != "gateway:t2i:550e8400-e29b-41d4-a716-446655440002" {
		t.Fatalf("创建响应或命令错误：status=%d command=%#v", recorder.Code, creator.command)
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			ImageID        string `json:"imageId"`
			JobID          string `json:"jobId"`
			Status         string `json:"status"`
			Cost           int64  `json:"cost"`
			CoinsCharged   int64  `json:"coinsCharged"`
			CoinsRemaining int64  `json:"coinsRemaining"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success || response.Data.ImageID == "" || response.Data.JobID == "" || response.Data.Status != "generating" || response.Data.Cost != 20 || response.Data.CoinsCharged != 20 || response.Data.CoinsRemaining != 80 {
		t.Fatalf("响应信封 = %s", recorder.Body.String())
	}
}

func TestHandler接受前端严格Go候选的兼容字段(t *testing.T) {
	creator := &recordingCreator{result: &creations.CreateReservedResult{
		Creation:    &creations.Creation{ID: "550e8400-e29b-41d4-a716-446655440101"},
		Steps:       []creations.CreationStep{{ID: "550e8400-e29b-41d4-a716-446655440102"}},
		Reservation: &ledger.Reservation{PriceDiamonds: 20, Source: ledger.BenefitSourceDailyQuota, ChargedDiamonds: 0},
	}}
	handler := NewHandler(staticAuthenticator{userID: "user-1"}, creator, staticStatuses{})
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"prompt":"portrait","aspectRatio":"1:1","operation":"generate","presetTags":[],"optimizePrompt":false}`))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || !creator.called || creator.command.Prompt != "portrait" || creator.command.AspectRatio != "1:1" {
		t.Fatalf("前端兼容字段不应阻止 T2I 创建：status=%d command=%#v", recorder.Code, creator.command)
	}
}

func TestHandler无效会话不进入业务用例(t *testing.T) {
	creator := &recordingCreator{}
	handler := NewHandler(staticAuthenticator{err: shared.ErrUnauthenticated}, creator, staticStatuses{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/chat/image/async", strings.NewReader(`{"prompt":"portrait"}`)))
	if recorder.Code != http.StatusUnauthorized || creator.called {
		t.Fatalf("无效会话 = status:%d creator:%t", recorder.Code, creator.called)
	}
}

// 同一幂等键承载不同请求时，客户端必须获得可区分的冲突响应，
// 不能误报为普通参数错误；这样调用方才不会把冲突重试为一笔新创建。
func TestHandler幂等键请求指纹冲突返回409(t *testing.T) {
	creator := &recordingCreator{err: creations.ErrCreationCommandConflict}
	handler := NewHandler(staticAuthenticator{userID: "user-1"}, creator, staticStatuses{})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"prompt":"portrait"}`)))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("幂等冲突响应状态 = %d，期望 %d；body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("幂等冲突 Content-Type = %q，期望 application/json", contentType)
	}
	var response struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Details any    `json:"details"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析幂等冲突响应：%v", err)
	}
	if response.Success || response.Code != "IDEMPOTENCY_CONFLICT" || response.Message != "Idempotency conflict" || response.Details != nil {
		t.Fatalf("幂等冲突根层信封错误：%s", recorder.Body.String())
	}
}

func TestWriteError保留Admission503原因码(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		code string
	}{
		{name: "依赖不可用", err: creations.ErrAdmissionDependencyUnavailable, code: "SERVICE_UNAVAILABLE"},
		{name: "发布配置错误", err: creations.ErrAdmissionConfigurationUnavailable, code: "GENERATION_CONFIGURATION_UNAVAILABLE"},
		{name: "产品配方缺失", err: creations.ErrB2BProductRecipeUnavailable, code: "GENERATION_RECIPE_UNAVAILABLE"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeError(recorder, testCase.err)
			var response struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != http.StatusServiceUnavailable || response.Code != testCase.code {
				t.Fatalf("status/code = %d/%q, want 503/%q", recorder.Code, response.Code, testCase.code)
			}
		})
	}
}

func TestHandler模板图编辑使用受控身份与服务端用例(t *testing.T) {
	editor := &recordingImageEditor{result: &creations.CreateReservedResult{
		Creation:    &creations.Creation{ID: "550e8400-e29b-41d4-a716-446655440010"},
		Steps:       []creations.CreationStep{{ID: "550e8400-e29b-41d4-a716-446655440011"}},
		Reservation: &ledger.Reservation{PriceDiamonds: 20, Source: ledger.BenefitSourceDiamonds, ChargedDiamonds: 20},
	}}
	handler := NewHandler(staticAuthenticator{userID: "user-1", contentAccess: "standard"}, &recordingCreator{}, staticStatuses{}, editor)
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"dress-up","inputImages":["https://uploads.example/source.png"],"prompt":"red dress"}`))
	request.Header.Set("X-Request-Id", "550e8400-e29b-41d4-a716-446655440012")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || editor.command.UserID != "user-1" || editor.command.ContentAccess != "standard" || editor.command.IdempotencyKey != "gateway:i2i:550e8400-e29b-41d4-a716-446655440012" || editor.command.TemplateID != "dress-up" {
		t.Fatalf("模板图编辑创建错误：status=%d command=%#v", recorder.Code, editor.command)
	}
}

func TestHandler模板图编辑接受前端冻结字段并保留画幅(t *testing.T) {
	editor := &recordingImageEditor{result: &creations.CreateReservedResult{
		Creation:    &creations.Creation{ID: "550e8400-e29b-41d4-a716-446655440020"},
		Steps:       []creations.CreationStep{{ID: "550e8400-e29b-41d4-a716-446655440021"}},
		Reservation: &ledger.Reservation{PriceDiamonds: 20, Source: ledger.BenefitSourceDiamonds, ChargedDiamonds: 20},
	}}
	handler := NewHandler(staticAuthenticator{userID: "user-1", contentAccess: "standard"}, &recordingCreator{}, staticStatuses{}, editor)
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"dress-up","inputImages":["https://uploads.example/source.png"],"prompt":"red dress","operation":"generate","optimizePrompt":false,"aspectRatio":"9:16","presetTags":["dress-up"]}`))
	request.Header.Set("X-Request-Id", "550e8400-e29b-41d4-a716-446655440022")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || editor.command.AspectRatio != "9:16" {
		t.Fatalf("前端模板编辑字段没有完整进入 Go 命令：status=%d command=%#v", recorder.Code, editor.command)
	}
}

type staticAuthenticator struct {
	userID        string
	contentAccess string
	err           error
}

func (auth staticAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if auth.err != nil {
		return nil, auth.err
	}
	return &sessionauth.AuthenticatedIdentity{UserID: auth.userID, ContentAccess: auth.contentAccess}, nil
}

type recordingImageEditor struct {
	command bizt2i.ImageEditCommand
	result  *creations.CreateReservedResult
	err     error
}

func (editor *recordingImageEditor) Create(_ context.Context, command bizt2i.ImageEditCommand) (*creations.CreateReservedResult, error) {
	editor.command = command
	return editor.result, editor.err
}

type recordingCreator struct {
	called  bool
	command bizt2i.CreateCommand
	result  *creations.CreateReservedResult
	err     error
}

func (creator *recordingCreator) Create(_ context.Context, command bizt2i.CreateCommand) (*creations.CreateReservedResult, error) {
	creator.called = true
	creator.command = command
	return creator.result, creator.err
}

type staticStatuses struct{}

func (staticStatuses) List(context.Context, string, []string) ([]bizt2i.ImageView, error) {
	return nil, nil
}
func (staticStatuses) Get(context.Context, string, string) (*bizt2i.ImageView, error) {
	return nil, bizt2i.ErrImageNotFound
}
