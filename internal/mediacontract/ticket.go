package mediacontract

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// DirectUploadKindVideo 是当前静态源码基线中唯一可直接 PUT 的媒体类别。
	// 图片必须经服务端 /ugc 净化链，本包不会把它当作直传兼容项。
	DirectUploadKindVideo = "video"

	// DirectUploadMaxBytes 固定为 50 MiB，与票据中公开给调用方的上限一致。
	DirectUploadMaxBytes int64 = 50 << 20
)

var (
	errUnsupportedKind       = errors.New("media contract: direct upload kind is unsupported")
	errInvalidContentType    = errors.New("media contract: content type is invalid")
	errInvalidSize           = errors.New("media contract: file size or ticket limit is invalid")
	errInvalidUploadURL      = errors.New("media contract: upload URL is invalid")
	errInvalidPublicURL      = errors.New("media contract: public URL is invalid")
	errInvalidObjectLocation = errors.New("media contract: object key or filename is invalid")
	errInvalidContentHeader  = errors.New("media contract: Content-Type header is missing or invalid")
)

// PresignRequest 是离线兼容与安全校验使用的请求字段；本包不读取文件、请求体或对象存储。
type PresignRequest struct {
	Kind          string
	ContentType   string
	FileSizeBytes int64
}

// PresignTicket 是调用方已脱敏的直传票据公开字段。UploadURL 可能带签名查询参数，
// 因此校验错误始终使用固定描述，不能把 URL 或任意票据内容回显给日志调用方。
// PublicURL 为空是允许的，因为对象公开可见性尚未由静态源码冻结。
type PresignTicket struct {
	UploadURL   string
	PublicURL   string
	Key         string
	Filename    string
	ContentType string
	MaxBytes    int64
	Headers     http.Header
}

// ValidateDirectUploadTicket 校验单个已脱敏的直传视频请求及其票据是否符合当前
// 静态源码合同。它是纯函数：不发网络请求、不访问对象存储，也不会修改输入结构。
func ValidateDirectUploadTicket(request PresignRequest, ticket PresignTicket) error {
	if request.Kind != DirectUploadKindVideo {
		return errUnsupportedKind
	}
	if !isAllowedVideoContentType(request.ContentType) || !isAllowedVideoContentType(ticket.ContentType) || request.ContentType != ticket.ContentType {
		return errInvalidContentType
	}
	if request.FileSizeBytes < 0 || ticket.MaxBytes != DirectUploadMaxBytes || request.FileSizeBytes > ticket.MaxBytes {
		return errInvalidSize
	}
	if !isSafeHTTPURL(ticket.UploadURL) {
		return errInvalidUploadURL
	}
	if ticket.PublicURL != "" && !isSafeHTTPURL(ticket.PublicURL) {
		return errInvalidPublicURL
	}
	if !hasVideoObjectKey(ticket.Key, ticket.Filename) {
		return errInvalidObjectLocation
	}
	if !hasSingleHeaderValue(ticket.Headers, "Content-Type", ticket.ContentType) {
		return errInvalidContentHeader
	}

	return nil
}

func isAllowedVideoContentType(contentType string) bool {
	switch contentType {
	case "video/mp4", "video/webm", "video/quicktime":
		return true
	default:
		return false
	}
}

func isSafeHTTPURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	if !hasSafeURLPort(parsed) {
		return false
	}

	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func hasSafeURLPort(parsed *url.URL) bool {
	port := parsed.Port()
	if port == "" {
		if strings.HasPrefix(parsed.Host, "[") {
			return strings.HasSuffix(parsed.Host, "]")
		}
		return !strings.Contains(parsed.Host, ":")
	}

	value, err := strconv.ParseUint(port, 10, 16)
	return err == nil && value > 0
}

func hasVideoObjectKey(key, filename string) bool {
	// Node 票据把完整对象 key 原样作为 filename 返回，二者必须完全一致。
	if key == "" || filename == "" || key != filename || strings.Contains(key, "\\") || strings.Contains(filename, "\\") {
		return false
	}

	segments := strings.Split(key, "/")
	if len(segments) != 4 || segments[0] != "ugc" || segments[2] != "videos" {
		return false
	}

	return isSafeObjectSegment(segments[1]) && isSafeObjectSegment(segments[3])
}

func isSafeObjectSegment(segment string) bool {
	return segment != "" && segment != "." && segment != ".."
}

func hasSingleHeaderValue(header http.Header, name, want string) bool {
	matchingKeys := 0
	for actualName, actualValues := range header {
		if strings.EqualFold(actualName, name) {
			matchingKeys++
			if matchingKeys != 1 || len(actualValues) != 1 || actualValues[0] != want {
				return false
			}
		}
	}

	return matchingKeys == 1
}
