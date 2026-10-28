// Package lanematrix answers "which harness can run which lane, and which of
// those combinations exist yet" from the launch owner's published conformance
// projections plus compound's harness registry.
//
// It exists because that question was previously answered by listing launcher
// files, which is the enumeration mistake docs/NAMING.md §Identifier grammar
// rule 5 names directly: a surface that enumerates by listing a directory
// reports containers, stale backups, and hand-written one-offs as members. One
// observed deployment carried 52 launcher files against 11 lanes, so the
// directory listing over-reported realized combinations by a factor of five.
//
// Compound is a CONSUMER here (see the projection's own authority block). The
// launch owner publishes profile conformance; this package reads it and never
// writes, probes, or infers launch state.
package lanematrix

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProjectionSchema is the published conformance contract this package reads. A
// projection announcing a different schema is skipped rather than guessed at:
// docs/NAMING.md forbids reading an unreadable input as an absence.
const ProjectionSchema = "switchback/profile-conformance@1"

// Cell states. Deliberately three, not two: the useful answer to "can this
// harness use that lane" is the middle one, and collapsing Unknown into
// Incompatible would report a missing input as a definite no.
const (
	// StateRealized: a published profile binds this harness to this lane.
	StateRealized = "realized"
	// StatePossible: no profile binds them, and nothing declares them incompatible.
	StatePossible = "possible"
	// StateUnknown: compatibility cannot be decided from published facts.
	StateUnknown = "unknown"
)

// Lane is one published profile, reduced to the facts a matrix needs.
//
// Route is the canonical lane identity (docs/NAMING.md: `<vendor>/<model>`).
// ID is the composite profile id, which is NOT a lane name -- keeping both is
// what stops this surface from re-flattening the two into one string.
type Lane struct {
	ProfileID     string `json:"profile_id"`
	Route         string `json:"route"`
	ProviderLane  string `json:"provider_lane"`
	Harness       string `json:"harness"`
	CapturePolicy string `json:"capture_policy,omitempty"`
	Revision      string `json:"conformance_revision,omitempty"`
	SourcePath    string `json:"source_path,omitempty"`
}

