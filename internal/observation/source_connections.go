package observation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/knowledge"
	"compound/internal/product"
)

const (
	SourceConnectionsProjectionSchemaVersion = "compound.source_connections_projection.v0.2"
	SourceConnectionsAuthorityRef            = "compound://sources/connections"
)

type SourceConnectionsProjection struct {
	SchemaVersion string                  `json:"schema_version"`
	AuthorityRef  string                  `json:"authority_ref"`
	Status        string                  `json:"status"`
	GeneratedAt   string                  `json:"generated_at"`
	Counts        SourceConnectionsCounts `json:"counts"`
	Connections   []SourceConnection      `json:"connections"`
	NextCommand   []string                `json:"next_command"`
}

type SourceConnectionsCounts struct {
	Total                          int `json:"total"`
	Connected                      int `json:"connected"`
	Imported                       int `json:"imported"`
	Observed                       int `json:"observed"`
	NeedsInput                     int `json:"needs_input"`
	NeedsReview                    int `json:"needs_review"`
	Stale                          int `json:"stale"`
	NotConnected                   int `json:"not_connected"`
	ActionRequired                 int `json:"action_required"`
	PrivateSetup                   int `json:"private_setup"`
	AvailabilityUnknown            int `json:"availability_unknown"`
	AvailabilityNotConfigured      int `json:"availability_not_configured"`
	AvailabilityNeedsPrivateConfig int `json:"availability_needs_private_config"`
	AvailabilityConfigured         int `json:"availability_configured"`
	AvailabilityHealthy            int `json:"availability_healthy"`
	AvailabilityFailing            int `json:"availability_failing"`
	EvidenceNeverSeen              int `json:"evidence_never_seen"`
	EvidenceObserved               int `json:"evidence_observed"`
	EvidenceImported               int `json:"evidence_imported"`
	EvidenceStale                  int `json:"evidence_stale"`
	EvidenceHasCards               int `json:"evidence_has_cards"`
}

type SourceConnection struct {
	ID                 string                   `json:"id"`
	Label              string                   `json:"label"`
	Status             string                   `json:"status"`
	Availability       string                   `json:"availability"`
	EvidenceState      string                   `json:"evidence_state"`
	SourceKinds        []string                 `json:"source_kinds,omitempty"`
	SourceKind         string                   `json:"source_kind,omitempty"`
	SourceRef          string                   `json:"source_ref,omitempty"`
	LastSeen           string                   `json:"last_seen,omitempty"`
	Produces           []string                 `json:"produces,omitempty"`
	NextSafeAction     string                   `json:"next_safe_action"`
	PrivateSetupNeeded bool                     `json:"private_setup_needed,omitempty"`
	RowCount           int                      `json:"row_count"`
	LatestCardCount    int                      `json:"latest_card_count"`
	LatestCards        []SourceIntakeActionCard `json:"latest_cards,omitempty"`
}

type sourceConnectionDef struct {
	ID                 string
	Label              string
	SourceKinds        []string
	DefaultStatus      string
	Produces           []string
	NextSafeAction     string
	PrivateSetupNeeded bool
}

type knowledgeConnectionSummary struct {
	SourceKind string
	SourceRef  string
	LastSeen   string
	Count      int
}

type productSignalConnectionSummary struct {
	SourceKind string
	SourceRef  string
	LastSeen   string
	Count      int
}

