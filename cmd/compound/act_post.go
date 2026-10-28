package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/product"
	"compound/internal/work"
)

func runActPost(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fromWork := strings.TrimSpace(flagValue(args, "--from-work"))
	draftID := strings.TrimSpace(flagValue(args, "--draft"))
	hasContent := hasFlag(args, "--content")
	transport := strings.TrimSpace(flagValue(args, "--transport"))
	sourceCount := 0
	if fromWork != "" {
		sourceCount++
	}
	if draftID != "" {
		sourceCount++
	}
	if hasContent {
		sourceCount++
	}
	if sourceCount != 1 || (hasContent && flagValue(args, "--content") != "-") || transport == "" {
		fmt.Fprintln(stderr, "usage: compound act post (--from-work W | --draft D (--at RFC3339 | --reconcile) | --content -) [--work W] --transport NAME [--confirm]")
		return 2
	}
	if hasFlag(args, "--capability") {
		fmt.Fprintln(stderr, "error: --capability is not caller-selectable for act post; the capability is always "+product.ContentPostCapability)
		return 2
	}
	workID := strings.TrimSpace(flagValue(args, "--work"))
	if hasFlag(args, "--confirm") && workID == "" && draftID == "" {
		fmt.Fprintln(stderr, "error: --work W is required for --confirm (a governed act belongs to a work)")
		return 1
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	if draftID != "" {
		var at time.Time
		if !hasFlag(args, "--reconcile") {
			at, err = time.Parse(time.RFC3339, strings.TrimSpace(flagValue(args, "--at")))
			if err != nil {
				fmt.Fprintln(stderr, "error: --at must be RFC3339")
				return 2
			}
		}
		out, postErr := product.ExecuteContentPost(homePath, draftID, transport, workID, at, hasFlag(args, "--confirm"), hasFlag(args, "--reconcile"), time.Now())
		if hasFlag(args, "--json") {
			if err := encodeJSON(stdout, out); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		} else {
			fmt.Fprintf(stdout, "# content post: %s\n- attempt: %s\n- next: %s\n", out.Mode, out.AttemptID, out.NextCommand)
			if out.Request != nil {
				fmt.Fprintf(stdout, "- package: %s\n- account: %s\n- text: %s\n", out.Request.PackageDigest, out.Request.AccountID, out.Request.Body)
			}
		}
		if postErr != nil {
			fmt.Fprintln(stderr, "error:", postErr)
			return 1
		}
		return 0
	}
	var body string
	var forward []byte
	if fromWork != "" {
		body, err = composeReceiptCard(homePath, fromWork)
		forward = []byte(body)
	} else {
		forward, err = io.ReadAll(stdin)
		body = string(forward)
	}

	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	capability := product.ContentPostCapability
	if workID == "" {
		workID = "work_dryrun"
	}
	target := envelope.ActionTarget{
		Kind: "transport",
		ID:   transport,
		Path: "actuator/transports/" + transport + ".json",
	}
	now := time.Now()
	ai := governedPostIntent(workID, capability, target, string(forward), now)
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	pd := policy.Evaluate(ai, cfg, now)
	if !hasFlag(args, "--confirm") {
		return printActPostPreview(stdout, transport, target.Path, body, nil, pd, hasFlag(args, "--json"))
	}
	if !policy.Allowed(pd) {
		return printActPostHeld(stdout, transport, body, pd, hasFlag(args, "--json"))
	}
	artifactStore, err := act.NewArtifactStore(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	result, err := act.Execute(artifactStore, act.NewExecTransportAdapter(transport, homePath), ai, pd, forward, now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return printActPostResult(stdout, transport, result, hasFlag(args, "--json"))
}

func governedPostIntent(workID, capability string, target envelope.ActionTarget, body string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actIDFor("act_", workID, capability, target.ID, body),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "operator",
		Capability:    capability,
		Action:        "post_content",
		Target:        target,
		MutationClass: "external_side_effect",
		// The generic exec transport adapter has no delete verb, so nothing
		// guarantees a posted body can be recalled. A future richer adapter
		// that PROVES deletion may re-declare "manual" with a concrete
		// rollback plan.
		Reversibility: "irreversible",
		BlastRadius:   "local",
		Preview:       "post content through transport " + target.ID,
		RollbackPlan:  "none guaranteed by the transport adapter",
	}
}

func governedDraftPostIntent(workID string, target envelope.ActionTarget, request product.ContentPostTransportRequest, auth product.ContentPostAuthorization, forward []byte, now time.Time) envelope.ActionIntent {
	evidence := append([]string(nil), auth.ActionIntent.EvidenceRefs...)
	evidence = append(evidence,
		auth.BodyRef,
		"content-post-authorization:"+auth.DraftID,
		"action:"+auth.ActionIntentID,
		"policy:"+auth.PolicyDecisionID,
	)
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actIDFor("act_", workID, product.ContentPostCapability, target.ID, request.DraftID, request.ScheduleAt, string(forward)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Scope:         envelope.Scope{ProductID: request.ProductID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  evidence,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "operator",
		Capability:    product.ContentPostCapability,
		Action:        "post_content",
		Target:        target,
		MutationClass: "external_side_effect",
		Reversibility: "irreversible",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("schedule approved draft %s to %s through transport %s", request.DraftID, request.Platform, target.ID),
		RollbackPlan:  "delete or return the scheduled transport post to draft before publication; no deletion is guaranteed after publication",
	}
}

// printActPostPreview names the exact resolved transport and its adapter
// config path so the operator sees precisely which authority would fire on
// --confirm.
func printActPostPreview(stdout io.Writer, transport, transportConfig, body string, draft *product.ContentPostTransportRequest, pd envelope.PolicyDecision, asJSON bool) int {
	allowed := policy.Allowed(pd)
	if asJSON {
		payload := map[string]any{
			"mode": "dry-run", "transport": transport, "transport_config": transportConfig, "card": body,
			"policy_outcome": pd.Outcome, "allowed": allowed,
		}
		if draft != nil {
			payload["draft"] = draft
		}
		if err := encodeJSON(stdout, payload); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# act post (dry-run — nothing sent)")
	fmt.Fprintln(stdout, body)
	fmt.Fprintf(stdout, "- transport: %s\n", transport)
	fmt.Fprintf(stdout, "- transport-config: %s\n", transportConfig)
	if draft != nil {
		fmt.Fprintf(stdout, "- draft: %s product=%s platform=%s integration=%s schedule=%s\n",
			draft.DraftID, draft.ProductID, draft.Platform, draft.IntegrationID, draft.ScheduleAt)
	}
	if allowed {
		fmt.Fprintf(stdout, "- policy: %s (ALLOWED) — run again with --confirm --work W to execute\n", pd.Outcome)
	} else {
		fmt.Fprintf(stdout, "- policy: %s (BLOCKED) — no executable write occurred\n", pd.Outcome)
	}
	return 0
}

func printActPostHeld(stdout io.Writer, transport, body string, pd envelope.PolicyDecision, asJSON bool) int {
	if asJSON {
		if err := encodeJSON(stdout, map[string]any{
			"mode": "held", "transport": transport, "card": body,
			"policy_outcome": pd.Outcome, "allowed": false,
		}); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# act post (held — nothing sent)")
	fmt.Fprintln(stdout, body)
	fmt.Fprintf(stdout, "- transport: %s\n", transport)
	fmt.Fprintf(stdout, "- policy: %s (BLOCKED) — no executable write occurred\n", pd.Outcome)
	return 0
}

func printActPostResult(stdout io.Writer, transport string, result act.Result, asJSON bool) int {
	if asJSON {
		if err := encodeJSON(stdout, map[string]any{
			"mode":           "confirmed",
			"transport":      transport,
			"receipt_id":     result.Receipt.ID,
			"transition_id":  result.Transition.ID,
			"move_type":      result.Receipt.MoveType,
			"pre_image_ref":  result.PreImageRef,
			"post_image_ref": result.PostImageRef,
		}); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# act post (confirmed — governed transport executed)")
	fmt.Fprintf(stdout, "- transport: %s\n", transport)
	fmt.Fprintf(stdout, "- receipt=%s transition=%s move=%s\n", result.Receipt.ID, result.Transition.ID, result.Receipt.MoveType)
	fmt.Fprintf(stdout, "- pre-image=%s post-image=%s\n", result.PreImageRef, result.PostImageRef)
	return 0
}

func composeReceiptCard(homePath, workID string) (string, error) {
	store, err := work.Open(homePath)
	if err != nil {
		return "", fmt.Errorf("compose receipt card: %w", err)
	}
	w, lifecycle, err := store.Show(workID)
	if err != nil {
		return "", fmt.Errorf("compose receipt card: show work %q: %w", workID, err)
	}
	if !terminalWorkState(w.State) {
		return "", fmt.Errorf("compose receipt card: work %q is not terminal (state %q)", workID, w.State)
	}
	var receiptID, transitionID string
	for i := len(lifecycle) - 1; i >= 0; i-- {
		event := lifecycle[i]
		if event.To != w.State || !terminalWorkState(event.To) {
			continue
		}
		for _, ref := range event.Attached {
			switch {
			case receiptID == "" && strings.HasPrefix(ref, "rec_"):
				receiptID = ref
			case transitionID == "" && strings.HasPrefix(ref, "tr_"):
				transitionID = ref
			}
		}
		if receiptID != "" && transitionID != "" {
			break
		}
	}
	if receiptID == "" || transitionID == "" {
		return "", fmt.Errorf("compose receipt card: terminal work %q has no attached receipt/transition", workID)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return "", fmt.Errorf("compose receipt card: open event store: %w", err)
	}
	record, err := events.Read(receiptID)
	if err != nil {
		return "", fmt.Errorf("compose receipt card: read receipt %s: %w", receiptID, err)
	}
	var receipt envelope.Receipt
	if err := json.Unmarshal(record.Bytes, &receipt); err != nil {
		return "", fmt.Errorf("compose receipt card: decode receipt %s: %w", receiptID, err)
	}
	if err := receipt.Validate(); err != nil {
		return "", fmt.Errorf("compose receipt card: receipt %s invalid: %w", receiptID, err)
	}
	if receipt.IDs.WorkID != workID {
		return "", fmt.Errorf("compose receipt card: receipt %s belongs to %q, not %q", receiptID, receipt.IDs.WorkID, workID)
	}

	proofs := nonEmptyCardProofs(receipt.ProofRefs, 4)
	proofLine := "none"
	if len(proofs) > 0 {
		proofLine = strings.Join(proofs, ", ")
	}
	predicate := strings.Join(strings.Fields(w.SuccessPredicate), " ")
	return strings.Join([]string{
		fmt.Sprintf("🧾 [%s] %s — %s", receiptCardVoice(receipt.MoveType), workID, receipt.Outcome),
		predicate,
		fmt.Sprintf("- move: %s · receipt: %s · transition: %s", receipt.MoveType, receiptID, transitionID),
		"- proofs: " + proofLine,
	}, "\n"), nil
}

func terminalWorkState(state string) bool {
	switch state {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func nonEmptyCardProofs(refs []string, limit int) []string {
	out := make([]string, 0, limit)
	for _, ref := range refs {
		if ref = strings.TrimSpace(ref); ref != "" {
			out = append(out, ref)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

// receiptCardVoice mirrors the unexported voice mapping in internal/narrate/narrate.go:692.
func receiptCardVoice(moveType string) string {
	switch moveType {
	case "memory_delta":
		return "Memory"
	case "rule_delta":
		return "The rules"
	case "control_plane_repair":
		return "The repair log"
	case "audit_only":
		return "The audit"
	default:
		return "The ledger"
	}
}