// Cell is one (harness, lane) combination.
type Cell struct {
	Harness   string `json:"harness"`
	Route     string `json:"route"`
	ProfileID string `json:"profile_id,omitempty"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

// Matrix is the read-only projection.
type Matrix struct {
	Harnesses []string `json:"harnesses"`
	Lanes     []Lane   `json:"lanes"`
	Cells     []Cell   `json:"cells"`
	// Limits records what this projection could NOT decide, so a caller reading
	// only the cells still learns the answer is partial.
	Limits []string `json:"limits,omitempty"`
}

// projectionFile mirrors only the published fields this package consumes.
type projectionFile struct {
	Schema              string `json:"schema"`
	ConformanceRevision string `json:"conformance_revision"`
	Profile             struct {
		ID            string `json:"id"`
		Harness       string `json:"harness"`
		ProviderLane  string `json:"provider_lane"`
		Route         string `json:"route"`
		CapturePolicy struct {
			Mode string `json:"mode"`
		} `json:"capture_policy"`
	} `json:"profile"`
}

// LoadLanes reads every published conformance projection in dir.
//
// A file that is unreadable, unparseable, or announces a different schema is
// skipped and reported through the returned skip list -- never silently treated
// as absent, and never guessed at.
func LoadLanes(dir string) ([]Lane, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read projection dir: %w", err)
	}
	var lanes []Lane
	var skipped []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			skipped = append(skipped, fmt.Sprintf("%s: unreadable (%v)", e.Name(), readErr))
			continue
		}
		var pf projectionFile
		if jsonErr := json.Unmarshal(raw, &pf); jsonErr != nil {
			skipped = append(skipped, fmt.Sprintf("%s: unparseable (%v)", e.Name(), jsonErr))
			continue
		}
		if strings.TrimSpace(pf.Schema) != ProjectionSchema {
			skipped = append(skipped, fmt.Sprintf("%s: schema %q, want %q", e.Name(), pf.Schema, ProjectionSchema))
			continue
		}
		if strings.TrimSpace(pf.Profile.Route) == "" {
			skipped = append(skipped, fmt.Sprintf("%s: no route (canonical lane identity absent)", e.Name()))
			continue
		}
		lanes = append(lanes, Lane{
			ProfileID:     pf.Profile.ID,
			Route:         pf.Profile.Route,
			ProviderLane:  pf.Profile.ProviderLane,
			Harness:       pf.Profile.Harness,
			CapturePolicy: pf.Profile.CapturePolicy.Mode,
			Revision:      pf.ConformanceRevision,
			SourcePath:    path,
		})
	}
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Route < lanes[j].Route })
	return lanes, skipped, nil
}

// NormalizeHarness maps a harness identifier to a comparable form.
//
// Compound spells harnesses snake_case (docs/NAMING.md: `harness <program>`);
// the launch owner spells the same family with dashes. Comparing them raw is
// how a realized combination reads as possible.
func NormalizeHarness(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(strings.ToLower(name)), "_", "-")
}

// Build composes harnesses against lanes.
//
// A cell is realized when a published profile BINDS that harness to that route.
// The binding key is the profile id, not the harness family: matching on family
// would mark every harness in a family realized on every lane the family
// reaches, which is precisely the conflation this design is repairing. That
// compound's harness identifiers currently normalize onto profile ids is the
// symptom -- docs/NAMING.md keeps `harness` and `profile` as distinct nouns, and
// this surface makes the overlap visible rather than hiding it.
//
// harnessProfile overrides which profile id a harness binds to; a harness absent
// from the map binds to its own name.
//
// Compatibility is NOT inferred. The published projection carries no wire
// protocol and no tap binding, so a combination the launch owner never bound is
// reported StateUnknown with the missing input named. Reporting it as
// StatePossible would assert a compatibility nobody verified -- the over-claim
// docs/NAMING.md §Capability calls the unsafe direction.
func Build(harnesses []string, lanes []Lane, harnessProfile map[string]string) Matrix {
	m := Matrix{Harnesses: append([]string(nil), harnesses...), Lanes: lanes}
	sort.Strings(m.Harnesses)

	realized := make(map[string]Lane, len(lanes))
	for _, l := range lanes {
		realized[NormalizeHarness(l.ProfileID)+"\x00"+l.Route] = l
	}

	for _, h := range m.Harnesses {
		binding := h
		if b, ok := harnessProfile[h]; ok && strings.TrimSpace(b) != "" {
			binding = b
		}
		nf := NormalizeHarness(binding)
		for _, l := range lanes {
			cell := Cell{Harness: h, Route: l.Route}
			if bound, ok := realized[nf+"\x00"+l.Route]; ok {
				cell.State = StateRealized
				cell.ProfileID = bound.ProfileID
			} else {
				cell.State = StateUnknown
				cell.Reason = "published projection carries no wire protocol or tap binding for this lane"
			}
			m.Cells = append(m.Cells, cell)
		}
	}
	sort.Slice(m.Cells, func(i, j int) bool {
		if m.Cells[i].Harness != m.Cells[j].Harness {
			return m.Cells[i].Harness < m.Cells[j].Harness
		}
		return m.Cells[i].Route < m.Cells[j].Route
	})

	if len(lanes) > 0 {
		m.Limits = append(m.Limits,
			"compatibility undecidable: profile-conformance@1 publishes no wire_api or anthropic_tap; "+
				"unbound combinations report unknown, never possible")
	}
	return m
}

// Counts summarizes cells by state, for a caller that wants the shape without
// walking every cell.
func (m Matrix) Counts() map[string]int {
	out := map[string]int{}
	for _, c := range m.Cells {
		out[c.State]++
	}
	return out
}
