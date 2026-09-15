package works

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Cursor 是按创建时间和作品 ID 排序的稳定分页位置。
type Cursor struct {
	CreatedAt time.Time
	WorkID    string
}

type cursorPayload struct {
	CreatedAt string `json:"created_at"`
	WorkID    string `json:"work_id"`
}

// EncodeCursor 编码不透明作品游标，客户端只能原样回传。
func EncodeCursor(createdAt time.Time, workID string) (string, error) {
	if createdAt.IsZero() || strings.TrimSpace(workID) == "" {
		return "", ErrInvalidQuery
	}
	payload, err := json.Marshal(cursorPayload{CreatedAt: createdAt.UTC().Format(time.RFC3339Nano), WorkID: workID})
	if err != nil {
		return "", ErrInvalidQuery
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// ParseCursor 解析并校验客户端回传的复合游标。
func ParseCursor(raw string) (Cursor, error) {
	if strings.TrimSpace(raw) == "" {
		return Cursor{}, ErrInvalidQuery
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Cursor{}, ErrInvalidQuery
	}
	var payload cursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil || strings.TrimSpace(payload.WorkID) == "" {
		return Cursor{}, ErrInvalidQuery
	}
	createdAt, err := time.Parse(time.RFC3339Nano, payload.CreatedAt)
	if err != nil || createdAt.IsZero() {
		return Cursor{}, ErrInvalidQuery
	}
	return Cursor{CreatedAt: createdAt.UTC(), WorkID: payload.WorkID}, nil
}
