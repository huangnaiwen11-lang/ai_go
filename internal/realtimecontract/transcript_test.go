package realtimecontract

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestParse接受真实样式SSE头与任意切片(t *testing.T) {
	privateSnapshot := `{"taskId":"snapshot-private"}`
	privateChange := `{"taskId":"change-private"}`
	capture := validCapture(nil)
	capture.Chunks = stringChunks(
		": heart",
		"beat 2026-09-02T12:00:00Z\n\n",
		"data: {\"kind\":\"snapshot\",\"snapshot\":",
		privateSnapshot,
		",\"generatedAt\":\"2026-09-02T12:00:00Z\"}\n\n",
		"data: {\"kind\":\"change\",\"change\":",
		privateChange,
		"}\n\n",
	)

	frames, err := Parse(capture)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := frameKinds(frames), []Kind{Heartbeat, Snapshot, Change}; !sameKinds(got, want) {
		t.Fatalf("帧顺序 = %v，期望 %v", got, want)
	}
}

func TestParse接受所有单切分点(t *testing.T) {
	body := []byte(": heartbeat 2026-09-02T12:00:00Z\n\ndata: {\"kind\":\"change\",\"change\":{}}\n\ndata: {\"kind\":\"snapshot\",\"snapshot\":{},\"generatedAt\":\"2026-09-02\"}\n\n")
	want := []Kind{Heartbeat, Change, Snapshot}

	for splitAt := 0; splitAt <= len(body); splitAt++ {
		t.Run("切分点", func(t *testing.T) {
			capture := validCapture(nil)
			capture.Chunks = [][]byte{body[:splitAt], body[splitAt:]}

			frames, err := Parse(capture)
			if err != nil {
				t.Fatalf("切分点 %d 的 Parse() error = %v", splitAt, err)
			}
			if got := frameKinds(frames); !sameKinds(got, want) {
				t.Fatalf("切分点 %d 的帧顺序 = %v，期望 %v", splitAt, got, want)
			}
		})
	}
}

func TestParse接受分片边界与CRLF(t *testing.T) {
	t.Run("nil Chunks 继续读取 Body", func(t *testing.T) {
		frames, err := Parse(validCapture([]byte("data: {\"kind\":\"change\",\"change\":{}}\n\n")))
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		if got, want := frameKinds(frames), []Kind{Change}; !sameKinds(got, want) {
			t.Fatalf("帧顺序 = %v，期望 %v", got, want)
		}
	})

	t.Run("空 Chunks 是空字节流", func(t *testing.T) {
		capture := validCapture(nil)
		capture.Chunks = [][]byte{}

		frames, err := Parse(capture)
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		if len(frames) != 0 {
			t.Fatalf("空字节流帧数 = %d，期望 0", len(frames))
		}
	})

	t.Run("CRLF 恰好跨 Chunks", func(t *testing.T) {
		capture := validCapture(nil)
		capture.Chunks = [][]byte{
			[]byte("data: {\"kind\":\"change\",\"change\":{}}\r"),
			[]byte("\n\r"),
			[]byte("\n"),
		}

		frames, err := Parse(capture)
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		if got, want := frameKinds(frames), []Kind{Change}; !sameKinds(got, want) {
			t.Fatalf("帧顺序 = %v，期望 %v", got, want)
		}
	})

	t.Run("Body 与 Chunks 互斥", func(t *testing.T) {
		capture := validCapture([]byte("body-private-secret"))
		capture.Chunks = [][]byte{[]byte("chunk-private-secret")}

		assertSafeError(t, parseError(capture), ErrAmbiguousBody, "body-private-secret", "chunk-private-secret")
	})
}

func TestParse使用默认资源上限且不泄露载荷(t *testing.T) {
	limits := DefaultLimits()
	frame := []byte("data: {\"kind\":\"change\",\"change\":{},\"secret\":\"frame-limit-secret\"}\n\n")
	testCases := []struct {
		name          string
		capture       Capture
		want          error
		privateValues []string
	}{
		{
			name: "Body 超过总字节上限",
			capture: validCapture(append(
				[]byte("body-limit-secret"),
				bytes.Repeat([]byte("x"), limits.MaxCaptureBytes)...,
			)),
			want:          ErrCaptureTooLarge,
			privateValues: []string{"body-limit-secret"},
		},
		{
			name: "Chunks 在拼接前超过总字节上限",
			capture: Capture{
				StatusCode: http.StatusOK,
				Header:     validCapture(nil).Header,
				Chunks: [][]byte{
					[]byte("chunk-limit-secret"),
					bytes.Repeat([]byte("x"), limits.MaxCaptureBytes),
				},
			},
			want:          ErrCaptureTooLarge,
			privateValues: []string{"chunk-limit-secret"},
		},
		{
			name: "单行超过上限",
			capture: validCapture(append(
				[]byte("line-limit-secret"),
				append(bytes.Repeat([]byte("x"), limits.MaxLineBytes), '\n')...,
			)),
			want:          ErrLineTooLong,
			privateValues: []string{"line-limit-secret"},
		},
		{
			name:          "帧数超过上限",
			capture:       validCapture(bytes.Repeat(frame, limits.MaxFrames+1)),
			want:          ErrFrameLimitExceeded,
			privateValues: []string{"frame-limit-secret"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assertSafeError(t, parseError(testCase.capture), testCase.want, testCase.privateValues...)
		})
	}
}

