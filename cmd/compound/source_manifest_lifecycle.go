package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/sourceobs"
	"compound/internal/work"
)

func cmdSourceManifestLifecycleAdvance(args []string) {
	if code := runSourceManifestLifecycleAdvance(args, os.Stdout, os.Stderr, time.Now()); code != 0 {
		os.Exit(code)
	}
}

func runSourceManifestLifecycleAdvance(args []string, stdout, stderr io.Writer, now time.Time) int {
	parsed, err := parseSourceManifestLifecycleAdvanceArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "source manifest-advance:", err)
		return 1
	}
	manifest, err := sourceobs.LoadSourceManifest(homePath, parsed.family)
	if err != nil {
		fmt.Fprintln(stderr, "source manifest-advance:", err)
		return 1
	}
	workStore, err := work.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "source manifest-advance:", err)
		return 1
	}
	w, err := workStore.Get(parsed.workID)
	if err != nil {
		fmt.Fprintln(stderr, "source manifest-advance:", err)
		return 1
	}
	if w.State != "running" {
		fmt.Fprintf(stderr, "source manifest-advance: work %s must be running, got %s\n", w.ID, w.State)
		return 1
	}
	if strings.TrimSpace(w.Authority.Owner) != parsed.actor {
		fmt.Fprintf(stderr, "source manifest-advance: work %s is leased by %q, not %q\n", w.ID, w.Authority.Owner, parsed.actor)
		return 1
	}
	req := sourceobs.SourceManifestLifecycleAdvanceRequest{
		ToState:          parsed.toState,
		ExpectedRevision: manifest.Lifecycle.Revision,
		WorkID:           parsed.workID,
		Actor:            parsed.actor,
		Proofs:           parsed.proofs,
		WriterHandoff:    parsed.writerHandoff,
	}
	ai := sourceManifestLifecycleActionIntent(homePath, w, manifest, req, now)
	cfg, policySource, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "source manifest-advance:", err)
		return 1
	}
	pd := policy.Evaluate(ai, cfg, now)
	req.ActionIntentID = ai.ID
	req.PolicyDecisionID = pd.ID
	plan, err := sourceobs.PlanSourceManifestLifecycleAdvance(homePath, parsed.family, req, now)
	if err != nil {
		fmt.Fprintln(stderr, "source manifest-advance:", err)
		return 1
	}
	if !parsed.confirm {
		return printSourceManifestLifecycleAdvance(stdout, "dry-run", plan, ai, pd, policySource, parsed.jsonOut)
	}
	if !policy.Allowed(pd) {
		fmt.Fprintf(stderr, "source manifest-advance: policy %s (%s) refuses %s -> %s; import an exact reviewed authority before retrying\n", pd.Outcome, pd.ID, plan.FromState, plan.ToState)
		return 1
	}
	if _, err := workStore.RecordAction(w.ID, ai, pd, now); err != nil {
		fmt.Fprintln(stderr, "source manifest-advance:", err)
		return 1
	}
	applied, err := sourceobs.AdvanceSourceManifestLifecycle(homePath, parsed.family, req, now)
	if err != nil {
		fmt.Fprintln(stderr, "source manifest-advance:", err)
		return 1
	}
	return printSourceManifestLifecycleAdvance(stdout, "confirmed", applied, ai, pd, policySource, parsed.jsonOut)
}

type sourceManifestLifecycleAdvanceArgs struct {
	family        string
	toState       string
	workID        string
	actor         string
	proofs        []sourceobs.SourceLifecycleProof
	writerHandoff *sourceobs.SourceWriterHandoff
	confirm       bool
	jsonOut       bool
}