func LoadSourceConnectionsProjection(homePath string, now time.Time) (SourceConnectionsProjection, error) {
	if now.IsZero() {
		now = time.Now()
	}
	intake, err := LoadSourceIntakeProjection(homePath, now, 0)
	if err != nil {
		return SourceConnectionsProjection{}, err
	}

	defs := sourceConnectionDefs()
	byID := map[string]*SourceConnection{}
	kindToID := map[string]string{}
	defaultStatusByID := map[string]string{}
	for _, def := range defs {
		conn := &SourceConnection{
			ID:                 def.ID,
			Label:              def.Label,
			SourceKinds:        append([]string{}, def.SourceKinds...),
			Produces:           append([]string{}, def.Produces...),
			NextSafeAction:     def.NextSafeAction,
			PrivateSetupNeeded: def.PrivateSetupNeeded,
		}
		byID[def.ID] = conn
		defaultStatusByID[def.ID] = def.DefaultStatus
		for _, kind := range def.SourceKinds {
			kindToID[strings.TrimSpace(kind)] = def.ID
		}
	}

	for _, row := range intake.Rows {
		id := kindToID[strings.TrimSpace(row.SourceKind)]
		if id == "" {
			id = "source_" + slugPart(row.SourceKind)
			if _, ok := byID[id]; !ok {
				byID[id] = &SourceConnection{
					ID:             id,
					Label:          titleFromSlug(row.SourceKind),
					Status:         "observed",
					SourceKinds:    []string{row.SourceKind},
					Produces:       producesForSourceRow(row),
					NextSafeAction: "Review latest intake row and route it through the existing source intake command.",
				}
			}
		}
		conn := byID[id]
		conn.RowCount++
		conn.SourceKind = firstNonEmpty(conn.SourceKind, row.SourceKind)
		conn.SourceRef = firstNonEmpty(conn.SourceRef, row.SourceRef)
		conn.LastSeen = newestTimestamp(conn.LastSeen, firstNonEmpty(row.FreshnessAt, row.SourceCapturedAt, row.GeneratedAt))
		conn.Produces = compact(append(conn.Produces, producesForSourceRow(row)...))
		conn.PrivateSetupNeeded = conn.PrivateSetupNeeded || row.Privacy == "private" || row.Status == RouteBlockedPrivate
		if len(row.ActionCards) > 0 {
			conn.LatestCards = append(conn.LatestCards, row.ActionCards...)
			conn.LatestCardCount += len(row.ActionCards)
			if conn.NextSafeAction == "" || sourceConnectionStatusRank(conn.Status) < sourceConnectionStatusRank("needs_review") {
				conn.NextSafeAction = row.ActionCards[0].Title
			}
		}
		conn.Status = strongerSourceConnectionStatus(conn.Status, sourceConnectionRowStatus(conn.ID, row, now))
		if conn.NextSafeAction == "" {
			conn.NextSafeAction = sourceConnectionNextAction(conn.Status)
		}
	}

	knowledgeImports, err := loadKnowledgeConnectionImports(homePath)
	if err != nil {
		return SourceConnectionsProjection{}, err
	}
	for sourceKind, summary := range knowledgeImports {
		id := kindToID[strings.TrimSpace(sourceKind)]
		if id == "" {
			id = "source_" + slugPart(sourceKind)
			if _, ok := byID[id]; !ok {
				byID[id] = &SourceConnection{
					ID:             id,
					Label:          titleFromSlug(sourceKind),
					Status:         "imported",
					SourceKinds:    []string{sourceKind},
					Produces:       []string{"knowledge evidence", "memory candidate"},
					NextSafeAction: "Review imported knowledge rows and resolve existing source cards.",
				}
			}
		}
		conn := byID[id]
		conn.RowCount += summary.Count
		conn.SourceKind = firstNonEmpty(conn.SourceKind, summary.SourceKind)
		conn.SourceRef = firstNonEmpty(conn.SourceRef, summary.SourceRef)
		conn.LastSeen = newestTimestamp(conn.LastSeen, summary.LastSeen)
		conn.Produces = compact(append(conn.Produces, "knowledge evidence", "memory candidate"))
		conn.Status = strongerSourceConnectionStatus(conn.Status, "imported")
		if conn.NextSafeAction == "" || sourceConnectionStatusRank(conn.Status) <= sourceConnectionStatusRank("imported") {
			conn.NextSafeAction = knowledgeConnectionNextAction(conn.ID, summary.Count)
		}
	}

	productSignalSummaries, err := loadProductSignalConnectionSummaries(homePath)
	if err != nil {
		return SourceConnectionsProjection{}, err
	}
	for sourceKind, summary := range productSignalSummaries {
		id := kindToID[strings.TrimSpace(sourceKind)]
		if id == "" {
			id = "source_" + slugPart(sourceKind)
			if _, ok := byID[id]; !ok {
				byID[id] = &SourceConnection{
					ID:             id,
					Label:          titleFromSlug(sourceKind),
					Status:         "observed",
					SourceKinds:    []string{sourceKind},
					Produces:       []string{"product signal", "product loop evidence"},
					NextSafeAction: "Review latest product signals and continue through product loop cards.",
				}
			}
		}
		conn := byID[id]
		conn.RowCount += summary.Count
		conn.SourceKind = firstNonEmpty(conn.SourceKind, summary.SourceKind)
		conn.SourceRef = firstNonEmpty(conn.SourceRef, summary.SourceRef)
		conn.LastSeen = newestTimestamp(conn.LastSeen, summary.LastSeen)
		conn.Produces = compact(append(conn.Produces, "product signal", "product loop evidence"))
		conn.Status = strongerSourceConnectionStatus(conn.Status, "observed")
		if conn.LatestCardCount == 0 && sourceConnectionStatusRank(conn.Status) <= sourceConnectionStatusRank("observed") {
			conn.NextSafeAction = productSignalConnectionNextAction(conn.ID)
		}
	}

	out := SourceConnectionsProjection{
		SchemaVersion: SourceConnectionsProjectionSchemaVersion,
		AuthorityRef:  SourceConnectionsAuthorityRef,
		Status:        "ok",
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		NextCommand:   []string{"compound", "source", "connections", "--json"},
	}
	for _, conn := range byID {
		conn.Produces = compact(conn.Produces)
		if conn.Status == "" {
			conn.Status = firstNonEmpty(defaultStatusByID[conn.ID], "not_connected")
		}
		conn.EvidenceState = sourceConnectionEvidenceState(*conn)
		conn.Availability = sourceConnectionAvailability(*conn)
		conn.Status = sourceConnectionLegacyStatus(conn.Availability, conn.EvidenceState, conn.Status)
		if conn.NextSafeAction == "" {
			conn.NextSafeAction = sourceConnectionNextAction(conn.Status)
		}
		sort.SliceStable(conn.LatestCards, func(i, j int) bool {
			if conn.LatestCards[i].Severity != conn.LatestCards[j].Severity {
				return sourceSeverityRank(conn.LatestCards[i].Severity) > sourceSeverityRank(conn.LatestCards[j].Severity)
			}
			return conn.LatestCards[i].ID < conn.LatestCards[j].ID
		})
		if len(conn.LatestCards) > 3 {
			conn.LatestCards = conn.LatestCards[:3]
		}
		out.Connections = append(out.Connections, *conn)
	}
	sort.SliceStable(out.Connections, func(i, j int) bool {
		if sourceConnectionStatusRank(out.Connections[i].Status) != sourceConnectionStatusRank(out.Connections[j].Status) {
			return sourceConnectionStatusRank(out.Connections[i].Status) > sourceConnectionStatusRank(out.Connections[j].Status)
		}
		if out.Connections[i].LastSeen != out.Connections[j].LastSeen {
			return out.Connections[i].LastSeen > out.Connections[j].LastSeen
		}
		return out.Connections[i].ID < out.Connections[j].ID
	})
	out.Counts.Total = len(out.Connections)
	for _, conn := range out.Connections {
		switch conn.Availability {
		case "not_configured":
			out.Counts.AvailabilityNotConfigured++
		case "needs_private_config":
			out.Counts.AvailabilityNeedsPrivateConfig++
		case "configured":
			out.Counts.AvailabilityConfigured++
		case "healthy":
			out.Counts.AvailabilityHealthy++
		case "failing":
			out.Counts.AvailabilityFailing++
		default:
			out.Counts.AvailabilityUnknown++
		}
		switch conn.EvidenceState {
		case "observed":
			out.Counts.EvidenceObserved++
		case "imported":
			out.Counts.EvidenceImported++
		case "stale":
			out.Counts.EvidenceStale++
		case "has_cards":
			out.Counts.EvidenceHasCards++
		default:
			out.Counts.EvidenceNeverSeen++
		}
		switch conn.Status {
		case "connected":
			out.Counts.Connected++
		case "imported":
			out.Counts.Imported++
		case "observed":
			out.Counts.Observed++
		case "blocked_private_config", "needs_private_input", "ready_preflight":
			out.Counts.NeedsInput++
		case "needs_review", "needs_mapping":
			out.Counts.NeedsReview++
		case "stale":
			out.Counts.Stale++
		case "not_connected":
			out.Counts.NotConnected++
		}
		if conn.LatestCardCount > 0 {
			out.Counts.ActionRequired++
		}
		if conn.PrivateSetupNeeded {
			out.Counts.PrivateSetup++
		}
	}
	return out, nil
}

