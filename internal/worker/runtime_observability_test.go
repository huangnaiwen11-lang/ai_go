package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/outbox"
)

func TestRuntimeObservabilityExposesOnlySafeMetricsAfterReady(t *testing.T) {
	observability, err := NewRuntimeObservability("127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRuntimeObservability() error = %v", err)
	}
	if err := observability.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := observability.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	})

	baseURL := "http://" + observability.listener.Addr().String()
	assertHTTPStatus(t, baseURL+runtimeHealthPath, http.StatusOK)
	assertHTTPStatus(t, baseURL+runtimeReadyPath, http.StatusServiceUnavailable)

	// The raw error deliberately contains all label classes prohibited by the
	// B2B contract. Metrics may only expose the fixed "internal" class.
	unsafeError := errors.New("https://provider.example/result?api_key=secret prompt=private event=event-123")
	observability.ObserveUnit(WorkerUnit("event-123"), time.Now().Add(-20*time.Millisecond), unsafeError)
	observability.ObserveTransition(outbox.EventType("generation.result-materialize"), "needs_attention", outbox.AttentionReasonMaterialUploadBudget)
	observability.ObserveBacklog(outbox.BacklogSnapshot{
		Pending:                  2,
		NeedsAttention:           1,
		OldestActiveCreatedAt:    time.Now().Add(-5 * time.Minute),
		OldestAttentionCreatedAt: time.Now().Add(-10 * time.Minute),
	}, time.Now().UTC())
	observability.MarkReady()

	assertHTTPStatus(t, baseURL+runtimeReadyPath, http.StatusOK)
	response, err := http.Get(baseURL + runtimeMetricsPath)
	if err != nil {
		t.Fatalf("GET metrics error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET metrics status = %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll(metrics) error = %v", err)
	}
	metrics := string(body)
	for _, expected := range []string{
		"cling_generation_worker_ready 1",
		`unit="unknown"`,
		`error_class="internal"`,
		`event_type="generation.result-materialize"`,
		`attention_reason="provider_result_material_upload_budget_exhausted"`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Errorf("metrics does not contain %q:\n%s", expected, metrics)
		}
	}
	for _, forbidden := range []string{"https://provider.example", "api_key", "secret", "private", "event-123", "prompt="} {
		if strings.Contains(metrics, forbidden) {
			t.Errorf("metrics leaked forbidden content %q:\n%s", forbidden, metrics)
		}
	}
}

func TestRuntimeObservabilityRejectsNonLoopbackAddress(t *testing.T) {
	if _, err := NewRuntimeObservability("0.0.0.0:19082", nil); err == nil {
		t.Fatal("NewRuntimeObservability() accepted a public bind address")
	}
}

func assertHTTPStatus(t *testing.T, rawURL string, want int) {
	t.Helper()
	response, err := http.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s error = %v", rawURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("GET %s status = %d, want %d", rawURL, response.StatusCode, want)
	}
}
