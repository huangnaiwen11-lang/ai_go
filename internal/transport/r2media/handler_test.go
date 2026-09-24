package r2media

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/integrations/r2"
)

type recordingObjectStore struct {
	presign r2.PresignGetInput
	object  *r2.Object
	head    r2.ObjectInfo
	heads   []headCall
	gets    []r2.GetInput
	puts    []putCall
}

type headCall struct {
	bucket string
	key    string
}

type putCall struct {
	bucket       string
	key          string
	contentType  string
	cacheControl string
	body         []byte
}

func (store *recordingObjectStore) PrivateBucketName() string { return "cling-ai-private" }
func (store *recordingObjectStore) PublicBucketName() string  { return "cling-ai" }
func (store *recordingObjectStore) PresignGet(_ context.Context, input r2.PresignGetInput) (string, error) {
	store.presign = input
	return "https://signed.example.test/private-object", nil
}
func (store *recordingObjectStore) Get(_ context.Context, input r2.GetInput) (*r2.Object, error) {
	store.gets = append(store.gets, input)
	return store.object, nil
}
func (store *recordingObjectStore) Head(_ context.Context, bucket, key string) (r2.ObjectInfo, error) {
	store.heads = append(store.heads, headCall{bucket: bucket, key: key})
	return store.head, nil
}
func (store *recordingObjectStore) Put(_ context.Context, input r2.PutInput) (r2.ObjectInfo, error) {
	body, err := io.ReadAll(input.Body)
	if err != nil {
		return r2.ObjectInfo{}, err
	}
	store.puts = append(store.puts, putCall{bucket: input.Bucket, key: input.Key, contentType: input.ContentType, cacheControl: input.CacheControl, body: body})
	return r2.ObjectInfo{Exists: true}, nil
}

