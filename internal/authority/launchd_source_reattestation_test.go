package authority

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Upgrade a source only after the predecessor has completed. Its immutable
// source proof remains historical; the successor explicitly supplies the new one.
func changedLaunchdHistoricalSource(t *testing.T, f *adversarialLiveFixture, index int, symlink bool) LaunchdSourceProof {
	t.Helper()
	path := f.f.proofs[index].Path
	if symlink {
		target := path + ".current"
		if err := os.WriteFile(target, []byte("current synthetic source\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, path+".historical"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(path, []byte("current synthetic source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	proof, err := inspectLaunchdSource(LaunchdSourceProof{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	f.f.proofs[index] = proof
	return proof
}

func TestLaunchdTerminalHandoffReattestsHistoricalSourceDrift(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "content", true: "symlink"}[symlink], func(t *testing.T) {
			f := completedLaunchdPredecessor(t)
			old := f.plan
			history := snapshotLaunchdHistory(t, f, old)
			current := changedLaunchdHistoricalSource(t, f, 1, symlink)
			p, err := launchdSuccessorPlan(t, f, old, true)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(p.Handoff)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			var changes []struct {
				Historical LaunchdSourceProof `json:"historical"`
				Current    LaunchdSourceProof `json:"current"`
			}
			if err = json.Unmarshal(fields["source_reattestations"], &changes); err != nil {
				t.Fatal(err)
			}
			if len(changes) != 1 || changes[0].Historical != old.SourceProofs[1] || changes[0].Current != current {
				t.Fatalf("missing exact source attestation: %s", raw)
			}
			// Removing only the attestation must invalidate the digest; an earlier
			// authorization cannot authorize the new source transition.
			delete(fields, "source_reattestations")
			stripped, _ := json.Marshal(fields)
			q := p
			// Unmarshal into a fresh value: absent fields must not retain old values.
			q.Handoff = &LaunchdTerminalHandoff{}
			if err = json.Unmarshal(stripped, q.Handoff); err != nil {
				t.Fatal(err)
			}
			if err = validateLaunchdPlan(q); err == nil {
				t.Fatal("omitted attestation preserved authorization")
			}
			recordLaunchdSuccessor(t, f, p)
			if _, err = f.apply(); err != nil {
				t.Fatal(err)
			}
			f.review = f.sealReview(t, true)
			if _, err = RollbackLaunchdRepair(context.Background(), f.home, p.ID, f.review.ID, p.Actor, f.f.env); err != nil {
				t.Fatal(err)
			}
			owner, err := readLaunchdOwner(f.home, p)
			if err != nil || !bytesEqual(owner, launchdOwnerBytes(old.ID)) {
				t.Fatalf("rollback lost historical owner: %s %v", owner, err)
			}
			for name, before := range history {
				path, _ := launchdArtifactPath(f.home, name)
				after, err := os.ReadFile(path)
				if err != nil || string(after) != before {
					t.Fatalf("rewrote predecessor evidence %s: %v", name, err)
				}
			}
		})
	}
}

func TestLaunchdTerminalHandoffHistoricalSourceDriftRequiresCurrentProof(t *testing.T) {
	f := completedLaunchdPredecessor(t)
	old := f.plan
	p, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	current := changedLaunchdHistoricalSource(t, f, 1, false)
	// A missing, stale, aliased or duplicate proof cannot attest historical drift.
	cases := map[string][]LaunchdSourceProof{
		"missing":           p.SourceProofs[:1],
		"stale":             p.SourceProofs,
		"wrong-path":        append(append([]LaunchdSourceProof{}, p.SourceProofs[:1]...), LaunchdSourceProof{Path: current.Path + ".other", ResolvedPath: current.ResolvedPath, SHA256: current.SHA256}),
		"noncanonical-path": append(append([]LaunchdSourceProof{}, p.SourceProofs[:1]...), LaunchdSourceProof{Path: filepath.Dir(current.Path) + "/./" + filepath.Base(current.Path), ResolvedPath: current.ResolvedPath, SHA256: current.SHA256}),
		"duplicate":         append(append([]LaunchdSourceProof{}, f.f.proofs...), current),
	}
	for name, proofs := range cases {
		t.Run(name, func(t *testing.T) {
			q := p
			q.SourceProofs = proofs
			if _, err := captureLaunchdTerminalHandoff(f.home, q, old.ID, false); err == nil {
				t.Fatal("historical drift accepted without unambiguous exact current proof")
			}
		})
	}
	owner, _ := readLaunchdOwner(f.home, p)
	if !bytesEqual(owner, launchdOwnerBytes(old.ID)) || len(f.effects) != 2 {
		t.Fatal("rejected proofs changed runtime or owner")
	}
}

func TestLaunchdTerminalHandoffReattestationNeedsFreshConsumedReview(t *testing.T) {
	f := completedLaunchdPredecessor(t)
	old := f.plan
	changedLaunchdHistoricalSource(t, f, 1, true)
	p, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	f.plan = p
	for i := range f.cfg.StandingGrants {
		if f.cfg.StandingGrants[i].ID == "grant_example_launchd" {
			f.cfg.StandingGrants[i].Destination = launchdPlanDestination(p)
		}
	}
	f.writeConfig(t)
	if err = RecordLaunchdRepairPlan(f.home, p, f.now); err != nil {
		t.Fatal(err)
	}
	pending, err := ReviewLaunchdRepairPlan(f.home, p.ID, "grant_example_launchd", "operator:fixture", "fixture:source-reattestation", f.now.Add(time.Minute), f.now)
	if err != nil || !pending.Pending {
		t.Fatalf("drift review was not held for fresh approval: %+v %v", pending, err)
	}
	if _, err = ApplyLaunchdRepair(context.Background(), f.home, p.ID, pending.ActionIntentID, p.Actor, f.f.env); err == nil {
		t.Fatal("unconsumed review authorized source drift")
	}
	owner, _ := readLaunchdOwner(f.home, p)
	if !bytesEqual(owner, launchdOwnerBytes(old.ID)) || len(f.effects) != 2 {
		t.Fatal("unapproved source drift changed owner or runtime")
	}
}

func TestLaunchdTerminalHandoffReattestationRechecksBeforeAnyEffect(t *testing.T) {
	for _, kind := range []string{"source", "owner", "service"} {
		t.Run(kind, func(t *testing.T) {
			f := completedLaunchdPredecessor(t)
			old := f.plan
			changedLaunchdHistoricalSource(t, f, 1, false)
			p, err := launchdSuccessorPlan(t, f, old, true)
			if err != nil {
				t.Fatal(err)
			}
			recordLaunchdSuccessor(t, f, p)
			switch kind {
			case "source":
				if err = os.WriteFile(p.SourceProofs[1].Path, []byte("unreviewed later source\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "owner":
				ownerPath, pathErr := launchdArtifactPath(f.home, launchdActiveName(p))
				if pathErr != nil {
					t.Fatal(pathErr)
				}
				if err = os.WriteFile(ownerPath, launchdOwnerBytes("ldrp_other"), 0600); err != nil {
					t.Fatal(err)
				}
			case "service":
				f.loaded = true
				f.disabled = false
			}
			ownerBefore, _ := readLaunchdOwner(f.home, p)
			if _, err = f.apply(); err == nil {
				t.Fatal("post-review drift authorized effect")
			}
			ownerAfter, _ := readLaunchdOwner(f.home, p)
			if !bytesEqual(ownerBefore, ownerAfter) || len(f.effects) != 2 {
				t.Fatal("post-review drift changed owner or OS")
			}
		})
	}
}

func TestLaunchdTerminalHandoffUnchangedSourceSerializationCompatible(t *testing.T) {
	f := completedLaunchdPredecessor(t)
	old := f.plan
	p, err := launchdSuccessorPlan(t, f, old, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p.Handoff)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "source_reattestations") {
		t.Fatalf("unchanged handoff changed legacy serialization: %s", raw)
	}
	var decoded LaunchdTerminalHandoff
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, *p.Handoff) || validateLaunchdPlan(p) != nil {
		t.Fatal("unchanged handoff lost legacy digest compatibility")
	}
}
