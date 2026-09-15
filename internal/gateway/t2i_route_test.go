package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 只有 inputImages 完全缺失的普通生成请求才属于首个 Go T2I 切片。
func TestClassifyT2ICreateCandidate只接受纯文生图请求(t *testing.T) {
	testCases := []struct {
		name string
		body string
		want bool
	}{
		{name: "最小纯文生图", body: `{"prompt":"portrait"}`, want: true},
		{name: "允许 generate 操作", body: `{"prompt":"portrait","operation":"generate"}`, want: true},
		{name: "允许空预设标签", body: `{"prompt":"portrait","presetTags":[]}`, want: true},
		{name: "允许关闭提示词优化", body: `{"prompt":"portrait","optimizePrompt":false}`, want: true},
		{name: "尺寸参数暂归 Node", body: `{"prompt":"portrait","width":1024,"height":1024}`, want: false},
		{name: "空图片数组仍归 Node", body: `{"prompt":"portrait","inputImages":[]}`, want: false},
		{name: "空图片字段仍归 Node", body: `{"prompt":"portrait","inputImages":null}`, want: false},
		{name: "模板语义归 Node", body: `{"prompt":"portrait","templateId":"template-1"}`, want: false},
		{name: "聊天关联归 Node", body: `{"prompt":"portrait","messageId":"message-1"}`, want: false},
		{name: "未知字段归 Node", body: `{"prompt":"portrait","model":"client-selected"}`, want: false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, t2iCreatePath, bytes.NewBufferString(testCase.body))
			if got := isT2ICreateCandidate(request); got != testCase.want {
				t.Fatalf("isT2ICreateCandidate() = %t, want %t", got, testCase.want)
			}
			if body, err := io.ReadAll(request.Body); err != nil || string(body) != testCase.body {
				t.Fatalf("候选分类破坏了代理所需请求体: %q / %v", body, err)
			}
		})
	}
}

func TestClassifyImageEditCreateCandidate接受前端冻结的模板编辑字段(t *testing.T) {
	testCases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "前端模板换装请求",
			body: `{"prompt":"本地 I2I 模板验证","operation":"generate","inputImages":["https://uploads.example/source.png"],"optimizePrompt":false,"templateId":"local-image-edit-dress-up","aspectRatio":"9:16","presetTags":["local-image-edit-dress-up"]}`,
			want: true,
		},
		{
			name: "标签与模板不一致",
			body: `{"prompt":"本地 I2I 模板验证","operation":"generate","inputImages":["https://uploads.example/source.png"],"optimizePrompt":false,"templateId":"local-image-edit-dress-up","aspectRatio":"9:16","presetTags":["other-template"]}`,
			want: false,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, t2iCreatePath, bytes.NewBufferString(testCase.body))
			if got := isImageEditCreateCandidate(request); got != testCase.want {
				t.Fatalf("isImageEditCreateCandidate() = %t, want %t", got, testCase.want)
			}
			if body, err := io.ReadAll(request.Body); err != nil || string(body) != testCase.body {
				t.Fatalf("候选分类破坏了代理所需请求体: %q / %v", body, err)
			}
		})
	}
}

// 路径、方法和 query 均必须是冻结的字面量形态，避免前缀或编码路径被错误切流。
func TestMatchT2ILocalRoute只接受精确路由(t *testing.T) {
	testCases := []struct {
		name   string
		method string
		target string
		want   t2iRoute
	}{
		{name: "创建", method: http.MethodPost, target: t2iCreatePath, want: t2iRouteCreate},
		{name: "批量状态", method: http.MethodPost, target: t2iStatusesPath, want: t2iRouteStatuses},
		{name: "单条详情", method: http.MethodGet, target: "/api/images/550e8400-e29b-41d4-a716-446655440000", want: t2iRouteDetail},
		{name: "尾部斜杠", method: http.MethodPost, target: t2iCreatePath + "/", want: t2iRouteNone},
		{name: "携带 query", method: http.MethodPost, target: t2iCreatePath + "?source=web", want: t2iRouteNone},
		{name: "编码路径", method: http.MethodPost, target: "/api/chat%2fimage/async", want: t2iRouteNone},
		{name: "错误方法", method: http.MethodGet, target: t2iCreatePath, want: t2iRouteNone},
		{name: "Node ObjectId 详情", method: http.MethodGet, target: "/api/images/507f1f77bcf86cd799439011", want: t2iRouteNone},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.target, nil)
			if got := matchT2ILocalRoute(request); got != testCase.want {
				t.Fatalf("matchT2ILocalRoute() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// 命中 Go 开关和纯 T2I 候选后必须直达本地 Handler，绝不能再请求 Node 造成双预扣。
func TestHandler将纯T2I直接交给本地处理器(t *testing.T) {
	nodeCalls := 0
	node := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		nodeCalls++
		_, _ = writer.Write([]byte(`node`))
	}))
	defer node.Close()

	localCalls := 0
	gateway := New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch:     enabledRouteSwitch{enabled: exactRouteKey{method: http.MethodPost, path: t2iCreatePath}},
		T2IHandler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			localCalls++
			if request.Header.Get("X-Request-Id") == "" {
				t.Fatal("本地处理器缺少入口请求 ID")
			}
			writer.WriteHeader(http.StatusAccepted)
		}),
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, t2iCreatePath, bytes.NewBufferString(`{"prompt":"portrait"}`))
	gateway.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted || localCalls != 1 || nodeCalls != 0 {
		t.Fatalf("status/local/node = %d/%d/%d, want 202/1/0", recorder.Code, localCalls, nodeCalls)
	}
}

// 非候选请求即使路由开关开启，也必须保持 Node 负责的 I2I 语义。
func TestHandler将带InputImages的请求继续代理Node(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"prompt":"portrait","inputImages":[]}` {
			t.Fatalf("Node 收到的请求体 = %q", body)
		}
		_, _ = writer.Write([]byte(`node`))
	}))
	defer node.Close()

	gateway := New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch:     enabledRouteSwitch{enabled: exactRouteKey{method: http.MethodPost, path: t2iCreatePath}},
		T2IHandler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("I2I 请求不得进入 Go T2I Handler")
		}),
	})
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, t2iCreatePath, bytes.NewBufferString(`{"prompt":"portrait","inputImages":[]}`)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "node" {
		t.Fatalf("响应 = %d %q, want Node fallback", recorder.Code, recorder.Body.String())
	}
}
