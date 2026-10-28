package steamspy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"compound/internal/game/source/gamesource"
)

func TestFetcherRoundtrip(t *testing.T) {
	body := readFixtureSpy(t, "appdetail_440.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("request") != "appdetails" {
			t.Errorf("expected request=appdetails")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := New(srv.URL, srv.Client())
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	res, err := f.Fetch(context.Background(), gamesource.FetchRequest{AppIDs: []int64{440}, Now: now})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.AdapterID != gamesource.AdapterIDSteamSpy || res.SourceSys != gamesource.SourceSystemSteamSpy {
		t.Errorf("identity mismatch")
	}
	if !res.FetchedAt.Equal(now) {
		t.Errorf("fetched_at not pinned")
	}
	if res.RawSHA256 == "" {
		t.Errorf("expected revision hash")
	}
	counts := map[string]int{}
	for _, it := range res.Items {
		counts[it.Kind]++
	}
	if counts["spy_estimate"] != 1 {
		t.Errorf("expected 1 spy_estimate, got %d", counts["spy_estimate"])
	}
	if counts["company"] != 1 {
		t.Errorf("expected 1 company (dev==pub deduped), got %d", counts["company"])
	}
	for _, it := range res.Items {
		if it.Confidence != 0.5 {
			t.Errorf("kind=%s confidence=%v want 0.5", it.Kind, it.Confidence)
		}
	}
}

func TestFetcherNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	f := New(srv.URL, srv.Client())
	res, err := f.Fetch(context.Background(), gamesource.FetchRequest{AppIDs: []int64{9999999}})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Confidence != 0 {
		t.Errorf("not-found mismatch: %+v", res.Items)
	}
}

func TestFetcherEmptyAppIDs(t *testing.T) {
	f := New("http://example.invalid", nil)
	if _, err := f.Fetch(context.Background(), gamesource.FetchRequest{}); err == nil {
		t.Fatalf("expected error")
	}
}

func TestFetcherPublisherDifferentFromDeveloper(t *testing.T) {
	body := `{"appid":"1","name":"X","developer":"Studio A","publisher":"Publisher B","owners":"1,000 .. 2,000","ccu":"10","tags":""}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	f := New(srv.URL, srv.Client())
	res, err := f.Fetch(context.Background(), gamesource.FetchRequest{AppIDs: []int64{1}})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("expected 3 observations, got %d", len(res.Items))
	}
	seen := map[string]bool{"Studio A": false, "Publisher B": false}
	for _, it := range res.Items {
		if _, ok := seen[it.Subject]; ok {
			seen[it.Subject] = true
		}
	}
	for s, v := range seen {
		if !v {
			t.Errorf("missing company observation for %q", s)
		}
	}
}
