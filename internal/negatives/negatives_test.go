package negatives

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
)

var t0 = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

func writeFailurePattern(t *testing.T, home, id string, fields map[string]any) {
	t.Helper()
	doc := map[string]any{
		"id":   id,
		"kind": string(candidates.FailurePattern),
	}
	for k, v := range fields {
		doc[k] = v
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, candidates.InboxDir(candidates.FailurePattern))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRouteRemorse(t *testing.T, home, id string, fields map[string]any) {
	t.Helper()
	doc := map[string]any{
		"schema_version": routeOutcomeReviewSchemaVersion,
		"id":             id,
	}
	for k, v := range fields {
		doc[k] = v
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "reviews", routeRemorseReviewArea)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func findRecord(records []Record, refID string) (Record, bool) {
	for _, r := range records {
		if r.RefID == refID {
			return r, true
		}
	}
	return Record{}, false
}

func TestBuild_MissingFailurePatternInboxIsEmpty(t *testing.T) {
	proj, err := Build(t.TempDir(), BuildOptions{Now: t0})
	if err != nil {
		t.Fatalf("Build on fresh home: %v", err)
	}
	if len(proj.Records) != 0 || len(proj.Warnings) != 0 {
		t.Fatalf("fresh-home projection = %+v", proj)
	}
}

// Falsifier: negatives is a base-orient consumer, so it must take failure
// patterns through candidates' descriptor-rooted read path. A corrupt entry is
// excluded and surfaced by its stable, named custody finding; it must never
// mint a NegativeCard, and it must not hide a later valid candidate.
func TestBuild_FailurePatternCustodyCorruptionIsExcludedAndSurfaced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		filename string
		write    func(t *testing.T, dir string)
		wantCode candidates.CorruptionCode
	}{
		{
			name:     "invalid filename",
			filename: "scratch.tmp",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "scratch.tmp"), []byte("not a canonical candidate"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: candidates.CorruptionInvalidFilename,
		},
		{
			name:     "symlink",
			filename: "bad-symlink.json",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Symlink("zz-valid.json", filepath.Join(dir, "bad-symlink.json")); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: candidates.CorruptionSymlink,
		},
		{
			name:     "filename JSON id mismatch",
			filename: "bad-mismatch.json",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "bad-mismatch.json"), []byte(`{"id":"different-id"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: candidates.CorruptionIDMismatch,
		},
		{
			name:     "blank id",
			filename: "bad-blank.json",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "bad-blank.json"), []byte(`{"id":""}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: candidates.CorruptionBlankID,
		},
		{
			name:     "malformed body",
			filename: "bad-malformed.json",
			write: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "bad-malformed.json"), []byte(`{`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: candidates.CorruptionInvalidJSON,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, candidates.InboxDir(candidates.FailurePattern))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFailurePattern(t, home, "zz-valid", map[string]any{
				"subject":   "later-valid-negative",
				"statement": "the valid candidate is still projected after corruption",
				"detail":    "later-valid-negative",
				"verdict":   candidates.Park,
			})
			tc.write(t, dir)

			proj, err := Build(home, BuildOptions{Now: t0})
			if err == nil {
				t.Fatal("Build accepted corrupt failure-pattern entry")
			}
			var corruption *candidates.Corruption
			if !errors.As(err, &corruption) {
				t.Fatalf("Build error = %v, want named candidate corruption", err)
			}
			if corruption.EntryCursor != candidates.EntryCursor(tc.filename) || corruption.Code != tc.wantCode {
				t.Fatalf("corruption = %+v, want cursor=%q code=%q", corruption, tc.filename, tc.wantCode)
			}
			if _, found := findRecord(proj.Records, strings.TrimSuffix(tc.filename, ".json")); found {
				t.Fatalf("corrupt candidate created a negative card: %+v", proj.Records)
			}
			if _, found := findRecord(proj.Records, "zz-valid"); !found {
				t.Fatalf("later valid candidate was hidden by corrupt entry: %+v", proj.Records)
			}
			if len(proj.Warnings) != 1 || proj.Warnings[0].Source != SourceFailurePattern || proj.Warnings[0].Code != tc.wantCode || proj.Warnings[0].Count != 1 {
				t.Fatalf("provider-safe warnings = %+v, want source=%q code=%q count=1", proj.Warnings, SourceFailurePattern, tc.wantCode)
			}
			if strings.Contains(fmt.Sprint(proj.Warnings), tc.filename) {
				t.Fatalf("projection warning leaked operator-controlled filename %q: %+v", tc.filename, proj.Warnings)
			}
		})
	}
}

