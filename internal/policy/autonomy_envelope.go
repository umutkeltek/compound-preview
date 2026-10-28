package policy

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
)

// The operator autonomy envelope.
//
// D3 says humans own irreversible decisions BY SETTING THE ENVELOPE, not by
// being paged each time. Standing grants were the first half of that; this is
// the half that makes the first half reach shell work.
//
// The failure it repairs: a shell intent carries no file path, so a path-scoped
// standing grant could never match it -- matchGrant missed with "target path
// empty" on every command. What survived was the class baseline, so an ordinary
// `bun install` or `python3 -c` inside a granted project reached the operator as
// a hold. On an interactive session a hold is a prompt; on an unattended lane it
// is a freeze, and a boundary that freezes the work it was meant to govern gets
// removed rather than obeyed. Adding one more verb to the read allowlist buys a
// day; the allowlist is not the shape of the problem.
//
// So the envelope is stated once, as an envelope: at L3 autonomy, inside a
// standing grant's target_path, a hold clears. The three things it does NOT do
// are what keep it a boundary rather than an off switch:
//
//  1. It never touches `deny`. Forbidden paths, invalid source lineage, sink
//     violations, and malicious-classifier verdicts are unaffected.
//  2. It never clears a floor reason outside autonomyEnvelopeClearableFloors --
//     an operator-gated shape, an unreadable command, a suspicious-classifier
//     verdict, or a missing schema/destination stays held.
//  3. It requires a grant that names a target_path, and requires the intent's
//     path to be inside it. No grant, no path, no envelope.
//
// What it deliberately DOES ignore is the grant's class/reversibility/blast
// fields. Requiring a grant to pre-enumerate every mutation class a project's
// build might produce is the same "enumerate the safe shapes" move the read
// allowlist already lost at. The operator authorizes a SCOPE; inside it, the
// class is a description of the work, not a second gate.
const (
	// AutonomyLevelFullProjectScope is the autonomy level at which the envelope
	// is in force. Lower levels leave every hold exactly where it was.
	AutonomyLevelFullProjectScope = "L3"

	// UnknownClassificationFloorReason is the fail-closed floor for a tool or
	// command shape the classifier does not recognize.
	UnknownClassificationFloorReason = "unknown tool/action classification fails closed"
)

// autonomyEnvelopeClearableFloors is the ONLY security-floor reason an envelope
// may clear. It is an allowlist, not a denylist, because a floor added later
// must be non-clearable until someone decides otherwise -- the safe default for
// a new floor is that the operator still sees it.
var autonomyEnvelopeClearableFloors = map[string]bool{
	UnknownClassificationFloorReason: true,
}

// applyAutonomyEnvelope clears a hold that the operator's standing envelope has
// already answered. It only ever lowers `hold`; every other outcome, and every
// non-clearable floor, passes through untouched.
func applyAutonomyEnvelope(pd envelope.PolicyDecision, ai envelope.ActionIntent, cfg Config, now time.Time) envelope.PolicyDecision {
	if pd.Outcome != "hold" {
		return pd
	}
	if !autonomyEnvelopeInForce(cfg) || autonomyEnvelopeBlocked(ai) {
		return pd
	}
	path := strings.TrimSpace(ai.Target.Path)
	if path == "" {
		return pd
	}
	// Restated here rather than trusted from evaluateBase: this function also
	// runs on a decision the security floor has already raised, and a hard
	// safety rule that only holds on one of two paths is not a hard safety rule.
	if _, forbidden := matchForbidden(cfg.ForbiddenPaths, path); forbidden {
		return pd
	}
	grant, ok := autonomyEnvelopeScopeGrant(ai, cfg, now)
	if !ok {
		return pd
	}
	if autonomyEnvelopeNarrowedAway(ai, cfg, now) {
		pd.Reasons = append(pd.Reasons, fmt.Sprintf(
			"autonomy envelope narrowed: operator presence is away, so %s:%s clears only local_reversible holds; %s/%s needs an exact capability grant with requires_presence away",
			grantID(grant), grant.TargetPath, ai.Capability, ai.MutationClass))
		return pd
	}
	pd.Outcome = "allow_with_obligations"
	pd.Obligations = grantObligations(ai, cfg)
	pd.StandingGrantID = grant.ID
	pd.ExpiresAt = grant.ExpiresAt
	pd.Reasons = append(pd.Reasons, fmt.Sprintf(
		"autonomy envelope %s: hold auto-cleared inside %s:%s; classification=%s/%s",
		AutonomyLevelFullProjectScope, grantID(grant), grant.TargetPath, ai.Action, ai.MutationClass))
	if ref := strings.TrimSpace(cfg.OperatorAutonomyAuthorityRef); ref != "" {
		pd.EvidenceRefs = mergeStrings(pd.EvidenceRefs, ref)
	}
	pd.ID = idFor("pol_", ai.ID, pd.Outcome, "autonomy-envelope", ts(now))
	return pd
}

