package langfuse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxBatchEvents is the Langfuse ingestion per-batch event cap; callers chunk
// longer event lists into batches of this size.
const MaxBatchEvents = 100

// Event is one entry in a Langfuse ingestion batch ("trace-create",
// "generation-create", "span-create", "score-create"). ID is the deterministic
// dedup key (EventID); Body is the type-specific resource body.
type Event struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Body      any    `json:"body"`
}

// TraceBody is the body of a "trace-create" event.
type TraceBody struct {
	ID        string         `json:"id"`
	Timestamp string         `json:"timestamp,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     any            `json:"input,omitempty"`
	Output    any            `json:"output,omitempty"`
	SessionID string         `json:"sessionId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// ObsBody is the body of a "generation-create" or "span-create" event. Fields
// specific to generations are omitempty so tool spans stay lean.
type ObsBody struct {
	ID                  string         `json:"id"`
	TraceID             string         `json:"traceId"`
	Name                string         `json:"name,omitempty"`
	StartTime           string         `json:"startTime,omitempty"`
	EndTime             string         `json:"endTime,omitempty"`
	Input               any            `json:"input,omitempty"`
	Output              any            `json:"output,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	Level               string         `json:"level,omitempty"`
	StatusMessage       string         `json:"statusMessage,omitempty"`
	ParentObservationID string         `json:"parentObservationId,omitempty"`
	// Generation-only:
	CompletionStartTime string             `json:"completionStartTime,omitempty"`
	Model               string             `json:"model,omitempty"`
	ModelParameters     map[string]any     `json:"modelParameters,omitempty"`
	UsageDetails        map[string]int     `json:"usageDetails,omitempty"`
	CostDetails         map[string]float64 `json:"costDetails,omitempty"`
}

// ScoreBody is the body of a "score-create" event.
type ScoreBody struct {
	ID       string  `json:"id"`
	TraceID  string  `json:"traceId"`
	Name     string  `json:"name"`
	Value    float64 `json:"value"`
	DataType string  `json:"dataType"`
	Comment  string  `json:"comment,omitempty"`
}

// Client posts ingestion batches to a Langfuse host over REST (Basic auth).
type Client struct {
	baseURL   string
	publicKey string
	secretKey string
	http      *http.Client
}

// NewClient builds a Client. host may carry a trailing slash; it's trimmed.
func NewClient(host, pub, sec string) *Client {
	for len(host) > 0 && host[len(host)-1] == '/' {
		host = host[:len(host)-1]
	}
	return &Client{
		baseURL:   host,
		publicKey: pub,
		secretKey: sec,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

type ingestResponse struct {
	Errors []struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Error any    `json:"error"`
	} `json:"errors"`
}

// Ingest posts events to {baseURL}/api/public/ingestion as a single batch,
// chunked internally to MaxBatchEvents. Retries on network errors, 5xx, and 429
// (up to 3 attempts, exponential backoff capped at 4s); 4xx is non-retryable.
// A per-event errors[] in a 2xx response surfaces as an error (these are
// validation rejections, not transport failures).
func (c *Client) Ingest(ctx context.Context, events []Event) error {
	for start := 0; start < len(events); start += MaxBatchEvents {
		end := start + MaxBatchEvents
		if end > len(events) {
			end = len(events)
		}
		batch := events[start:end]
		if err := c.ingestBatch(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) ingestBatch(ctx context.Context, batch []Event) error {
	body := struct {
		Batch    []Event           `json:"batch"`
		Metadata map[string]string `json:"metadata"`
	}{
		Batch: batch,
		Metadata: map[string]string{
			"source": "zero-langfuse",
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("langfuse: marshal batch: %w", err)
	}

	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			shift := attempt - 1
			if shift > 10 {
				shift = 10
			}
			backoff := time.Duration(500<<shift) * time.Millisecond
			if backoff > 4*time.Second {
				backoff = 4 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		status, respBody, err := c.postOnce(ctx, payload)
		if err != nil {
			lastErr = err
			continue // transport error → retry
		}
		if status == 429 || status >= 500 {
			lastErr = fmt.Errorf("langfuse: ingestion returned %d: %s", status, truncStr(string(respBody), 300))
			continue
		}
		if status >= 400 {
			// Non-retryable client error.
			return fmt.Errorf("langfuse: ingestion returned %d: %s", status, truncStr(string(respBody), 300))
		}
		// 2xx: still check per-event errors[].
		var ir ingestResponse
		if jerr := json.Unmarshal(respBody, &ir); jerr == nil && len(ir.Errors) > 0 {
			return fmt.Errorf("langfuse: ingestion reported %d event error(s): %s", len(ir.Errors), truncStr(string(respBody), 500))
		}
		return nil
	}
	return lastErr
}

func (c *Client) postOnce(ctx context.Context, payload []byte) (status int, respBody []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/public/ingestion", bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.publicKey, c.secretKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
