// Package r2media implements the frozen legacy R2 read contracts. It only
// adapts the reviewed R2 client; it never exposes a private bucket URL.
package r2media

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"ai-business-service/internal/integrations/r2"
)

const (
	objectPath = "/api/v1/media/object"
	imagePath  = "/api/media/image"
	// refreshPath 是**前端实际请求**的路径：前端 API 基址是 `/api`，调用实参是
	// `/media/refresh-download-link`（`ai-frontend/src/utils/download.ts`），
	// 拼起来即本常量。旧站曾双挂载 `/api/media/...` 与 `/api/v1/media/...`。
	refreshPath = "/api/media/refresh-download-link"
	// legacyRefreshPath 是历史拼写，保留为别名，**不能在 route-switch 仍带旧键时删掉**：
	// 该开关是 fail-closed 的（JSON 里出现代码不认识的键 → 整份失效 → 全部受约束路由
	// 回退 Node，即 502）。退役顺序必须是「代码先同时认两种 → 再改 JSON → 最后才删代码旧键」。
	legacyRefreshPath = "/media/refresh-download-link"

	defaultSignedExpiry = time.Hour
	maxKeyLength        = 1024

	maxImageSourceBytes     = 25 << 20
	maxImageInputPixels     = 40_000_000
	maxImageOutputDimension = 4096
	immutableMediaCache     = "public, max-age=31536000, immutable"
)

var privatePrefixes = []string{"ugc/", "images/", "videos/", "animate/", "animate-thumb/", "double-action/", "ai-body/", "agents/", "sex-pose/"}
var imageWidths = []int{240, 320, 480, 640, 800, 1080, 1440}

type objectStore interface {
	PrivateBucketName() string
	PublicBucketName() string
	PresignGet(context.Context, r2.PresignGetInput) (string, error)
	Get(context.Context, r2.GetInput) (*r2.Object, error)
	Head(context.Context, string, string) (r2.ObjectInfo, error)
	Put(context.Context, r2.PutInput) (r2.ObjectInfo, error)
}

// Config contains only the frozen proxy controls. Secrets must arrive from
// process configuration and never from an HTTP request.
type Config struct {
	ProxySecret          string
	PublicURL            string
	Redirect             bool
	AttachmentRedirect   bool
	SharedRedirectCache  bool
	RedirectCacheControl string
	Expires              time.Duration
}

type Handler struct {
	objects objectStore
	config  Config
}