func TestReadFresh_CorruptFailurePatternStillUsesValidRecords(t *testing.T) {
	home := t.TempDir()
	writeFailurePattern(t, home, "zz-valid", map[string]any{
		"subject":   "later-valid-negative",
		"statement": "the valid candidate remains available after sibling corruption",
		"detail":    "later-valid-negative",
		"verdict":   candidates.Park,
	})
	dir := filepath.Join(home, candidates.InboxDir(candidates.FailurePattern))
	if err := os.WriteFile(filepath.Join(dir, "bad-mismatch.json"), []byte(`{"id":"different-id"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	proj, ok, err := ReadFresh(home, t0)
	if err != nil || !ok {
		t.Fatalf("ReadFresh: ok=%t err=%v", ok, err)
	}
	if _, found := findRecord(proj.Records, "zz-valid"); !found {
		t.Fatalf("valid sibling was discarded: %+v", proj.Records)
	}
	if len(proj.Warnings) != 1 || proj.Warnings[0].Code != candidates.CorruptionIDMismatch {
		t.Fatalf("ReadFresh warnings = %+v", proj.Warnings)
	}
	if _, statErr := os.Stat(filepath.Join(home, filepath.FromSlash(RelPath))); !os.IsNotExist(statErr) {
		t.Fatalf("ReadFresh mutated projection path: %v", statErr)
	}
}

// Falsifier (a): scope match, not dump. An intent whose owner/task-class matches a
// recorded failure gets the negative card; a disjoint intent gets zero — a planted
// off-scope failure must never surface as a firehose.
func TestMatchingRecords_ScopeMatchNotDump(t *testing.T) {
	home := t.TempDir()
	writeFailurePattern(t, home, "fp_scope", map[string]any{
		"subject":       "wrong-fold-target-merge",
		"statement":     "agent folded new content into the wrong merge target",
		"detail":        "fold-target-mismatch",
		"confidence":    0.8,
		"evidence_refs": []string{"trace:fold-1", "owner:console"},
		"verdict":       candidates.Park,
	})
	proj, err := Build(home, BuildOptions{Now: t0})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rec, ok := findRecord(proj.Records, "fp_scope")
	if !ok {
		t.Fatalf("expected fp_scope in projection, got %+v", proj.Records)
	}

	matchingTask := ScopeTokens("please fold the new section into the merge target again")
	ownerYes := ScopeTokens("console")
	if got := MatchingRecords(proj.Records, ownerYes, matchingTask, 5); len(got) != 1 || got[0].RefID != rec.RefID {
		t.Fatalf("expected fp_scope to match in-scope task, got %+v", got)
	}

	disjointTask := ScopeTokens("generate ios app icon assets for the store listing")
	if got := MatchingRecords(proj.Records, ownerYes, disjointTask, 5); len(got) != 0 {
		t.Fatalf("expected ZERO negatives for disjoint task scope, got %+v", got)
	}

	ownerNo := ScopeTokens("otherowner")
	if got := MatchingRecords(proj.Records, ownerNo, matchingTask, 5); len(got) != 0 {
		t.Fatalf("expected ZERO negatives for disjoint owner scope, got %+v", got)
	}
}

// Falsifier (c): producer-agnostic. Both a failure-patterns candidate and a
// route-remorse review that match scope surface through the SAME section — one
// reader, two producers.
func TestBuild_ProducerAgnosticBothSourcesSurface(t *testing.T) {
	home := t.TempDir()
	writeFailurePattern(t, home, "fp_dual", map[string]any{
		"subject":       "dirty-source-control-hold",
		"statement":     "agent repeatedly tries mutating a dirty repo without cleanup review",
		"detail":        "repeated-policy-hold",
		"confidence":    0.9,
		"evidence_refs": []string{"trace:dirty-hold"},
		"verdict":       candidates.Park,
	})
	writeRouteRemorse(t, home, "ror_dual", map[string]any{
		"selected_target":    map[string]any{"provider": "policyhold", "model": "route-x", "protocol": "http"},
		"capability_classes": []string{"repeated-policy-hold"},
		"assessment": map[string]any{
			"class":                "route_remorse",
			"should_review_policy": true,
			"rationale":            "route repeatedly held for the same dirty-hold policy reason",
		},
	})
	proj, err := Build(home, BuildOptions{Now: t0})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	fp, fpOK := findRecord(proj.Records, "fp_dual")
	ror, rorOK := findRecord(proj.Records, "ror_dual")
	if !fpOK || !rorOK {
		t.Fatalf("expected both producers in projection, got %+v", proj.Records)
	}
	if fp.Source != SourceFailurePattern {
		t.Fatalf("fp_dual source = %q, want %q", fp.Source, SourceFailurePattern)
	}
	if ror.Source != SourceRouteRemorse {
		t.Fatalf("ror_dual source = %q, want %q", ror.Source, SourceRouteRemorse)
	}

	task := ScopeTokens("hold policy again for repeated dirty state")
	owner := ScopeTokens("")
	got := MatchingRecords(proj.Records, owner, task, 5)
	if len(got) != 2 {
		t.Fatalf("expected both producers to surface through the same section, got %+v", got)
	}
}

// Falsifier (d): rebuild is lossless and fresh. Deleting the projection and rebuilding
// yields the same negatives; a resolved/reversed failure (an approach that later
// succeeded) drops out on rebuild — never a stale tombstone.
func TestBuild_RebuildDropsResolvedNeverStaleTombstone(t *testing.T) {
	home := t.TempDir()
	writeFailurePattern(t, home, "fp_resolve", map[string]any{
		"subject":       "flaky-retry-loop",
		"statement":     "agent retried the same flaky call without backoff",
		"detail":        "flaky-retry-loop",
		"confidence":    0.7,
		"evidence_refs": []string{"trace:flaky-1"},
		"verdict":       candidates.Park,
	})
	writeRouteRemorse(t, home, "ror_resolve", map[string]any{
		"selected_target": map[string]any{"provider": "flakyroute", "model": "route-y", "protocol": "http"},
		"assessment": map[string]any{
			"class":                "route_remorse",
			"should_review_policy": true,
			"rationale":            "route produced a flaky retry loop",
		},
	})

	proj1, err := Build(home, BuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findRecord(proj1.Records, "fp_resolve"); !ok {
		t.Fatalf("expected fp_resolve present before resolution")
	}
	if _, ok := findRecord(proj1.Records, "ror_resolve"); !ok {
		t.Fatalf("expected ror_resolve present before resolution")
	}

	// Rebuild with unchanged inputs must be lossless (deterministic, byte-identical
	// records modulo ComputedAt).
	proj1b, err := Build(home, BuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(proj1.Records, proj1b.Records) {
		t.Fatalf("rebuild with unchanged inputs must be lossless: %+v vs %+v", proj1.Records, proj1b.Records)
	}

	// The failure-pattern candidate is later rejected (dismissed as not a real
	// failure) and the route-remorse review is later resolved (no policy concern) —
	// both must drop out of the REBUILT projection.
	writeFailurePattern(t, home, "fp_resolve", map[string]any{
		"subject":       "flaky-retry-loop",
		"statement":     "agent retried the same flaky call without backoff",
		"detail":        "flaky-retry-loop",
		"confidence":    0.7,
		"evidence_refs": []string{"trace:flaky-1"},
		"verdict":       candidates.Reject,
	})
	writeRouteRemorse(t, home, "ror_resolve", map[string]any{
		"selected_target": map[string]any{"provider": "flakyroute", "model": "route-y", "protocol": "http"},
		"assessment": map[string]any{
			"class":                "route_remorse",
			"should_review_policy": false,
			"rationale":            "route no longer produces the flaky retry loop",
		},
	})

	proj2, err := Build(home, BuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findRecord(proj2.Records, "fp_resolve"); ok {
		t.Fatalf("rejected failure pattern must drop out on rebuild, never a stale tombstone: %+v", proj2.Records)
	}
	if _, ok := findRecord(proj2.Records, "ror_resolve"); ok {
		t.Fatalf("resolved route-remorse review must drop out on rebuild, never a stale tombstone: %+v", proj2.Records)
	}
}

// Falsifier (f): privacy. A negative carrying operator-personal content in free text
// is reduced to structured form (approach-key + ref) in the card; planted personal
// text never appears anywhere in the projection.
func TestBuild_PrivacyReducesPersonalFreeTextToStructuredForm(t *testing.T) {
	home := t.TempDir()
	const personalMarker = "person@example.invalid"
	writeFailurePattern(t, home, "fp_personal", map[string]any{
		"subject":       "leaked-personal-detail",
		"statement":     "operator contact " + personalMarker + " was pasted into the trace by mistake",
		"detail":        "/Users/testoperator/private-notes leaked into the same trace",
		"confidence":    0.6,
		"evidence_refs": []string{"trace:leak-1"},
		"verdict":       candidates.Park,
	})
	proj, err := Build(home, BuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := findRecord(proj.Records, "fp_personal")
	if !ok {
		t.Fatalf("expected fp_personal in projection")
	}
	if rec.WhatWasTried != "" || rec.WhyItFailed != "" {
		t.Fatalf("expected free text reduced to structured form (empty), got what=%q why=%q", rec.WhatWasTried, rec.WhyItFailed)
	}
	if rec.ApproachKey == "" || rec.Ref == "" {
		t.Fatalf("expected approach_key and ref to remain on a privacy-reduced record: %+v", rec)
	}
	raw, err := json.Marshal(proj)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), personalMarker) || strings.Contains(string(raw), "/Users/testoperator") {
		t.Fatalf("planted personal text must never appear in the projection JSON: %s", raw)
	}
}

// TestBuild_PrivacyReducesMachineScratchPaths is the regression for
// find_7c177ad1dd32: a negative card is provider-bound (it renders into the harness
// digest, so its content is sent to a model provider), and the privacy gate only
// knew about home directories. macOS resolves temp paths under /private, outside the
// home tree, so every scratch path walked straight through.
//
// Observed live, not hypothesized: a card shipped a
// `tried=method:/private/tmp/<run-scratch-dir>/<file>` value into a real prompt
// injection (find_7c177ad1dd32).
//
// Each case is a root a scratch path actually comes from on a platform this repo
// runs on; a table so a newly-added root cannot be waved through as "similar to one
// already covered". Paths are synthetic per the public/private boundary — the shape
// is what matters, and a real observed path belongs to the private overlay.
func TestBuild_PrivacyReducesMachineScratchPaths(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leaked   string
		fragment string
	}{
		{"macos private tmp", "method:/private/tmp/compound-read-replay.Ab12Cd/clone-failure.txt", "/private/tmp/compound-read-replay.Ab12Cd"},
		{"macos var folders", "cache under /private/var/folders/9x/T/compound-run-4821 was reused", "/private/var/folders/9x/T/compound-run-4821"},
		{"bare var folders", "cache under /var/folders/9x/T/compound-run-4821 was reused", "/var/folders/9x/T/compound-run-4821"},
		{"linux tmp", "wrote the replay to /tmp/compound-replay-7781/out.json first", "/tmp/compound-replay-7781"},
		{"var tmp", "staged under /var/tmp/compound-stage-3312 before the run", "/var/tmp/compound-stage-3312"},
		{"executor worktree", "ran inside /worktrees/example-lane-20260101 instead of the checkout", "example-lane-20260101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeFailurePattern(t, home, "fp_scratch", map[string]any{
				"subject":       "scratch-path-leak",
				"statement":     tc.leaked,
				"detail":        "external_runtime_evidence",
				"confidence":    1.0,
				"evidence_refs": []string{"trace:scratch-1"},
				"verdict":       candidates.Park,
			})
			proj, err := Build(home, BuildOptions{Now: t0})
			if err != nil {
				t.Fatal(err)
			}
			rec, ok := findRecord(proj.Records, "fp_scratch")
			if !ok {
				t.Fatalf("expected fp_scratch in projection")
			}
			// The dirty field is the statement, so that is the field that blanks.
			if rec.WhatWasTried != "" {
				t.Fatalf("statement carrying a scratch path must blank, got what=%q", rec.WhatWasTried)
			}
			// Redaction is per field: the clean detail survives, because one dirty
			// field does not make the rest of the record unsafe.
			if rec.WhyItFailed != "external_runtime_evidence" {
				t.Fatalf("clean detail must survive redaction of a dirty statement, got why=%q", rec.WhyItFailed)
			}
			// The regression that per-field redaction exists to prevent: ApproachKey and
			// TaskClass are the only inputs to inScope, so blanking them alongside the
			// dirty field turns redaction into silent deletion — the card stays in the
			// projection and can never match a task again.
			if rec.ApproachKey != "scratch-path-leak" {
				t.Fatalf("clean subject must still seed a matchable approach key, got %q (an opaque key can never match a task again)", rec.ApproachKey)
			}
			if rec.TaskClass != "external_runtime_evidence" {
				t.Fatalf("clean detail must still seed the task class, got %q", rec.TaskClass)
			}
			if rec.Ref == "" {
				t.Fatalf("expected ref to remain on a privacy-reduced record: %+v", rec)
			}
			raw, err := json.Marshal(proj)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), tc.fragment) {
				t.Fatalf("scratch path %q reached the projection JSON, which the digest renders to a provider: %s", tc.fragment, raw)
			}
		})
	}
}

// Sanity: WriteProjection/ReadProjection round-trip, mirroring the mem/temperature.go
// projection discipline (rebuilt from scratch, written only under projections/,
// absent-projection reads fail open).
func TestWriteReadProjectionRoundTrip(t *testing.T) {
	home := t.TempDir()
	if _, ok, err := ReadProjection(home); err != nil || ok {
		t.Fatalf("expected no projection yet, got ok=%v err=%v", ok, err)
	}
	writeFailurePattern(t, home, "fp_roundtrip", map[string]any{
		"subject":       "roundtrip-check",
		"statement":     "roundtrip statement",
		"detail":        "roundtrip-detail",
		"confidence":    0.5,
		"evidence_refs": []string{},
		"verdict":       candidates.Park,
	})
	built, err := Build(home, BuildOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteProjection(home, built)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected projection written to %s: %v", path, err)
	}
	loaded, ok, err := ReadProjection(home)
	if err != nil || !ok {
		t.Fatalf("expected projection readable, ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(built.Records, loaded.Records) {
		t.Fatalf("round-tripped records differ: %+v vs %+v", built.Records, loaded.Records)
	}
}

// TestReadFresh_StalePersistedProjectionRebuilds is the falsifier for the failure
// this constant exists to prevent: nothing in-repo schedules a rebuild, so a
// persisted projection can sit for weeks holding an empty view while real failure
// patterns accumulate. A reader must not trust it past StaleAfter.
func TestReadFresh_StalePersistedProjectionRebuilds(t *testing.T) {
	home := t.TempDir()
	// A persisted projection that is EMPTY and old — exactly the observed state.
	stalePath, err := WriteProjection(home, Projection{
		SchemaVersion: SchemaVersion,
		ProjectionRef: ProjectionRef,
		ComputedAt:    t0.Add(-15 * 24 * time.Hour).UTC().Format(time.RFC3339),
		Records:       []Record{},
	})
	if err != nil {
		t.Fatalf("WriteProjection: %v", err)
	}
	before, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	// Real evidence that landed in the inbox AFTER that build.
	writeFailurePattern(t, home, "fp_stale", map[string]any{
		"subject":       "single-signal-verdict",
		"statement":     "agent sized a branch backlog from one signal that is blind to reimplementation",
		"detail":        "single-signal-verdict",
		"confidence":    0.95,
		"evidence_refs": []string{"trace:stale-1"},
		"verdict":       candidates.Park,
	})

	proj, ok, err := ReadFresh(home, t0)
	if err != nil {
		t.Fatalf("ReadFresh: %v", err)
	}
	if !ok {
		t.Fatal("ReadFresh returned not-ok for a rebuildable home")
	}
	if _, found := findRecord(proj.Records, "fp_stale"); !found {
		t.Fatalf("stale projection was trusted; inbox evidence never surfaced: %+v", proj.Records)
	}
	// Orient is a read path: ReadFresh must never persist.
	after, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("ReadFresh persisted a rebuild; the read path must not write")
	}
}

// TestReadFresh_FreshPersistedProjectionIsTrusted pins the other half: inside the
// window the persisted body is used as-is, so the common case stays one read.
func TestReadFresh_FreshPersistedProjectionIsTrusted(t *testing.T) {
	home := t.TempDir()
	if _, err := WriteProjection(home, Projection{
		SchemaVersion: SchemaVersion,
		ProjectionRef: ProjectionRef,
		ComputedAt:    t0.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		Records:       []Record{{Source: SourceFailurePattern, RefID: "fp_persisted", ApproachKey: "persisted-only", Ref: "candidate://failure_pattern/fp_persisted"}},
	}); err != nil {
		t.Fatalf("WriteProjection: %v", err)
	}
	// Inbox evidence that a rebuild WOULD surface — proving we did not rebuild.
	writeFailurePattern(t, home, "fp_unbuilt", map[string]any{
		"subject":       "not-yet-built",
		"statement":     "an approach that has not been folded into the projection yet",
		"detail":        "not-yet-built",
		"confidence":    0.8,
		"evidence_refs": []string{"trace:unbuilt"},
		"verdict":       candidates.Park,
	})

	proj, ok, err := ReadFresh(home, t0)
	if err != nil || !ok {
		t.Fatalf("ReadFresh: %v ok=%t", err, ok)
	}
	if _, found := findRecord(proj.Records, "fp_persisted"); !found {
		t.Fatalf("fresh persisted projection was not used: %+v", proj.Records)
	}
	if _, found := findRecord(proj.Records, "fp_unbuilt"); found {
		t.Fatal("ReadFresh rebuilt inside the freshness window")
	}
}

// TestReadFresh_UndatedProjectionCountsAsStale — a body that cannot prove its own
// age must never be trusted as fresh.
func TestReadFresh_UndatedProjectionCountsAsStale(t *testing.T) {
	home := t.TempDir()
	if _, err := WriteProjection(home, Projection{
		SchemaVersion: SchemaVersion,
		ProjectionRef: ProjectionRef,
		ComputedAt:    "",
		Records:       []Record{},
	}); err != nil {
		t.Fatalf("WriteProjection: %v", err)
	}
	writeFailurePattern(t, home, "fp_undated", map[string]any{
		"subject":       "undated-rebuild",
		"statement":     "an approach that must surface when the projection cannot date itself",
		"detail":        "undated-rebuild",
		"confidence":    0.9,
		"evidence_refs": []string{"trace:undated"},
		"verdict":       candidates.Park,
	})

	proj, ok, err := ReadFresh(home, t0)
	if err != nil || !ok {
		t.Fatalf("ReadFresh: %v ok=%t", err, ok)
	}
	if _, found := findRecord(proj.Records, "fp_undated"); !found {
		t.Fatalf("undated projection was trusted as fresh: %+v", proj.Records)
	}
}

// TestReadFresh_MissingProjectionBuildsInMemory — a home that never ran `negatives
// build` still gets its evidence, without the read path creating the file.
func TestReadFresh_MissingProjectionBuildsInMemory(t *testing.T) {
	home := t.TempDir()
	writeFailurePattern(t, home, "fp_nofile", map[string]any{
		"subject":       "no-projection-yet",
		"statement":     "an approach recorded before any projection build ran",
		"detail":        "no-projection-yet",
		"confidence":    0.9,
		"evidence_refs": []string{"trace:nofile"},
		"verdict":       candidates.Park,
	})

	proj, ok, err := ReadFresh(home, t0)
	if err != nil || !ok {
		t.Fatalf("ReadFresh: %v ok=%t", err, ok)
	}
	if _, found := findRecord(proj.Records, "fp_nofile"); !found {
		t.Fatalf("missing projection did not build in memory: %+v", proj.Records)
	}
	if _, statErr := os.Stat(filepath.Join(home, filepath.FromSlash(RelPath))); statErr == nil {
		t.Fatal("ReadFresh created the projection file; the read path must not write")
	}
}