func parseSourceManifestLifecycleAdvanceArgs(args []string) (sourceManifestLifecycleAdvanceArgs, error) {
	const usage = "usage: compound source manifest-advance <family> --to <state> --work <id> --by <actor> --proof <gate>=<evidence-ref>... [writer handoff/window flags] [--confirm] [--json]"
	var out sourceManifestLifecycleAdvanceArgs
	var legacyWriters []string
	compoundWriter, rollbackRef, fenceRef := "", "", ""
	writerEpoch, quietSeconds := int64(0), int64(0)
	windowStart, windowEnd := "", ""
	var observedReads, observedWrites, unexplainedReads, unexplainedWrites int64
	windowFlags := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf(usage)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "--confirm":
			out.confirm = true
		case "--json":
			out.jsonOut = true
		case "--to", "--work", "--by", "--proof", "--legacy-writer", "--compound-writer", "--rollback", "--writer-fence", "--writer-epoch", "--quiet-window", "--window-start", "--window-end", "--observed-reads", "--observed-writes", "--unexplained-reads", "--unexplained-writes":
			value, err := next()
			if err != nil {
				return out, err
			}
			switch arg {
			case "--to":
				out.toState = value
			case "--work":
				out.workID = value
			case "--by":
				out.actor = value
			case "--proof":
				parts := strings.SplitN(value, "=", 2)
				if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
					return out, fmt.Errorf("--proof must be <gate>=<evidence-ref>")
				}
				out.proofs = append(out.proofs, sourceobs.SourceLifecycleProof{Gate: parts[0], EvidenceRef: parts[1]})
			case "--legacy-writer":
				legacyWriters = append(legacyWriters, value)
			case "--compound-writer":
				compoundWriter = value
			case "--rollback":
				rollbackRef = value
			case "--writer-fence":
				fenceRef = value
			case "--writer-epoch":
				writerEpoch, err = parseNonNegativeInt64(arg, value)
			case "--quiet-window":
				var duration time.Duration
				duration, err = time.ParseDuration(value)
				quietSeconds = int64(duration.Seconds())
				if err == nil && quietSeconds <= 0 {
					err = fmt.Errorf("--quiet-window must be positive")
				}
			case "--window-start":
				windowStart, windowFlags = value, true
			case "--window-end":
				windowEnd, windowFlags = value, true
			case "--observed-reads":
				observedReads, err = parseNonNegativeInt64(arg, value)
				windowFlags = true
			case "--observed-writes":
				observedWrites, err = parseNonNegativeInt64(arg, value)
				windowFlags = true
			case "--unexplained-reads":
				unexplainedReads, err = parseNonNegativeInt64(arg, value)
				windowFlags = true
			case "--unexplained-writes":
				unexplainedWrites, err = parseNonNegativeInt64(arg, value)
				windowFlags = true
			}
			if err != nil {
				return out, err
			}
		default:
			if strings.HasPrefix(arg, "-") || out.family != "" {
				return out, fmt.Errorf(usage)
			}
			out.family = arg
		}
	}
	if strings.TrimSpace(out.family) == "" || strings.TrimSpace(out.toState) == "" || strings.TrimSpace(out.workID) == "" || strings.TrimSpace(out.actor) == "" || len(out.proofs) == 0 {
		return out, fmt.Errorf(usage)
	}
	if len(legacyWriters) > 0 || compoundWriter != "" || rollbackRef != "" || fenceRef != "" || writerEpoch != 0 || quietSeconds != 0 {
		out.writerHandoff = &sourceobs.SourceWriterHandoff{LegacyWriterRefs: legacyWriters, CompoundWriterRef: compoundWriter, RollbackRef: rollbackRef, FenceRef: fenceRef, WriterEpoch: writerEpoch, RequiredQuietWindowSeconds: quietSeconds}
	}
	if windowFlags {
		found := false
		for i := range out.proofs {
			if out.proofs[i].Gate == sourceobs.SourceLifecycleGateQuietWindowMeasurement {
				out.proofs[i].QuietWindow = &sourceobs.SourceQuietWindowMeasurement{StartedAt: windowStart, EndedAt: windowEnd, ObservedReads: observedReads, ObservedWrites: observedWrites, UnexplainedReads: unexplainedReads, UnexplainedWrites: unexplainedWrites}
				found = true
			}
		}
		if !found {
			return out, fmt.Errorf("quiet-window measurement flags require --proof quiet_window_measurement=<ref>")
		}
	}
	return out, nil
}

func parseNonNegativeInt64(flag, value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s requires a non-negative integer", flag)
	}
	return parsed, nil
}