func NewHandler(objects objectStore, config Config) (*Handler, error) {
	if objects == nil || strings.TrimSpace(config.ProxySecret) == "" || strings.TrimSpace(config.PublicURL) == "" || config.Expires <= 0 || config.Expires > 7*24*time.Hour {
		return nil, errors.New("r2 media handler configuration is invalid")
	}
	parsed, err := url.Parse(config.PublicURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil || parsed.String() != config.PublicURL {
		return nil, errors.New("r2 media public url is invalid")
	}
	if strings.TrimSpace(objects.PrivateBucketName()) == "" {
		return nil, errors.New("r2 private bucket is required for frozen media routes")
	}
	return &Handler{objects: objects, config: config}, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || request == nil || request.URL == nil {
		notFound(writer)
		return
	}
	switch {
	case request.URL.Path == objectPath:
		handler.object(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == imagePath:
		handler.image(writer, request)
	case request.Method == http.MethodPost && (request.URL.Path == refreshPath || request.URL.Path == legacyRefreshPath):
		handler.refresh(writer, request)
	default:
		notFound(writer)
	}
}

func (handler *Handler) object(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodOptions {
		applyCORS(writer)
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		notFound(writer)
		return
	}
	applyCORS(writer)
	key, ok := handler.validateObjectRequest(request.URL.Query())
	if !ok {
		notFound(writer)
		return
	}
	disposition := ""
	if request.URL.Query().Get("dl") == "1" {
		disposition = attachmentDisposition(request.URL.Query().Get("fn"))
	}
	redirectAllowed := handler.config.Redirect && (!isAndroidWebView(request.UserAgent()) || disposition == "") && (disposition == "" || handler.config.AttachmentRedirect)
	if redirectAllowed {
		signed, err := handler.objects.PresignGet(request.Context(), r2.PresignGetInput{Bucket: handler.objects.PrivateBucketName(), Key: key, Expires: handler.config.Expires, ResponseContentDisposition: disposition})
		if err != nil || signed == "" {
			notFound(writer)
			return
		}
		if disposition != "" {
			writer.Header().Set("Cache-Control", "private, no-store")
			writer.Header().Set("X-Signed-Url-Cache", "bypass")
		} else if handler.config.SharedRedirectCache {
			writer.Header().Set("Cache-Control", handler.config.RedirectCacheControl)
			writer.Header().Set("X-Signed-Url-Cache", "miss")
		} else {
			writer.Header().Set("Cache-Control", "private, max-age=3600")
			writer.Header().Set("X-Signed-Url-Cache", "miss")
		}
		http.Redirect(writer, request, signed, http.StatusFound)
		return
	}
	object, err := handler.objects.Get(request.Context(), r2.GetInput{Bucket: handler.objects.PrivateBucketName(), Key: key, Range: request.Header.Get("Range")})
	if err != nil || object == nil || !object.Exists || object.Body == nil {
		notFound(writer)
		return
	}
	defer object.Body.Close()
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Type", contentTypeOrBinary(object.ContentType))
	writer.Header().Set("Cache-Control", "private, max-age=3600")
	if object.ETag != "" {
		writer.Header().Set("ETag", object.ETag)
	}
	if !object.LastModified.IsZero() {
		writer.Header().Set("Last-Modified", object.LastModified.UTC().Format(http.TimeFormat))
	}
	if disposition != "" {
		writer.Header().Set("Content-Disposition", disposition)
	}
	status := http.StatusOK
	if object.ContentRange != "" {
		writer.Header().Set("Content-Range", object.ContentRange)
		status = http.StatusPartialContent
	}
	if object.ContentLength >= 0 {
		writer.Header().Set("Content-Length", strconv.FormatInt(object.ContentLength, 10))
	}
	writer.WriteHeader(status)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(writer, object.Body)
	}
}

func (handler *Handler) image(writer http.ResponseWriter, request *http.Request) {
	raw := strings.TrimSpace(request.URL.Query().Get("url"))
	sourceKey, ok := handler.publicObjectKey(raw)
	if !ok {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"success": false, "code": "INVALID_IMAGE", "message": "Only R2 public URLs are allowed"})
		return
	}
	options, ok := parseImageOptions(raw, request.URL.Query())
	if !ok {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"success": false, "code": "INVALID_IMAGE"})
		return
	}
	cacheKey := imageCacheObjectKey(options)
	if cached, err := handler.objects.Head(request.Context(), handler.objects.PublicBucketName(), cacheKey); err == nil && cached.Exists {
		writer.Header().Set("Cache-Control", immutableMediaCache)
		http.Redirect(writer, request, handler.config.PublicURL+"/"+cacheKey, http.StatusFound)
		return
	}

	source, err := handler.objects.Get(request.Context(), r2.GetInput{Bucket: handler.objects.PublicBucketName(), Key: sourceKey})
	if err != nil || source == nil || !source.Exists || source.Body == nil {
		writeImageUnavailable(writer)
		return
	}
	defer source.Body.Close()
	sourcePath, cleanup, err := materializeImageSource(source)
	if err != nil {
		if errors.Is(err, errImageSourceTooLarge) {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"success": false, "code": "INVALID_IMAGE"})
			return
		}
		writeImageUnavailable(writer)
		return
	}
	defer cleanup()
	output, contentType, err := transformImage(request.Context(), sourcePath, options)
	if err != nil {
		writeImageUnavailable(writer)
		return
	}
	length := int64(len(output))
	if _, err := handler.objects.Put(request.Context(), r2.PutInput{Bucket: handler.objects.PublicBucketName(), Key: cacheKey, Body: bytes.NewReader(output), ContentType: contentType, ContentLength: &length, CacheControl: immutableMediaCache}); err != nil {
		writeImageUnavailable(writer)
		return
	}
	writer.Header().Set("Cache-Control", immutableMediaCache)
	http.Redirect(writer, request, handler.config.PublicURL+"/"+cacheKey, http.StatusFound)
}

type imageOptions struct {
	URL     string `json:"url"`
	Width   int    `json:"width"`
	Height  *int   `json:"height,omitempty"`
	Quality int    `json:"quality"`
	Format  string `json:"format"`
}

