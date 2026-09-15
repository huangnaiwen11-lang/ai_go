package video

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
	bizvideo "ai-business-service/internal/biz/video"
	"ai-business-service/internal/transport/sessionauth"
)

// Handler 必须用认证后的用户和内容访问级别创建视频模板任务，并保持现网成功信封字段。
func TestHandler创建视频模板返回兼容任务信封(t *testing.T) {
	creator := &recordingCreator{result: &creations.CreateReservedResult{
		Creation:            &creations.Creation{ID: "550e8400-e29b-41d4-a716-446655440201"},
		Reservation:         &ledger.Reservation{PriceDiamonds: 50},
		DiamondBalanceAfter: 150,
	}}
	handler := NewHandler(staticAuthenticator{userID: "user-1", contentAccess: "standard"}, creator, staticStatusReader{})
	request := httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"template-1","imageUrl":"https://uploads.example.test/source.png","durationSeconds":5}`))
	request.Header.Set("X-Request-Id", "550e8400-e29b-41d4-a716-446655440202")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || creator.command.UserID != "user-1" || creator.command.ContentAccess != "standard" || creator.command.IdempotencyKey != "gateway:video:image:550e8400-e29b-41d4-a716-446655440202" || creator.command.Input.UserImageURL == "" {
		t.Fatalf("创建响应或命令错误：status=%d command=%#v", recorder.Code, creator.command)
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			TaskID          string `json:"taskId"`
			Status          string `json:"status"`
			CoinsUsed       int64  `json:"coinsUsed"`
			CoinsRemaining  int64  `json:"coinsRemaining"`
			DurationSeconds int32  `json:"durationSeconds"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success || response.Data.TaskID == "" || response.Data.Status != "generating" || response.Data.CoinsUsed != 50 || response.Data.CoinsRemaining != 150 || response.Data.DurationSeconds != 5 {
		t.Fatalf("响应信封 = %s", recorder.Body.String())
	}
}

// 三类门禁错误必须保留不同 HTTP 状态与错误码，不能收敛成笼统的“不能生成”。
func TestHandler保留绑定VIP和余额不足的独立错误码(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "需要绑定", err: shared.ErrAccountBindingRequired, wantStatus: 403, wantCode: "ACCOUNT_BINDING_REQUIRED"},
		{name: "需要 VIP", err: shared.ErrVIPRequired, wantStatus: 403, wantCode: "VIP_REQUIRED"},
		{name: "余额不足", err: shared.ErrInsufficientFunds, wantStatus: 402, wantCode: "INSUFFICIENT_FUNDS"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			handler := NewHandler(staticAuthenticator{userID: "user-1", contentAccess: "standard"}, &recordingCreator{err: testCase.err}, staticStatusReader{})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"template-1","prompt":"海边漫步"}`)))
			var response struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(recorder.Body.Bytes(), &response)
			if recorder.Code != testCase.wantStatus || response.Code != testCase.wantCode {
				t.Fatalf("status/code = %d/%q, want %d/%q", recorder.Code, response.Code, testCase.wantStatus, testCase.wantCode)
			}
		})
	}
}

// 视频与图片创作共用同一预扣幂等语义；相同键的不同请求不能被误报为可修正的参数错误。
func TestHandler幂等键请求指纹冲突返回409(t *testing.T) {
	handler := NewHandler(
		staticAuthenticator{userID: "user-1", contentAccess: "standard"},
		&recordingCreator{err: creations.ErrCreationCommandConflict},
		staticStatusReader{},
	)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, createPath, strings.NewReader(`{"templateId":"template-1","prompt":"海边漫步"}`)))

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

// 读取状态必须把 video 业务视图按现网字段回写，并且不将内部配方或资产事实泄露给客户端。
func TestHandler读取视频状态(t *testing.T) {
	statuses := staticStatusReader{view: &bizvideo.VideoView{TaskID: "550e8400-e29b-41d4-a716-446655440203", Status: bizvideo.GenerationStatusCompleted, DurationSeconds: 10, VideoURL: "https://assets.example.test/final.mp4"}}
	handler := NewHandler(staticAuthenticator{userID: "user-1", contentAccess: "standard"}, &recordingCreator{}, statuses)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/chat/video/550e8400-e29b-41d4-a716-446655440203", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"videoUrl":"https://assets.example.test/final.mp4"`) || !strings.Contains(recorder.Body.String(), `"durationSeconds":10`) {
		t.Fatalf("状态响应 = %d %s", recorder.Code, recorder.Body.String())
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

type recordingCreator struct {
	command bizvideo.CreateCommand
	result  *creations.CreateReservedResult
	err     error
}

func (creator *recordingCreator) Create(_ context.Context, command bizvideo.CreateCommand) (*creations.CreateReservedResult, error) {
	creator.command = command
	return creator.result, creator.err
}

type staticStatusReader struct {
	view *bizvideo.VideoView
	err  error
}

func (reader staticStatusReader) List(context.Context, string, []string) ([]bizvideo.VideoView, error) {
	if reader.view == nil {
		return nil, reader.err
	}
	return []bizvideo.VideoView{*reader.view}, reader.err
}

func (reader staticStatusReader) Get(context.Context, string, string) (*bizvideo.VideoView, error) {
	return reader.view, reader.err
}
