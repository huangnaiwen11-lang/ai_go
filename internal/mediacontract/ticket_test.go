package mediacontract

import (
	"net/http"
	"strings"
	"testing"
)

const testSignedUploadURL = "https://upload.example.test/ugc/user-123/videos/clip.mp4?X-Amz-Signature=private-upload-signature"

func TestValidateDirectUploadTicket接受允许的视频类型(t *testing.T) {
	for _, contentType := range []string{"video/mp4", "video/webm", "video/quicktime"} {
		t.Run(contentType, func(t *testing.T) {
			request := validRequest(contentType)
			ticket := validTicket(contentType)

			if err := ValidateDirectUploadTicket(request, ticket); err != nil {
				t.Fatalf("ValidateDirectUploadTicket() error = %v", err)
			}
		})
	}
}

func TestValidateDirectUploadTicket拒绝非视频Kind(t *testing.T) {
	for _, kind := range []string{"image", "audio", "", "VIDEO"} {
		t.Run(kind, func(t *testing.T) {
			request := validRequest("video/mp4")
			request.Kind = kind

			assertInvalidTicket(t, request, validTicket("video/mp4"))
		})
	}
}

func TestValidateDirectUploadTicket拒绝不允许或不一致的类型(t *testing.T) {
	testCases := []struct {
		name    string
		request PresignRequest
		ticket  PresignTicket
	}{
		{
			name:    "请求类型不允许",
			request: validRequest("image/png"),
			ticket:  validTicket("video/mp4"),
		},
		{
			name:    "票据类型不允许",
			request: validRequest("video/mp4"),
			ticket:  validTicket("image/png"),
		},
		{
			name:    "请求与票据类型不一致",
			request: validRequest("video/mp4"),
			ticket:  validTicket("video/webm"),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assertInvalidTicket(t, testCase.request, testCase.ticket)
		})
	}
}

func TestValidateDirectUploadTicket校验尺寸与固定上限(t *testing.T) {
	testCases := []struct {
		name    string
		request PresignRequest
		ticket  PresignTicket
	}{
		{
			name:    "恰好 50 MiB 可以上传",
			request: validRequestWithSize("video/mp4", 50<<20),
			ticket:  validTicket("video/mp4"),
		},
		{
			name:    "文件超过票据上限",
			request: validRequestWithSize("video/mp4", 50<<20+1),
			ticket:  validTicket("video/mp4"),
		},
		{
			name:    "负文件大小",
			request: validRequestWithSize("video/mp4", -1),
			ticket:  validTicket("video/mp4"),
		},
		{
			name:    "票据上限偏离 50 MiB",
			request: validRequest("video/mp4"),
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.MaxBytes = 50<<20 - 1
				return ticket
			}(),
		},
		{
			name:    "票据上限超过 50 MiB",
			request: validRequest("video/mp4"),
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.MaxBytes = 50<<20 + 1
				return ticket
			}(),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateDirectUploadTicket(testCase.request, testCase.ticket)
			if testCase.name == "恰好 50 MiB 可以上传" {
				if err != nil {
					t.Fatalf("ValidateDirectUploadTicket() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateDirectUploadTicket() error = nil，期望拒绝")
			}
		})
	}
}