func sourceManifestLifecycleActionIntent(homePath string, w envelope.WorkEnvelope, manifest sourceobs.SourceManifest, req sourceobs.SourceManifestLifecycleAdvanceRequest, now time.Time) envelope.ActionIntent {
	mutationClass, reversibility, blastRadius := "local_reversible", "easy", "local"
	if sourceLifecycleHighRisk(manifest.Lifecycle.State) || sourceLifecycleHighRisk(req.ToState) {
		mutationClass, reversibility, blastRadius = "external_side_effect", "hard", "user-data"
	}
	if req.ToState == sourceobs.SourceLifecycleRetired {
		mutationClass, reversibility = "destructive", "irreversible"
	}
	refs := []string{manifest.AuthorityRef}
	for _, proof := range req.Proofs {
		refs = append(refs, strings.TrimSpace(proof.EvidenceRef))
	}
	rollback := "restore the previous SourceManifest pre-image; keep legacy reads and recovery available"
	if req.WriterHandoff != nil && strings.TrimSpace(req.WriterHandoff.RollbackRef) != "" {
		rollback = req.WriterHandoff.RollbackRef
	}
	if manifest.Lifecycle != nil && manifest.Lifecycle.WriterHandoff != nil && strings.TrimSpace(manifest.Lifecycle.WriterHandoff.RollbackRef) != "" {
		rollback = manifest.Lifecycle.WriterHandoff.RollbackRef
	}
	id := sourceManifestLifecycleID(w.ID, manifest.SourceFamily, manifest.Lifecycle.State, req.ToState, fmt.Sprintf("%d", manifest.Lifecycle.Revision), strings.Join(refs, "|"), now.UTC().Format(time.RFC3339Nano))
	return envelope.ActionIntent{
		Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: id, CreatedAt: now.UTC().Format(time.RFC3339), Producer: envelope.Producer{Kind: "cli", Name: "compound", LaneID: req.Actor}, IDs: envelope.IDs{WorkID: w.ID, ContextPackID: w.IDs.ContextPackID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1", EvidenceRefs: refs, Uncertainty: envelope.Uncertainty{Level: "known"}},
		Actor:  req.Actor, Capability: "source_manifest.lifecycle.advance", Action: "advance_source_manifest_to_" + req.ToState,
		Target:        envelope.ActionTarget{Kind: "source_manifest", ID: manifest.SourceFamily, Path: sourceobs.SourceManifestPath(homePath, manifest.SourceFamily)},
		MutationClass: mutationClass, Reversibility: reversibility, BlastRadius: blastRadius,
		Preview: fmt.Sprintf("advance source family %s from %s to %s with typed proofs", manifest.SourceFamily, manifest.Lifecycle.State, req.ToState), RollbackPlan: rollback,
	}
}

func sourceLifecycleHighRisk(state string) bool {
	return state == sourceobs.SourceLifecycleCompoundPrimaryWriter || state == sourceobs.SourceLifecycleLegacyFrozen || state == sourceobs.SourceLifecycleQuietWindowProven || state == sourceobs.SourceLifecycleArchived || state == sourceobs.SourceLifecycleRetired
}

func sourceManifestLifecycleID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "act_" + hex.EncodeToString(sum[:])[:12]
}

func printSourceManifestLifecycleAdvance(stdout io.Writer, mode string, report sourceobs.SourceManifestLifecycleAdvanceReport, ai envelope.ActionIntent, pd envelope.PolicyDecision, policySource string, jsonOut bool) int {
	if jsonOut {
		out := map[string]any{"schema_version": report.SchemaVersion, "mode": mode, "applied": report.Applied, "source_family": report.SourceFamily, "manifest_ref": report.ManifestRef, "from_state": report.FromState, "to_state": report.ToState, "revision": report.Revision, "transition": report.Transition, "lifecycle": report.Lifecycle, "status": report.Status, "action_intent_id": ai.ID, "policy_decision_id": pd.ID, "policy_outcome": pd.Outcome, "policy_allowed": policy.Allowed(pd), "policy_source": policySource}
		if err := encodeJSON(stdout, out); err != nil {
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# source manifest lifecycle (%s)\n\n", mode)
	fmt.Fprintf(stdout, "- family: %s\n- state: %s -> %s\n- revision: %d\n", report.SourceFamily, report.FromState, report.ToState, report.Revision)
	fmt.Fprintf(stdout, "- policy: %s allowed=%t decision=%s\n", pd.Outcome, policy.Allowed(pd), pd.ID)
	if mode == "dry-run" {
		fmt.Fprintln(stdout, "- mutation: none; add --confirm only after the policy and typed proofs are correct")
	} else {
		fmt.Fprintf(stdout, "- action: %s\n- next: %s\n", ai.ID, report.Status.NextSafeAction)
	}
	return 0
}
