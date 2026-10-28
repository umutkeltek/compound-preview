package steamweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"compound/internal/game/source/gamesource"
)

func TestFetcherRoundtrip(t *testing.T) {
	body := readFixtureSteamweb(t, "appdetail_440.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/appdetails") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := New(srv.URL, srv.Client())
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	res, err := f.Fetch(context.Background(), gamesource.FetchRequest{AppIDs: []int64{440}, Now: now})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.AdapterID != gamesource.AdapterIDSteamWeb || res.SourceSys != gamesource.SourceSystemSteamWeb {
		t.Errorf("identity mismatch")
	}
	if !res.FetchedAt.Equal(now) {
		t.Errorf("fetched_at: got %v want %v", res.FetchedAt, now)
	}
	if res.RawSHA256 == "" {
		t.Errorf("expected non-empty revision hash")
	}
	want := map[string]int{"app": 1, "genre": 2, "company": 2}
	got := map[string]int{}
	for _, it := range res.Items {
		got[it.Kind]++
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("kind %s: got %d, want %d", k, got[k], v)
		}
	}
}

func TestFetcherMultipleAppIDs(t *testing.T) {
	body := readFixtureSteamweb(t, "appdetail_440_570.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	f := New(srv.URL, srv.Client())
	res, err := f.Fetch(context.Background(), gamesource.FetchRequest{AppIDs: []int64{440, 570}})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	counts := map[string]int{}
	for _, it := range res.Items {
		counts[it.Kind]++
	}
	if counts["app"] != 2 {
		t.Errorf("expected 2 apps, got %d", counts["app"])
	}
	if counts["genre"] < 2 {
		t.Errorf("expected >= 2 unique genres, got %d", counts["genre"])
	}
}

func TestFetcherNotFound(t *testing.T) {
	body := readFixtureSteamweb(t, "appdetail_404.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	f := New(srv.URL, srv.Client())
	res, err := f.Fetch(context.Background(), gamesource.FetchRequest{AppIDs: []int64{9999999}})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Subject != "9999999" || res.Items[0].Confidence != 0 {
		t.Errorf("not-found mismatch: %+v", res.Items)
	}
}

func TestFetcherEmptyAppIDs(t *testing.T) {
	f := New("http://example.invalid", nil)
	if _, err := f.Fetch(context.Background(), gamesource.FetchRequest{}); err == nil {
		t.Fatalf("expected error on empty AppIDs")
	}
}

func TestSlugifySteamweb(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Action", "action"},
		{"Free to Play", "free-to-play"},
		{"  Valve  ", "valve"},
		{"!!!", "unknown"},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
