package realtimecontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

var (
	// ErrInvalidLimits 表示调用方提供的离线资源上限缺失、无效或相互矛盾。
	ErrInvalidLimits = errors.New("SSE contract: invalid parse limits")
	// ErrAmbiguousBody 表示 Body 与 Chunks 同时出现，无法确定应校验的字节流。
	ErrAmbiguousBody = errors.New("SSE contract: body representation is ambiguous")
	// ErrCaptureTooLarge 表示捕获总字节数超过本地解析器的安全上限。
	ErrCaptureTooLarge = errors.New("SSE contract: capture exceeds byte limit")
	// ErrLineTooLong 表示单个 SSE 行超过本地解析器的安全上限。
	ErrLineTooLong = errors.New("SSE contract: line exceeds byte limit")
	// ErrFrameLimitExceeded 表示已完成的 SSE 帧数超过本地解析器的安全上限。
	ErrFrameLimitExceeded = errors.New("SSE contract: frame count exceeds limit")

	errUnexpectedStatus    = errors.New("SSE contract: unexpected HTTP status")
	errInvalidHeader       = errors.New("SSE contract: required response header is missing or invalid")
	errUnterminatedFrame   = errors.New("SSE contract: frame is not terminated")
	errInvalidLineEnding   = errors.New("SSE contract: invalid line ending")
	errInvalidFrame        = errors.New("SSE contract: invalid frame structure")
	errUnknownSSEField     = errors.New("SSE contract: unknown SSE field")
	errInvalidHeartbeat    = errors.New("SSE contract: invalid heartbeat frame")
	errInvalidJSON         = errors.New("SSE contract: invalid JSON object")
	errInvalidMessageShape = errors.New("SSE contract: invalid message shape")
)

const (
	defaultMaxCaptureBytes = 1 << 20
	defaultMaxLineBytes    = 64 << 10
	defaultMaxFrames       = 1024
)

// Limits 限制单次离线捕获解析的资源占用。每个字段都必须为正数，且
// MaxLineBytes 不得大于 MaxCaptureBytes，避免出现没有实际约束含义的配置。
type Limits struct {
	MaxCaptureBytes int
	MaxLineBytes    int
	MaxFrames       int
}

// DefaultLimits 返回离线工具的审慎默认上限：1 MiB 捕获、64 KiB 单行、1024 帧。
// 这些值仅用于限制本机内存和 CPU 消耗，不推断生产 SSE 的时长、帧率或策略。
func DefaultLimits() Limits {
	return Limits{
		MaxCaptureBytes: defaultMaxCaptureBytes,
		MaxLineBytes:    defaultMaxLineBytes,
		MaxFrames:       defaultMaxFrames,
	}
}

// Capture 是一次已完成的 HTTP SSE 捕获。Body 与 Chunks 是互斥的两种正文表示：
// Body 保持已聚合捕获的兼容性，Chunks 用于保留任意底层读取边界。解析器会在
// 校验前按顺序安全拼接 Chunks，避免把读取边界误当成 SSE 帧边界。
type Capture struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	Chunks     [][]byte
}

// Kind 是公开 SSE 消息的类别。Frame 有意不保留 comment 或 data 内容，避免
// 离线校验工具在结果中继续传播任务、令牌或其他事件载荷。
type Kind string

const (
	Heartbeat Kind = "heartbeat"
	Snapshot  Kind = "snapshot"
	Change    Kind = "change"
	Error     Kind = "error"
)

// Frame 仅描述已通过静态合同校验的帧类别，顺序与捕获中的 SSE 顺序一致。
type Frame struct {
	Kind Kind
}

// Parse 校验公开响应头并解析 SSE 帧。它接受 LF 和 CRLF，允许 change 出现在
// snapshot 之前以保持 Node 的订阅竞态语义；任何未以空行结束的帧都会失败。它
// 使用 DefaultLimits 限制本地资源；需要更小受控上限时可调用 ParseWithLimits。
func Parse(capture Capture) ([]Frame, error) {
	return ParseWithLimits(capture, DefaultLimits())
}

// ParseWithLimits 使用调用方明确提供的本地资源上限校验并解析捕获。该函数只处理
// 已捕获字节，不创建网络连接；上限是离线工具保护，不代表生产 SSE 行为。
func ParseWithLimits(capture Capture, limits Limits) ([]Frame, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if err := validateCapture(capture); err != nil {
		return nil, err
	}
	body, err := captureBody(capture, limits)
	if err != nil {
		return nil, err
	}

	return parseFrames(body, limits)
}

// Validate 只报告捕获是否符合静态公开合同，不暴露或比较任何事件载荷值。
func Validate(capture Capture) error {
	_, err := Parse(capture)
	return err
}

func validateCapture(capture Capture) error {
	if capture.StatusCode != http.StatusOK {
		return errUnexpectedStatus
	}

	for _, required := range []struct {
		name  string
		value string
	}{
		{name: "Content-Type", value: "text/event-stream"},
		{name: "Cache-Control", value: "no-cache, no-transform"},
		{name: "Connection", value: "keep-alive"},
		{name: "X-Accel-Buffering", value: "no"},
	} {
		if !hasSingleHeaderValue(capture.Header, required.name, required.value) {
			return errInvalidHeader
		}
	}

	return nil
}

func hasSingleHeaderValue(header http.Header, name, want string) bool {
	var values []string
	for actualName, actualValues := range header {
		if strings.EqualFold(actualName, name) {
			values = append(values, actualValues...)
		}
	}

	return len(values) == 1 && values[0] == want
}

