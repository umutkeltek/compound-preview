package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const ReadinessSchemaVersion = "compound.console_readiness.v0.1"

type ReadinessProbe struct {
	State         string
	Detail        string
	HTTPStatus    int
	ExpectedHome  string
	ObservedHome  string
	BuildKind     string
	AssetsChecked int
	FailedAsset   string
}

type readinessResponse struct {
	SchemaVersion string   `json:"schema_version"`
	HomeHash      string   `json:"home_hash"`
	Explicit      bool     `json:"explicit"`
	BuildKind     string   `json:"build_kind"`
	BuildID       string   `json:"build_id"`
	StaticAssets  []string `json:"static_assets"`
	DataFreshness string   `json:"data_freshness"`
}

// InspectReadinessAt checks service identity and bounded current-build assets.
func InspectReadinessAt(address, expectedHome string, client *http.Client) ReadinessProbe {
	status := ReadinessProbe{State: "unhealthy", ExpectedHome: expectedHome}
	inspectReadinessAt(&status, address, client)
	return status
}

// inspectReadinessAt probes the shallow route without waiting for the
// dashboard's projection-backed root render.
func inspectReadinessAt(status *ReadinessProbe, address string, client *http.Client) bool {
	reject := func(detail string) bool { status.Detail = detail; return true }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	localClient := *client
	localClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/api/readiness", nil)
	if err != nil {
		return reject("console readiness request could not be built")
	}
	response, err := localClient.Do(request)
	if err != nil {
		return reject("console readiness response could not be reached")
	}
	defer response.Body.Close()
	status.HTTPStatus = response.StatusCode
	if response.StatusCode == http.StatusNotFound {
		return reject("console readiness endpoint unavailable")
	}
	if response.StatusCode != http.StatusOK {
		return reject(fmt.Sprintf("console readiness returned HTTP %d", response.StatusCode))
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return reject("console readiness response is not JSON")
	}

	const maxReadinessBytes = 64 << 10
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReadinessBytes+1))
	if err != nil {
		return reject("console readiness response could not be read")
	}
	if len(body) > maxReadinessBytes {
		return reject("console readiness response exceeded its bound")
	}
	var ready readinessResponse
	if json.Unmarshal(body, &ready) != nil {
		return reject("console readiness response is malformed")
	}
	if ready.SchemaVersion != ReadinessSchemaVersion || ready.DataFreshness != "not_checked" {
		return reject("console readiness contract is unsupported")
	}
	status.ObservedHome, status.BuildKind = ready.HomeHash, ready.BuildKind
	if ready.HomeHash == "" || ready.HomeHash != status.ExpectedHome || !ready.Explicit {
		status.State = "occupied_foreign"
		return reject("port owner is not explicitly pinned canonical Compound console")
	}
	if ready.BuildKind != "development" && ready.BuildKind != "production" {
		return reject("console identity missing its lifecycle build kind")
	}
	if ready.BuildKind == "production" && ready.BuildID == "" {
		return reject("console readiness response missing its production build identity")
	}
	if len(ready.StaticAssets) == 0 || len(ready.StaticAssets) > 32 {
		return reject("console readiness response named no bounded Next static assets")
	}
	seen := make(map[string]struct{}, len(ready.StaticAssets))
	for _, assetPath := range ready.StaticAssets {
		if !safeConsoleStaticAsset(assetPath) {
			return reject("console readiness response named an invalid Next static asset")
		}
		if _, duplicate := seen[assetPath]; duplicate {
			return reject("console readiness response repeated a Next static asset")
		}
		seen[assetPath] = struct{}{}
		assetRequest := request.Clone(ctx)
		assetRequest.URL.Path = assetPath
		assetResponse, err := localClient.Do(assetRequest)
		status.FailedAsset = assetPath
		if err != nil {
			return reject("console static asset probe failed")
		}
		const maxAssetBytes = 4 << 20
		bytesRead, readErr := io.Copy(io.Discard, io.LimitReader(assetResponse.Body, maxAssetBytes+1))
		_ = assetResponse.Body.Close()
		if readErr != nil {
			return reject("console static asset response could not be read")
		}
		if assetResponse.StatusCode != http.StatusOK {
			return reject(fmt.Sprintf("console static asset returned HTTP %d", assetResponse.StatusCode))
		}
		if bytesRead == 0 || bytesRead > maxAssetBytes {
			return reject("console static asset response has invalid size")
		}
		contentType := strings.TrimSpace(strings.SplitN(assetResponse.Header.Get("Content-Type"), ";", 2)[0])
		if (strings.HasSuffix(assetPath, ".css") && contentType != "text/css") ||
			(strings.HasSuffix(assetPath, ".js") && contentType != "text/javascript" && contentType != "application/javascript") {
			return reject("console static asset response has invalid content type")
		}
		status.AssetsChecked++
		status.FailedAsset = ""
	}
	status.State = "healthy"
	status.Detail = ""
	return true
}

var consoleStaticAssetPattern = regexp.MustCompile(`^/_next/static/[A-Za-z0-9/_.-]+\.(js|css)$`)

func safeConsoleStaticAsset(asset string) bool {
	return !strings.Contains(asset, "..") && consoleStaticAssetPattern.MatchString(asset)
}
