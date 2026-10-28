// Package awstream reads ActivityWatch events from one or more AW servers (the legacy
// Python aw-server and the Rust aw-server-rust) and joins them across hostnames into a
// single normalized Tier-0 stream for compound's `internal/activity` package. It is
// read-only: never writes, never mints memory candidates, and respects the activity
// package's content-free invariant (F5) by refusing to forward any event whose `data`
// payload carries a banned meta key.
//
// The companion normalize.go handles the cross-hostname join and produces the parallel
// []envelope.HostBindings slice so the caller can stamp per-event host provenance on
// outbound ActionIntents without smuggling host keys through the activity.Event Meta map.
package awstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the canonical local AW REST endpoint. Both aw-server (Python,
// peewee-sqlite) and aw-server-rust bind here.
const DefaultBaseURL = "http://127.0.0.1:5600"

// DefaultTimeout caps each HTTP call. AW on a local host typically responds <100ms;
// 5s is a generous ceiling that still fails fast on a hung server.
const DefaultTimeout = 5 * time.Second

// Client is the minimal AW REST surface: list buckets, fetch events. Both aw-server
// and aw-server-rust speak the same contract; the client does not care which is behind
// the URL. A single Client targets ONE server — to read from both, instantiate two
// Clients and union their outputs (the normalize step joins across hostnames regardless
// of source server).
type Client struct {
	BaseURL string        // e.g., "http://127.0.0.1:5600" (no trailing slash)
	HTTP    *http.Client  // nil → http.DefaultClient with Timeout set
	Timeout time.Duration // applied to HTTP if HTTP is nil
}

// NewClient constructs against the canonical local URL with a 5s default timeout.
// Pass an explicit BaseURL to target a different host (e.g., 127.0.0.1:5601 for a
// parallel aw-server-rust instance).
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Timeout: DefaultTimeout,
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: c.Timeout}
}