func newHandler(t *testing.T, store *recordingObjectStore) *Handler {
	t.Helper()
	handler, err := NewHandler(store, Config{ProxySecret: "test-secret", PublicURL: "https://pub.example.test", Redirect: true, AttachmentRedirect: true, SharedRedirectCache: true, RedirectCacheControl: "public, max-age=300, s-maxage=900, stale-while-revalidate=600", Expires: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func signedObjectPath(handler *Handler, key string) string {
	return objectPath + "?b=private&k=" + url.QueryEscape(key) + "&sig=" + url.QueryEscape(handler.signature(key))
}

func TestObject拒绝无效签名且不访问对象存储(t *testing.T) {
	store := &recordingObjectStore{}
	handler := newHandler(t, store)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, objectPath+"?b=private&k=videos/a.mp4&sig=forged-signature", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if store.presign.Key != "" {
		t.Fatalf("invalid signature presigned %q", store.presign.Key)
	}
}

func TestObject有效签名只签私有桶并返回短时重定向(t *testing.T) {
	store := &recordingObjectStore{}
	handler := newHandler(t, store)
	request := httptest.NewRequest(http.MethodGet, signedObjectPath(handler, "videos/final.mp4"), nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "https://signed.example.test/private-object" {
		t.Fatalf("redirect = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if store.presign.Bucket != "cling-ai-private" || store.presign.Key != "videos/final.mp4" || store.presign.Expires != time.Hour {
		t.Fatalf("presign = %#v", store.presign)
	}
	if recorder.Header().Get("X-Signed-Url-Cache") != "miss" || recorder.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("headers = %#v", recorder.Header())
	}
}

func TestObjectAndroidWebView下载改为同源流式输出(t *testing.T) {
	store := &recordingObjectStore{object: &r2.Object{ObjectInfo: r2.ObjectInfo{Exists: true, ContentType: "video/mp4", ContentLength: 3, ETag: "etag"}, Body: io.NopCloser(strings.NewReader("abc"))}}
	handler := newHandler(t, store)
	request := httptest.NewRequest(http.MethodGet, signedObjectPath(handler, "videos/final.mp4")+"&dl=1&fn=clip.mp4", nil)
	request.Header.Set("User-Agent", "Mozilla wv")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "abc" || recorder.Header().Get("Content-Disposition") == "" {
		t.Fatalf("stream = %d %q %#v", recorder.Code, recorder.Body.String(), recorder.Header())
	}
	if store.presign.Key != "" {
		t.Fatal("android webview must not receive a presigned attachment redirect")
	}
}

func TestRefresh只接受当前同源且有效的签名代理URL(t *testing.T) {
	store := &recordingObjectStore{}
	handler := newHandler(t, store)
	path := signedObjectPath(handler, "videos/final.mp4")
	request := httptest.NewRequest(http.MethodPost, refreshPath, strings.NewReader(`{"url":"`+path+`"}`))
	request.Host = "127.0.0.1:18000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "videos%2Ffinal.mp4") {
		t.Fatalf("refresh = %d %q", recorder.Code, recorder.Body.String())
	}

	foreign := httptest.NewRequest(http.MethodPost, refreshPath, strings.NewReader(`{"url":"https://evil.example`+path+`"}`))
	foreign.Host = "127.0.0.1:18000"
	foreignRecorder := httptest.NewRecorder()
	handler.ServeHTTP(foreignRecorder, foreign)
	if foreignRecorder.Code != http.StatusBadRequest {
		t.Fatalf("foreign refresh status = %d, want 400", foreignRecorder.Code)
	}
}

func TestImage缓存命中时重定向到确定性R2缓存对象(t *testing.T) {
	store := &recordingObjectStore{head: r2.ObjectInfo{Exists: true}}
	handler := newHandler(t, store)
	valid := httptest.NewRequest(http.MethodGet, imagePath+"?url="+url.QueryEscape("https://pub.example.test/agents/a.png")+"&w=500", nil)
	validRecorder := httptest.NewRecorder()
	handler.ServeHTTP(validRecorder, valid)
	// 对齐旧 Node 的 JSON.stringify：省略未指定的 height，字段序固定为
	// url/width/height/quality/format，然后以 SHA-1 生成公开缓存 key。
	const wantKey = "img-cache/8b88526bca5e1faa6ccc814bb7aa249742aaaff3.webp"
	if validRecorder.Code != http.StatusFound || validRecorder.Header().Get("Location") != "https://pub.example.test/"+wantKey {
		t.Fatalf("image redirect = %d %q", validRecorder.Code, validRecorder.Header().Get("Location"))
	}
	if len(store.heads) != 1 || store.heads[0] != (headCall{bucket: "cling-ai", key: wantKey}) {
		t.Fatalf("cache head = %#v", store.heads)
	}
	if validRecorder.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("cache headers = %#v", validRecorder.Header())
	}

	invalid := httptest.NewRequest(http.MethodGet, imagePath+"?url="+url.QueryEscape("https://evil.example/a.png"), nil)
	invalidRecorder := httptest.NewRecorder()
	handler.ServeHTTP(invalidRecorder, invalid)
	if invalidRecorder.Code != http.StatusBadRequest {
		t.Fatalf("foreign image status = %d, want 400", invalidRecorder.Code)
	}
}

func TestImage缓存未命中时转换公开源并写回不可变缓存(t *testing.T) {
	installFakeVips(t)
	const source = "source-image"
	store := &recordingObjectStore{object: &r2.Object{ObjectInfo: r2.ObjectInfo{Exists: true, ContentType: "image/png", ContentLength: int64(len(source))}, Body: io.NopCloser(strings.NewReader(source))}}
	handler := newHandler(t, store)
	request := httptest.NewRequest(http.MethodGet, imagePath+"?url="+url.QueryEscape("https://pub.example.test/agents/a.png")+"&w=500&fmt=avif&q=42", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	const wantKey = "img-cache/0c72c707ebe5c124afcd38daf628a3075b4db4d9.avif"
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "https://pub.example.test/"+wantKey {
		t.Fatalf("redirect = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if len(store.gets) != 1 || store.gets[0] != (r2.GetInput{Bucket: "cling-ai", Key: "agents/a.png"}) {
		t.Fatalf("source get = %#v", store.gets)
	}
	if len(store.puts) != 1 {
		t.Fatalf("cache puts = %#v", store.puts)
	}
	put := store.puts[0]
	if put.bucket != "cling-ai" || put.key != wantKey || put.contentType != "image/avif" || put.cacheControl != "public, max-age=31536000, immutable" || string(put.body) != source {
		t.Fatalf("cache put = %#v", put)
	}
}

func TestImage拒绝超过25MiB的公开源且不调用转换器(t *testing.T) {
	store := &recordingObjectStore{object: &r2.Object{ObjectInfo: r2.ObjectInfo{Exists: true, ContentType: "image/png", ContentLength: maxImageSourceBytes + 1}, Body: io.NopCloser(strings.NewReader("unread"))}}
	handler := newHandler(t, store)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, imagePath+"?url="+url.QueryEscape("https://pub.example.test/agents/a.png"), nil))
	if recorder.Code != http.StatusBadRequest || len(store.puts) != 0 {
		t.Fatalf("response=%d puts=%#v", recorder.Code, store.puts)
	}
}

func TestImage拒绝超过四千万像素的公开源且不写缓存(t *testing.T) {
	installFakeVipsDimensions(t, 10000, 4001)
	store := &recordingObjectStore{object: &r2.Object{ObjectInfo: r2.ObjectInfo{Exists: true, ContentType: "image/png", ContentLength: int64(len("source"))}, Body: io.NopCloser(strings.NewReader("source"))}}
	handler := newHandler(t, store)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, imagePath+"?url="+url.QueryEscape("https://pub.example.test/agents/a.png"), nil))
	if recorder.Code != http.StatusServiceUnavailable || len(store.puts) != 0 {
		t.Fatalf("response=%d puts=%#v", recorder.Code, store.puts)
	}
}

func installFakeVips(t *testing.T) {
	installFakeVipsDimensions(t, 10, 10)
}

func installFakeVipsDimensions(t *testing.T, width, height int) {
	t.Helper()
	directory := t.TempDir()
	writeExecutable(t, filepath.Join(directory, "vipsheader"), `#!/bin/sh
case "$2" in
  width) printf '`+strconv.Itoa(width)+`\n' ;;
  height) printf '`+strconv.Itoa(height)+`\n' ;;
  *) exit 64 ;;
esac
`)
	writeExecutable(t, filepath.Join(directory, "vips"), `#!/bin/sh
if [ "$1" != thumbnail ]; then exit 64; fi
output="$3"
output="${output%%\[*}"
cp "$2" "$output"
`)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
