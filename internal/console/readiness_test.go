package console

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadinessProbeBoundsResponsesAndAssets(t *testing.T) {
	t.Parallel()
	homePath := filepath.Join(t.TempDir(), ".compound")
	valid := fmt.Sprintf(`{"schema_version":"compound.console_readiness.v0.1","home_hash":%q,"explicit":true,"build_kind":"production","build_id":"synthetic-build","static_assets":["/_next/static/chunks/app.js"],"data_freshness":"not_checked"}`, HomeShortHash(homePath))
	for _, tc := range []struct {
		name          string
		readinessCode int
		assetCode     int
		assetType     string
		assetBody     string
		hangAsset     bool
		wantDetail    string
	}{
		{name: "readiness unavailable", readinessCode: http.StatusNotFound, wantDetail: "console readiness endpoint unavailable"},
		{name: "empty asset", assetType: "application/javascript", wantDetail: "console static asset response has invalid size"},
		{name: "no content asset", assetCode: http.StatusNoContent, assetType: "application/javascript", wantDetail: "console static asset returned HTTP 204"},
		{name: "HTML fallback", assetType: "text/html", assetBody: "<html>fallback</html>", wantDetail: "console static asset response has invalid content type"},
		{name: "oversized asset", assetType: "application/javascript", assetBody: strings.Repeat("a", (4<<20)+1), wantDetail: "console static asset response has invalid size"},
		{name: "streamed stalled asset", assetType: "application/javascript", assetBody: "a", hangAsset: true, wantDetail: "console static asset response could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/readiness":
					if tc.readinessCode != 0 {
						w.WriteHeader(tc.readinessCode)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, valid)
				case "/_next/static/chunks/app.js":
					w.Header().Set("Content-Type", tc.assetType)
					if tc.assetCode != 0 {
						w.WriteHeader(tc.assetCode)
					}
					_, _ = io.WriteString(w, tc.assetBody)
					if tc.hangAsset {
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					}
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			status := InspectReadinessAt(strings.TrimPrefix(server.URL, "http://"), HomeShortHash(homePath), &http.Client{Timeout: 100 * time.Millisecond})
			if status.State != "unhealthy" || status.Detail != tc.wantDetail || status.AssetsChecked != 0 {
				t.Fatalf("status = %+v, want unhealthy detail %q with no checked assets", status, tc.wantDetail)
			}
		})
	}
}

func TestReadinessProbeRequiresJSONMediaType(t *testing.T) {
	for _, tc := range []struct {
		mediaType string
		healthy   bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/jsonp", false},
		{"application/json-invalid", false},
		{"application/json; broken", false},
	} {
		t.Run(tc.mediaType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/readiness" {
					w.Header().Set("Content-Type", tc.mediaType)
					_, _ = io.WriteString(w, `{"schema_version":"compound.console_readiness.v0.1","home_hash":"testhash","explicit":true,"build_kind":"production","build_id":"test-build","static_assets":["/_next/static/app.js"],"data_freshness":"not_checked"}`)
					return
				}
				w.Header().Set("Content-Type", "application/javascript")
				_, _ = io.WriteString(w, "export {};")
			}))
			defer server.Close()
			status := InspectReadinessAt(strings.TrimPrefix(server.URL, "http://"), "testhash", server.Client())
			if (status.State == "healthy") != tc.healthy {
				t.Fatalf("status=%+v, want healthy=%v", status, tc.healthy)
			}
			if !tc.healthy && status.AssetsChecked != 0 {
				t.Fatalf("invalid readiness media type reached assets: %+v", status)
			}
		})
	}
}
