package home

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProjectionFreshness is one projection under $COMPOUND_HOME/projections and how
// its recorded build time compares to the code that would rebuild it.
type ProjectionFreshness struct {
	// Name is the projection file's base name, e.g. "negatives.json".
	Name string `json:"name"`
	// RelPath is home-relative, so the row is quotable without leaking an absolute path.
	RelPath string `json:"rel_path"`
	// ComputedAt is the projection's own recorded build time, from whichever
	// timestamp field it carries. Empty when it records none.
	ComputedAt string `json:"computed_at,omitempty"`
	// TimestampField names the field ComputedAt was read from, so a reader can tell
	// "this projection dates itself" from "this projection cannot".
	TimestampField string `json:"timestamp_field,omitempty"`
	// OlderThanCode is true when ComputedAt precedes the reference build time. The
	// body may still be correct — this says only that it was NOT produced by the
	// code now installed.
	OlderThanCode bool `json:"older_than_code"`
	// Undated is true when the projection records no build time at all. Treated as
	// unprovable rather than stale: a body that cannot date itself cannot be
	// cleared, but neither can it be accused.
	Undated bool `json:"undated"`
	// AgeHours is how old ComputedAt is relative to now, for the operator's sense of
	// scale. Zero when Undated.
	AgeHours float64 `json:"age_hours,omitempty"`
	// RebuildCommand is the command that regenerates this projection, when one is
	// known. Empty rather than guessed: a wrong command in a remediation line is
	// worse than no command, because it will be run.
	RebuildCommand string `json:"rebuild_command,omitempty"`
}

// projectionRebuildCommands maps a projection body to the command that rebuilds it.
//
// Only entries verified to actually WRITE the projection are listed, and verified by
// running them and re-reading the body's timestamp — not by reading the help text.
// `compound memory temperature` without --rebuild prints a preview and leaves the
// persisted body untouched, so listing the bare form here would have told the
// operator to run a command that changes nothing and then report the row as
// unfixable. A projection written only as a side effect of another flow is left
// without a command at all, because "no command shown" is honest and a command that
// does not do what the row claims will still be run.
var projectionRebuildCommands = map[string]string{
	"negatives.json":          "compound negatives build",
	"memory-temperature.json": "compound memory temperature --rebuild",
	"capability-usage.json":   "compound capability usage",
}

// projectionTimestampFields are the field names a projection may use to date
// itself, in the order they are trusted. Different producers picked different
// names; the freshness check reads whichever is present rather than forcing a
// migration on bodies whose only problem is vocabulary.
var projectionTimestampFields = []string{"computed_at", "generated_at", "imported_at", "updated_at"}

// ProjectionFreshnessReport compares every REBUILDABLE projection body under
// $COMPOUND_HOME/projections against a reference build time — normally the mtime
// of the running binary — and reports which ones predate it.
//
// Rebuildable is the load-bearing word, and it is resolved from the habitat catalog
// (HabitatForPath) rather than from the directory name. `projections/` is
// HabitatClassRebuildable as a whole, but several children are carved out as
// HabitatClassOperational because they are STATE, not derived caches:
// orient-operator-last-seq.json is a cursor, narrate-thread-seen.json is a
// seen-marker, account-projection.json is admission state. For those, age is
// meaningful data — "the operator surface has not been used in 76h" — and not
// staleness at all. Reporting them as unrebuilt caches was measured to produce
// three false alarms out of five rows on a real home, which is precisely the
// cry-wolf failure this check is supposed to avoid.
//
// WHY THIS EXISTS: a fix to a projection-backed surface is not live until the
// projection is rebuilt, and every signal short of observing the real path can say
// otherwise. Recorded as find_b27a3de99682 after exactly that: a privacy fix to
// internal/negatives passed its tests, landed, and was reinstalled, and the live
// digest kept emitting the unredacted value — because the read path trusts a
// persisted body inside its freshness window, and that body had been produced by
// the pre-fix binary. The test could not catch it: a test that rebuilds its own
// inputs is structurally blind to a stale persisted input on the real path.
//
// It reports, and never rebuilds. Rebuilding here would make a read-only health
// command mutate shared state that concurrent lanes are reading, and would also
// destroy the evidence the operator is being shown.
//
// A row is a WARNING, not a defect: code newer than a projection only matters when
// the change touched that projection's producer, which this cannot know. The honest
// claim is "not built by the code now installed", so that is what it says.
func ProjectionFreshnessReport(homePath string, codeBuiltAt, now time.Time) ([]ProjectionFreshness, error) {
	dir := filepath.Join(strings.TrimSpace(homePath), "projections")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rows := make([]ProjectionFreshness, 0, len(entries))
	for _, entry := range entries {
		// Only top-level .json bodies: subdirectories hold per-key projections whose
		// freshness belongs to their own owner, and .jsonl is an append log whose
		// age is not a build time.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("projections", entry.Name()))
		// Only derived caches are subject to a freshness claim. State, config and
		// cursors that happen to live under projections/ are excluded by CLASS, from
		// the catalog that already owns that distinction — not by a second list here
		// that would drift away from it.
		if habitat, ok := HabitatForPath(rel); ok && habitat.Class != HabitatClassRebuildable {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(body, &doc) != nil {
			continue // foreign or torn json is not this check's business
		}

		row := ProjectionFreshness{
			Name:           entry.Name(),
			RelPath:        rel,
			RebuildCommand: projectionRebuildCommands[entry.Name()],
		}
		field, stamp, ok := projectionComputedAt(doc)
		if !ok {
			row.Undated = true
			rows = append(rows, row)
			continue
		}
		row.TimestampField = field
		row.ComputedAt = stamp.UTC().Format(time.RFC3339)
		row.OlderThanCode = stamp.Before(codeBuiltAt)
		if age := now.UTC().Sub(stamp.UTC()); age > 0 {
			row.AgeHours = age.Hours()
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

func projectionComputedAt(doc map[string]json.RawMessage) (string, time.Time, bool) {
	for _, field := range projectionTimestampFields {
		raw, present := doc[field]
		if !present {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
			continue
		}
		stamp, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
		if err != nil {
			continue
		}
		return field, stamp, true
	}
	return "", time.Time{}, false
}

// RunningBinaryBuiltAt is the reference build time for the freshness check: the
// mtime of the executable currently running.
//
// Deliberately the RUNNING binary rather than a recorded release version. The
// question a reader needs answered is "was this body produced by the code I am
// about to trust", and the code they are about to trust is the one executing.
func RunningBinaryBuiltAt() (time.Time, error) {
	exe, err := os.Executable()
	if err != nil {
		return time.Time{}, err
	}
	// Resolve symlinks so a versioned install target reports the real artifact's
	// mtime rather than the link's.
	if resolved, linkErr := filepath.EvalSymlinks(exe); linkErr == nil {
		exe = resolved
	}
	info, err := os.Stat(exe)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime().UTC(), nil
}

// StaleProjectionSummary is the one-line operator summary: how many projections
// predate the running code, and the oldest offender.
func StaleProjectionSummary(rows []ProjectionFreshness) (stale int, undated int, oldest string) {
	var oldestAge float64
	for _, row := range rows {
		if row.Undated {
			undated++
			continue
		}
		if !row.OlderThanCode {
			continue
		}
		stale++
		if row.AgeHours > oldestAge {
			oldestAge = row.AgeHours
			oldest = fmt.Sprintf("%s (%s, %.0fh old)", row.Name, row.TimestampField, row.AgeHours)
		}
	}
	return stale, undated, oldest
}
