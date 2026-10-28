package gamesource

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/sourceobs"
)

type ApplyResult struct {
	AdapterID    string
	SourceSystem string
	Revision     string
	Written      int
	Failed       int
	CandidateIDs []string
	NoPromoteIDs []string
}

func Apply(ctx context.Context, homePath string, res FetchResult, now time.Time) (ApplyResult, error) {
	if res.AdapterID == "" {
		return ApplyResult{}, fmt.Errorf("gamesource.Apply: AdapterID required")
	}
	if res.SourceSys == "" {
		return ApplyResult{}, fmt.Errorf("gamesource.Apply: SourceSys required")
	}
	if homePath == "" {
		return ApplyResult{}, fmt.Errorf("gamesource.Apply: homePath required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	out := ApplyResult{
		AdapterID:    res.AdapterID,
		SourceSystem: res.SourceSys,
		Revision:     res.RawSHA256,
	}

	for i, it := range res.Items {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		obsRev := uniqueObservationRevision(res.RawSHA256, now, i)
		obs, err := BuildObservation(res.AdapterID, res.SourceSys, obsRev, it, now)
		if err != nil {
			out.Failed++
			return out, err
		}
		report, err := sourceobs.Apply(homePath, obs, now)
		if err != nil {
			out.Failed++
			return out, fmt.Errorf("gamesource.Apply: %s/%s: %w", it.Kind, it.SourceKey, err)
		}
		out.Written++
		out.CandidateIDs = append(out.CandidateIDs, report.CandidateIDs...)
		out.NoPromoteIDs = append(out.NoPromoteIDs, report.NoPromoteIDs...)
	}
	return out, nil
}

func BuildObservation(adapterID, sourceSys, revision string, it RawObservation, now time.Time) (sourceobs.SourceObservation, error) {
	if adapterID == "" {
		return sourceobs.SourceObservation{}, fmt.Errorf("BuildObservation: adapterID required")
	}
	if sourceSys == "" {
		return sourceobs.SourceObservation{}, fmt.Errorf("BuildObservation: sourceSys required")
	}
	if it.Kind == "" {
		return sourceobs.SourceObservation{}, fmt.Errorf("BuildObservation: Kind required")
	}
	if it.SourceKey == "" {
		return sourceobs.SourceObservation{}, fmt.Errorf("BuildObservation: SourceKey required")
	}
	if len(it.Evidence) == 0 {
		return sourceobs.SourceObservation{}, fmt.Errorf("BuildObservation: Evidence required (URL + revision only)")
	}

	path := it.Evidence[0]
	id := "srcobs_" + gameIDFor(adapterID, it.SourceKey, it.Kind)

	return sourceobs.SourceObservation{
		SchemaVersion: sourceobs.SchemaVersion,
		ID:            id,
		AdapterID:     adapterID,
		SourceSystem:  sourceSys,
		ObservedAt:    rfc3339UTC(now),
		SourceRef: sourceobs.SourceRef{
			Path:        path,
			Kind:        "api_view",
			Owner:       adapterID,
			SourceClass: sourceSys,
			Revision:    revision,
		},
		MapsTo: []sourceobs.MapTarget{{
			ObjectFamily:   "game",
			CandidateKind:  string(candidates.CapabilityObs),
			EntityHint:     entityHint(it.Kind, it.SourceKey, it.Subject, observationMetadata(it, sourceSys)),
			PromoteAllowed: false,
		}},
		EvidenceRefs: append([]string(nil), it.Evidence...),
	}, nil
}

// entityHint encodes (kind, source_key, label, key=value metadata) into the
// EntityHint field. CandidateFromMap stores EntityHint verbatim in
// CandidateRecord.Subject, so the projection reads it back to recover
// grouping key, label, and per-observation metadata.
//
// Format: "game:<kind>:<source_key>::<label>|<k1=v1;k2=v2>". metadata
// fields are positional after a single `|` and `;`-separated. Empty
// metadata omits the trailing `|...`.
func entityHint(kind, sourceKey, label string, metadata map[string]string) string {
	out := "game:" + kind + ":" + sourceKey + "::" + label
	if len(metadata) > 0 {
		// stable ordering: appid, role, then any others alphabetically
		keys := make([]string, 0, len(metadata))
		for k := range metadata {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			// prioritize canonical keys
			prio := func(k string) int {
				switch k {
				case "appid":
					return 0
				case "role":
					return 1
				}
				return 2
			}
			if prio(keys[i]) != prio(keys[j]) {
				return prio(keys[i]) < prio(keys[j])
			}
			return keys[i] < keys[j]
		})
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+metadata[k])
		}
		out += "|" + strings.Join(parts, ";")
	}
	return out
}

func rfc3339UTC(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func gameIDFor(adapterID, sourceKey, kind string) string {
	parts := []string{
		trimSpace(adapterID),
		trimSpace(sourceKey),
		trimSpace(kind),
	}
	return sha256Hex12(joinWithUS(parts))
}

// observationMetadata extracts the per-observation metadata that the
// projection needs: appid, role, and the short source-system name.
func observationMetadata(it RawObservation, sourceSys string) map[string]string {
	md := map[string]string{}
	switch it.Kind {
	case "app", "spy_estimate":
		md["appid"] = it.SourceKey
	}
	// Genre/company observations carry the source appid in their Statement
	// string ("... on appid 440"). Lift it out so the projection can
	// attribute apps to a genre/company without depending on the candidate
	// Statement (which CandidateFromMap rewrites to "family:source_ref").
	if appid := appidFromStatement(it.Statement); appid != "" {
		md["appid"] = appid
	}
	if role, ok := roleFromText(it.Detail); ok {
		md["role"] = role
	} else if role, ok := roleFromText(it.Statement); ok {
		md["role"] = role
	}
	if sourceSys != "" {
		md["source"] = sourceSys
	}
	if len(md) == 0 {
		return nil
	}
	return md
}

// appidFromStatement lifts a Steam appid out of a Statement string. The
// adapter writes Statements like `Game is in genre "Action" on appid 440`
// so this finds the first numeric token after "appid ".
func appidFromStatement(statement string) string {
	s := statement
	if i := strings.Index(s, "appid "); i >= 0 {
		tail := strings.TrimSpace(s[i+len("appid "):])
		end := 0
		for end < len(tail) && tail[end] >= '0' && tail[end] <= '9' {
			end++
		}
		if end > 0 {
			return tail[:end]
		}
	}
	return ""
}

// roleFromText extracts the "role: developer" / "role: publisher" tag from
// either the Detail or Statement field. The adapter sets Detail ("role:
// developer"), test seeds (and any future writer) put the hint in Statement
// ("develops appid 440"). Both are accepted so seeds match adapter output.
func roleFromText(s string) (string, bool) {
	d := strings.ToLower(s)
	switch {
	case strings.Contains(d, "role: developer"), strings.Contains(d, "develops appid"):
		return "developer", true
	case strings.Contains(d, "role: publisher"), strings.Contains(d, "publishes appid"):
		return "publisher", true
	}
	return "", false
}
