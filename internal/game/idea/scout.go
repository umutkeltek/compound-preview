// Package idea composes game ideas from observed game-signal evidence and
// (optionally) rents an LLM to write a structured one-pager.
//
// scout.go is deterministic: it reads the on-disk SourceObservation store,
// matches the operator's query against observation Subjects, and returns a
// ScoutPacket that bundles anchors (genre/company/app hits), evidence refs
// (with freshness stamps), and the latest trending slice. No LLM call, no
// network — pure read model.
//
// draft.go layers an LLM on top: it calls scout first (so the LLM has
// evidence anchored in compound), then rents a model lane to draft a one-
// pager, then parks the result as a KindProductHypothesis candidate. The
// verdict is always Park — promotion requires the operator to run
// `compound product hypothesis-plan` against the candidate id.
package idea

import (
	"context"
	"sort"
	"strings"
	"time"

	"compound/internal/game"
	"compound/internal/sourceobs"
)

// ScoutAnchor is one observed entity that matched the query.
type ScoutAnchor struct {
	Kind        string   `json:"kind"`
	SourceKey   string   `json:"source_key"`
	Label       string   `json:"label"`
	Source      string   `json:"source"`
	Observation int      `json:"observation_count"`
	AppIDs      []string `json:"app_ids,omitempty"`
}

// ScoutEvidence is a SourceObservation reference carrying its freshness
// stamp. Used in the ScoutPacket so the LLM (and the human reviewer) can
// see how current each data point is.
type ScoutEvidence struct {
	ObservationID string    `json:"observation_id"`
	Kind          string    `json:"kind"`
	Label         string    `json:"label"`
	Source        string    `json:"source"`
	Statement     string    `json:"statement,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// MomentumItem is one trending observation included in the packet.
type MomentumItem struct {
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Hits   int    `json:"hits"`
	Source string `json:"source"`
}

// ScoutPacket is the deterministic evidence bundle the LLM (or the human
// reviewer) sees. Schema-stable across runs against the same store so
// diffable.
type ScoutPacket struct {
	Schema         string          `json:"schema"`
	ID             string          `json:"id"`
	Query          string          `json:"query"`
	CompiledAt     time.Time       `json:"compiled_at"`
	Anchors        []ScoutAnchor   `json:"anchors"`
	Evidence       []ScoutEvidence `json:"evidence"`
	RecentMomentum []MomentumItem  `json:"recent_momentum"`
	Warnings       []string        `json:"warnings,omitempty"`
}

const scoutSchema = "compound/game-scout-packet.v0.1"

// ComposeScoutPacket reads the on-disk observation store and builds a
// ScoutPacket for the given query. Matching is case-insensitive substring
// against observation Subjects and Statements; results are grouped by
// (kind, source_key) and capped at maxAnchors to keep the packet compact.
func ComposeScoutPacket(ctx context.Context, homePath, query string, now time.Time, maxAnchors int) (ScoutPacket, error) {
	if maxAnchors <= 0 {
		maxAnchors = 20
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	pkt := ScoutPacket{
		Schema:     scoutSchema,
		ID:         game.GameID("scout", strings.ToLower(strings.TrimSpace(query)), "packet"),
		Query:      query,
		CompiledAt: now,
	}

	if strings.TrimSpace(query) == "" {
		pkt.Warnings = append(pkt.Warnings, "empty query; packet includes only trending")
	}

	list, err := sourceobs.ListSourceObservations(homePath, now)
	if err != nil {
		return pkt, err
	}

	q := strings.ToLower(strings.TrimSpace(query))
	type anchorBucket struct {
		anchor ScoutAnchor
		appSet map[string]bool
		obs    []ScoutEvidence
	}
	buckets := map[string]*anchorBucket{}

	for _, it := range list.Items {
		if !matchesQuery(it.Subject, q) && !matchesQuery(it.Statement, q) {
			continue
		}
		// Parse the Subject to recover kind + source_key. Use the
		// 4-value signature from projection; ignore label/metadata.
		kind, sourceKey, label, md, ok := parseScoutSubject(it.Subject)
		if !ok {
			continue
		}
		gk := kind + "|" + sourceKey
		b, exists := buckets[gk]
		if !exists {
			b = &anchorBucket{
				anchor: ScoutAnchor{
					Kind:      kind,
					SourceKey: sourceKey,
					Label:     label,
					Source:    md["source"],
				},
				appSet: map[string]bool{},
			}
			buckets[gk] = b
		}
		b.anchor.Observation++
		if appid := md["appid"]; appid != "" {
			b.appSet[appid] = true
		}
		if b.anchor.Source == "" {
			b.anchor.Source = md["source"]
		}
		b.obs = append(b.obs, ScoutEvidence{
			ObservationID: it.SourceObservationID,
			Kind:          kind,
			Label:         label,
			Source:        b.anchor.Source,
			Statement:     it.Statement,
			ObservedAt:    parseScoutCreated(it.CreatedAt),
		})
	}

	for _, b := range buckets {
		if len(b.anchor.AppIDs) == 0 && len(b.appSet) > 0 {
			for appid := range b.appSet {
				b.anchor.AppIDs = append(b.anchor.AppIDs, appid)
			}
			sort.Strings(b.anchor.AppIDs)
		}
		pkt.Anchors = append(pkt.Anchors, b.anchor)
		pkt.Evidence = append(pkt.Evidence, b.obs...)
	}
	sort.Slice(pkt.Anchors, func(i, j int) bool {
		if pkt.Anchors[i].Observation != pkt.Anchors[j].Observation {
			return pkt.Anchors[i].Observation > pkt.Anchors[j].Observation
		}
		return pkt.Anchors[i].Label < pkt.Anchors[j].Label
	})
	if len(pkt.Anchors) > maxAnchors {
		pkt.Anchors = pkt.Anchors[:maxAnchors]
	}
	sort.Slice(pkt.Evidence, func(i, j int) bool {
		return pkt.Evidence[i].ObservedAt.After(pkt.Evidence[j].ObservedAt)
	})

	// Always include the top 5 trending observations regardless of query.
	trending, err := game.ProjectTrending(homePath, now, 5, 7)
	if err == nil {
		for _, e := range trending.Entries {
			pkt.RecentMomentum = append(pkt.RecentMomentum, MomentumItem{
				Kind:   e.Kind,
				Label:  e.Label,
				Hits:   e.Hits,
				Source: "trending",
			})
		}
	} else {
		pkt.Warnings = append(pkt.Warnings, "trending unavailable: "+err.Error())
	}

	return pkt, nil
}

func matchesQuery(haystack, needle string) bool {
	if needle == "" {
		return false // empty query matches nothing; trending is the only output
	}
	if haystack == "" {
		return false
	}
	return strings.Contains(strings.ToLower(haystack), needle)
}

// parseScoutSubject is the projection's parseGameSubject inlined so the
// idea package does not depend on internal internals beyond the public
// gamesource types. Same format: "game:<kind>:<sourceKey>::<label>|<metadata>".
func parseScoutSubject(subject string) (kind, sourceKey, label string, metadata map[string]string, ok bool) {
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

func parseScoutCreated(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