func TestParseWithLimits拒绝不明确资源配置(t *testing.T) {
	testCases := []Limits{
		{},
		{MaxCaptureBytes: 1, MaxLineBytes: 1, MaxFrames: 0},
		{MaxCaptureBytes: 1, MaxLineBytes: -1, MaxFrames: 1},
		{MaxCaptureBytes: 1, MaxLineBytes: 2, MaxFrames: 1},
	}

	for _, limits := range testCases {
		_, err := ParseWithLimits(validCapture([]byte("data: {\"kind\":\"change\",\"change\":{}}\n\n")), limits)
		assertSafeError(t, err, ErrInvalidLimits, "change")
	}
}

func TestParse允许Change先于Snapshot(t *testing.T) {
	capture := validCapture([]byte("data: {\"kind\":\"change\",\"change\":{}}\n\ndata: {\"kind\":\"snapshot\",\"snapshot\":{},\"generatedAt\":\"2026-09-02\"}\n\n"))

	frames, err := Parse(capture)
	if err != nil {
		t.Fatalf("change 可以先于 snapshot：%v", err)
	}
	if got, want := frameKinds(frames), []Kind{Change, Snapshot}; !sameKinds(got, want) {
		t.Fatalf("帧顺序 = %v，期望 %v", got, want)
	}
}

func TestParse接受SnapshotChange与暂态Error(t *testing.T) {
	capture := validCapture([]byte("data: {\"kind\":\"snapshot\",\"snapshot\":[],\"generatedAt\":\"2026-09-02\"}\n\ndata: {\"kind\":\"change\",\"change\":{\"id\":\"public-shape-only\"}}\n\ndata: {\"kind\":\"error\",\"transient\":true}\n\n"))

	frames, err := Parse(capture)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := frameKinds(frames), []Kind{Snapshot, Change, Error}; !sameKinds(got, want) {
		t.Fatalf("帧顺序 = %v，期望 %v", got, want)
	}
}

func TestParse接受CRLF(t *testing.T) {
	capture := validCapture([]byte("data: {\"kind\":\"change\",\"change\":{}}\r\n\r\n"))

	if _, err := Parse(capture); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestParse拒绝未知Kind与非暂态Error且不泄露载荷(t *testing.T) {
	testCases := []struct {
		name string
		body string
	}{
		{
			name: "未知 kind",
			body: "data: {\"kind\":\"private-unknown-kind\",\"secret\":\"unknown-payload-secret\"}\n\n",
		},
		{
			name: "非暂态 error",
			body: "data: {\"kind\":\"error\",\"transient\":false,\"secret\":\"error-payload-secret\"}\n\n",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := Validate(validCapture([]byte(testCase.body)))
			if err == nil {
				t.Fatal("Validate() error = nil，期望合同校验失败")
			}
			for _, privateValue := range []string{"private-unknown-kind", "unknown-payload-secret", "error-payload-secret"} {
				if strings.Contains(err.Error(), privateValue) {
					t.Fatalf("错误不得泄露 SSE 载荷：%q", err)
				}
			}
		})
	}
}

func TestParse拒绝非对象与JSON尾随内容且不泄露载荷(t *testing.T) {
	testCases := []struct {
		name string
		body string
	}{
		{
			name: "JSON 数组不是对象",
			body: "data: [\"non-object-payload-secret\"]\n\n",
		},
		{
			name: "JSON 后存在尾随内容",
			body: "data: {\"kind\":\"change\",\"change\":{}} trailing-payload-secret\n\n",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := Validate(validCapture([]byte(testCase.body)))
			if err == nil {
				t.Fatal("Validate() error = nil，期望 JSON 结构校验失败")
			}
			for _, privateValue := range []string{"non-object-payload-secret", "trailing-payload-secret"} {
				if strings.Contains(err.Error(), privateValue) {
					t.Fatalf("错误不得泄露 SSE 载荷：%q", err)
				}
			}
		})
	}
}

