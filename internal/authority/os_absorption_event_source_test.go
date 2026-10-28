package authority

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/eventsource"
	"compound/internal/eventstore"
)

// TestOSAbsorptionEventSourceAuthorityProof exercises the
// "event_source_authority" proof mode against a synthetic example-os events/
// directory: two live .ndjson streams plus a README.md that must never be
// treated as a data file. The base fixture already carries a real
// eventsource.LoadSourceAuthority record per file (via a real
// eventsource.ImportOSEvents call) and real TraceEnvelope files under
// COMPOUND_HOME/events/, so the ok case proves the row machine-proves from
// existing evidence with zero new imports, exactly like the real
// os_source_example_events row does against evsrc_0ccb6af33dab /
// evsrc_16152da98c8b.
func TestOSAbsorptionEventSourceAuthorityProof(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(t *testing.T, fixture eventSourceFixture)
		wantStatus string
		wantDetail string
	}{
		{
			name:       "existing import evidence machine-proves ok with zero new imports",
			wantStatus: "ok",
		},
		{
			name: "editing README.md never affects the row (excluded from the ndjson scan)",
			mutate: func(t *testing.T, fixture eventSourceFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.sourceDir, "README.md"), []byte("# events\n\nchurned notes, not a data file.\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: "ok",
		},
		{
			name: "lines appended to a tracked file without re-import prove pending drift",
			mutate: func(t *testing.T, fixture eventSourceFixture) {
				t.Helper()
				appendEventSourceFixtureLine(t, fixture.controlEventsPath, "extra-after-import")
			},
			wantStatus: "pending",
			wantDetail: "appended since import",
		},
		{
			name: "an untracked .ndjson file with no import record proves pending, not failed",
			mutate: func(t *testing.T, fixture eventSourceFixture) {
				t.Helper()
				writeEventSourceFixtureFile(t, filepath.Join(fixture.sourceDir, "never-imported.ndjson"), 1)
			},
			wantStatus: "pending",
			wantDetail: "no eventsource.LoadSourceAuthority record",
		},
		{
			name: "a spot-checked trace id that no longer resolves fails the row closed",
			mutate: func(t *testing.T, fixture eventSourceFixture) {
				t.Helper()
				deleteEventSourceSpotCheckTrace(t, fixture.compoundHome, fixture.controlEventsPath)
			},
			wantStatus: "failed",
			wantDetail: "does not resolve under",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := installEventSourceFixture(t)
			if tt.mutate != nil {
				tt.mutate(t, fixture)
			}

			doctor, err := OSAbsorptionDoctor(fixture.compoundHome)
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			got, ok := findOSAbsorptionDoctorRow(doctor.Rows, "os_source_example_events")
			if !ok {
				t.Fatalf("doctor row os_source_example_events missing: %+v", doctor.Rows)
			}
			if got.ProofStatus != tt.wantStatus {
				t.Fatalf("proof_status=%q, want %q (detail=%q)", got.ProofStatus, tt.wantStatus, got.ProofDetail)
			}
			switch tt.wantStatus {
			case "ok":
				if !strings.Contains(got.ProofRef, "evsrc_") {
					t.Fatalf("proof_ref %q missing evsrc_ id", got.ProofRef)
				}
			default:
				if !strings.Contains(got.ProofDetail, tt.wantDetail) {
					t.Fatalf("proof_detail %q missing %q", got.ProofDetail, tt.wantDetail)
				}
			}
		})
	}
}

// TestOSAbsorptionEventSourceAuthorityProofRefNamesBothFiles is J's positive
// proof for spec point 5 (proofRef = matching SourceAuthorityRecord id(s)):
// with two live streams under source_ref, the ok proof_ref must cite both
// files' matching record ids, not just one.
func TestOSAbsorptionEventSourceAuthorityProofRefNamesBothFiles(t *testing.T) {
	fixture := installEventSourceFixture(t)
	doctor, err := OSAbsorptionDoctor(fixture.compoundHome)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	got, ok := findOSAbsorptionDoctorRow(doctor.Rows, "os_source_example_events")
	if !ok {
		t.Fatalf("doctor row os_source_example_events missing: %+v", doctor.Rows)
	}
	if got.ProofStatus != "ok" {
		t.Fatalf("proof_status = %q, want ok (detail=%q)", got.ProofStatus, got.ProofDetail)
	}
	ids := strings.Split(got.ProofRef, ",")
	if len(ids) != 2 {
		t.Fatalf("proof_ref = %q, want exactly 2 comma-joined record ids (one per file)", got.ProofRef)
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "evsrc_") {
			t.Fatalf("proof_ref entry %q is not an evsrc_ id: %q", id, got.ProofRef)
		}
	}
}

