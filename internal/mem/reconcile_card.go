package mem

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"compound/internal/reviewartifact"
)

const ReconcileContestedArea = "reconcile-contested"
const reconcileContestedArea = ReconcileContestedArea

const reconcileContestedArtifactSchema = "compound.synthesis_reconcile_contested.v0.1"

// SynthesisReconcileContestedCard is the deterministic operator-facing review
// surface for a split current position. It asks; it never promotes or supersedes.
type SynthesisReconcileContestedCard struct {
	ID         string                             `json:"id"`
	ClusterKey string                             `json:"cluster_key"`
	Entity     string                             `json:"entity"`
	Topic      string                             `json:"topic"`
	CreatedAt  string                             `json:"created_at"`
	Claims     []SynthesisReconcileContestedClaim `json:"claims"`
	ReviewRef  string                             `json:"review_ref"`
}

type SynthesisReconcileContestedClaim struct {
	ClaimID string `json:"claim_id"`
	Object  string `json:"object"`
	Source  string `json:"source"`
	Date    string `json:"date"`
}

type reconcileContestedArtifact struct {
	SchemaVersion string                          `json:"schema_version"`
	ID            string                          `json:"id"`
	CreatedAt     string                          `json:"created_at"`
	Status        string                          `json:"status"`
	Readiness     string                          `json:"readiness"`
	Owner         string                          `json:"owner,omitempty"`
	SourceKind    string                          `json:"source_kind,omitempty"`
	NextAction    string                          `json:"next_action,omitempty"`
	NotToDo       string                          `json:"not_to_do,omitempty"`
	Boundaries    []string                        `json:"boundaries,omitempty"`
	EvidenceRefs  []string                        `json:"evidence_refs,omitempty"`
	Card          SynthesisReconcileContestedCard `json:"card"`
}

func (a reconcileContestedArtifact) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return a.Card.ReviewOperatorCard()
}

func (c SynthesisReconcileContestedCard) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	if len(c.Claims) == 0 {
		return reviewartifact.OperatorCard{}, fmt.Errorf("contested card has no claims")
	}
	memory := c.Claims[0]
	evidence := c.Claims[len(c.Claims)-1]
	if len(c.Claims) > 1 {
		for _, claim := range c.Claims {
			if claim.ClaimID != memory.ClaimID {
				evidence = claim
				break
			}
		}
	}
	summary := fmt.Sprintf("In %s, in %s, memory holds %s (%s, %s) but evidence shows %s (%s, %s) \u2014 representation correct?",
		reviewartifact.PlainLabel(c.Entity), reviewartifact.PlainLabel(c.Topic),
		reviewartifact.PlainLabel(memory.Object), memory.Source, memory.Date,
		reviewartifact.PlainLabel(evidence.Object), evidence.Source, evidence.Date)
	return reviewartifact.NewOperatorCard(summary, reviewartifact.PlainLabel(c.Entity), "reconcile-contested|"+reviewartifact.PlainLabel(c.Entity)+"|"+reviewartifact.PlainLabel(c.Topic), "memory carries one current representation")
}

type reconcileContestedGroups struct {
	byCluster map[string]*reconcileContestedGroup
}

type reconcileContestedGroup struct {
	clusterKey string
	evidenceID string
	claims     map[string]Claim
}

func newReconcileContestedGroups() *reconcileContestedGroups {
	return &reconcileContestedGroups{byCluster: map[string]*reconcileContestedGroup{}}
}

func (g *reconcileContestedGroups) add(proposal SynthesisContradictionProposal, claims map[string]Claim, evidenceID string) {
	if proposal.ClusterKey == "" {
		return
	}
	group := g.byCluster[proposal.ClusterKey]
	if group == nil {
		group = &reconcileContestedGroup{clusterKey: proposal.ClusterKey, evidenceID: evidenceID, claims: map[string]Claim{}}
		g.byCluster[proposal.ClusterKey] = group
	}
	if group.evidenceID == "" {
		group.evidenceID = evidenceID
	}
	for _, claimID := range proposal.ClaimIDs {
		if claim, ok := claims[claimID]; ok {
			group.claims[claim.ID] = claim
		}
	}
}