func parseImageOptions(rawURL string, values url.Values) (imageOptions, bool) {
	options := imageOptions{URL: rawURL, Width: roundedImageWidth(values.Get("w")), Quality: 70, Format: "webp"}
	if rawHeight := values.Get("h"); rawHeight != "" {
		height, err := strconv.Atoi(rawHeight)
		if err != nil || height < 1 || height > 4096 {
			return imageOptions{}, false
		}
		options.Height = &height
	}
	if format := values.Get("fmt"); format != "" {
		if format != "webp" && format != "jpeg" && format != "avif" {
			return imageOptions{}, false
		}
		options.Format = format
	}
	if rawQuality := values.Get("q"); rawQuality != "" {
		quality, err := strconv.Atoi(rawQuality)
		if err != nil || quality < 1 || quality > 100 {
			return imageOptions{}, false
		}
		options.Quality = quality
	}
	return options, true
}

func imageCacheObjectKey(options imageOptions) string {
	encoded, _ := json.Marshal(options)
	digest := sha1.Sum(encoded)
	extension := options.Format
	if extension == "jpeg" {
		extension = "jpg"
	}
	return "img-cache/" + hex.EncodeToString(digest[:]) + "." + extension
}

var errImageSourceTooLarge = errors.New("r2 image source exceeds the maximum size")

func (handler *Handler) publicObjectKey(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	base, err := url.Parse(handler.config.PublicURL)
	if err != nil || !strings.EqualFold(parsed.Host, base.Host) || parsed.Scheme != base.Scheme {
		return "", false
	}
	key, err := url.PathUnescape(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil || !validPublicObjectKey(key) {
		return "", false
	}
	return key, true
}

func (handler *Handler) isPublicObjectURL(raw string) bool {
	_, ok := handler.publicObjectKey(raw)
	return ok
}

func validPublicObjectKey(key string) bool {
	if key == "" || len(key) > maxKeyLength || strings.HasPrefix(key, "/") || strings.TrimSpace(key) != key || path.Clean(key) != key || strings.Contains(key, "..") {
		return false
	}
	for _, char := range key {
		if char < 32 || char == 127 {
			return false
		}
	}
	return true
}

func materializeImageSource(source *r2.Object) (string, func(), error) {
	if source == nil || source.Body == nil || source.ContentLength > maxImageSourceBytes {
		return "", func() {}, errImageSourceTooLarge
	}
	file, err := os.CreateTemp("", "ai-r2-image-source-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	written, copyErr := io.Copy(file, io.LimitReader(source.Body, maxImageSourceBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		cleanup()
		return "", func() {}, copyErr
	}
	if closeErr != nil {
		cleanup()
		return "", func() {}, closeErr
	}
	if written > maxImageSourceBytes {
		cleanup()
		return "", func() {}, errImageSourceTooLarge
	}
	return file.Name(), cleanup, nil
}

func transformImage(ctx context.Context, sourcePath string, options imageOptions) ([]byte, string, error) {
	width, err := vipsDimension(ctx, sourcePath, "width")
	if err != nil {
		return nil, "", err
	}
	height, err := vipsDimension(ctx, sourcePath, "height")
	if err != nil {
		return nil, "", err
	}
	if width <= 0 || height <= 0 || uint64(width)*uint64(height) > maxImageInputPixels {
		return nil, "", errors.New("r2 image dimensions are invalid")
	}

	extension, contentType := imageOutputFormat(options.Format)
	output, err := os.CreateTemp("", "ai-r2-image-output-*."+extension)
	if err != nil {
		return nil, "", err
	}
	outputPath := output.Name()
	if err := output.Close(); err != nil {
		_ = os.Remove(outputPath)
		return nil, "", err
	}
	defer os.Remove(outputPath)

	targetHeight := maxImageOutputDimension
	if options.Height != nil {
		targetHeight = *options.Height
	}
	args := []string{"thumbnail", sourcePath, vipsSavePath(outputPath, options.Format, options.Quality), strconv.Itoa(options.Width), "--height", strconv.Itoa(targetHeight), "--size", "down", "--auto-rotate"}
	if output, err := exec.CommandContext(ctx, "vips", args...).CombinedOutput(); err != nil {
		return nil, "", fmt.Errorf("transform image: %w: %s", err, strings.TrimSpace(string(output)))
	}
	bytes, err := os.ReadFile(outputPath)
	if err != nil || len(bytes) == 0 {
		if err == nil {
			err = errors.New("vips produced an empty image")
		}
		return nil, "", err
	}
	return bytes, contentType, nil
}

func vipsDimension(ctx context.Context, sourcePath, field string) (int, error) {
	output, err := exec.CommandContext(ctx, "vipsheader", "-f", field, sourcePath).Output()
	if err != nil {
		return 0, fmt.Errorf("read image %s: %w", field, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, fmt.Errorf("parse image %s: %w", field, err)
	}
	return value, nil
}

func imageOutputFormat(format string) (string, string) {
	switch format {
	case "jpeg":
		return "jpg", "image/jpeg"
	case "avif":
		return "avif", "image/avif"
	default:
		return "webp", "image/webp"
	}
}

func vipsSavePath(outputPath, format string, quality int) string {
	switch format {
	case "jpeg":
		return outputPath + "[Q=" + strconv.Itoa(quality) + ",optimize_coding=true,strip=true]"
	case "avif":
		return outputPath + "[Q=" + strconv.Itoa(quality) + ",effort=4]"
	default:
		return outputPath + "[Q=" + strconv.Itoa(quality) + ",effort=4]"
	}
}

func writeImageUnavailable(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"success": false, "code": "SERVICE_UNAVAILABLE", "message": "Service unavailable", "details": nil})
}

