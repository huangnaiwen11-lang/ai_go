package media

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	bizmedia "ai-business-service/internal/biz/media"
	"ai-business-service/internal/transport/sessionauth"
)

func TestHandler已登录用户可上传经检测的PNG素材(t *testing.T) {
	usecase := &recordingMediaUsecase{image: &bizmedia.Image{ID: "media-1", OwnerID: "user-1", Reference: "https://local-media.invalid/assets/media-1", ContentType: "image/png", SizeBytes: 67}}
	handler := NewHandler(staticAuthenticator{userID: "user-1"}, usecase)
	request := imageUploadRequest(t, "portrait.png", "image/png", tinyPNG)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated || usecase.command.OwnerID != "user-1" || usecase.command.ContentType != "image/png" {
		t.Fatalf("上传结果 = status:%d command:%#v body:%s", recorder.Code, usecase.command, recorder.Body.String())
	}
}

func TestHandler拒绝未登录和伪造图片类型(t *testing.T) {
	usecase := &recordingMediaUsecase{}
	handler := NewHandler(staticAuthenticator{}, usecase)
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, imageUploadRequest(t, "portrait.png", "image/png", tinyPNG))
	if unauthenticated.Code != http.StatusUnauthorized || usecase.called {
		t.Fatalf("未登录上传 = status:%d called:%t", unauthenticated.Code, usecase.called)
	}

	handler = NewHandler(staticAuthenticator{userID: "user-1"}, usecase)
	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, imageUploadRequest(t, "fake.png", "image/png", []byte("not really an image")))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("伪造 MIME 上传状态 = %d，body=%s", invalid.Code, invalid.Body.String())
	}
}

type staticAuthenticator struct{ userID string }

func (auth staticAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if auth.userID == "" {
		return nil, bizmedia.ErrImageAccessDenied
	}
	return &sessionauth.AuthenticatedIdentity{UserID: auth.userID}, nil
}

type recordingMediaUsecase struct {
	called  bool
	command bizmedia.SaveImageCommand
	image   *bizmedia.Image
	err     error
}

func (usecase *recordingMediaUsecase) SaveImage(_ context.Context, command bizmedia.SaveImageCommand) (*bizmedia.Image, error) {
	usecase.called = true
	usecase.command = command
	return usecase.image, usecase.err
}

func (usecase *recordingMediaUsecase) OpenOwnedImage(context.Context, string, string) (*bizmedia.Image, []byte, error) {
	return nil, nil, bizmedia.ErrImageAccessDenied
}

func imageUploadRequest(t *testing.T, filename, contentType string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/media/images", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Test-Content-Type", contentType)
	return request
}

var tinyPNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52}
