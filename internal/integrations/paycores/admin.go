package paycores

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrAdminNotConfigured = errors.New("paycores admin client not configured")
	ErrAdminUnavailable   = errors.New("paycores admin upstream unavailable")
)

const adminChannelsPath = "/api/admin/channels-overview"

// AdminClient implements the legacy Node -> PayCores read-only admin GET
// contract. PayCores signs req.path (not the /api/admin mount prefix).
type AdminClient struct {
	baseURL *url.URL
	key     []byte
	http    *http.Client
	now     func() time.Time
}

func NewAdminClient(baseURL, key string, client *http.Client, now func() time.Time) (*AdminClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || len(strings.TrimSpace(key)) < minimumRequestKeyLength || now == nil {
		return nil, ErrAdminNotConfigured
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &AdminClient{baseURL: parsed, key: []byte(strings.TrimSpace(key)), http: client, now: now}, nil
}

// ChannelsOverview returns PayCores' actual data object, including channel
// availability and policy fields. Missing or malformed data must not look like
// an empty (successfully configured) channel list.
func (client *AdminClient) ChannelsOverview(ctx context.Context) (json.RawMessage, error) {
	if client == nil || client.baseURL == nil || len(client.key) < minimumRequestKeyLength || client.http == nil || client.now == nil {
		return nil, ErrAdminNotConfigured
	}
	timestamp := strconv.FormatInt(client.now().UTC().UnixMilli(), 10)
	mac := hmac.New(sha256.New, client.key)
	_, _ = io.WriteString(mac, "/channels-overview:"+timestamp)
	endpoint := client.baseURL.ResolveReference(&url.URL{Path: adminChannelsPath})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, ErrAdminUnavailable
	}
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(req)
	if err != nil {
		return nil, ErrAdminUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrAdminUnavailable
	}
	const maxAdminResponseBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAdminResponseBytes+1))
	if err != nil || len(body) > maxAdminResponseBytes {
		return nil, ErrAdminUnavailable
	}
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || !envelope.Success || len(envelope.Data) == 0 {
		return nil, ErrAdminUnavailable
	}
	var overview struct {
		Channels []json.RawMessage `json:"channels"`
	}
	if err := json.Unmarshal(envelope.Data, &overview); err != nil || overview.Channels == nil {
		return nil, ErrAdminUnavailable
	}
	return envelope.Data, nil
}