func TestValidateDirectUploadTicket校验脱敏URL(t *testing.T) {
	testCases := []struct {
		name    string
		ticket  PresignTicket
		wantErr bool
	}{
		{
			name: "允许 https 上传地址和可选公开地址",
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.PublicURL = "https://cdn.example.test/ugc/user-123/videos/clip.mp4"
				return ticket
			}(),
		},
		{
			name:   "公开地址允许为空",
			ticket: validTicket("video/mp4"),
		},
		{
			name: "允许 http 上传地址",
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.UploadURL = "http://upload.example.test/ugc/user-123/videos/clip.mp4?private-http-signature=yes"
				return ticket
			}(),
		},
		{
			name: "上传地址不是 HTTP(S)",
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.UploadURL = "ftp://upload.example.test/object?private-ftp-signature=yes"
				return ticket
			}(),
			wantErr: true,
		},
		{
			name: "上传地址带 credentials",
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.UploadURL = "https://private-user:private-password@upload.example.test/object?X-Amz-Signature=private-upload-signature"
				return ticket
			}(),
			wantErr: true,
		},
		{
			name: "上传地址没有 scheme",
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.UploadURL = "upload.example.test/object?X-Amz-Signature=private-upload-signature"
				return ticket
			}(),
			wantErr: true,
		},
		{
			name: "上传地址主机名为空",
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.UploadURL = "https://:443/object?private-empty-upload-host=yes"
				return ticket
			}(),
			wantErr: true,
		},
		{
			name: "公开地址带 credentials",
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.PublicURL = "https://private-user:private-password@cdn.example.test/object?private-public-query=yes"
				return ticket
			}(),
			wantErr: true,
		},
		{
			name: "公开地址主机名为空",
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.PublicURL = "https://:443/object?private-empty-public-host=yes"
				return ticket
			}(),
			wantErr: true,
		},
		{
			name: "公开地址没有 scheme",
			ticket: func() PresignTicket {
				ticket := validTicket("video/mp4")
				ticket.PublicURL = "cdn.example.test/object?private-public-query=yes"
				return ticket
			}(),
			wantErr: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateDirectUploadTicket(validRequest("video/mp4"), testCase.ticket)
			if testCase.wantErr {
				if err == nil {
					t.Fatal("ValidateDirectUploadTicket() error = nil，期望 URL 被拒绝")
				}
				for _, privateValue := range []string{"private-upload-signature", "private-password", "private-public-query"} {
					if strings.Contains(err.Error(), privateValue) {
						t.Fatalf("错误泄露 URL 私有值：%q", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateDirectUploadTicket() error = %v", err)
			}
		})
	}
}

func TestValidateDirectUploadTicket拒绝异常端口(t *testing.T) {
	for _, rawURL := range []string{
		"https://upload.example.test:/object",
		"https://upload.example.test:0/object",
		"https://upload.example.test:65536/object",
	} {
		t.Run(rawURL, func(t *testing.T) {
			ticket := validTicket("video/mp4")
			ticket.UploadURL = rawURL

			assertInvalidTicket(t, validRequest("video/mp4"), ticket)
		})
	}
}

func TestValidateDirectUploadTicket校验Key与文件名(t *testing.T) {
	testCases := []struct {
		name     string
		key      string
		filename string
		wantErr  bool
	}{
		{name: "合法用户片段不要求 UUID", key: "ugc/user_name-2026/videos/clip.mp4", filename: "ugc/user_name-2026/videos/clip.mp4"},
		{name: "文件名仅为对象末段", key: "ugc/user-123/videos/clip.mp4", filename: "clip.mp4", wantErr: true},
		{name: "文件名不一致", key: "ugc/user-123/videos/clip.mp4", filename: "other.mp4", wantErr: true},
		{name: "key 与文件名均为空", key: "", filename: "", wantErr: true},
		{name: "用户片段为空", key: "ugc//videos/clip.mp4", filename: "ugc//videos/clip.mp4", wantErr: true},
		{name: "对象名为空", key: "ugc/user-123/videos/", filename: "ugc/user-123/videos/", wantErr: true},
		{name: "对象名为点", key: "ugc/user-123/videos/.", filename: "ugc/user-123/videos/.", wantErr: true},
		{name: "对象名为父目录", key: "ugc/user-123/videos/..", filename: "ugc/user-123/videos/..", wantErr: true},
		{name: "含父目录", key: "ugc/../videos/clip.mp4", filename: "ugc/../videos/clip.mp4", wantErr: true},
		{name: "含反斜杠", key: "ugc/user-123\\videos/clip.mp4", filename: "ugc/user-123\\videos/clip.mp4", wantErr: true},
		{name: "前导斜杠", key: "/ugc/user-123/videos/clip.mp4", filename: "/ugc/user-123/videos/clip.mp4", wantErr: true},
		{name: "尾随斜杠", key: "ugc/user-123/videos/clip.mp4/", filename: "ugc/user-123/videos/clip.mp4/", wantErr: true},
		{name: "额外目录", key: "ugc/user-123/videos/nested/clip.mp4", filename: "ugc/user-123/videos/nested/clip.mp4", wantErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ticket := validTicket("video/mp4")
			ticket.Key = testCase.key
			ticket.Filename = testCase.filename
			err := ValidateDirectUploadTicket(validRequest("video/mp4"), ticket)
			if testCase.wantErr && err == nil {
				t.Fatal("ValidateDirectUploadTicket() error = nil，期望 key 被拒绝")
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("ValidateDirectUploadTicket() error = %v", err)
			}
		})
	}
}

