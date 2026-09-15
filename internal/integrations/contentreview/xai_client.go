// Package contentreview 提供 Go 自有的内容审核供应商适配器。
package contentreview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	bizreview "ai-business-service/internal/biz/contentreview"
)

const reviewPath = "chat/completions"

// Client 通过兼容 OpenAI Chat Completions 的受控端点审核文本生成请求。
// 它独立于 Node 服务，不读取 Node 运行时环境或登录态。
type Client struct {
	baseURL    *url.URL
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewClient 使用受控依赖创建审核客户端。baseURL 只允许根路径或 /v1 API 前缀。
func NewClient(baseURL, apiKey, model string, timeout time.Duration, httpClient *http.Client) (*Client, error) {
	parsed, err := parseBaseURL(baseURL)
	if err != nil || strings.TrimSpace(apiKey) == "" || strings.TrimSpace(model) == "" || timeout <= 0 || httpClient == nil {
		return nil, bizreview.ErrUnavailable
	}
	controlledClient := *httpClient
	controlledClient.Timeout = timeout
	controlledClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		baseURL:    parsed,
		apiKey:     strings.TrimSpace(apiKey),
		model:      strings.TrimSpace(model),
		httpClient: &controlledClient,
	}, nil
}

// newConfiguredHTTPClient 为运行时配置装配一份不带全局共享状态的 HTTP 客户端。
func newConfiguredHTTPClient() *http.Client {
	return &http.Client{}
}

// Review 提交最小提示词载荷，并把供应商结果收敛为允许、拒绝或不可用。
func (client *Client) Review(ctx context.Context, request bizreview.Request) (bizreview.Decision, error) {
	if client == nil || client.baseURL == nil || client.httpClient == nil {
		return bizreview.Decision{}, bizreview.ErrUnavailable
	}
	body, err := json.Marshal(reviewPayload{
		Model: client.model,
		Messages: []reviewMessage{
			{Role: "system", Content: "Classify whether the supplied generation prompt is safe for a review-restricted audience. Reply with JSON only: {\"allowed\": true|false}."},
			{Role: "user", Content: reviewInput{Prompt: request.Prompt, NegativePrompt: request.NegativePrompt}},
		},
		Temperature: 0,
	})
	if err != nil {
		return bizreview.Decision{}, bizreview.ErrUnavailable
	}

	target := *client.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/" + reviewPath
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return bizreview.Decision{}, bizreview.ErrUnavailable
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+client.apiKey)

	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return bizreview.Decision{}, bizreview.ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return bizreview.Decision{}, bizreview.ErrUnavailable
	}

	decision, err := decodeDecision(response.Body)
	if err != nil {
		return bizreview.Decision{}, bizreview.ErrUnavailable
	}
	return decision, nil
}

type reviewInput struct {
	Prompt         string `json:"prompt"`
	NegativePrompt string `json:"negativePrompt,omitempty"`
}

type reviewMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type reviewPayload struct {
	Model       string          `json:"model"`
	Messages    []reviewMessage `json:"messages"`
	Temperature int             `json:"temperature"`
}

type reviewResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func decodeDecision(reader io.Reader) (bizreview.Decision, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<10))
	var response reviewResponse
	if err := decoder.Decode(&response); err != nil || len(response.Choices) != 1 {
		return bizreview.Decision{}, errors.New("invalid review response")
	}
	var result struct {
		Allowed *bool `json:"allowed"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(response.Choices[0].Message.Content)), &result); err != nil || result.Allowed == nil {
		return bizreview.Decision{}, errors.New("invalid review decision")
	}
	decision := bizreview.Decision{Outcome: bizreview.OutcomeRejected}
	if *result.Allowed {
		decision.Outcome = bizreview.OutcomeAllowed
	}
	if err := decision.Validate(); err != nil {
		return bizreview.Decision{}, err
	}
	return decision, nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	normalized := strings.TrimSpace(raw)
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("invalid content review base url")
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if path != "" && path != "/v1" {
		return nil, errors.New("invalid content review base path")
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed, nil
}

var _ bizreview.Reviewer = (*Client)(nil)