// autonomyEnvelopeInForce reports whether the operator has actually set an L3
// envelope AND named the authority that says so. An autonomy level with no
// authority ref is a config value nobody signed; it does not widen anything.
func autonomyEnvelopeInForce(cfg Config) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.OperatorAutonomyLevel), AutonomyLevelFullProjectScope) &&
		strings.TrimSpace(cfg.OperatorAutonomyAuthorityRef) != ""
}

// autonomyEnvelopeBlocked reports whether this intent carries a floor the
// envelope must leave alone.
func autonomyEnvelopeBlocked(ai envelope.ActionIntent) bool {
	if strings.TrimSpace(ai.SecurityFloorOutcome) == "deny" {
		return true
	}
	if strings.TrimSpace(ai.Action) == OperatorGatedOperation {
		return true
	}
	for _, reason := range ai.SecurityReasons {
		if !autonomyEnvelopeClearableFloors[strings.TrimSpace(reason)] {
			return true
		}
	}
	return false
}

// AwayEnvelopeClearableClass is the one mutation class the L3 envelope may
// still clear while the operator's presence is away.
const AwayEnvelopeClearableClass = "local_reversible"

// autonomyEnvelopeNarrowedAway is the away-mode narrowing. While the operator
// is away, a path scope stops being an authorization for consequences: the
// envelope clears a hold only on a local_reversible intent. Everything else --
// external_side_effect, financial, destructive, public, security, unknown --
// needs a grant that matchGrant accepts on its own (exact capability and
// class), and for a held class that grant is written with requires_presence
// away. This deliberately ignores the grant's own mutation_class: the envelope
// never matched class, so a class-bearing path grant would otherwise reopen
// the same hole. Presence only ever narrows here; it never clears anything.
func autonomyEnvelopeNarrowedAway(ai envelope.ActionIntent, cfg Config, now time.Time) bool {
	return cfg.OperatorPresence.Effective(now) == PresenceAway &&
		strings.TrimSpace(ai.MutationClass) != AwayEnvelopeClearableClass
}

// autonomyEnvelopeGrant is the envelope's answer for this intent: the scope
// grant, unless the away narrowing withholds it.
func autonomyEnvelopeGrant(ai envelope.ActionIntent, cfg Config, now time.Time) (StandingGrant, bool) {
	if autonomyEnvelopeNarrowedAway(ai, cfg, now) {
		return StandingGrant{}, false
	}
	return autonomyEnvelopeScopeGrant(ai, cfg, now)
}

// autonomyEnvelopeScopeGrant finds the standing grant whose SCOPE covers this intent.
//
// It matches on the PATH the operator drew and, where the grant narrowed one, a
// destination and a spend ceiling. It deliberately does not match capability,
// mutation class, reversibility, or blast radius. Those four are exactly what
// the envelope exists to stop gating on: a grant that must pre-name every
// capability a build might invoke is another allowlist, and the allowlist is
// what this change is retiring. matchGrant still enforces all of them on the
// ordinary path, which is why a capability-pinned grant keeps working as the
// narrow authorization it was written to be -- the envelope is the wider
// statement layered over it, not a replacement for it.
//
// The one class rule that survives: a READ grant cannot draw an envelope. "You
// may read this tree" is not an authorization to mutate it, and letting a read
// grant open a mutation envelope would let the narrowest grant in the registry
// authorize the widest thing in it.
func autonomyEnvelopeScopeGrant(ai envelope.ActionIntent, cfg Config, now time.Time) (StandingGrant, bool) {
	var best StandingGrant
	bestRank := -1
	for _, g := range cfg.StandingGrants {
		if strings.TrimSpace(g.TargetPath) == "" {
			continue
		}
		if g.MutationClass == "read" {
			continue
		}
		if g.Destination != "" && strings.TrimSpace(g.Destination) != strings.TrimSpace(ai.Destination) {
			continue
		}
		if g.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, g.ExpiresAt); err == nil && !now.Before(t) {
				continue
			}
		}
		// The activation window and the presence key bind here exactly as in
		// matchGrant: an envelope must not open through a grant the ordinary
		// path would call inactive.
		if _, inactive := grantNotYetActive(g, now); inactive {
			continue
		}
		if _, miss := grantPresenceMiss(g, cfg, now); miss {
			continue
		}
		if len(grantCostMisses(g, ai.EstimatedCost)) != 0 {
			continue
		}
		if _, ok := matchForbidden([]string{g.TargetPath}, filepath.ToSlash(ai.Target.Path)); !ok {
			continue
		}
		if rank := autonomyEnvelopeGrantRank(g, cfg); rank > bestRank {
			best, bestRank = g, rank
		}
	}
	return best, bestRank >= 0
}