func validateLimits(limits Limits) error {
	if limits.MaxCaptureBytes <= 0 || limits.MaxLineBytes <= 0 || limits.MaxFrames <= 0 || limits.MaxLineBytes > limits.MaxCaptureBytes {
		return ErrInvalidLimits
	}

	return nil
}

func captureBody(capture Capture, limits Limits) ([]byte, error) {
	if capture.Chunks == nil {
		if len(capture.Body) > limits.MaxCaptureBytes {
			return nil, ErrCaptureTooLarge
		}
		return capture.Body, nil
	}
	if capture.Body != nil {
		return nil, ErrAmbiguousBody
	}

	totalBytes := 0
	for _, chunk := range capture.Chunks {
		// 用减法比较避免 totalBytes + len(chunk) 在极端输入下溢出；必须在
		// bytes.Join 分配前完成检查，防止异常离线捕获触发额外的大块复制。
		if len(chunk) > limits.MaxCaptureBytes-totalBytes {
			return nil, ErrCaptureTooLarge
		}
		totalBytes += len(chunk)
	}

	return bytes.Join(capture.Chunks, nil), nil
}

func parseFrames(body []byte, limits Limits) ([]Frame, error) {
	frames := make([]Frame, 0)
	lines := make([]string, 0, 1)
	remaining := body

	for len(remaining) > 0 {
		lineEnd := bytes.IndexByte(remaining, '\n')
		if lineEnd < 0 {
			if lineByteLength(remaining) > limits.MaxLineBytes {
				return nil, ErrLineTooLong
			}
			return nil, errUnterminatedFrame
		}
		if lineByteLength(remaining[:lineEnd]) > limits.MaxLineBytes {
			return nil, ErrLineTooLong
		}

		line, err := normalizeLine(remaining[:lineEnd])
		if err != nil {
			return nil, err
		}
		remaining = remaining[lineEnd+1:]

		if line != "" {
			lines = append(lines, line)
			continue
		}

		frame, err := parseFrame(lines)
		if err != nil {
			return nil, err
		}
		if len(frames) >= limits.MaxFrames {
			return nil, ErrFrameLimitExceeded
		}
		frames = append(frames, frame)
		lines = lines[:0]
	}

	if len(lines) != 0 {
		return nil, errUnterminatedFrame
	}

	return frames, nil
}

func lineByteLength(raw []byte) int {
	if len(raw) > 0 && raw[len(raw)-1] == '\r' {
		return len(raw) - 1
	}

	return len(raw)
}

func normalizeLine(raw []byte) (string, error) {
	if bytes.HasSuffix(raw, []byte{'\r'}) {
		raw = raw[:len(raw)-1]
	}
	if bytes.Contains(raw, []byte{'\r'}) {
		return "", errInvalidLineEnding
	}

	return string(raw), nil
}

func parseFrame(lines []string) (Frame, error) {
	if len(lines) != 1 {
		return Frame{}, errInvalidFrame
	}

	line := lines[0]
	if strings.HasPrefix(line, ":") {
		return parseHeartbeat(line)
	}
	if strings.HasPrefix(line, "data:") {
		return parseData(line)
	}

	return Frame{}, errUnknownSSEField
}

func parseHeartbeat(line string) (Frame, error) {
	const prefix = ": heartbeat "
	if !strings.HasPrefix(line, prefix) || strings.TrimSpace(strings.TrimPrefix(line, prefix)) == "" {
		return Frame{}, errInvalidHeartbeat
	}

	return Frame{Kind: Heartbeat}, nil
}

func parseData(line string) (Frame, error) {
	const prefix = "data: "
	if !strings.HasPrefix(line, prefix) {
		return Frame{}, errInvalidFrame
	}

	message, err := decodeJSONObject(strings.TrimPrefix(line, prefix))
	if err != nil {
		return Frame{}, err
	}

	kind, err := requiredString(message, "kind")
	if err != nil {
		return Frame{}, err
	}

	switch Kind(kind) {
	case Snapshot:
		if _, exists := message["snapshot"]; !exists {
			return Frame{}, errInvalidMessageShape
		}
		if _, err := requiredString(message, "generatedAt"); err != nil {
			return Frame{}, err
		}
		return Frame{Kind: Snapshot}, nil
	case Change:
		change, exists := message["change"]
		if !exists || !isJSONObject(change) {
			return Frame{}, errInvalidMessageShape
		}
		return Frame{Kind: Change}, nil
	case Error:
		if !isTransientError(message) {
			return Frame{}, errInvalidMessageShape
		}
		return Frame{Kind: Error}, nil
	default:
		return Frame{}, errInvalidMessageShape
	}
}

func decodeJSONObject(value string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	message := make(map[string]json.RawMessage)
	if err := decoder.Decode(&message); err != nil || message == nil {
		return nil, errInvalidJSON
	}
	if decoder.More() {
		return nil, errInvalidJSON
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errInvalidJSON
	}

	return message, nil
}

func requiredString(message map[string]json.RawMessage, key string) (string, error) {
	raw, exists := message[key]
	if !exists {
		return "", errInvalidMessageShape
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", errInvalidMessageShape
	}

	return value, nil
}

func isJSONObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func isTransientError(message map[string]json.RawMessage) bool {
	raw, exists := message["transient"]
	if !exists {
		return false
	}

	var transient bool
	return json.Unmarshal(raw, &transient) == nil && transient
}