func loadProductSignalConnectionSummaries(homePath string) (map[string]productSignalConnectionSummary, error) {
	out := map[string]productSignalConnectionSummary{}
	if strings.TrimSpace(homePath) == "" {
		return out, nil
	}
	path := filepath.Join(homePath, "ledger", "product-signals.jsonl")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// A missing signals ledger is not the end of the read: the ads ledger
	// below can still carry imported rows for a platform with no signal yet.
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var signal product.Signal
		if err := json.Unmarshal([]byte(line), &signal); err != nil || signal.ID == "" {
			continue
		}
		sourceKind := productSignalSourceKind(signal)
		if sourceKind == "" {
			continue
		}
		summary := out[sourceKind]
		summary.SourceKind = sourceKind
		summary.SourceRef = firstNonEmpty(summary.SourceRef, "product-signals:"+sourceKind)
		summary.LastSeen = newestTimestamp(summary.LastSeen, signal.ObservedAt)
		summary.Count++
		out[sourceKind] = summary
	}
	adRows, err := product.LoadAdRows(homePath)
	if err != nil {
		return nil, err
	}
	for _, row := range product.LatestAdRows(adRows) {
		summary := out[row.Platform]
		summary.SourceKind = row.Platform
		summary.SourceRef = firstNonEmpty(summary.SourceRef, "product-ads:"+row.Platform)
		summary.LastSeen = newestTimestamp(summary.LastSeen, row.ImportedAt)
		summary.Count++
		out[row.Platform] = summary
	}
	return out, nil
}