type eventSourceFixture struct {
	compoundHome      string
	sourceDir         string
	agentRunsPath     string
	controlEventsPath string
}

func installEventSourceFixture(t *testing.T) eventSourceFixture {
	t.Helper()
	root := t.TempDir()
	compoundHome := filepath.Join(root, "compound-home")
	sourceDir := filepath.Join(root, "events")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}

	agentRunsPath := filepath.Join(sourceDir, "agent-runs.ndjson")
	controlEventsPath := filepath.Join(sourceDir, "control-events.ndjson")
	writeEventSourceFixtureFile(t, agentRunsPath, 2)
	writeEventSourceFixtureFile(t, controlEventsPath, 3)
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("# events\n\nnot an ndjson stream.\n"), 0o644); err != nil {
		t.Fatalf("write README fixture: %v", err)
	}

	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	if _, err := eventsource.ImportOSEvents(compoundHome, agentRunsPath, eventsource.ImportOptions{Stream: "os-agent-runs"}, now); err != nil {
		t.Fatalf("import agent-runs fixture: %v", err)
	}
	if _, err := eventsource.ImportOSEvents(compoundHome, controlEventsPath, eventsource.ImportOptions{Stream: "os-control-events"}, now.Add(time.Minute)); err != nil {
		t.Fatalf("import control-events fixture: %v", err)
	}

	matrixPath := filepath.Join(root, "matrix.json")
	r := row("os_source_example_events", sourceDir, "absorbed", eventsource.TraceEventsAuthorityRef, "compound event import / trace and transition projections", osAbsorptionProofModeEventSource)
	r.SourceKind = "events_dir"
	writeJSONFixture(t, matrixPath, OSAbsorptionSourceMatrix{
		SchemaVersion: OSAbsorptionMatrixSchema,
		Rows:          []OSAbsorptionMatrixRow{r},
	})
	if _, err := ImportOSAbsorptionMatrix(compoundHome, matrixPath, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("import matrix: %v", err)
	}

	return eventSourceFixture{
		compoundHome:      compoundHome,
		sourceDir:         sourceDir,
		agentRunsPath:     agentRunsPath,
		controlEventsPath: controlEventsPath,
	}
}

// writeEventSourceFixtureFile writes n well-formed osEvent lines plus one
// deliberate blank line, so the ok case also proves blank lines are excluded
// from the non-blank line count the same way eventsource.ImportOSEvents
// excludes them from Imported/Skipped.
func writeEventSourceFixtureFile(t *testing.T, path string, n int) {
	t.Helper()
	base := filepath.Base(path)
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"schema_version":1,"id":"evt_%s_%d","ts":"2026-07-12T00:%02d:00Z","kind":"packet.generated","event_class":"planning","metadata_quality":"complete","mutation_class":"read","producer":{"caller":"planner"},"subject":{"path":"/workspace/out-%s-%d.md"},"summary":"Generated fixture line %d for %s."}`,
			strings.TrimSuffix(base, ".ndjson"), i, i, strings.TrimSuffix(base, ".ndjson"), i, i, base,
		))
	}
	body := strings.Join(lines, "\n") + "\n\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write event source fixture %s: %v", path, err)
	}
}

func appendEventSourceFixtureLine(t *testing.T, path, suffix string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s for append: %v", path, err)
	}
	defer f.Close()
	line := fmt.Sprintf(
		`{"schema_version":1,"id":"evt_%s","ts":"2026-07-12T01:00:00Z","kind":"packet.generated","event_class":"planning","metadata_quality":"complete","mutation_class":"read","producer":{"caller":"planner"},"subject":{"path":"/workspace/extra.md"},"summary":"Extra line appended after import."}`,
		suffix,
	)
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append line to %s: %v", path, err)
	}
}

func deleteEventSourceSpotCheckTrace(t *testing.T, compoundHome, sourcePath string) {
	t.Helper()
	records, err := eventsource.LoadSourceAuthority(compoundHome)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	for _, rec := range records {
		if rec.SourcePath != sourcePath || len(rec.TraceIDs) == 0 {
			continue
		}
		store, err := eventstore.Open(compoundHome)
		if err != nil {
			t.Fatalf("open event store: %v", err)
		}
		traceID := rec.TraceIDs[len(rec.TraceIDs)-1]
		if err := store.Remove(traceID); err != nil {
			t.Fatalf("remove trace fixture %s: %v", traceID, err)
		}
		return
	}
	t.Fatalf("no trace-bearing eventsource record found for %s to delete", sourcePath)
}
