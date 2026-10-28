package mem

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const SynthesisReconcileDigestSchemaVersion = "compound.synthesis_reconcile_digest.v0.1"

type SynthesisReconcileDigestOptions struct {
	Since time.Time
}

type SynthesisReconcileDigest struct {
	SchemaVersion string                         `json:"schema_version"`
	GeneratedAt   string                         `json:"generated_at"`
	Since         string                         `json:"since,omitempty"`
	CheckpointAt  string                         `json:"checkpoint_at,omitempty"`
	ZeroAction    bool                           `json:"zero_action"`
	Tier1         []SynthesisReconcileDigestLine `json:"tier1,omitempty"`
	Tier2         []SynthesisReconcileDigestLine `json:"tier2,omitempty"`
	Tier3         []SynthesisReconcileDigestLine `json:"tier3,omitempty"`
}

type SynthesisReconcileDigestLine struct {
	Tier        int      `json:"tier"`
	Kind        string   `json:"kind"`
	Summary     string   `json:"summary"`
	Count       int      `json:"count,omitempty"`
	Refs        []string `json:"refs,omitempty"`
	UndoCommand string   `json:"undo_command"`
}

func (s *Store) SynthesisReconcileDigest(now time.Time, opts SynthesisReconcileDigestOptions) (SynthesisReconcileDigest, error) {
	checkpoint, err := s.loadReconcileCheckpoint(now)
	if err != nil {
		return SynthesisReconcileDigest{}, err
	}
	report := checkpoint.LastProcessedReport
	claims, err := s.loadClaims()
	if err != nil {
		return SynthesisReconcileDigest{}, err
	}
	claimsByID := claimsByID(claims)
	positions, err := s.Positions()
	if err != nil {
		return SynthesisReconcileDigest{}, err
	}
	foldUse := claimFoldUseCounts(positions)

	digest := SynthesisReconcileDigest{
		SchemaVersion: SynthesisReconcileDigestSchemaVersion,
		GeneratedAt:   rfc3339(now),
		Since:         firstNonEmpty(rfc3339OrEmpty(opts.Since), checkpoint.Since),
		CheckpointAt:  checkpoint.UpdatedAt,
	}
	for _, reopened := range report.ReopenedClaims {
		claim, ok := claimsByID[reopened.ClaimID]
		if !ok {
			continue
		}
		class := EffectiveCanonClass(claim.ClaimType, claim.CanonClass)
		if !CanonClassOperatorPermanent(class) && foldUse[claim.ID] < 2 {
			continue
		}
		digest.Tier1 = append(digest.Tier1, SynthesisReconcileDigestLine{
			Tier:        1,
			Kind:        "reopened",
			Summary:     fmt.Sprintf("reopened %s claim %s for %s.%s", class, claim.ID, reviewPlain(claim.Entity), reviewPlain(claim.Topic())),
			Count:       1,
			Refs:        []string{"claim:" + claim.ID, reopened.ReviewRef},
			UndoCommand: reconcileUndoReviewCommand(claim.ID, reopened.ReviewRef),
		})
	}
	if report.ReconcileReopenedCount > 0 {
		digest.Tier2 = append(digest.Tier2, SynthesisReconcileDigestLine{
			Tier:        2,
			Kind:        "reopened",
			Summary:     fmt.Sprintf("reopened %d claim(s)", report.ReconcileReopenedCount),
			Count:       report.ReconcileReopenedCount,
			Refs:        reopenedDigestRefs(report.ReopenedClaims),
			UndoCommand: reconcileUndoReopenedCommand(report.ReopenedClaims),
		})
	}
	if report.ReconcileDecayRoutedCount > 0 {
		digest.Tier2 = append(digest.Tier2, SynthesisReconcileDigestLine{
			Tier:        2,
			Kind:        "decay_routed",
			Summary:     fmt.Sprintf("decay-routed %d usage or expiry signal(s)", report.ReconcileDecayRoutedCount),
			Count:       report.ReconcileDecayRoutedCount,
			Refs:        append([]string(nil), checkpoint.DecayRoutedRefs...),
			UndoCommand: "compound synthesis undo-checkpoint --kind decay_routed",
		})
	}
	if report.ReconcileParkedCount > 0 {
		digest.Tier2 = append(digest.Tier2, SynthesisReconcileDigestLine{
			Tier:        2,
			Kind:        "parked",
			Summary:     fmt.Sprintf("parked %d tension(s); top reasons: %s", report.ReconcileParkedCount, topReconcileParkedReasons(report.ReconcileParkedReasons, 3)),
			Count:       report.ReconcileParkedCount,
			Refs:        parkedDigestRefs(report.ReconcileParkedReasons),
			UndoCommand: "compound synthesis undo-checkpoint --kind parked",
		})
	}
	if report.ReconcileDismissedCount > 0 {
		digest.Tier3 = append(digest.Tier3, SynthesisReconcileDigestLine{
			Tier:        3,
			Kind:        "dismissed",
			Summary:     fmt.Sprintf("stage-0 dismissed %d no-tension event(s)", report.ReconcileDismissedCount),
			Count:       report.ReconcileDismissedCount,
			UndoCommand: "compound synthesis undo-checkpoint --kind dismissed",
		})
	}
	digest.ZeroAction = len(digest.Tier1) == 0 && len(digest.Tier2) == 0 && len(digest.Tier3) == 0
	return digest, nil
}

