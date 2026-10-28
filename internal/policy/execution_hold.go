package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
)

// HoldForExecutionEvidence records a stricter policy decision for an already
// evaluated action when new governed evidence invalidates execution at the
// adapter boundary. It can only raise caution to hold.
func HoldForExecutionEvidence(ai envelope.ActionIntent, evidenceRefs []string, reason string, now time.Time) envelope.PolicyDecision {
	refs := append([]string{}, evidenceRefs...)
	sort.Strings(refs)
	refs = compactExecutionHoldRefs(refs)
	digest := sha256.Sum256([]byte(ai.ID + "\x00" + strings.Join(refs, "\x00") + "\x00" + strings.TrimSpace(reason)))
	createdAt := strings.TrimSpace(ai.CreatedAt)
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		createdAt = now.UTC().Format(time.RFC3339)
	}
	return envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision,
			ID:            "pol_delivery_hold_" + hex.EncodeToString(digest[:])[:16],
			CreatedAt:     createdAt,
			Producer:      envelope.Producer{Kind: "policy", Name: "compound"},
			IDs:           envelope.IDs{WorkID: ai.IDs.WorkID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       ai.Privacy,
			Risk:          ai.Risk,
			EvidenceRefs:  refs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: ai.ID,
		Outcome:        "hold",
		Reasons:        []string{strings.TrimSpace(reason)},
		Obligations:    []string{},
	}
}

func compactExecutionHoldRefs(refs []string) []string {
	out := refs[:0]
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || (len(out) > 0 && out[len(out)-1] == ref) {
			continue
		}
		out = append(out, ref)
	}
	return out
}