// autonomyEnvelopeGrantRank orders candidate grants by how much consequence the
// operator already accepted in that path, so the decision names the STRONGEST
// true statement backing the clearance rather than whichever grant happened to
// be first in the file.
//
// This matters only for the audit trail -- every candidate here would clear the
// same hold -- but the audit trail is the whole point of naming a grant. A real
// registry can hold twenty path-scoped grants, and a decision reading
// "cleared inside <a collaboration-followup grant>" because that row sorted
// first tells an auditor something misleading, while "cleared inside <the
// destructive-shell-in-projects grant>" tells them the thing that actually
// makes clearing an install coherent.
//
// The ordering is the config's own class baseline, not a new scale invented
// here: a grant for a class the policy holds by default outranks one for a
// class it allows by default.
func autonomyEnvelopeGrantRank(g StandingGrant, cfg Config) int {
	if g.MutationClass == "" {
		// A grant that names a path and no class is the operator drawing the
		// scope itself, which is the widest statement available.
		return len(severity)
	}
	return severity[cfg.ClassOutcome[g.MutationClass].Outcome]
}

// shellTargetPath gives a shell (or PTY) intent the one path it can honestly
// state: the directory it runs in. Non-shell domains keep their own target path
// -- a filesystem intent already names the file it writes, and overwriting that
// with a cwd would widen the very field forbidden-path matching depends on.
func shellTargetPath(classification ActionClassification, cwd string) string {
	if classification.Domain != ActionDomainShell {
		return ""
	}
	if classification.TargetKind == "path" && strings.HasPrefix(classification.Destination, "path:") {
		if path := strings.TrimPrefix(classification.Destination, "path:"); filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
	}
	if cwd = strings.TrimSpace(cwd); cwd == "" {
		return ""
	}
	return filepath.Clean(cwd)
}

// autonomyEnvelopeCovers reports whether the operator's envelope is in force
// FOR THIS INTENT: L3 with a named authority, a grant drawn over the intent's
// path, no forbidden path, and no non-clearable floor. It is the same test
// applyAutonomyEnvelope runs, factored out so the source-control gate narrows
// on exactly the same condition rather than on a second, drifting copy of it.
func autonomyEnvelopeCovers(ai envelope.ActionIntent, cfg Config, now time.Time) bool {
	if !autonomyEnvelopeInForce(cfg) || autonomyEnvelopeBlocked(ai) {
		return false
	}
	path := strings.TrimSpace(ai.Target.Path)
	if path == "" {
		return false
	}
	if _, forbidden := matchForbidden(cfg.ForbiddenPaths, path); forbidden {
		return false
	}
	_, ok := autonomyEnvelopeGrant(ai, cfg, now)
	return ok
}

// intentCapturesForeignWorkInProgress reports whether this action could record
// or destroy uncommitted changes it does not own.
//
// The shell answer comes from the command shape, never from target_status: a
// shell intent's target path is the DIRECTORY it runs in, so "target dirty"
// only ever means "something under here is modified" -- it says nothing about
// whether this command touches it. Reading it as a per-action signal is exactly
// the conflation that held every command in a dirty repo.
//
// The file answer is the opposite: a write names the file it lands on, so the
// question is simply whether THAT file carries changes. Anything the boundary
// cannot resolve to a clean file stays held.
func intentCapturesForeignWorkInProgress(ai envelope.ActionIntent, sc SourceControlContext) bool {
	if ai.CapturesWorkInProgress {
		return true
	}
	switch strings.TrimSpace(ai.Target.Kind) {
	case "command", "session":
		return false
	case "file":
		status := sourceControlTargetStatus(ai, sc)
		if status == "clean" {
			return false
		}
		// A dirty file target is foreign work in progress UNLESS this session
		// wrote the dirt itself: continuing to edit a file the session already
		// touched rewrites its own work in progress, never anyone else's, and
		// a single-file write cannot record or destroy the foreign dirt that
		// lives on other paths. Unknown ownership stays fail-closed.
		return !(status == "dirty" && sc.SessionOwnsTarget)
	}
	return sourceControlTargetStatus(ai, sc) != "clean"
}
