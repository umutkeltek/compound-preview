package game

import (
	"sort"
	"strings"
	"time"

	"compound/internal/sourceobs"
)

const staleAfter = 14 * 24 * time.Hour

type CatalogEntry struct {
	ID               string    `json:"id"`
	Source           string    `json:"source"`
	SourceKey        string    `json:"source_key"`
	Label            string    `json:"label"`
	FirstSeen        time.Time `json:"first_seen_at"`
	LastSeen         time.Time `json:"last_seen_at"`
	ObservationCount int       `json:"observation_count"`
	Confidence       float64   `json:"confidence"`
	Stale            bool      `json:"stale"`
	Roles            []string  `json:"roles,omitempty"`
	Apps             []string  `json:"apps,omitempty"`
}

type CatalogReport struct {
	Schema     string         `json:"schema"`
	Kind       string         `json:"kind"`
	Generated  time.Time      `json:"generated_at"`
	StaleAfter time.Duration  `json:"stale_after"`
	Counts     map[string]int `json:"counts"`
	Entries    []CatalogEntry `json:"entries"`
	Warnings   []string       `json:"warnings,omitempty"`
}

type TrendingReport struct {
	Schema    string          `json:"schema"`
	Window    time.Duration   `json:"window"`
	Generated time.Time       `json:"generated_at"`
	Entries   []TrendingEntry `json:"entries"`
}

type TrendingEntry struct {
	Kind      string    `json:"kind"`
	SourceKey string    `json:"source_key"`
	Label     string    `json:"label"`
	Hits      int       `json:"hits"`
	LastSeen  time.Time `json:"last_seen_at"`
}

func ProjectGenres(homePath string, now time.Time) (CatalogReport, error) {
	return projectKind(homePath, "genre", SchemaGameGenres, now)
}

func ProjectCompanies(homePath string, now time.Time) (CatalogReport, error) {
	return projectKind(homePath, "company", SchemaGameCompanies, now)
}

func projectKind(homePath, kind, schema string, now time.Time) (CatalogReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	report := CatalogReport{
		Schema:     schema,
		Kind:       kind,
		Generated:  now,
		StaleAfter: staleAfter,
		Counts:     map[string]int{"entries": 0, "fresh": 0, "stale": 0, "sources": 0},
		Entries:    []CatalogEntry{},
	}
	list, err := sourceobs.ListSourceObservations(homePath, now)
	if err != nil {
		report.Warnings = append(report.Warnings, "list source observations: "+err.Error())
		return report, nil
	}

	type bucket struct {
		entry     CatalogEntry
		roleSet   map[string]bool
		appSet    map[string]bool
		sourceSet map[string]bool
	}
	buckets := map[string]*bucket{}

	for _, it := range list.Items {
		k, key, label, md, ok := parseGameSubject(it.Subject)
		if !ok || k != kind {
			continue
		}
		gk := k + "|" + key
		b, exists := buckets[gk]
		if !exists {
			b = &bucket{
				entry: CatalogEntry{
					ID:        GameID(k, key, "catalog"),
					Source:    "unknown",
					SourceKey: key,
					Label:     label,
				},
				roleSet:   map[string]bool{},
				appSet:    map[string]bool{},
				sourceSet: map[string]bool{},
			}
			buckets[gk] = b
		}
		created := parseCreated(it.CreatedAt)
		if b.entry.FirstSeen.IsZero() || (!created.IsZero() && created.Before(b.entry.FirstSeen)) {
			b.entry.FirstSeen = created
		}
		if created.After(b.entry.LastSeen) {
			b.entry.LastSeen = created
		}
		b.entry.ObservationCount++
		if label != "" {
			b.entry.Label = label
		}
		if role := md["role"]; role != "" {
			b.roleSet[role] = true
		}
		if appid := md["appid"]; appid != "" {
			b.appSet[appid] = true
		}
		src := md["source"]
		if src == "" {
			src = inferSourceSystem(it.SourceRef, it.EvidenceRefs)
		}
		if src != "" {
			b.sourceSet[src] = true
		}
	}

	for _, b := range buckets {
		b.entry.Stale = !b.entry.LastSeen.IsZero() && now.Sub(b.entry.LastSeen) > staleAfter
		if len(b.roleSet) > 0 {
			b.entry.Roles = sortedKeys(b.roleSet)
		}
		if len(b.appSet) > 0 {
			b.entry.Apps = sortedKeys(b.appSet)
		}
		if len(b.sourceSet) == 1 {
			for s := range b.sourceSet {
				b.entry.Source = s
			}
		} else if len(b.sourceSet) > 1 {
			b.entry.Source = "both"
		}
		report.Entries = append(report.Entries, b.entry)
	}

	sort.Slice(report.Entries, func(i, j int) bool {
		if report.Entries[i].Label != report.Entries[j].Label {
			return report.Entries[i].Label < report.Entries[j].Label
		}
		return report.Entries[i].SourceKey < report.Entries[j].SourceKey
	})

	for _, e := range report.Entries {
		report.Counts["entries"]++
		if e.Stale {
			report.Counts["stale"]++
		} else {
			report.Counts["fresh"]++
		}
	}
	report.Counts["sources"] = countDistinct(report.Entries, func(e CatalogEntry) string { return e.Source })
	return report, nil
}

