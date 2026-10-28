package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/work"
)

// workRefusalHome builds a temp home carrying one work already claimed by holder, and
// returns the work id. Nothing here touches a real home.
func workRefusalHome(t *testing.T, holder, predicate string) (string, string) {
	t.Helper()
	homePath := initializedTestHome(t)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{Owner: holder, SuccessPredicate: predicate}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, holder, cliTestNow); err != nil {
		t.Fatal(err)
	}
	return homePath, w.ID
}

// runCompound drives the real binary. The work verbs terminate through os.Exit, so the
// refusal text can only be observed the way an operator observes it: from a process.
func runCompound(t *testing.T, homePath string, args ...string) (string, error) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(compoundTestBinary(t), args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// The lease gate is what makes `proposed != held` real, and it fires on the lane most likely
// to ignore it: one that already believes the work is its own. The two correct responses are
// opposite in cost (wait for the holder, or take it with a recorded reason), so a refusal
// naming only the holder makes the expensive one look like the only one.
//
// RED WITHOUT the conversion: `compound work claim` died through die(err) with
// `error: work work_x already claimed by "lane-alpha"` — no gate name, no live-lease line,
// and no command, so assertTeachingRefusal fails on its first assertion.
func TestWorkClaimLeaseConflictRefusalTeaches(t *testing.T) {
	t.Parallel()
	homePath, workID := workRefusalHome(t, "lane-alpha", "lease conflict must teach the contending lane")

	out, err := runCompound(t, homePath, "work", "claim", workID, "--by", "lane-beta")
	if err == nil {
		t.Fatalf("claiming work held by another actor must exit non-zero, got 0:\n%s", out)
	}
	assertTeachingRefusal(t, out, "work_lease_holder",
		"lane-alpha",
		"compound work show "+workID,
		"compound coordination",
		"compound work dispatch-claim --next --actor lane-beta",
	)

	// The work must still belong to the original holder: a refusal that moved the lease
	// would be a takeover wearing a refusal's text.
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := store.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range leases {
		if lease.WorkID == workID && lease.By != "lane-alpha" {
			t.Fatalf("the refused claim moved the lease to %q", lease.By)
		}
	}
}

// The git-delivery proof gate is the one refusal in this packet whose enforcer is not typed,
// so this test is doing double duty: it proves the refusal teaches, AND it is the falsifier
// that keeps the marker match in work_lease_refusal.go honest. Reword the gate inside
// internal/work and this goes red at the seam instead of silently degrading the refusal.
func TestWorkCloseGitDeliveryProofRefusalTeaches(t *testing.T) {
	t.Parallel()
	homePath, workID := workRefusalHome(t, "lane-alpha", "a commit: proof pulls in the git_delivery contract")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	// completed is reachable from running, so the close under test is a real close attempt
	// rather than an invalid transition that would never reach the proof gate.
	if _, err := store.Advance(workID, work.AdvanceInput{To: "running", Actor: "lane-alpha", Note: "proof gate fixture"}, cliTestNow); err != nil {
		t.Fatal(err)
	}

	// A commit: ref DECLARES Git delivery, which is exactly the move a lane makes to get
	// past a proof requirement it does not understand.
	out, runErr := runCompound(t, homePath, "work", "close", workID,
		"--by", "lane-alpha", "--outcome", "completed", "--move-type", "memory_delta",
		"--proof", "commit:0123456789012345678901234567890123456789")
	if runErr == nil {
		t.Fatalf("closing completed with a commit: proof and no git_delivery contract must exit non-zero, got 0:\n%s", out)
	}
	assertTeachingRefusal(t, out, "git_delivery_proof",
		"commit:0123456789012345678901234567890123456789",
		"compound work land-check "+workID,
	)
	// The bridge case is the whole reason the refusal lists shapes rather than one ref:
	// without it, the fastest way past this gate is to invent a commit: ref.
	for _, want := range []string{"live:", "evidence:", "never borrow a commit: ref"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal must distinguish the bridge-envelope proof shapes (%q missing):\n%s", want, out)
		}
	}

	// Teeth on the marker seam itself: the classifier must not fire on an unrelated close
	// error, or every fault would be rendered as this gate.
	if isGitDeliveryProofRefusal(errors.New("work store unavailable")) {
		t.Error("the git-delivery marker match fired on an unrelated error — it would mis-teach every close fault")
	}
	if isGitDeliveryProofRefusal(nil) {
		t.Error("the git-delivery marker match fired on a nil error")
	}
}

// workLeaseConflict decides the lease gate by re-reading the lease rather than reading the
// enforcer's error text. These are the cases that must NOT be reported as a conflict, because
// each has a different remedy than "someone else holds it".
func TestWorkLeaseConflictOnlyReportsARealHolder(t *testing.T) {
	t.Parallel()
	homePath, workID := workRefusalHome(t, "lane-alpha", "conflict detection must be structural")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}

	if holder, conflicted := workLeaseConflict(store, workID, "lane-beta"); !conflicted || holder != "lane-alpha" {
		t.Fatalf("a live lease held by another actor must conflict; got holder=%q conflicted=%v", holder, conflicted)
	}
	if _, conflicted := workLeaseConflict(store, workID, "lane-alpha"); conflicted {
		t.Error("the holder's own claim must not be reported as a conflict")
	}
	if _, conflicted := workLeaseConflict(store, "work_does_not_exist", "lane-beta"); conflicted {
		t.Error("an unleased work must not be reported as a conflict — its failure has a different remedy")
	}
	if _, conflicted := workLeaseConflict(nil, workID, "lane-beta"); conflicted {
		t.Error("with no store to read, the renderer must decline to diagnose rather than guess")
	}
}

// Both verbs share one gate but not one recovery ladder: a contending claimer should look for
// unheld work, while a closer usually needs the holder to close or an explicit steal. Pinning
// this stops the two from being collapsed into one generic message later.
func TestWorkLeaseRefusalLaddersDifferByVerb(t *testing.T) {
	t.Parallel()
	observed := errors.New("work work_x already claimed by \"lane-alpha\"")

	claim := workLeaseHolderRefusal("claim", "work_x", "lane-beta", "lane-alpha", observed).String()
	assertTeachingRefusal(t, claim, "work_lease_holder", "dispatch-claim --next")
	if strings.Contains(claim, "--steal") {
		t.Errorf("the claim ladder should route a contending lane to unheld work, not to a steal:\n%s", claim)
	}

	closed := workLeaseHolderRefusal("close", "work_x", "lane-beta", "lane-alpha", observed).String()
	assertTeachingRefusal(t, closed, "work_lease_holder",
		"compound work close work_x --by lane-alpha",
		"--steal --reason",
	)
	if !strings.Contains(closed, "not closed") {
		t.Errorf("the close refusal must say the work did not close:\n%s", closed)
	}
}
