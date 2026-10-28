package product

import (
	"os"
	"testing"
	"time"

	"compound/internal/eventstore"
)

// Falsifier: once a home's event migration is complete, the event store reports
// a genuinely absent record as a %w-wrapped os.ErrNotExist (no *PathError).
// A first-time dry-run must treat that as "no prior intent", not a hard error.
func TestExecuteContentPostFirstDryRunSurvivesMigratedHomeAbsentRecord(t *testing.T) {
	h, d, now, marker := b2Fixture(t)
	events, err := eventstore.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Migrate(eventstore.MigrateOptions{Mode: eventstore.ModeApply}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !events.MigrationComplete() {
		t.Fatal("fixture home must have a completed event migration")
	}
	at := now.Add(30 * time.Minute)
	run, err := ExecuteContentPost(h, d.ID, "fixture", "", at, false, false, now)
	if err != nil {
		t.Fatalf("first dry-run on a migrated home must not fail on an absent record: %v", err)
	}
	if run.Mode != "dry-run" || run.PolicyOutcome != "allow_with_obligations" {
		t.Fatalf("dry-run = mode %q outcome %q, want dry-run allow_with_obligations", run.Mode, run.PolicyOutcome)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("dry-run must not invoke the transport")
	}
}
