package walletview

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// LedgerCursor 是账本倒序分页的稳定位置。
// CreatedAt 与 EntryID 必须成对使用：单独按时间分页会遗漏同一时间戳下尚未读取的分录。
type LedgerCursor struct {
	CreatedAt time.Time
	EntryID   string
}

type ledgerCursorPayload struct {
	CreatedAt string `json:"created_at"`
	EntryID   string `json:"entry_id"`
}

// EncodeLedgerCursor 编码不透明的账本分页游标。
// 客户端只能原样回传，不能把时间或分录 ID 当作两个可分别修改的请求参数。
func EncodeLedgerCursor(createdAt time.Time, entryID string) (string, error) {
	if createdAt.IsZero() || strings.TrimSpace(entryID) == "" {
		return "", ErrInvalidWalletViewQuery
	}
	payload, err := json.Marshal(ledgerCursorPayload{
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
		EntryID:   entryID,
	})
	if err != nil {
		return "", ErrInvalidWalletViewQuery
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// ParseLedgerCursor 解析并校验客户端原样回传的复合游标。
// 无效游标属于请求参数错误，必须由领域层拒绝，不能让数据层退化为任意时间范围查询。
func ParseLedgerCursor(raw string) (LedgerCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return LedgerCursor{}, ErrInvalidWalletViewQuery
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return LedgerCursor{}, ErrInvalidWalletViewQuery
	}
	var payload ledgerCursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return LedgerCursor{}, ErrInvalidWalletViewQuery
	}
	if strings.TrimSpace(payload.EntryID) == "" {
		return LedgerCursor{}, ErrInvalidWalletViewQuery
	}
	createdAt, err := time.Parse(time.RFC3339Nano, payload.CreatedAt)
	if err != nil || createdAt.IsZero() {
		return LedgerCursor{}, ErrInvalidWalletViewQuery
	}
	return LedgerCursor{CreatedAt: createdAt.UTC(), EntryID: payload.EntryID}, nil
}