func TestParse拒绝缺失或非法业务字段且不泄露载荷(t *testing.T) {
	testCases := []struct {
		name string
		body string
	}{
		{
			name: "snapshot 缺少 snapshot",
			body: "data: {\"kind\":\"snapshot\",\"generatedAt\":\"2026-09-02\",\"secret\":\"snapshot-missing-secret\"}\n\n",
		},
		{
			name: "snapshot 缺少 generatedAt",
			body: "data: {\"kind\":\"snapshot\",\"snapshot\":{},\"secret\":\"generated-at-missing-secret\"}\n\n",
		},
		{
			name: "snapshot 的 generatedAt 为空",
			body: "data: {\"kind\":\"snapshot\",\"snapshot\":{},\"generatedAt\":\"\",\"secret\":\"generated-at-empty-secret\"}\n\n",
		},
		{
			name: "change 缺少 change",
			body: "data: {\"kind\":\"change\",\"secret\":\"change-missing-secret\"}\n\n",
		},
		{
			name: "change 不是对象",
			body: "data: {\"kind\":\"change\",\"change\":\"change-non-object-secret\"}\n\n",
		},
		{
			name: "error 缺少 transient",
			body: "data: {\"kind\":\"error\",\"secret\":\"transient-missing-secret\"}\n\n",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := Validate(validCapture([]byte(testCase.body)))
			if err == nil {
				t.Fatal("Validate() error = nil，期望业务字段合同校验失败")
			}
			for _, privateValue := range []string{
				"snapshot-missing-secret",
				"generated-at-missing-secret",
				"generated-at-empty-secret",
				"change-missing-secret",
				"change-non-object-secret",
				"transient-missing-secret",
			} {
				if strings.Contains(err.Error(), privateValue) {
					t.Fatalf("错误不得泄露 SSE 载荷：%q", err)
				}
			}
		})
	}
}

func TestParse拒绝未终止帧且不泄露载荷(t *testing.T) {
	const privatePayload = "unterminated-payload-secret"
	err := Validate(validCapture([]byte("data: {\"kind\":\"change\",\"change\":{\"secret\":\"" + privatePayload + "\"}}")))
	if err == nil {
		t.Fatal("Validate() error = nil，期望未终止帧被拒绝")
	}
	if strings.Contains(err.Error(), privatePayload) {
		t.Fatalf("错误不得泄露未终止帧正文：%q", err)
	}
}

func TestParse拒绝缺失或错误公开头且不泄露私有值(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(http.Header)
	}{
		{
			name: "缺少 Cache-Control",
			mutate: func(header http.Header) {
				header.Del("Cache-Control")
			},
		},
		{
			name: "错误 X-Accel-Buffering",
			mutate: func(header http.Header) {
				header.Set("X-Accel-Buffering", "private-buffering-value")
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			capture := validCapture([]byte("data: {\"kind\":\"change\",\"change\":{}}\n\n"))
			testCase.mutate(capture.Header)

			err := Validate(capture)
			if err == nil {
				t.Fatal("Validate() error = nil，期望响应头合同校验失败")
			}
			if strings.Contains(err.Error(), "private-buffering-value") {
				t.Fatalf("错误不得泄露响应头私有值：%q", err)
			}
		})
	}
}

func TestParse拒绝混合重复与未知SSE字段(t *testing.T) {
	testCases := []struct {
		name string
		body string
	}{
		{
			name: "混合 comment 与 data",
			body: ": heartbeat 2026-09-02\ndata: {\"kind\":\"change\",\"change\":{}}\n\n",
		},
		{
			name: "重复 data",
			body: "data: {\"kind\":\"change\",\"change\":{}}\ndata: {\"kind\":\"change\",\"change\":{}}\n\n",
		},
		{
			name: "未知 SSE 字段",
			body: "event: private-event\n\n",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := Validate(validCapture([]byte(testCase.body))); err == nil {
				t.Fatal("Validate() error = nil，期望 SSE 帧字段校验失败")
			}
		})
	}
}

func validCapture(body []byte) Capture {
	return Capture{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":      {"text/event-stream"},
			"Cache-Control":     {"no-cache, no-transform"},
			"Connection":        {"keep-alive"},
			"X-Accel-Buffering": {"no"},
		},
		Body: body,
	}
}

func stringChunks(chunks ...string) [][]byte {
	result := make([][]byte, len(chunks))
	for index, chunk := range chunks {
		result[index] = []byte(chunk)
	}
	return result
}

func frameKinds(frames []Frame) []Kind {
	kinds := make([]Kind, len(frames))
	for index, frame := range frames {
		kinds[index] = frame.Kind
	}
	return kinds
}

func sameKinds(got, want []Kind) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func parseError(capture Capture) error {
	_, err := Parse(capture)
	return err
}

func assertSafeError(t *testing.T, err, want error, privateValues ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil，期望合同校验失败")
	}
	if !errors.Is(err, want) {
		t.Fatalf("error = %q，errors.Is(_, %v) = false", err, want)
	}
	for _, privateValue := range privateValues {
		if strings.Contains(err.Error(), privateValue) {
			t.Fatalf("错误不得泄露私有值：%q", err)
		}
	}
}
