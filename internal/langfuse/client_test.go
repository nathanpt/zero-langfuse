package langfuse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func sampleEvents() []Event {
	return []Event{
		{Type: "trace-create", ID: EventID("trace-create", "t1"), Timestamp: "2026-07-06T02:00:00Z", Body: TraceBody{ID: "t1", Name: "zero-agent"}},
	}
}

func TestIngestShape(t *testing.T) {
	var seen struct {
		path, auth string
		batch      []Event
		source     string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path = r.URL.Path
		seen.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Batch    []Event           `json:"batch"`
			Metadata map[string]string `json:"metadata"`
		}
		_ = json.Unmarshal(raw, &body)
		seen.batch = body.Batch
		seen.source = body.Metadata["source"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "pk-lf-x", "sk-lf-y")
	if err := c.Ingest(context.Background(), sampleEvents()); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if seen.path != "/api/public/ingestion" {
		t.Errorf("path = %q, want /api/public/ingestion", seen.path)
	}
	if !strings.HasPrefix(seen.auth, "Basic ") {
		t.Errorf("auth header missing Basic: %q", seen.auth)
	}
	if seen.source != "zero-langfuse" {
		t.Errorf("metadata.source = %q, want zero-langfuse", seen.source)
	}
	if len(seen.batch) != 1 {
		t.Errorf("batch len = %d, want 1", len(seen.batch))
	}
}

func TestIngestRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "pk", "sk")
	if err := c.Ingest(context.Background(), sampleEvents()); err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if calls < 2 {
		t.Errorf("expected ≥2 attempts, got %d", calls)
	}
}

func TestIngest4xxIsNonRetryable(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad creds"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "pk", "sk")
	err := c.Ingest(context.Background(), sampleEvents())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 attempt (no retry on 4xx), got %d", calls)
	}
}

func TestIngest429IsRetryable(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "pk", "sk")
	if err := c.Ingest(context.Background(), sampleEvents()); err != nil {
		t.Fatalf("expected success after 429 retry, got %v", err)
	}
	if calls < 2 {
		t.Errorf("expected ≥2 attempts on 429, got %d", calls)
	}
}

func TestIngestPartialErrorsSurface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"id":"ev1","type":"event","error":{"message":"bad timestamp"}}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "pk", "sk")
	err := c.Ingest(context.Background(), sampleEvents())
	if err == nil || !strings.Contains(err.Error(), "1 event error") {
		t.Fatalf("expected per-event error to surface, got %v", err)
	}
}

func TestIngestChunksLargeBatch(t *testing.T) {
	var batches int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&batches, 1)
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Batch []Event `json:"batch"`
		}
		_ = json.Unmarshal(raw, &body)
		if len(body.Batch) > MaxBatchEvents {
			t.Errorf("batch exceeded cap: %d", len(body.Batch))
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// 250 events → 3 chunks (100, 100, 50).
	events := make([]Event, 250)
	for i := range events {
		events[i] = Event{Type: "trace-create", ID: EventID("trace-create", "t"+itoa(i)), Body: TraceBody{ID: "t" + itoa(i)}}
	}
	c := NewClient(srv.URL, "pk", "sk")
	if err := c.Ingest(context.Background(), events); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := atomic.LoadInt32(&batches); got != 3 {
		t.Errorf("batches = %d, want 3", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
