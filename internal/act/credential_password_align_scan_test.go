package act

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/policy"
)

func TestCredentialPasswordAlignmentStartupDoesNotRepeatColdFenceScan(t *testing.T) {
	f := newAlignmentFixture(t)
	f.inspect(t)
	p, err := f.owner.Plan(f.r, f.workID, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	f.approve(t, p)
	store, err := eventstore.Open(f.owner.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Migrate(eventstore.MigrateOptions{Mode: eventstore.ModeApply}); err != nil {
		t.Fatal(err)
	}

	// A separate owner mirrors the two CLI processes. A cold history read costs
	// 45s of the real owner's unchanged 80s work budget; incremental reads are fast.
	owner, err := NewCredentialPasswordAligner(f.owner.home)
	if err != nil {
		t.Fatal(err)
	}
	now := f.now
	owner.now = func() time.Time { return now }
	owner.transport = f.transport
	cold, scans := 0, 0
	owner.fenceScan = func(opts eventstore.ScanOptions) (eventstore.ScanPage, error) {
		scans++
		if opts.Cursor == "" {
			cold++
			now = now.Add(45 * time.Second)
		}
		return store.ScanFrom(opts)
	}
	result, err := owner.Execute(context.Background(), p.PlanRef, "fixture")
	if err != nil || result.State != "completed" || f.transport.starts != 2 {
		t.Fatalf("startup repeated a cold scan before native launch: result=%+v err=%v cold=%d scans=%d starts=%d", result, err, cold, scans, f.transport.starts)
	}
	if cold != 1 || scans < 2 {
		t.Fatalf("cold=%d scans=%d; fresh admission checks must read deltas after one cold scan", cold, scans)
	}
}

func TestCredentialPasswordAlignmentIncrementalDiscoveryRechecksKnownAndNewFences(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		name := "legacy"
		if migrated {
			name = "migrated"
		}
		t.Run(name, func(t *testing.T) {
			f := newAlignmentFixture(t)
			f.inspect(t)
			p, err := f.owner.Plan(f.r, f.workID, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			f.approve(t, p)
			f.transport.stage = func() {
				if err := policy.WriteAutonomyHalt(f.owner.home, policy.AutonomyHalt{Global: true, Reason: "synthetic pre-start refusal"}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
			if err == nil || result.ReceiptID == "" || f.transport.starts != 1 {
				t.Fatalf("expected genuine pre-start refusal: %+v %v", result, err)
			}
			if err := policy.WriteAutonomyHalt(f.owner.home, policy.AutonomyHalt{}); err != nil {
				t.Fatal(err)
			}
			store, err := eventstore.Open(f.owner.home)
			if err != nil {
				t.Fatal(err)
			}
			if migrated {
				if _, err := store.Migrate(eventstore.MigrateOptions{Mode: eventstore.ModeApply}); err != nil {
					t.Fatal(err)
				}
			}
			owner, err := NewCredentialPasswordAligner(f.owner.home)
			if err != nil {
				t.Fatal(err)
			}
			check := func() error { return owner.priorAlignmentFence(f.r, "different-action") }
			if err := check(); err != nil {
				t.Fatal(err)
			}
			// Deliberately bypass the writer in a disposable fixture: malformed attempts
			// cannot be created through its normal API. A cached discovery must reread it.
			path := filepath.Join(f.owner.home, "run_attempts", result.RunAttemptID+".json")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			// Losing an ID from discovery must not hide a damaged known attempt.
			owner.fenceScan = func(eventstore.ScanOptions) (eventstore.ScanPage, error) {
				return eventstore.ScanPage{}, nil
			}
			if err := check(); err == nil {
				t.Fatal("known attempt was omitted after discovery inventory shrank")
			}
			owner.fenceScan = nil
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			owner.fenceCursor = "stale-cache-token"
			if err := check(); err != nil {
				t.Fatal("stale cursor must safely rescan:", err)
			}
			// A newly appended malformed fence must also be found after a warm scan.
			const id = "cpaf_0123456789ab"
			if err := recordEnvelope(f.owner.home, id, map[string]string{"schema_version": credentialPasswordAlignmentFenceSchema, "id": id, "created_at": time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
				t.Fatal(err)
			}
			if err := check(); err == nil {
				t.Fatal("newly appended fence was omitted after cached discovery")
			}
		})
	}
}

func TestRuntimeConfigInstallerSharesNativeFenceDiscovery(t *testing.T) {
	owner, err := NewRuntimeConfigInstaller(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := owner.transport.(runtimeConfigInstallNativeTransport)
	// Wiring is load-bearing: a separate reader in Start repeats the cold history
	// scan even when the installer's earlier checks use an incremental reader.
	if !ok || owner.alignment == nil || transport.alignment != owner.alignment {
		t.Fatal("native transport does not share the installer's fence discovery")
	}
}
