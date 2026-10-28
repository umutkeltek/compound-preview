package main

import (
	"fmt"
	"strings"

	"compound/internal/work"
)

// This file holds the two fail-closed gates that terminate `compound work claim` and
// `compound work close`. Both are enforced in internal/work; what lives here is only their
// RENDERING, because the CLI is where the operator reads them and the next command they can
// paste is a CLI command, not a package API.
//
// Both are selected structurally rather than by matching the enforcing error's text where
// that is possible: a lease conflict is decided by re-reading the lease, so an unreadable
// store or an invalid state transition keeps its own plain error and its own remedy. The
// git-delivery proof gate is the exception and says so at its own seam below.

// workLeaseConflict reports whether workID is held by an actor other than the one attempting
// verb, by reading the live lease projection rather than interpreting an error string. An
// unreadable projection returns no conflict: this is a refusal RENDERER, so when it cannot
// prove the gate fired the caller falls back to the plain error rather than inventing a
// diagnosis.
func workLeaseConflict(store *work.Store, workID, actor string) (holder string, conflicted bool) {
	if store == nil {
		return "", false
	}
	leases, err := store.ListLeases()
	if err != nil {
		return "", false
	}
	for _, lease := range leases {
		if lease.WorkID != workID {
			continue
		}
		held := strings.TrimSpace(lease.By)
		if held == "" || held == strings.TrimSpace(actor) {
			return held, false
		}
		return held, true
	}
	return "", false
}

// workLeaseHolderRefusal renders the shared claim/close lease gate.
//
// The gate is what makes `proposed != held` real: dispatch consumes proposed work, so an
// unclaimed item gets double-executed by a concurrent lane. When it fires, the reader is
// usually a lane that believes the work is theirs, and the two correct next moves are
// opposite in cost — read who holds it and wait, or take it with a recorded reason. A refusal
// that names only the holder makes the second one look like the only one.
func workLeaseHolderRefusal(verb, workID, actor, holder string, observed error) gateRefusal {
	me := firstNonEmptyRefusalValue(actor, "<actor>")
	id := firstNonEmptyRefusalValue(workID, "<work-id>")
	refusal := gateRefusal{
		Command: "compound work " + verb,
		Gate:    "work_lease_holder",
		Refused: fmt.Sprintf("%s is held by %q, not %q, so the work was not %s and its state is unchanged", id, holder, me, workLeasePastTense(verb)),
		Tried: []string{
			fmt.Sprintf("%s %s as %q: %v", verb, id, me, observed),
			fmt.Sprintf("live lease for %s: held by %q", id, holder),
		},
		Prevents: "two lanes executing the same work. The lease is what makes proposed different from held: dispatch consumes proposed work, so a claim that ignored a live lease would hand the same item to a second lane and land two diverging answers to one question.",
	}
	if verb == "close" {
		refusal.DoThis = []string{
			fmt.Sprintf("compound work show %s   # who holds it, and what the holder has already attached", id),
			fmt.Sprintf("compound work close %s --by %s --outcome completed --move-type <type>   # close as the holder", id, firstNonEmptyRefusalValue(holder, "<holder>")),
			fmt.Sprintf("compound work close %s --by %s --outcome completed --move-type <type> --steal --reason \"<why the holder cannot finish>\"   # the recorded takeover", id, me),
		}
		return refusal
	}
	refusal.DoThis = []string{
		fmt.Sprintf("compound work show %s   # who holds it and since when", id),
		"compound coordination   # the me / others / waiting-on view across every lane, before taking anything",
		fmt.Sprintf("compound work dispatch-claim --next --actor %s   # claim the next UNHELD item instead of contending for this one", me),
	}
	return refusal
}

func workLeasePastTense(verb string) string {
	if verb == "close" {
		return "closed"
	}
	return "claimed"
}

// gitDeliveryProofMarkers are the substrings that identify internal/work's git-delivery proof
// gate in the error it returns.
//
// This is the one place in this file that reads an error's TEXT, and it is a deliberate
// coupling rather than an oversight: the gate lives inside the close path, it is not typed,
// and re-deriving "would this close need a landed proof?" at the CLI would mean a second copy
// of the derivation — a copy that can disagree with the enforcer and tell an operator the
// gate did not fire when it did. The coupling is kept honest by driving the REAL gate in
// TestWorkCloseGitDeliveryProofRefusalTeaches: reword the enforcer and the test goes red
// here, at the seam, instead of silently degrading the refusal back to a bare error.
var gitDeliveryProofMarkers = []string{
	"requires git_delivery authority because",
	"requires a cited recorded land-check proof",
	"has no acceptable landed proof",
}

func isGitDeliveryProofRefusal(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	for _, marker := range gitDeliveryProofMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// gitDeliveryProofRefusal renders the close gate that requires a completed source-controlled
// work to cite a proof that its change actually landed.
//
// The refusal has to carry the SHAPE of an acceptable proof, not just the fact that one is
// missing, because the three shapes are not interchangeable and picking the wrong one is the
// common failure: a `commit:` ref declares Git delivery and therefore demands a git_delivery
// contract plus a recorded land-check, while a bridge envelope — work whose result is a live
// surface rather than a merge — closes on `live:` / `evidence:` and must NEVER be dressed up
// with a `commit:` ref to get past the gate. Naming only the missing ref invites exactly that
// move, and it produces an envelope whose proof chain points at a commit nobody verified.
func gitDeliveryProofRefusal(workID, outcome string, proofRefs []string, observed error) gateRefusal {
	id := firstNonEmptyRefusalValue(workID, "<work-id>")
	cited := "none passed"
	if joined := strings.TrimSpace(strings.Join(proofRefs, ", ")); joined != "" {
		cited = joined
	}
	return gateRefusal{
		Command: "compound work close",
		Gate:    "git_delivery_proof",
		Refused: fmt.Sprintf("%s cannot close %s without a proof that its change landed, so the work is unchanged and no receipt was minted", id, firstNonEmptyRefusalValue(outcome, "completed")),
		Tried: []string{
			fmt.Sprintf("--proof refs cited on this close: %s", cited),
			fmt.Sprintf("git-delivery proof gate for %s: %v", id, observed),
		},
		DoThis: []string{
			fmt.Sprintf("compound work land-check %s --tip <full-landed-sha> --json   # records the land-check whose evidence ref a merged change closes on", id),
			fmt.Sprintf("compound work show %s   # whether this envelope carries a git_delivery contract at all", id),
			fmt.Sprintf("compound work close %s --outcome completed --move-type audit_only --proof <live-or-evidence-ref>   # a bridge envelope: no merge, so no commit ref", id),
		},
		Prevents: "a work that reports completed while its change never landed. The three proof shapes are not interchangeable: a commit: ref DECLARES Git delivery and pulls in the git_delivery contract plus a recorded land-check; a bridge envelope whose result is a live surface closes on live:/evidence: and must never borrow a commit: ref to clear this gate, because that leaves a proof chain pointing at a commit nobody checked.",
	}
}