func TestValidateDirectUploadTicket接受Node完整Key作为文件名(t *testing.T) {
	ticket := validTicket("video/mp4")
	ticket.Filename = ticket.Key

	if err := ValidateDirectUploadTicket(validRequest("video/mp4"), ticket); err != nil {
		t.Fatalf("ValidateDirectUploadTicket() error = %v", err)
	}
}

func TestValidateDirectUploadTicket要求唯一ContentType请求头(t *testing.T) {
	testCases := []struct {
		name    string
		headers http.Header
		wantErr bool
	}{
		{
			name:    "唯一标准键",
			headers: http.Header{"Content-Type": {"video/mp4"}},
		},
		{
			name:    "唯一小写键",
			headers: http.Header{"content-type": {"video/mp4"}},
		},
		{
			name:    "缺失",
			headers: http.Header{},
			wantErr: true,
		},
		{
			name:    "一个键有多个值",
			headers: http.Header{"Content-Type": {"video/mp4", "video/mp4"}},
			wantErr: true,
		},
		{
			name:    "大小写重复键",
			headers: http.Header{"Content-Type": {"video/mp4"}, "content-type": {"video/mp4"}},
			wantErr: true,
		},
		{
			name:    "大小写重复键的空切片",
			headers: http.Header{"Content-Type": {"video/mp4"}, "content-type": []string{}},
			wantErr: true,
		},
		{
			name:    "大小写重复键的 nil 切片",
			headers: http.Header{"Content-Type": {"video/mp4"}, "content-type": nil},
			wantErr: true,
		},
		{
			name:    "请求头值不一致",
			headers: http.Header{"Content-Type": {"video/webm"}},
			wantErr: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ticket := validTicket("video/mp4")
			ticket.Headers = testCase.headers
			err := ValidateDirectUploadTicket(validRequest("video/mp4"), ticket)
			if testCase.wantErr && err == nil {
				t.Fatal("ValidateDirectUploadTicket() error = nil，期望 Content-Type 被拒绝")
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("ValidateDirectUploadTicket() error = %v", err)
			}
		})
	}
}

func validRequest(contentType string) PresignRequest {
	return validRequestWithSize(contentType, 0)
}

func validRequestWithSize(contentType string, size int64) PresignRequest {
	return PresignRequest{
		Kind:          DirectUploadKindVideo,
		ContentType:   contentType,
		FileSizeBytes: size,
	}
}

func validTicket(contentType string) PresignTicket {
	return PresignTicket{
		UploadURL:   testSignedUploadURL,
		Key:         "ugc/user-123/videos/clip.mp4",
		Filename:    "ugc/user-123/videos/clip.mp4",
		ContentType: contentType,
		MaxBytes:    50 << 20,
		Headers:     http.Header{"Content-Type": {contentType}},
	}
}

func assertInvalidTicket(t *testing.T, request PresignRequest, ticket PresignTicket) {
	t.Helper()
	if err := ValidateDirectUploadTicket(request, ticket); err == nil {
		t.Fatal("ValidateDirectUploadTicket() error = nil，期望合同校验失败")
	}
}
