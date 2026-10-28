package contract

import (
	"testing"
	"time"

	"compound/internal/harness"
)

// Exercise the real publication/render/read-only comparison seam. A global
// last-writer projection is not authority for another installed consumer.
func TestInstructionPackUnaffectedByUnrelatedMaterialization(t *testing.T) {
	for _, variant := range []string{"other-harness", "other-project", "other-task", "other-topic"} {
		t.Run(variant, func(t *testing.T) {
			homePath := t.TempDir()
			now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
			own := Input{
				Harness: "codex", Cwd: t.TempDir(), Task: "review synthetic project",
				RepoKind: "product", RepoKindVia: "flag", Mode: "review",
			}
			record := func(in Input, at time.Time) string {
				t.Helper()
				runtime, err := MaterializeRuntime(in, DefaultPolicy(), homePath, at)
				if err != nil {
					t.Fatal(err)
				}
				sealed, err := SealRuntimeMaterialization(runtime)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := RecordRuntimeMaterialization(homePath, sealed); err != nil {
					t.Fatal(err)
				}
				return sealed.ContextPack.ID
			}
			contextID := record(own, now)
			if _, err := RenderInstructionPackForContext(homePath, own.Harness, "", contextID, now); err != nil {
				t.Fatal(err)
			}
			before, err := harness.InstructionPackDrift(homePath, own.Harness, "", now)
			if err != nil || before.Drifted {
				t.Fatalf("initial drift: %+v, err=%v", before, err)
			}
			unrelated := own
			switch variant {
			case "other-topic":
				unrelated.Topic = "synthetic-other-topic"
			case "other-harness":
				unrelated.Harness = "hermes"
			case "other-project":
				unrelated.Cwd = t.TempDir()
			case "other-task":
				unrelated.Task = "review a different synthetic task"
			}
			record(unrelated, now.Add(time.Second))
			after, err := harness.InstructionPackDrift(homePath, own.Harness, "", now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if after.Drifted {
				t.Fatalf("unrelated materialization invalidated installed consumer: %v", after.Details)
			}
		})
	}
}