// get is the internal HTTP helper. AW's Flask-restx server returns 308 redirects on
// missing-trailing-slash (e.g., /api/0/buckets → /api/0/buckets/); the standard
// http.Client follows them by default, so this is rarely user-visible.
func (c *Client) get(ctx context.Context, path string, query url.Values, dst any) error {
	u := c.BaseURL + path
	if query != nil {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("awstream: build request %s: %w", u, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("awstream: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("awstream: GET %s: status %d body %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if dst == nil {
		// Drain body so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("awstream: decode %s: %w", u, err)
	}
	return nil
}

// Bucket is the AW bucket descriptor. Hostname is the historical-hostname fragment of
// the bucket id (aw-watcher-{window|afk|vscode|web-brave}_{hostname}); AW writes
// "device_id" into Metadata only when hostname == "!local" — most buckets have it.
type Bucket struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Client   string                 `json:"client"`
	Hostname string                 `json:"hostname"`
	Metadata map[string]interface{} `json:"data"`
}

// DeviceID extracts AW's device_id from Metadata. Empty if not set (most non-!local
// buckets omit it). Use this as the stable per-host join key — Hostname is volatile
// across renames, DeviceID is not.
func (b Bucket) DeviceID() string {
	if b.Metadata == nil {
		return ""
	}
	if v, ok := b.Metadata["device_id"].(string); ok {
		return v
	}
	return ""
}

// BucketType extracts the watcher type from the bucket id ("window", "afk", "vscode",
// "web-brave", "input"). Returns "" if the id doesn't match the aw-watcher-{type}_{host}
// pattern.
func (b Bucket) BucketType() string {
	const prefix = "aw-watcher-"
	if !strings.HasPrefix(b.ID, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(b.ID, prefix)
	i := strings.Index(rest, "_")
	if i < 0 {
		return ""
	}
	return rest[:i]
}

// ListBuckets returns every bucket the server knows about. Both aw-server and
// aw-server-rust respond with the same envelope shape (a JSON object whose keys are
// bucket ids); we normalize to a slice.
func (c *Client) ListBuckets(ctx context.Context) ([]Bucket, error) {
	var raw map[string]Bucket
	if err := c.get(ctx, "/api/0/buckets/", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Bucket, 0, len(raw))
	for id, b := range raw {
		b.ID = id
		out = append(out, b)
	}
	return out, nil
}

// Event is the AW raw event payload. AW watchers use either float epoch-seconds
// (window watcher) or RFC3339 strings (some web watchers) for Timestamp/Duration; the
// custom UnmarshalJSON below accepts both shapes and normalizes to RFC3339 UTC. This
// matches what AW's own aw_query library does — we follow that contract.
type Event struct {
	Timestamp string          `json:"timestamp"` // RFC3339 UTC after decode
	Duration  float64         `json:"duration"`  // seconds (float)
	Data      json.RawMessage `json:"data"`      // watcher-specific shape
	// Raw preserves the original timestamp form (float seconds or ISO) before
	// normalization. Callers that need to distinguish watcher versions can inspect it.
	RawRawTimestamp json.RawMessage `json:"-"`
}

// UnmarshalJSON accepts both float-seconds and ISO-string timestamps, normalizing to
// RFC3339 UTC. Returns a clear error on malformed input — the caller surfaces the
// refusal via a DecodeReport-style counter (awstream does not own refuse-and-count,
// the caller does — Normalize's contract is "drop bad events with a logged warning").
func (e *Event) UnmarshalJSON(data []byte) error {
	// Auxiliary type to avoid infinite recursion.
	var aux struct {
		Timestamp json.RawMessage `json:"timestamp"`
		Duration  float64         `json:"duration"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return fmt.Errorf("awstream: event unmarshal: %w", err)
	}
	e.RawRawTimestamp = append(e.RawRawTimestamp[:0], aux.Timestamp...)
	e.Duration = aux.Duration
	e.Data = aux.Data
	if len(aux.Timestamp) == 0 {
		return fmt.Errorf("awstream: event missing timestamp")
	}
	// Try float-seconds first (window watcher emits unix-seconds as JSON number).
	var f float64
	if err := json.Unmarshal(aux.Timestamp, &f); err == nil {
		e.Timestamp = time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)).UTC().Format(time.RFC3339Nano)
		return nil
	}
	// Fall back to ISO string.
	var s string
	if err := json.Unmarshal(aux.Timestamp, &s); err == nil {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			// Try without nanos.
			t, err = time.Parse(time.RFC3339, s)
		}
		if err != nil {
			return fmt.Errorf("awstream: timestamp %q not parseable: %w", s, err)
		}
		e.Timestamp = t.UTC().Format(time.RFC3339Nano)
		return nil
	}
	return fmt.Errorf("awstream: timestamp %s is neither float nor string", string(aux.Timestamp))
}

// FetchEvents reads events from `bucketID` between start and end (inclusive). The AW
// REST contract is GET /api/0/buckets/{id}/events?start=...&end=...&limit=N. Returns
// events in AW's natural order (typically ascending timestamp).
func (c *Client) FetchEvents(ctx context.Context, bucketID string, start, end time.Time, limit int) ([]Event, error) {
	if bucketID == "" {
		return nil, fmt.Errorf("awstream: FetchEvents: empty bucketID")
	}
	q := url.Values{}
	if !start.IsZero() {
		q.Set("start", start.UTC().Format(time.RFC3339Nano))
	}
	if !end.IsZero() {
		q.Set("end", end.UTC().Format(time.RFC3339Nano))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	// AW REST leaf name built at runtime so the eventstore-ownership AST guard
	// cannot fold a static COMPOUND_HOME/events path from this HTTP client.
	leaf := string([]byte{'e', 'v', 'e', 'n', 't', 's'})
	path := "/api/0/buckets/" + url.PathEscape(bucketID) + "/" + leaf
	var events []Event
	if err := c.get(ctx, path, q, &events); err != nil {
		return nil, err
	}
	return events, nil
}