func productSignalSourceKind(signal product.Signal) string {
	for _, ref := range signal.EvidenceRefs {
		ref = strings.TrimSpace(ref)
		if strings.HasPrefix(ref, "ads:") {
			// ads:<platform>:... — the bridge names the platform in every ref.
			if parts := strings.SplitN(ref, ":", 3); len(parts) >= 2 {
				if _, ok := product.LookupAdPlatform(parts[1]); ok {
					return parts[1]
				}
			}
		}
		switch {
		case strings.HasPrefix(ref, "asc:"):
			return "app_store_connect"
		case strings.HasPrefix(ref, "gsc:"), strings.HasPrefix(ref, "google_search_console:"):
			return "google_search_console"
		case strings.HasPrefix(ref, "ga4:"), strings.HasPrefix(ref, "google_analytics:"):
			return "google_analytics"
		case strings.HasPrefix(ref, "adsense:"), strings.HasPrefix(ref, "google_adsense:"):
			return "google_adsense"
		case strings.HasPrefix(ref, "revenuecat:"):
			return "revenuecat"
		case strings.HasPrefix(ref, "posthog:"):
			return "posthog"
		}
	}
	metric := strings.TrimSpace(signal.MetricName)
	switch {
	case strings.HasPrefix(metric, "app_store_connect_"):
		return "app_store_connect"
	case strings.HasPrefix(metric, "gsc_"):
		return "google_search_console"
	case strings.HasPrefix(metric, "ga4_"):
		return "google_analytics"
	case strings.HasPrefix(metric, "adsense_"):
		return "google_adsense"
	case strings.HasPrefix(metric, "revenuecat_"):
		return "revenuecat"
	case strings.HasPrefix(metric, "posthog_"):
		return "posthog"
	default:
		return ""
	}
}

