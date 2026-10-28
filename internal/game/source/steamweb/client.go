package steamweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
		baseURL = "https://store.steampowered.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Fetcher{baseURL: baseURL, client: client}
}

func (f *Fetcher) ID() string     { return gamesource.AdapterIDSteamWeb }
func (f *Fetcher) System() string { return gamesource.SourceSystemSteamWeb }

func (f *Fetcher) Fetch(ctx context.Context, req gamesource.FetchRequest) (gamesource.FetchResult, error) {
	if len(req.AppIDs) == 0 {
		return gamesource.FetchResult{}, fmt.Errorf("steamweb: AppIDs required")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	body, status, err := f.fetchAppDetails(ctx, req.AppIDs)
	if err != nil {
		return gamesource.FetchResult{}, err
	}
	if status < 200 || status >= 300 {
		return gamesource.FetchResult{}, fmt.Errorf("steamweb: appdetails http %d", status)
	}

	sum := sha256.Sum256(body)
	rev := hex.EncodeToString(sum[:])

	parsed, err := decodeAppDetails(body)
	if err != nil {
		return gamesource.FetchResult{}, fmt.Errorf("steamweb: decode appdetails: %w", err)
	}

	result := gamesource.FetchResult{
		AdapterID: f.ID(),
		SourceSys: f.System(),
		FetchedAt: now,
		RawSHA256: rev,
	}

	for _, appid := range req.AppIDs {
		key := fmt.Sprintf("%d", appid)
		entry, ok := parsed[key]
		if !ok || !entry.Success || entry.Data == nil {
			result.Items = append(result.Items, gamesource.RawObservation{
				Kind:       "app",
				SourceKey:  key,
				Subject:    key,
				Statement:  fmt.Sprintf("appid %s not found in steamweb", key),
				Confidence: 0,
				Evidence:   f.evidence(rev),
			})
			continue
		}
		d := entry.Data
		result.Items = append(result.Items, f.appObservation(d, rev))
		for _, g := range d.Genres {
			if strings.TrimSpace(g.Description) == "" {
				continue
			}
			if !genreMatches(g.Description, req.Genres) {
				continue
			}
			result.Items = append(result.Items, gamesource.RawObservation{
				Kind:       "genre",
				SourceKey:  slugify(g.Description),
				Subject:    g.Description,
				Statement:  fmt.Sprintf("%s is in genre %q on appid %d", d.Name, g.Description, appid),
				Confidence: 1.0,
				Evidence:   f.evidence(rev),
			})
		}
		for _, dev := range d.Developers {
			if strings.TrimSpace(dev) == "" {
				continue
			}
			if !companyMatches(dev, req.Companies) {
				continue
			}
			result.Items = append(result.Items, gamesource.RawObservation{
				Kind:       "company",
				SourceKey:  slugify(dev),
				Subject:    dev,
				Statement:  fmt.Sprintf("%s develops appid %d (%s)", dev, appid, d.Name),
				Detail:     "role: developer",
				Confidence: 1.0,
				Evidence:   f.evidence(rev),
			})
		}
		for _, pub := range d.Publishers {
			if strings.TrimSpace(pub) == "" {
				continue
			}
			if !companyMatches(pub, req.Companies) {
				continue
			}
			result.Items = append(result.Items, gamesource.RawObservation{
				Kind:       "company",
				SourceKey:  slugify(pub),
				Subject:    pub,
				Statement:  fmt.Sprintf("%s publishes appid %d (%s)", pub, appid, d.Name),
				Detail:     "role: publisher",
				Confidence: 1.0,
				Evidence:   f.evidence(rev),
			})
		}
	}

	return result, nil
}

func (f *Fetcher) fetchAppDetails(ctx context.Context, appids []int64) ([]byte, int, error) {
	q := url.Values{}
	parts := make([]string, 0, len(appids))
	for _, a := range appids {
		parts = append(parts, fmt.Sprintf("%d", a))
	}
	q.Set("appids", strings.Join(parts, ","))
	endpoint := f.baseURL + "/api/appdetails?" + q.Encode()

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

func (f *Fetcher) appObservation(d *appDetailsData, rev string) gamesource.RawObservation {
	genres := make([]string, 0, len(d.Genres))
	for _, g := range d.Genres {
		if strings.TrimSpace(g.Description) != "" {
			genres = append(genres, g.Description)
		}
	}
	detail := fmt.Sprintf("genres=%s; developers=%s; publishers=%s; release_date=%s",
		strings.Join(genres, ","),
		strings.Join(d.Developers, ","),
		strings.Join(d.Publishers, ","),
		d.ReleaseDate.Date,
	)
	if d.PriceOverview != nil {
		detail += fmt.Sprintf("; price=%s %d (discount %d%%)",
			d.PriceOverview.Currency, d.PriceOverview.Final, d.PriceOverview.DiscountPercent)
	}
	return gamesource.RawObservation{
		Kind:       "app",
		SourceKey:  fmt.Sprintf("%d", d.SteamAppid),
		Subject:    d.Name,
		Statement:  fmt.Sprintf("appid %d is %q", d.SteamAppid, d.Name),
		Detail:     detail,
		Confidence: 1.0,
		Evidence:   f.evidence(rev),
	}
}

func (f *Fetcher) evidence(rev string) []string {
	return []string{
		f.baseURL + "/api/appdetails",
		"revision:" + rev,
	}
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

func genreMatches(subject string, filters []string) bool {
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

func companyMatches(subject string, filters []string) bool { return genreMatches(subject, filters) }