func (d SynthesisReconcileDigest) RenderText() string {
	var b strings.Builder
	if d.ZeroAction {
		b.WriteString("reconcile digest: zero actions\n")
		return b.String()
	}
	b.WriteString("reconcile digest\n")
	for _, section := range []struct {
		name  string
		lines []SynthesisReconcileDigestLine
	}{
		{name: "tier-1", lines: d.Tier1},
		{name: "tier-2", lines: d.Tier2},
		{name: "tier-3", lines: d.Tier3},
	} {
		if len(section.lines) == 0 {
			continue
		}
		b.WriteString(section.name)
		b.WriteString("\n")
		for _, line := range section.lines {
			b.WriteString("- ")
			b.WriteString(line.Summary)
			if line.UndoCommand != "" {
				b.WriteString(" | undo: ")
				b.WriteString(line.UndoCommand)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (s *Store) ApplySynthesisReconcileDigestUndoCommand(command string, now time.Time) error {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return fmt.Errorf("undo command required")
	}
	if len(fields) >= 6 && fields[0] == "compound" && fields[1] == "synthesis" && fields[2] == "undo-review" {
		var claimID, reviewRef string
		for i := 3; i < len(fields); i++ {
			switch fields[i] {
			case "--claim":
				if i+1 >= len(fields) {
					return fmt.Errorf("--claim requires value")
				}
				i++
				claimID = fields[i]
			case "--review":
				if i+1 >= len(fields) {
					return fmt.Errorf("--review requires value")
				}
				i++
				reviewRef = fields[i]
			default:
				return fmt.Errorf("unknown undo-review arg %q", fields[i])
			}
		}
		return s.clearClaimReviewMarker(claimID, reviewRef, now)
	}
	if len(fields) >= 4 && fields[0] == "compound" && fields[1] == "synthesis" && (fields[2] == "undo-checkpoint" || fields[2] == "undo-reopened") {
		return nil
	}
	return fmt.Errorf("unsupported undo command %q", command)
}

func (s *Store) clearClaimReviewMarker(claimID, reviewRef string, now time.Time) error {
	claimID = strings.TrimSpace(claimID)
	reviewRef = strings.TrimSpace(reviewRef)
	if claimID == "" || reviewRef == "" {
		return fmt.Errorf("claim and review ref are required")
	}
	return s.withLock(func() error {
		seq, err := s.nextSeq()
		if err != nil {
			return err
		}
		edge := Edge{
			Seq:          seq,
			Kind:         EdgeKindReviewCleared,
			From:         reviewRef,
			To:           claimID,
			Confidence:   1,
			EvidenceRefs: []string{reviewRef},
			CreatedAt:    rfc3339(now),
		}
		return appendJSONL(s.edgesLog(), &edge)
	})
}

func reconcileUndoReviewCommand(claimID, reviewRef string) string {
	return "compound synthesis undo-review --claim " + claimID + " --review " + reviewRef
}

func reconcileUndoReopenedCommand(reopened []SynthesisClaimReview) string {
	pairs := make([]string, 0, len(reopened))
	for _, item := range reopened {
		pairs = append(pairs, item.ClaimID+":"+item.ReviewRef)
	}
	sort.Strings(pairs)
	return "compound synthesis undo-reopened --reviews " + strings.Join(pairs, ",")
}

func reopenedDigestRefs(reopened []SynthesisClaimReview) []string {
	refs := make([]string, 0, len(reopened)*2)
	for _, item := range reopened {
		refs = append(refs, "claim:"+item.ClaimID, item.ReviewRef)
	}
	return uniqueStrings(refs)
}

func parkedDigestRefs(reasons []SynthesisReconcileParkedReason) []string {
	var refs []string
	for _, reason := range reasons {
		refs = append(refs, reason.EventIDs...)
	}
	return uniqueStrings(refs)
}

func topReconcileParkedReasons(reasons []SynthesisReconcileParkedReason, limit int) string {
	if len(reasons) == 0 || limit <= 0 {
		return "none"
	}
	copyReasons := append([]SynthesisReconcileParkedReason(nil), reasons...)
	sort.Slice(copyReasons, func(i, j int) bool {
		if copyReasons[i].Count == copyReasons[j].Count {
			return copyReasons[i].Reason < copyReasons[j].Reason
		}
		return copyReasons[i].Count > copyReasons[j].Count
	})
	if len(copyReasons) > limit {
		copyReasons = copyReasons[:limit]
	}
	parts := make([]string, 0, len(copyReasons))
	for _, reason := range copyReasons {
		parts = append(parts, reason.Reason+"="+strconv.Itoa(reason.Count))
	}
	return strings.Join(parts, ", ")
}

func claimFoldUseCounts(positions []Position) map[string]int {
	counts := map[string]int{}
	for _, position := range positions {
		seen := map[string]bool{}
		for _, ref := range position.SupportingClaimRefs {
			seen[ref] = true
		}
		for _, ref := range position.ClaimChain {
			seen[ref] = true
		}
		for ref := range seen {
			counts[ref]++
		}
	}
	return counts
}

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return rfc3339(t)
}

func reviewPlain(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), ":", " ")
}
