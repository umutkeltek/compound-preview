package decisions

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"compound/internal/mem"
	"compound/internal/policy"
)

const RefPrefix = "decision://"

// ActiveRecord is the exact read model served for one active decision.
type ActiveRecord struct {
	Ref       string    `json:"ref"`
	Claim     mem.Claim `json:"claim"`
	Lifecycle Lifecycle `json:"lifecycle"`
}

// LoadActive reads and resolves one active decision from the canonical memory store.
func LoadActive(store *mem.Store, query string) (ActiveRecord, error) {
	claims, err := store.AllCurrent()
	if err != nil {
		return ActiveRecord{}, err
	}
	claim, ref, ok := ResolveActive(claims, query)
	if !ok {
		return ActiveRecord{}, fmt.Errorf("active decision %q not found", query)
	}
	return ActiveRecord{Ref: ref, Claim: claim, Lifecycle: LifecycleFromClaim(claim)}, nil
}

// Render writes the stable JSON or Markdown representation of one exact decision.
func (record ActiveRecord) Render(w io.Writer, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(record)
	}
	var body strings.Builder
	fmt.Fprintln(&body, "# decision")
	fmt.Fprintln(&body)
	fmt.Fprintf(&body, "- ref: %s\n", record.Ref)
	fmt.Fprintf(&body, "- claim_id: %s\n", record.Claim.ID)
	fmt.Fprintf(&body, "- kind: %s\n", record.Lifecycle.Kind)
	fmt.Fprintf(&body, "- status: %s\n", record.Lifecycle.Status)
	if record.Lifecycle.Scope != "" {
		fmt.Fprintf(&body, "- scope: %s\n", record.Lifecycle.Scope)
	}
	if record.Lifecycle.Owner != "" {
		fmt.Fprintf(&body, "- owner: %s\n", record.Lifecycle.Owner)
	}
	fmt.Fprintf(&body, "- choice: %s\n", strings.Join(strings.Fields(record.Claim.Object), " "))
	if record.Lifecycle.Falsifier != "" {
		fmt.Fprintf(&body, "- falsifier: %s\n", strings.Join(strings.Fields(record.Lifecycle.Falsifier), " "))
	}
	if record.Lifecycle.ReviewTrigger != "" {
		fmt.Fprintf(&body, "- review_trigger: %s\n", strings.Join(strings.Fields(record.Lifecycle.ReviewTrigger), " "))
	}
	_, err := io.WriteString(w, body.String())
	return err
}

// CanonicalRef is the one served identity for a decision claim. Imported decisions
// keep their durable decision_id; legacy decision-shaped claims fall back to the
// claim id so every emitted ref still resolves exactly.
func CanonicalRef(claim mem.Claim) string {
	id := ""
	if claim.Qualifiers != nil {
		id = strings.TrimSpace(claim.Qualifiers["decision_id"])
	}
	if id == "" {
		id = strings.TrimSpace(claim.ID)
	}
	return RefPrefix + id
}

// ActiveByRef projects accepted, non-superseded decisions onto CanonicalRef and
// collapses duplicate imports to the newest claim revision.
func ActiveByRef(claims []mem.Claim) map[string]mem.Claim {
	byRef := map[string]mem.Claim{}
	for _, claim := range AllActiveDecisions(claims) {
		ref := CanonicalRef(claim)
		if prior, ok := byRef[ref]; !ok || claim.Seq > prior.Seq || (claim.Seq == prior.Seq && claim.ID > prior.ID) {
			byRef[ref] = claim
		}
	}
	return byRef
}

// ResolveActive resolves exactly one canonical ref, decision id, or claim id from
// the active decision projection. It never falls back to a full inventory search.
func ResolveActive(claims []mem.Claim, query string) (mem.Claim, string, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return mem.Claim{}, "", false
	}
	byRef := ActiveByRef(claims)
	ref := query
	if !strings.HasPrefix(ref, RefPrefix) {
		ref = RefPrefix + ref
	}
	if claim, ok := byRef[ref]; ok {
		return claim, ref, true
	}
	for candidateRef, claim := range byRef {
		if query == claim.ID {
			return claim, candidateRef, true
		}
	}
	return mem.Claim{}, "", false
}

// AllActiveDecisions returns accepted decision claims that are NOT superseded
// by any other current decision record's supersedes qualifier.
//
// This is the ONE read path every grant/freeze consumer uses (director's
// apply-grant resolution, work's freeze admission guard): it reads ONLY
// decision records folded through mem.Store — never markdown, worklog, or
// brain prose (grants-as-gates 2026-07-10, falsifier e). Undo is modeled as
// a superseding record (falsifier c): a NEW accepted record whose
// `supersedes` qualifier names the old decision id flips this projection's
// answer without deleting the old claim — it stays in AllCurrent() history,
// just excluded from this ACTIVE projection.
func AllActiveDecisions(claims []mem.Claim) []mem.Claim {
	superseded := map[string]bool{}
	for _, c := range claims {
		if c.ClaimType != "decision" {
			continue
		}
		lifecycle := LifecycleFromClaim(c)
		if lifecycle.Status != StatusAccepted {
			continue
		}
		for _, id := range lifecycle.Supersedes {
			if id != "" {
				superseded[id] = true
			}
		}
	}
	var out []mem.Claim
	for _, c := range claims {
		if c.ClaimType != "decision" {
			continue
		}
		l := LifecycleFromClaim(c)
		if l.Status != StatusAccepted {
			continue
		}
		if l.ID != "" && superseded[l.ID] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ActiveDecisions narrows the canonical active projection to one operational
// kind (for example KindGrant or KindFreeze).
func ActiveDecisions(claims []mem.Claim, kind string) []mem.Claim {
	var out []mem.Claim
	for _, c := range AllActiveDecisions(claims) {
		if LifecycleFromClaim(c).Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// AuthoritativeActiveDecisions is the stricter projection for decisions that
// can release autonomous work. It admits only claims minted by the dedicated
// policy-decision import door and lets only equally authoritative claims
// supersede them. Generic recalled meaning remains evidence even if it copies
// every visible lifecycle qualifier.
func AuthoritativeActiveDecisions(claims []mem.Claim, kind string) []mem.Claim {
	var authoritative []mem.Claim
	for _, c := range claims {
		if c.ClaimType == "decision" && mem.HasProtectedOperationalDecisionAuthority(c) && policy.HasOperatorProvenance(c.Qualifiers["reviewed_by"]) {
			authoritative = append(authoritative, c)
		}
	}
	var out []mem.Claim
	for _, c := range AllActiveDecisions(authoritative) {
		if LifecycleFromClaim(c).Kind == kind {
			out = append(out, c)
		}
	}
	return out
}