func loadKnowledgeConnectionImports(homePath string) (map[string]knowledgeConnectionSummary, error) {
	out := map[string]knowledgeConnectionSummary{}
	if strings.TrimSpace(homePath) == "" {
		return out, nil
	}
	path := filepath.Join(homePath, "ledger", "knowledge-imports.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row knowledge.ImportIndexRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		sourceKind := strings.TrimSpace(row.SourceKind)
		if sourceKind == "" {
			continue
		}
		summary := out[sourceKind]
		summary.SourceKind = sourceKind
		summary.SourceRef = firstNonEmpty(summary.SourceRef, row.SourceRef)
		summary.LastSeen = newestTimestamp(summary.LastSeen, row.ImportedAt)
		summary.Count++
		out[sourceKind] = summary
	}
	return out, nil
}

func knowledgeConnectionNextAction(connectionID string, count int) string {
	switch connectionID {
	case "x_bookmarks":
		return "Review imported bookmark observations and resolve no-promote candidates before any promotion."
	default:
		if count > 0 {
			return "Review imported knowledge rows and resolve pending extraction or review cards."
		}
		return "Connect a bounded read-only export before automation."
	}
}

func productSignalConnectionNextAction(connectionID string) string {
	switch connectionID {
	case "asc_daily_signals":
		return "Review latest ASC product signals; run credential smoke or scheduler setup only for automatic refresh."
	default:
		return "Review latest product signals and continue through product loop cards."
	}
}