func ProjectTrending(homePath string, now time.Time, limit int, windowDays int) (TrendingReport, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if windowDays <= 0 {
		windowDays = 7
	}
	if limit <= 0 {
		limit = 10
	}
	window := time.Duration(windowDays) * 24 * time.Hour
	report := TrendingReport{
		Schema:    SchemaGameTrending,
		Window:    window,
		Generated: now,
		Entries:   []TrendingEntry{},
	}
	cutoff := now.Add(-window)
	type bucket struct {
		entry   TrendingEntry
		hits    int
		lastHit time.Time
	}
	buckets := map[string]*bucket{}

	list, err := sourceobs.ListSourceObservations(homePath, now)
	if err != nil {
		return report, err
	}
	for _, it := range list.Items {
		k, key, label, _, ok := parseGameSubject(it.Subject)
		if !ok {
			continue
		}
		created := parseCreated(it.CreatedAt)
		if created.Before(cutoff) {
			continue
		}
		gk := k + "|" + key
		b, exists := buckets[gk]
		if !exists {
			b = &bucket{entry: TrendingEntry{Kind: k, SourceKey: key, Label: label}}
			buckets[gk] = b
		}
		b.hits++
		if created.After(b.lastHit) {
			b.lastHit = created
			if label != "" {
				b.entry.Label = label
			}
			b.entry.LastSeen = created
		}
	}

	for _, b := range buckets {
		b.entry.Hits = b.hits
		report.Entries = append(report.Entries, b.entry)
	}
	sort.Slice(report.Entries, func(i, j int) bool {
		if report.Entries[i].Hits != report.Entries[j].Hits {
			return report.Entries[i].Hits > report.Entries[j].Hits
		}
		return report.Entries[i].Label < report.Entries[j].Label
	})
	if len(report.Entries) > limit {
		report.Entries = report.Entries[:limit]
	}
	return report, nil
}

// parseGameSubject extracts (kind, sourceKey, label, metadata) from a Subject
// of the form "game:<kind>:<sourceKey>::<label>|<metadata>" where
// metadata is a `;`-separated list of `key=value` pairs.
func parseGameSubject(subject string) (kind, sourceKey, label string, metadata map[string]string, ok bool) {
	subject = strings.TrimSpace(subject)
	if !strings.HasPrefix(subject, "game:") {
		return "", "", "", nil, false
	}
	rest := strings.TrimPrefix(subject, "game:")
	var md map[string]string
	if i := strings.Index(rest, "|"); i >= 0 {
		metaPart := rest[i+1:]
		rest = rest[:i]
		if metaPart != "" {
			md = map[string]string{}
			for _, kv := range strings.Split(metaPart, ";") {
				if eq := strings.Index(kv, "="); eq > 0 {
					md[kv[:eq]] = kv[eq+1:]
				}
			}
		}
	}
	var labelPart string
	if i := strings.Index(rest, "::"); i >= 0 {
		labelPart = rest[i+2:]
		rest = rest[:i]
	}
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", nil, false
	}
	return parts[0], parts[1], labelPart, md, true
}

func parseCreated(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func inferSourceSystem(sourceRef string, evidence []string) string {
	for _, candidate := range append([]string{sourceRef}, evidence...) {
		c := strings.ToLower(candidate)
		switch {
		case strings.Contains(c, "steampowered"), strings.Contains(c, "steamweb"):
			return "steamweb"
		case strings.Contains(c, "steamspy"):
			return "steamspy"
		}
	}
	return ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func countDistinct[T any, K comparable](items []T, key func(T) K) int {
	seen := map[K]struct{}{}
	for _, it := range items {
		seen[key(it)] = struct{}{}
	}
	return len(seen)
}