func (g *reconcileContestedGroups) len() int {
	if g == nil {
		return 0
	}
	return len(g.byCluster)
}

func canOpenContestedReconcileCard(evidence Claim, different []Claim, marks map[string]claimReviewMark) bool {
	if hasContestedReviewMark(marks[evidence.ID]) || !CanonClassOperatorPermanent(EffectiveCanonClass(evidence.ClaimType, evidence.CanonClass)) {
		return false
	}
	evidenceSource := reconcileClaimSource(evidence)
	if evidenceSource == "" {
		return false
	}
	for _, target := range different {
		if hasContestedReviewMark(marks[target.ID]) {
			continue
		}
		if !CanonClassOperatorPermanent(EffectiveCanonClass(target.ClaimType, target.CanonClass)) {
			continue
		}
		if targetSource := reconcileClaimSource(target); targetSource != "" && targetSource != evidenceSource {
			return true
		}
	}
	return false
}

func contestedReconcileProposal(clusterKey string, evidence Claim, different []Claim, objects map[string]bool) SynthesisContradictionProposal {
	claimSet := map[string]bool{evidence.ID: true}
	for _, target := range different {
		claimSet[target.ID] = true
	}
	claimIDs := sortedKeysBool(claimSet)
	objectList := sortedKeysBool(objects)
	return SynthesisContradictionProposal{
		ID:         idFor("synth_reconcile_contested_", clusterKey, strings.Join(claimIDs, ","), strings.Join(objectList, ",")),
		ClusterKey: clusterKey,
		ClaimIDs:   claimIDs,
		Objects:    objectList,
	}
}

func (s *Store) writeReconcileContestedCards(groups *reconcileContestedGroups, marks map[string]claimReviewMark, now time.Time) ([]SynthesisReconcileContestedCard, error) {
	if groups == nil || len(groups.byCluster) == 0 {
		return nil, nil
	}
	keys := sortedKeys(groups.byCluster)
	cards := make([]SynthesisReconcileContestedCard, 0, len(keys))
	for _, key := range keys {
		group := groups.byCluster[key]
		card := contestedCardFromGroup(group, now)
		if card.ID == "" || contestedGroupAlreadyMarked(card, marks) {
			continue
		}
		if err := s.markContestedClaims(card, now); err != nil {
			return cards, err
		}
		artifact := reconcileContestedArtifact{
			SchemaVersion: reconcileContestedArtifactSchema,
			ID:            card.ID,
			CreatedAt:     rfc3339(now),
			Status:        "review_required",
			Readiness:     "needs_review",
			Owner:         "Memory synthesis",
			SourceKind:    "memory-reconcile-contested",
			NextAction:    "answer whether the current representation is correct; do not auto-supersede from this card",
			NotToDo:       "do not promote, supersede, or retire memory from contested reconciliation alone",
			Boundaries: []string{
				"card asks only",
				"needs_review markers are append-only",
				"no claim promotion",
				"no claim supersession",
			},
			EvidenceRefs: contestedCardEvidenceRefs(card),
			Card:         card,
		}
		if _, err := reviewartifact.Write(s.home, ReconcileContestedArea, card.ID, artifact, "  ", renderReconcileContestedMarkdown(card)); err != nil {
			return cards, err
		}
		cards = append(cards, card)
	}
	return cards, nil
}