func sourceConnectionDefs() []sourceConnectionDef {
	return []sourceConnectionDef{
		{
			ID:                 "asc_daily_signals",
			Label:              "ASC daily signals",
			SourceKinds:        []string{"app_store_connect"},
			DefaultStatus:      "blocked_private_config",
			Produces:           []string{"product signal", "product loop evidence"},
			NextSafeAction:     "Run the read-only ASC preflight with private product mapping config.",
			PrivateSetupNeeded: true,
		},
		{
			ID:             "apple_search_ads",
			Label:          "Apple Search Ads",
			SourceKinds:    []string{"apple_search_ads"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"paid acquisition rows", "product signal"},
			NextSafeAction: "Export a daily campaign report (CSV) from Apple Search Ads and import it with `compound product ads import --platform apple_search_ads`.",
		},
		{
			ID:             "google_ads",
			Label:          "Google Ads",
			SourceKinds:    []string{"google_ads"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"paid acquisition rows", "product signal"},
			NextSafeAction: "Export a daily campaign report (CSV) from Google Ads and import it with `compound product ads import --platform google_ads`.",
		},
		{
			ID:             "meta_ads",
			Label:          "Meta Ads",
			SourceKinds:    []string{"meta_ads"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"paid acquisition rows", "product signal"},
			NextSafeAction: "Export a daily campaign report (CSV) from Meta Ads and import it with `compound product ads import --platform meta_ads`.",
		},
		{
			ID:             "tiktok_ads",
			Label:          "TikTok Ads",
			SourceKinds:    []string{"tiktok_ads"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"paid acquisition rows", "product signal"},
			NextSafeAction: "Export a daily campaign report (CSV) from TikTok Ads and import it with `compound product ads import --platform tiktok_ads`.",
		},
		{
			ID:             "x_ads",
			Label:          "X Ads",
			SourceKinds:    []string{"x_ads"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"paid acquisition rows", "product signal"},
			NextSafeAction: "Export a daily campaign report (CSV) from X Ads and import it with `compound product ads import --platform x_ads`.",
		},
		{
			ID:             "reddit_ads",
			Label:          "Reddit Ads",
			SourceKinds:    []string{"reddit_ads"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"paid acquisition rows", "product signal"},
			NextSafeAction: "Export a daily campaign report (CSV) from Reddit Ads and import it with `compound product ads import --platform reddit_ads`.",
		},
		{
			ID:             "linkedin_ads",
			Label:          "LinkedIn Ads",
			SourceKinds:    []string{"linkedin_ads"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"paid acquisition rows", "product signal"},
			NextSafeAction: "Export a daily campaign report (CSV) from LinkedIn Ads and import it with `compound product ads import --platform linkedin_ads`.",
		},
		{
			ID:             "google_search_console",
			Label:          "Google Search Console",
			SourceKinds:    []string{"google_search_console"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"product signal", "search signal"},
			NextSafeAction: "Observe a bounded GSC export through source intake.",
		},
		{
			ID:                 "app_store_reviews",
			Label:              "App Store customer reviews",
			SourceKinds:        []string{"app_store_reviews"},
			DefaultStatus:      "blocked_private_config",
			Produces:           []string{"source observation", "product source issue evidence"},
			NextSafeAction:     "Normalize a bounded reviews pull into an observation batch and dry-run it through source observe-batch.",
			PrivateSetupNeeded: true,
		},
		{
			ID:             "playtest_findings",
			Label:          "Playtest findings",
			SourceKinds:    []string{"playtest_findings", "playtest_run"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"source observation", "ux friction evidence"},
			NextSafeAction: "Convert a playtest run's finding files into an observation batch and dry-run it through source observe-batch.",
		},
		{
			ID:                 "chatgpt_export",
			Label:              "ChatGPT export",
			SourceKinds:        []string{"chatgpt", "chatgpt_export", "conversation_export"},
			DefaultStatus:      "ready_preflight",
			Produces:           []string{"knowledge evidence", "memory candidate"},
			NextSafeAction:     "Run the conversation import preflight on a bounded private export.",
			PrivateSetupNeeded: true,
		},
		{
			ID:             "github_stars",
			Label:          "GitHub stars",
			SourceKinds:    []string{"github_stars", "github_starred_repos"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"tool candidate", "research reference"},
			NextSafeAction: "Connect a read-only stars export before synthesis.",
		},
		{
			ID:             "x_bookmarks",
			Label:          "X bookmarks",
			SourceKinds:    []string{"x_bookmarks", "twitter_bookmarks"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"reference", "explicit candidate classes"},
			NextSafeAction: "Import a bounded bookmarks export as source material, not memory.",
		},
		{
			ID:             "postiz",
			Label:          "Postiz",
			SourceKinds:    []string{"postiz", "postiz_metrics"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"distribution signal", "social metric"},
			NextSafeAction: "Connect read-only post/channel metrics before any publishing flow.",
		},
		{
			ID:             "codex_sessions",
			Label:          "Codex sessions",
			SourceKinds:    []string{"codex", "codex_session", "codex_sessions"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"session evidence", "skill candidate"},
			NextSafeAction: "Run a small session-mining dry run before broad import.",
		},
		{
			ID:             "claude_sessions",
			Label:          "Claude Code sessions",
			SourceKinds:    []string{"claude", "claude_code", "claude_sessions"},
			DefaultStatus:  "not_connected",
			Produces:       []string{"session evidence", "skill candidate"},
			NextSafeAction: "Run a small session-mining dry run before broad import.",
		},
	}
}

func sourceConnectionRowStatus(connectionID string, row SourceIntakeRow, now time.Time) string {
	if row.Status == RouteBlockedPrivate {
		if connectionID == "asc_daily_signals" {
			return "blocked_private_config"
		}
		return "needs_private_input"
	}
	if row.Status == RouteUnsupportedKind {
		return "needs_mapping"
	}
	if sourceIntakeRowIsStale(row, now) {
		return "stale"
	}
	switch row.Status {
	case "applied", "review_recorded", "planned":
		// Applied evidence stays observed even when advisory action cards ride
		// along — cards surface operator work, they do not demote seen evidence.
		return "observed"
	}
	if row.ActionRequired {
		return "needs_review"
	}
	if row.Status == "skipped" {
		return "needs_review"
	}
	return firstNonEmpty(row.Status, "observed")
}

