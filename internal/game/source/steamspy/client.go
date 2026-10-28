package steamspy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"compound/internal/game/source/gamesource"
)

type Fetcher struct {
	baseURL string
	client  *http.Client
}

func New(baseURL string, client *http.Client) *Fetcher {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://steamspy.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Fetcher{baseURL: baseURL, client: client}
}

func (f *Fetcher) ID() string     { return gamesource.AdapterIDSteamSpy }
func (f *Fetcher) System() string { return gamesource.SourceSystemSteamSpy }

func (f *Fetcher) Fetch(ctx context.Context, req gamesource.FetchRequest) (gamesource.FetchResult, error) {
	if len(req.AppIDs) == 0 {
		return gamesource.FetchResult{}, fmt.Errorf("steamspy: AppIDs required")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := gamesource.FetchResult{
		AdapterID: f.ID(),
		SourceSys: f.System(),
		FetchedAt: now,
	}

	for _, appid := range req.AppIDs {
		body, status, err := f.fetchOne(ctx, appid)
		if err != nil {
			return gamesource.FetchResult{}, err
		}
		if status == http.StatusNotFound {
			result.Items = append(result.Items, gamesource.RawObservation{
				Kind:       "spy_estimate",
				SourceKey:  fmt.Sprintf("%d", appid),
				Subject:    fmt.Sprintf("%d", appid),
				Statement:  fmt.Sprintf("steamspy has no estimate for appid %d", appid),
				Confidence: 0,
				Evidence:   []string{f.baseURL + "/api.php?request=appdetails"},
			})
			continue
		}
		if status < 200 || status >= 300 {
			return gamesource.FetchResult{}, fmt.Errorf("steamspy: appid %d http %d", appid, status)
		}

		sum := sha256.Sum256(body)
		rev := hex.EncodeToString(sum[:])
		est, err := decodeEstimate(body)
		if err != nil {
			return gamesource.FetchResult{}, fmt.Errorf("steamspy: decode appid %d: %w", appid, err)
		}
		result.RawSHA256 = rev
		result.Items = append(result.Items, f.toObservations(appid, est, rev, req)...)
	}

	return result, nil
}

func (f *Fetcher) fetchOne(ctx context.Context, appid int64) ([]byte, int, error) {
	q := url.Values{}
	q.Set("request", "appdetails")
	q.Set("appid", strconv.FormatInt(appid, 10))
	endpoint := f.baseURL + "/api.php?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (f *Fetcher) toObservations(appid int64, e estimate, rev string, req gamesource.FetchRequest) []gamesource.RawObservation {
	evidence := []string{
		f.baseURL + "/api.php?request=appdetails&appid=" + e.AppID,
		"revision:" + rev,
	}
	detail := fmt.Sprintf("owners=%s; ccu=%s; players_2_weeks=%s; avg_2_weeks=%s",
		e.Owners, e.CCU, e.Players2Weeks, e.Average2Weeks,
	)

	out := []gamesource.RawObservation{{
		Kind:       "spy_estimate",
		SourceKey:  e.AppID,
		Subject:    strings.TrimSpace(e.Name),
		Statement:  fmt.Sprintf("steamspy estimate for appid %s (%s)", e.AppID, e.Name),
		Detail:     detail,
		Confidence: 0.5,
		Evidence:   evidence,
	}}

	if strings.TrimSpace(e.Developer) != "" && companyMatches(e.Developer, req.Companies) {
		out = append(out, gamesource.RawObservation{
			Kind:       "company",
			SourceKey:  slugify(e.Developer),
			Subject:    e.Developer,
			Statement:  fmt.Sprintf("steamspy lists %s as developer of appid %s", e.Developer, e.AppID),
			Detail:     "role: developer; source: steamspy",
			Confidence: 0.5,
			Evidence:   evidence,
		})
	}
	if strings.TrimSpace(e.Publisher) != "" && e.Publisher != e.Developer && companyMatches(e.Publisher, req.Companies) {
		out = append(out, gamesource.RawObservation{
			Kind:       "company",
			SourceKey:  slugify(e.Publisher),
			Subject:    e.Publisher,
			Statement:  fmt.Sprintf("steamspy lists %s as publisher of appid %s", e.Publisher, e.AppID),
			Detail:     "role: publisher; source: steamspy",
			Confidence: 0.5,
			Evidence:   evidence,
		})
	}
	return out
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

func companyMatches(subject string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	subject = strings.ToLower(strings.TrimSpace(subject))
	for _, f := range filters {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if f == subject || strings.Contains(subject, f) {
			return true
		}
	}
	return false
}