func contestedCardFromGroup(group *reconcileContestedGroup, now time.Time) SynthesisReconcileContestedCard {
	if group == nil || len(group.claims) < 2 {
		return SynthesisReconcileContestedCard{}
	}
	claimIDs := sortedKeys(group.claims)
	claims := make([]Claim, 0, len(claimIDs))
	for _, id := range claimIDs {
		claims = append(claims, group.claims[id])
	}
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].ID == group.evidenceID {
			return false
		}
		if claims[j].ID == group.evidenceID {
			return true
		}
		if claims[i].Seq == claims[j].Seq {
			return claims[i].ID < claims[j].ID
		}
		return claims[i].Seq < claims[j].Seq
	})
	cardClaims := make([]SynthesisReconcileContestedClaim, 0, len(claims))
	objects := make([]string, 0, len(claims))
	for _, claim := range claims {
		objects = append(objects, NormalizeClaimValue(claim.Object))
		cardClaims = append(cardClaims, SynthesisReconcileContestedClaim{
			ClaimID: claim.ID,
			Object:  claim.Object,
			Source:  reconcileClaimSourceLabel(claim),
			Date:    shortReconcileDate(claim.ValidFrom),
		})
	}
	first := claims[0]
	return SynthesisReconcileContestedCard{
		ID:         idFor("synth_reconcile_contested_", group.clusterKey, strings.Join(claimIDs, ","), strings.Join(objects, ",")),
		ClusterKey: group.clusterKey,
		Entity:     first.Entity,
		Topic:      first.Topic(),
		CreatedAt:  rfc3339(now),
		Claims:     cardClaims,
		ReviewRef:  idFor("synth_reconcile_contested_", group.clusterKey, strings.Join(claimIDs, ","), strings.Join(objects, ",")),
	}
}

func contestedGroupAlreadyMarked(card SynthesisReconcileContestedCard, marks map[string]claimReviewMark) bool {
	for _, claim := range card.Claims {
		if hasContestedReviewMark(marks[claim.ClaimID]) {
			return true
		}
	}
	return false
}

func hasContestedReviewMark(mark claimReviewMark) bool {
	for _, ref := range mark.Refs {
		if strings.HasPrefix(ref, "synth_reconcile_contested_") {
			return true
		}
	}
	return false
}

func (s *Store) markContestedClaims(card SynthesisReconcileContestedCard, now time.Time) error {
	return s.withLock(func() error {
		for _, claim := range card.Claims {
			seq, err := s.nextSeq()
			if err != nil {
				return err
			}
			edge := Edge{
				Seq:          seq,
				Kind:         EdgeKindNeedsReview,
				From:         card.ReviewRef,
				To:           claim.ClaimID,
				Confidence:   1,
				EvidenceRefs: []string{card.ReviewRef},
				CreatedAt:    rfc3339(now),
			}
			if err := appendJSONL(s.edgesLog(), &edge); err != nil {
				return err
			}
		}
		return nil
	})
}

func contestedCardEvidenceRefs(card SynthesisReconcileContestedCard) []string {
	refs := make([]string, 0, len(card.Claims)+1)
	refs = append(refs, card.ReviewRef)
	for _, claim := range card.Claims {
		refs = append(refs, "claim:"+claim.ClaimID)
	}
	return uniqueStrings(refs)
}

func renderReconcileContestedMarkdown(card SynthesisReconcileContestedCard) string {
	var b strings.Builder
	operator, _ := card.ReviewOperatorCard()
	b.WriteString("# Reconcile contested position\n\n")
	b.WriteString(operator.Summary)
	b.WriteString("\n\n## Representations\n\n")
	for _, claim := range card.Claims {
		b.WriteString(fmt.Sprintf("- `%s`: %s (%s, %s)\n", claim.ClaimID, claim.Object, claim.Source, claim.Date))
	}
	return b.String()
}

func reconcileClaimSource(claim Claim) string {
	if len(claim.EvidenceRefs) > 0 {
		return strings.TrimSpace(claim.EvidenceRefs[0])
	}
	return strings.TrimSpace(claim.ProvenanceLane)
}

func reconcileClaimSourceLabel(claim Claim) string {
	source := reconcileClaimSource(claim)
	if source == "" {
		source = claim.ID
	}
	return reviewartifact.PlainLabel(source)
}

func shortReconcileDate(raw string) string {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	if i := strings.Index(raw, "T"); i > 0 {
		return raw[:i]
	}
	return strings.TrimSpace(raw)
}
