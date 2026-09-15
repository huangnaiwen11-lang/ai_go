// Package media 提供 Go 自有用户图片素材的最小 HTTP 合同。
package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	bizmedia "ai-business-service/internal/biz/media"
	"ai-business-service/internal/transport/sessionauth"
)

const (
	uploadImagePath = "/api/media/images"
	imagePathPrefix = "/api/media/images/"
	maxUploadBody   = bizmedia.MaxImageBytes + (1 << 20)
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type usecase interface {
	SaveImage(context.Context, bizmedia.SaveImageCommand) (*bizmedia.Image, error)
	OpenOwnedImage(context.Context, string, string) (*bizmedia.Image, []byte, error)
}

// Handler 不注册监听地址，仅供 Gateway 对经审核的精确媒体路由调用。
type Handler struct {
	authenticator authenticator
	usecase       usecase
}

func NewHandler(authenticator authenticator, usecase usecase) http.Handler {
	return &Handler{authenticator: authenticator, usecase: usecase}
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "登录已失效，请重新登录")
		return
	}
	if request == nil || request.URL == nil {
		writeError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == uploadImagePath:
		handler.uploadImage(writer, request, identity.UserID)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, imagePathPrefix):
		handler.readImage(writer, request, identity.UserID)
	default:
		writeError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
	}
}

func (handler *Handler) authenticate(request *http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if handler == nil || handler.authenticator == nil || handler.usecase == nil {
		return nil, errors.New("media handler is not configured")
	}
	identity, err := handler.authenticator.Authenticate(request)
	if err != nil || identity == nil || strings.TrimSpace(identity.UserID) == "" {
		return nil, errors.New("unauthenticated")
	}
	return identity, nil
}

func (handler *Handler) uploadImage(writer http.ResponseWriter, request *http.Request, userID string) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxUploadBody)
	if err := request.ParseMultipartForm(maxUploadBody); err != nil {
		writeError(writer, http.StatusRequestEntityTooLarge, "IMAGE_TOO_LARGE", "图片文件超过大小限制")
		return
	}
	file, header, err := request.FormFile("file")
	if err != nil || header == nil || strings.TrimSpace(header.Filename) == "" {
		writeError(writer, http.StatusBadRequest, "INVALID_IMAGE", "请选择图片文件")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, bizmedia.MaxImageBytes+1))
	if err != nil || len(content) == 0 || len(content) > bizmedia.MaxImageBytes {
		writeError(writer, http.StatusBadRequest, "INVALID_IMAGE", "图片文件无效")
		return
	}
	// 以文件字节检测 MIME，不信任浏览器可伪造的 multipart Content-Type。
	contentType := http.DetectContentType(content)
	if !isSupportedImageContentType(contentType) {
		writeError(writer, http.StatusBadRequest, "INVALID_IMAGE", "仅支持 JPG、PNG、WebP 或 GIF 图片")
		return
	}
	image, err := handler.usecase.SaveImage(request.Context(), bizmedia.SaveImageCommand{
		OwnerID: userID, Filename: header.Filename, ContentType: contentType, Content: content,
	})
	if err != nil || image == nil {
		writeError(writer, http.StatusBadRequest, "INVALID_IMAGE", "图片文件无效")
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"success": true, "data": map[string]any{
		"id": image.ID, "reference": image.Reference, "contentType": image.ContentType, "sizeBytes": image.SizeBytes,
		"downloadUrl": imagePathPrefix + image.ID,
	}})
}

func (handler *Handler) readImage(writer http.ResponseWriter, request *http.Request, userID string) {
	imageID := strings.TrimPrefix(request.URL.Path, imagePathPrefix)
	if imageID == "" || strings.Contains(imageID, "/") {
		writeError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	image, content, err := handler.usecase.OpenOwnedImage(request.Context(), userID, imageID)
	if err != nil || image == nil {
		writeError(writer, http.StatusForbidden, "MEDIA_ACCESS_DENIED", "无权访问该素材")
		return
	}
	writer.Header().Set("Content-Type", image.ContentType)
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(content)
}

func isSupportedImageContentType(contentType string) bool {
	return contentType == "image/jpeg" || contentType == "image/png" || contentType == "image/webp" || contentType == "image/gif"
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

var _ http.Handler = (*Handler)(nil)
