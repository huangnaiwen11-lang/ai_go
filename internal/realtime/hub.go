// Package realtime 提供用户生成状态的本地 SSE 合同层。
// 事件源使用进程内 Hub，后续可替换为 Redis 适配器；HTTP framing 和鉴权边界保持不变。
package realtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ai-business-service/internal/transport/sessionauth"
)

const StreamPath = "/api/users/me/generations/stream"

var errUnauthenticated = errors.New("unauthenticated")

type Event struct {
	ID   string
	Type string
	Data []byte
}

// StreamSource 是 SSE 传输层依赖的最小事件源合同。
// 进程内 Hub 和未来的 Redis 适配器都必须实现该接口，避免 HTTP 层绑定具体消息系统。
type StreamSource interface {
	SubscribeSince(userID, lastID string) (<-chan Event, func())
}

type Hub struct {
	mu         sync.RWMutex
	bufferSize int
	historySize int
	nextID     map[string]uint64
	subs       map[string]map[chan Event]struct{}
	history    map[string][]Event
}

func NewHub(bufferSize int) *Hub {
	if bufferSize < 1 {
		bufferSize = 1
	}
	return &Hub{bufferSize: bufferSize, historySize: bufferSize * 4, nextID: make(map[string]uint64), subs: make(map[string]map[chan Event]struct{}), history: make(map[string][]Event)}
}

func (hub *Hub) Subscribe(userID string) (<-chan Event, func()) {
	return hub.SubscribeSince(userID, "")
}

// SubscribeSince 订阅用户事件，并在连接建立时重放 Last-Event-ID 之后的有限历史。
func (hub *Hub) SubscribeSince(userID, lastID string) (<-chan Event, func()) {
	channel := make(chan Event, hub.bufferSize)
	hub.mu.Lock()
	if hub.subs[userID] == nil {
		hub.subs[userID] = make(map[chan Event]struct{})
	}
	hub.subs[userID][channel] = struct{}{}
	if lastID != "" {
		for _, event := range eventsAfter(hub.history[userID], lastID) {
			select {
			case channel <- event:
			default:
			}
		}
	}
	hub.mu.Unlock()
	return channel, func() {
		hub.mu.Lock()
		if subscribers := hub.subs[userID]; subscribers != nil {
			delete(subscribers, channel)
			if len(subscribers) == 0 {
				delete(hub.subs, userID)
			}
		}
		hub.mu.Unlock()
	}
}

func (hub *Hub) Publish(userID string, event Event) {
	if hub == nil || strings.TrimSpace(userID) == "" {
		return
	}
	hub.mu.Lock()
	hub.nextID[userID]++
	if strings.TrimSpace(event.ID) == "" {
		event.ID = fmt.Sprintf("%s-%d", userID, hub.nextID[userID])
	}
	hub.history[userID] = append(hub.history[userID], event)
	if len(hub.history[userID]) > hub.historySize {
		hub.history[userID] = hub.history[userID][len(hub.history[userID])-hub.historySize:]
	}
	for subscriber := range hub.subs[userID] {
		select {
		case subscriber <- event:
		default:
			// 慢客户端不阻塞生成回调；断线客户端会通过下一次状态读取恢复。
		}
	}
	hub.mu.Unlock()
}

func eventsAfter(history []Event, lastID string) []Event {
	for index, event := range history {
		if event.ID == lastID {
			return history[index+1:]
		}
	}
	return nil
}

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type Handler struct {
	authenticator authenticator
	stream        StreamSource
	heartbeat     time.Duration
}

func NewHandler(authenticator authenticator, hub *Hub, heartbeat time.Duration) http.Handler {
	return NewHandlerWithStream(authenticator, hub, heartbeat)
}

// NewHandlerWithStream 使用抽象事件源装配 SSE，保持现有构造函数兼容。
func NewHandlerWithStream(authenticator authenticator, stream StreamSource, heartbeat time.Duration) http.Handler {
	if heartbeat <= 0 {
		heartbeat = 15 * time.Second
	}
	return &Handler{authenticator: authenticator, stream: stream, heartbeat: heartbeat}
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.Method != http.MethodGet || request.URL == nil || request.URL.Path != StreamPath {
		writeError(writer, http.StatusNotFound, "NOT_FOUND")
		return
	}
	if handler == nil || handler.authenticator == nil || handler.stream == nil {
		writeError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
		return
	}
	identity, err := handler.authenticator.Authenticate(request)
	if err != nil || identity == nil || strings.TrimSpace(identity.UserID) == "" {
		writeError(writer, http.StatusUnauthorized, "UNAUTHENTICATED")
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "SSE_UNSUPPORTED")
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	channel, unsubscribe := handler.stream.SubscribeSince(identity.UserID, request.Header.Get("Last-Event-ID"))
	defer unsubscribe()
	writeEvent(writer, Event{Type: "ready", Data: []byte(`{"connected":true}`)})
	flusher.Flush()
	ticker := time.NewTicker(handler.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case event := <-channel:
			writeEvent(writer, event)
			flusher.Flush()
		case <-ticker.C:
			_, _ = writer.Write([]byte(": heartbeat\n\n"))
			flusher.Flush()
		}
	}
}

func writeEvent(writer http.ResponseWriter, event Event) {
	if event.Type == "" {
		event.Type = "change"
	}
	if event.ID != "" {
		_, _ = fmt.Fprintf(writer, "id: %s\n", event.ID)
	}
	_, _ = fmt.Fprintf(writer, "event: %s\n", event.Type)
	data := event.Data
	if len(data) == 0 {
		data = []byte(`{}`)
	}
	if !json.Valid(data) {
		data = []byte(`{"data":null}`)
	}
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", data)
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = fmt.Fprintf(writer, `{"success":false,"code":%q}`, code)
}

var _ http.Handler = (*Handler)(nil)