func (handler *Handler) refresh(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, 16<<10))
	if decoder.Decode(&body) != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"success": false, "message": "Unsupported media URL"})
		return
	}
	parsed, err := url.Parse(strings.TrimSpace(body.URL))
	if err != nil || parsed.Path != objectPath || parsed.User != nil || (parsed.IsAbs() && (parsed.Scheme != "http" && parsed.Scheme != "https" || !sameRequestHost(parsed, request))) {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"success": false, "message": "Unsupported media URL"})
		return
	}
	key, ok := handler.validateObjectRequest(parsed.Query())
	if !ok {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"success": false, "message": "Unsupported media URL"})
		return
	}
	query := url.Values{"b": {"private"}, "k": {key}, "sig": {handler.signature(key)}}
	if parsed.Query().Get("dl") == "1" {
		query.Set("dl", "1")
	}
	if filename := parsed.Query().Get("fn"); filename != "" {
		query.Set("fn", filename)
	}
	parsed.RawQuery = query.Encode()
	writeJSON(writer, http.StatusOK, map[string]any{"success": true, "data": map[string]string{"url": parsed.String()}})
}

func (handler *Handler) validateObjectRequest(values url.Values) (string, bool) {
	if values.Get("b") != "private" || len(values.Get("b")) > 32 || len(values.Get("sig")) < 8 || len(values.Get("sig")) > 256 {
		return "", false
	}
	key := values.Get("k")
	if !validPrivateKey(key) || !hmac.Equal([]byte(values.Get("sig")), []byte(handler.signature(key))) {
		return "", false
	}
	return key, true
}

func (handler *Handler) signature(key string) string {
	mac := hmac.New(sha256.New, []byte(handler.config.ProxySecret))
	_, _ = io.WriteString(mac, "b=private&k="+key)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validPrivateKey(key string) bool {
	if key == "" || len(key) > maxKeyLength || strings.HasPrefix(key, "/") || strings.TrimSpace(key) != key || path.Clean(key) != key || strings.Contains(key, "..") {
		return false
	}
	for _, char := range key {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '/' || char == '_' || char == '-' || char == '.') {
			return false
		}
	}
	for _, prefix := range privatePrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func roundedImageWidth(raw string) int {
	requested := 540
	if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
		requested = parsed
	}
	for _, width := range imageWidths {
		if requested <= width {
			return width
		}
	}
	return imageWidths[len(imageWidths)-1]
}

func attachmentDisposition(filename string) string {
	filename = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(filename, "\r", ""), "\n", ""))
	if filename == "" {
		filename = "download"
	}
	if len(filename) > 200 {
		filename = filename[:200]
	}
	return fmt.Sprintf("attachment; filename=%q", filename)
}

func isAndroidWebView(userAgent string) bool {
	return strings.Contains(userAgent, "wv") || strings.Contains(userAgent, "WebView")
}
func applyCORS(writer http.ResponseWriter) {
	writer.Header().Set("Access-Control-Allow-Origin", "*")
	writer.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	writer.Header().Set("Access-Control-Allow-Headers", "Range")
	writer.Header().Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Range, Content-Length, ETag, Last-Modified")
	writer.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
}
func sameRequestHost(parsed *url.URL, request *http.Request) bool {
	return request != nil && request.Host != "" && strings.EqualFold(parsed.Host, request.Host)
}
func contentTypeOrBinary(value string) string {
	if value == "" {
		return "application/octet-stream"
	}
	return value
}
func notFound(writer http.ResponseWriter) { http.NotFound(writer, &http.Request{}) }
func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
