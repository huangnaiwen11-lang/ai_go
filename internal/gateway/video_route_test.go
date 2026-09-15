package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 视频候选仅覆盖已选模板的单图和文本首帧两条路径；相邻视频业务必须保留给 Node。
func TestClassifyVideoCreateCandidate只接管两条模板路径(t *testing.T) {
	testCases := []struct {
		name string
		body string
		want bool
	}{
		{name: "单图模板", body: `{"templateId":"template-1","imageUrl":"https://uploads.example.test/source.png","durationSeconds":5}`, want: true},
		{name: "文本首帧模板", body: `{"templateId":"template-1","prompt":"海边漫步","durationSeconds":10}`, want: true},
		{name: "不传时长使用现有默认语义", body: `{"templateId":"template-1","prompt":"海边漫步"}`, want: true},
		{name: "音频暂不接管", body: `{"templateId":"template-1","prompt":"海边漫步","enableAudio":true}`, want: false},
		{name: "同时文本和图片", body: `{"templateId":"template-1","prompt":"海边漫步","imageUrl":"https://uploads.example.test/source.png"}`, want: false},
		{name: "多图语义", body: `{"templateId":"template-1","imageUrl":"https://uploads.example.test/source.png","additionalImageUrls":["https://uploads.example.test/extra.png"]}`, want: false},
		{name: "非 HTTPS 图片", body: `{"templateId":"template-1","imageUrl":"http://uploads.example.test/source.png"}`, want: false},
		{name: "缺失主机的 HTTPS 图片", body: `{"templateId":"template-1","imageUrl":"https://"}`, want: false},
		{name: "带用户信息的 HTTPS 图片", body: `{"templateId":"template-1","imageUrl":"https://user:pass@uploads.example.test/source.png"}`, want: false},
		{name: "带片段的 HTTPS 图片", body: `{"templateId":"template-1","imageUrl":"https://uploads.example.test/source.png#fragment"}`, want: false},
		{name: "客户端模型", body: `{"templateId":"template-1","prompt":"海边漫步","model":"client-selected"}`, want: false},
		{name: "LoRA", body: `{"templateId":"template-1","prompt":"海边漫步","loraFilename":"x.safetensors"}`, want: false},
		{name: "工作流", body: `{"templateId":"template-1","prompt":"海边漫步","videoWorkflowMode":"motion-control"}`, want: false},
		{name: "聊天关联", body: `{"templateId":"template-1","prompt":"海边漫步","messageId":"message-1"}`, want: false},
		{name: "未知字段", body: `{"templateId":"template-1","prompt":"海边漫步","unknown":true}`, want: false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, videoCreatePath, bytes.NewBufferString(testCase.body))
			if got := isVideoCreateCandidate(request); got != testCase.want {
				t.Fatalf("isVideoCreateCandidate() = %t, want %t", got, testCase.want)
			}
			if body, err := io.ReadAll(request.Body); err != nil || string(body) != testCase.body {
				t.Fatalf("候选分类破坏了 Node 代理请求体: %q / %v", body, err)
			}
		})
	}
}

// 路径、方法、query 与编码形态均必须是固定字面量，不能扩张为前缀切流。
func TestMatchVideoLocalRoute只接受精确路由(t *testing.T) {
	testCases := []struct {
		name   string
		method string
		target string
		want   videoRoute
	}{
		{name: "创建", method: http.MethodPost, target: videoCreatePath, want: videoRouteCreate},
		{name: "批量状态", method: http.MethodPost, target: videoStatusesPath, want: videoRouteStatuses},
		{name: "单条状态", method: http.MethodGet, target: "/api/chat/video/550e8400-e29b-41d4-a716-446655440201", want: videoRouteDetail},
		{name: "携带 query", method: http.MethodPost, target: videoCreatePath + "?source=web", want: videoRouteNone},
		{name: "编码路径", method: http.MethodPost, target: "/api/chat%2fvideo", want: videoRouteNone},
		{name: "HD 相邻路径", method: http.MethodPost, target: "/api/chat/video/550e8400-e29b-41d4-a716-446655440201/hd", want: videoRouteNone},
		{name: "stream 相邻路径", method: http.MethodGet, target: "/api/chat/video/550e8400-e29b-41d4-a716-446655440201/stream", want: videoRouteNone},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := matchVideoLocalRoute(httptest.NewRequest(testCase.method, testCase.target, nil)); got != testCase.want {
				t.Fatalf("matchVideoLocalRoute() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// 本地候选进入 Handler 后不得回退 Node，避免预扣和任务创建被双写。
func TestHandler将视频模板候选直接交给本地处理器(t *testing.T) {
	nodeCalls := 0
	node := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		nodeCalls++
		_, _ = writer.Write([]byte("node"))
	}))
	defer node.Close()
	localCalls := 0
	gateway := New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch:     videoEnabledRouteSwitch{},
		VideoHandler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			localCalls++
			if request.Header.Get("X-Request-Id") == "" {
				t.Fatal("本地视频处理器缺少入口请求 ID")
			}
			writer.WriteHeader(http.StatusAccepted)
		}),
	})

	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, videoCreatePath, bytes.NewBufferString(`{"templateId":"template-1","prompt":"海边漫步"}`)))
	if recorder.Code != http.StatusAccepted || localCalls != 1 || nodeCalls != 0 {
		t.Fatalf("status/local/node = %d/%d/%d, want 202/1/0", recorder.Code, localCalls, nodeCalls)
	}
}

// 不被支持的音频和相邻复杂视频请求必须完整代理给 Node。
func TestHandler将非候选视频请求继续代理Node(t *testing.T) {
	const body = `{"templateId":"template-1","prompt":"海边漫步","enableAudio":true}`
	node := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ := io.ReadAll(request.Body)
		if string(received) != body {
			t.Fatalf("Node 收到的请求体 = %q", received)
		}
		_, _ = writer.Write([]byte("node"))
	}))
	defer node.Close()
	gateway := New(Config{
		DefaultUpstream: mustURL(t, node.URL), RouteSwitch: videoEnabledRouteSwitch{},
		VideoHandler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("enableAudio=true 不得进入 Go Handler") }),
	})
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, videoCreatePath, bytes.NewBufferString(body)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "node" {
		t.Fatalf("响应 = %d %q, want Node fallback", recorder.Code, recorder.Body.String())
	}
}

type videoEnabledRouteSwitch struct{}

func (videoEnabledRouteSwitch) Enabled(route exactRouteKey) bool {
	return route == (exactRouteKey{method: http.MethodPost, path: videoCreatePath})
}