func sourceConnectionAvailability(conn SourceConnection) string {
	switch conn.Status {
	case "connected":
		return "healthy"
	case "blocked_private_config", "needs_private_input":
		return "needs_private_config"
	case "ready_preflight":
		return "configured"
	case "not_connected", "needs_mapping":
		return "not_configured"
	default:
		if conn.PrivateSetupNeeded {
			return "needs_private_config"
		}
		return "unknown"
	}
}

func sourceConnectionEvidenceState(conn SourceConnection) string {
	switch conn.Status {
	case "stale":
		return "stale"
	case "imported":
		return "imported"
	}
	if conn.RowCount > 0 || conn.LastSeen != "" || conn.SourceRef != "" {
		return "observed"
	}
	// Cards without any seen evidence: the source has pending operator action
	// but no data yet. Seen evidence above always outranks pending cards.
	if conn.LatestCardCount > 0 {
		return "has_cards"
	}
	return "never_seen"
}

func sourceConnectionLegacyStatus(availability, evidenceState, existing string) string {
	if strings.TrimSpace(existing) != "" {
		return existing
	}
	if evidenceState != "" && evidenceState != "never_seen" {
		return evidenceState
	}
	switch availability {
	case "healthy":
		return "connected"
	case "needs_private_config":
		return "needs_private_input"
	case "configured":
		return "ready_preflight"
	case "not_configured":
		return "not_connected"
	default:
		return "not_connected"
	}
}

func strongerSourceConnectionStatus(a, b string) string {
	if sourceConnectionStatusRank(b) > sourceConnectionStatusRank(a) {
		return b
	}
	return a
}

func sourceConnectionStatusRank(status string) int {
	switch status {
	case "blocked_private_config", "needs_private_input":
		return 90
	case "ready_preflight":
		return 80
	case "needs_review", "needs_mapping":
		return 70
	case "stale":
		return 60
	case "connected":
		return 40
	case "imported":
		return 35
	case "observed":
		return 30
	case "not_connected":
		return 10
	default:
		return 20
	}
}

func sourceSeverityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func producesForSourceRow(row SourceIntakeRow) []string {
	switch row.Route {
	case RouteProductSignal:
		return []string{"product signal", "product loop evidence"}
	case RouteKnowledgeExport:
		return []string{"knowledge evidence", "memory candidate"}
	case RouteSourceObservation:
		return []string{"source observation", "candidate evidence"}
	case RouteRouteEvidence:
		return []string{"route evidence"}
	case RouteTraceEnvelope:
		return []string{"event trace", "workflow evidence"}
	case RouteCandidateOnly:
		return []string{"review candidate"}
	default:
		if row.SubjectKind != "" {
			return []string{row.SubjectKind + " evidence"}
		}
		return nil
	}
}

func sourceConnectionNextAction(status string) string {
	switch status {
	case "blocked_private_config", "needs_private_input":
		return "Supply private setup through runtime config or keep the source visibly blocked."
	case "ready_preflight":
		return "Run the read-only preflight before importing source material."
	case "needs_review":
		return "Resolve the latest source action card."
	case "needs_mapping":
		return "Map the source to an existing owner before import."
	case "stale":
		return "Refresh the source or keep the stale state visible with reason."
	case "connected", "imported", "observed":
		return "Review the latest source rows and continue through existing cards."
	default:
		return "Connect a bounded read-only export before automation."
	}
}

func newestTimestamp(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return strings.TrimSpace(b)
	}
	if strings.TrimSpace(b) == "" {
		return strings.TrimSpace(a)
	}
	at, aerr := time.Parse(time.RFC3339, a)
	bt, berr := time.Parse(time.RFC3339, b)
	if aerr == nil && berr == nil {
		if bt.After(at) {
			return b
		}
		return a
	}
	if b > a {
		return b
	}
	return a
}

func titleFromSlug(value string) string {
	parts := strings.Fields(strings.ReplaceAll(strings.TrimSpace(value), "_", " "))
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	if len(parts) == 0 {
		return "Unknown source"
	}
	return strings.Join(parts, " ")
}

func slugPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unknown"
	}
	return out
}
