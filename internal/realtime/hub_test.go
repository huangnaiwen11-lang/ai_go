package realtime

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/transport/sessionauth"
)

func TestHubPublishesOnlyToMatchingUser(t *testing.T) {
	hub := NewHub(8)
	left, cancelLeft := hub.Subscribe("user-left")
	defer cancelLeft()
	right, cancelRight := hub.Subscribe("user-right")
	defer cancelRight()
	hub.Publish("user-left", Event{ID: "e-1", Type: "change", Data: []byte(`{"status":"succeeded"}`)})
	select {
	case event := <-left:
		if event.ID != "e-1" || event.Type != "change" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("matching subscriber did not receive event")
	}
	select {
	case event := <-right:
		t.Fatalf("cross-user event = %#v", event)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestHubReplaysEventsAfterLastID(t *testing.T) {
	hub := NewHub(4)
	hub.Publish("user-1", Event{ID: "e-1", Type: "change", Data: []byte(`{"n":1}`)})
	hub.Publish("user-1", Event{ID: "e-2", Type: "change", Data: []byte(`{"n":2}`)})
	stream, cancel := hub.SubscribeSince("user-1", "e-1")
	defer cancel()
	event := <-stream
	if event.ID != "e-2" {
		t.Fatalf("replayed event = %#v, want e-2", event)
	}
}

func TestHandlerRejectsUnauthenticatedRequest(t *testing.T) {
	handler := NewHandler(staticAuthenticator{}, NewHub(2), 10*time.Millisecond)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, StreamPath, nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestHandlerWritesReadyFrameAndHeartbeat(t *testing.T) {
	hub := NewHub(2)
	handler := NewHandler(staticAuthenticator{userID: "user-1"}, hub, time.Millisecond)
	request := httptest.NewRequest(http.MethodGet, StreamPath, nil)
	streamContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	request = request.WithContext(streamContext)
	recorder := newFlushRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(recorder, request)
		close(done)
	}()
	time.Sleep(8 * time.Millisecond)
	hub.Publish("user-1", Event{ID: "e-2", Type: "change", Data: []byte(`{"id":"c-1"}`)})
	cancel()
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(recorder.String(), "event: change") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(recorder.String(), "event: ready") || !strings.Contains(recorder.String(), "event: change") {
		t.Fatalf("SSE body = %q", recorder.String())
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not stop after request cancellation")
	}
}

type staticAuthenticator struct{ userID string }

func (auth staticAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if auth.userID == "" {
		return nil, errUnauthenticated
	}
	return &sessionauth.AuthenticatedIdentity{UserID: auth.userID}, nil
}

type flushRecorder struct {
	header http.Header
	body   bytes.Buffer
	mu     sync.Mutex
}

func newFlushRecorder() *flushRecorder { return &flushRecorder{header: make(http.Header)} }
func (recorder *flushRecorder) Header() http.Header { return recorder.header }
func (recorder *flushRecorder) WriteHeader(int)       {}
func (recorder *flushRecorder) Write(data []byte) (int, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.body.Write(data)
}
func (recorder *flushRecorder) Flush() {}

// String 在读取测试响应时复用同一把锁，避免与 SSE 写协程产生数据竞态。
func (recorder *flushRecorder) String() string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.body.String()
}
