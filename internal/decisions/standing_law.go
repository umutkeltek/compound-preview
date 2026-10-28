package decisions

// Standing-law view — the operator-facing twin of what orient serves agents.
//
// The problem (ruling-harvest packet, 2026-07-11): "did I already rule this?"
// meant grepping worklogs, so settled law got re-litigated every session. Agents
// get standing law through orient; the operator was the only participant without
// a read. This is the read-only projection behind `compound decisions
// standing-law --json` and the console's /standing-law page.
//
// It reads ONLY decision records folded through mem.Store (never markdown,
// worklog, or brain prose), exactly like the grant/freeze admission guards:
//   - accepted, non-superseded records via AllActiveDecisions (the live law);
//   - each active record's superseded ancestors resolved from AllCurrent so a
//     superseded record renders struck-through UNDER its successor, never as
//     live law (falsifier c parity: undo is a superseding record, not a delete);
//   - corroboration evidence via mem.Store.CorroborationEvidence (the restatement
//     count — how much re-litigation the standing-law view is meant to retire);
//   - parked harvest candidates with harvest_outcome=conflict as a loud
//     contradiction strip (the "needs-you" surface the packet's design point 3
//     wants at the top).
//
// This is a pure READ: it mutates nothing, files no candidate, records no
// decision. Ratification stays the operator-provenance path (decisions
// candidate-plan -> source -> import); this view never accepts anything.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

const StandingLawSchemaVersion = "compound.standing_law_view.v0.1"

// HarvestConflictOutcome mirrors internal/harvest.OutcomeConflict's string
// value. It is duplicated (not imported) because harvest imports decisions —
// importing harvest here would be a cycle. TestHarvestConflictOutcomeMatches in
// the harvest package pins the two together so a rename cannot silently drop the
// contradiction strip.
const HarvestConflictOutcome = "conflict"

// StandingLawSupersededRecord is one dimmed ancestor shown under a live record.
type StandingLawSupersededRecord struct {
	DecisionID string `json:"decision_id"`
	ClaimID    string `json:"claim_id,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Title      string `json:"title,omitempty"`
	Date       string `json:"date,omitempty"`
}

// StandingLawRecord is one live ruling/grant/freeze in the standing-law view.
type StandingLawRecord struct {
	ClaimID               string                        `json:"claim_id"`
	DecisionID            string                        `json:"decision_id"`
	Kind                  string                        `json:"kind"`
	Title                 string                        `json:"title"`
	Scope                 string                        `json:"scope,omitempty"`
	Owner                 string                        `json:"owner,omitempty"`
	ReviewedBy            string                        `json:"reviewed_by,omitempty"`
	Date                  string                        `json:"date"`
	Falsifier             string                        `json:"falsifier,omitempty"`
	ReviewTrigger         string                        `json:"review_trigger,omitempty"`
	Domains               []string                      `json:"domains,omitempty"`
	Keywords              []string                      `json:"keywords,omitempty"`
	SourcePath            string                        `json:"source_path,omitempty"`
	CorroborationCount    int                           `json:"corroboration_count"`
	CorroborationEvidence []string                      `json:"corroboration_evidence,omitempty"`
	Supersedes            []StandingLawSupersededRecord `json:"supersedes,omitempty"`
}

// StandingLawConflict is one parked harvest candidate that reverses accepted
// law — rendered as the loud contradiction card linking the conflicting record.
type StandingLawConflict struct {
	CandidateID           string `json:"candidate_id"`
	Kind                  string `json:"kind,omitempty"`
	Restatement           string `json:"restatement,omitempty"`
	Verbatim              string `json:"verbatim,omitempty"`
	Session               string `json:"session,omitempty"`
	Transcript            string `json:"transcript,omitempty"`
	Line                  string `json:"line,omitempty"`
	ConflictsWithClaim    string `json:"conflicts_with_claim,omitempty"`
	ConflictsWithDecision string `json:"conflicts_with_decision,omitempty"`
	ConflictingTitle      string `json:"conflicting_title,omitempty"`
}

// StandingLawCounts is the meter strip the packet's design point 5 wants: how
// much law stands, and how loud the contradiction surface is right now.
type StandingLawCounts struct {
	Records    int `json:"records"`
	Rulings    int `json:"rulings"`
	Grants     int `json:"grants"`
	Freezes    int `json:"freezes"`
	Superseded int `json:"superseded"`
	Conflicts  int `json:"conflicts"`
}

// StandingLawView is the whole read-only projection.
type StandingLawView struct {
	SchemaVersion string                `json:"schema_version"`
	GeneratedAt   string                `json:"generated_at"`
	Records       []StandingLawRecord   `json:"records"`
	Conflicts     []StandingLawConflict `json:"conflicts"`
	Warnings      []string              `json:"warnings,omitempty"`
	Counts        StandingLawCounts     `json:"counts"`
}

// StandingLaw builds the read-only standing-law view from the store's current
// claims plus the parked decision-candidate inbox. A nil store returns an empty
// view (never an error): a home with no law is a clean seam, not a failure.
func StandingLaw(store *mem.Store, homePath string, now time.Time) (StandingLawView, error) {
	if now.IsZero() {
		now = time.Now()
	}
	view := StandingLawView{
		SchemaVersion: StandingLawSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Records:       []StandingLawRecord{},
		Conflicts:     []StandingLawConflict{},
	}
	if store == nil {
		return view, nil
	}

	claims, err := store.AllCurrent()
	if err != nil {
		return view, err
	}

	// Index every decision claim by its decision_id so a successor can resolve
	// the ancestors named in its `supersedes` qualifier — including ones whose
	// own lifecycle_status is still "accepted" but which AllActiveDecisions
	// excludes because a live record supersedes them.
	claimByDecisionID := map[string]mem.Claim{}
	titleByClaimID := map[string]string{}
	for _, c := range claims {
		if c.ClaimType != "decision" {
			continue
		}
		titleByClaimID[c.ID] = c.Object
		if id := LifecycleFromClaim(c).ID; id != "" {
			claimByDecisionID[id] = c
		}
	}

	// Evidence id -> source id, resolved once (never per-record) so the
	// disclosure can show the source path of a `decisions record` markdown.
	evidenceSource := map[string]string{}
	if evidence, err := store.Evidence(); err == nil {
		for _, e := range evidence {
			evidenceSource[e.ID] = e.SourceID
		}
	}

	active := AllActiveDecisions(claims)
	for _, c := range active {
		l := LifecycleFromClaim(c)
		refs, _ := store.CorroborationEvidence(c.ID)
		rec := StandingLawRecord{
			ClaimID:               c.ID,
			DecisionID:            l.ID,
			Kind:                  l.Kind,
			Title:                 strings.TrimSpace(c.Object),
			Scope:                 l.Scope,
			Owner:                 l.Owner,
			ReviewedBy:            l.ReviewedBy,
			Date:                  recordDate(c),
			Falsifier:             l.Falsifier,
			ReviewTrigger:         l.ReviewTrigger,
			Domains:               splitCSV(c.Qualifiers["domain"]),
			Keywords:              standingLawKeywords(l),
			SourcePath:            resolveSourcePath(c, evidenceSource),
			CorroborationCount:    len(refs),
			CorroborationEvidence: refs,
			Supersedes:            resolveSuperseded(l.Supersedes, claimByDecisionID),
		}
		view.Records = append(view.Records, rec)
	}

	// Newest first (design point 1). ValidFrom is the observation date; ties
	// break on claim id so the order is deterministic for the smoke.
	sort.SliceStable(view.Records, func(i, j int) bool {
		if view.Records[i].Date != view.Records[j].Date {
			return view.Records[i].Date > view.Records[j].Date
		}
		return view.Records[i].ClaimID > view.Records[j].ClaimID
	})

	view.Conflicts, view.Warnings = loadConflictCandidates(homePath, titleByClaimID)
	view.Counts = countStandingLaw(view)
	return view, nil
}

// standingLawKeywords is the searchable-facet bag a freeze/grant record carries
// beyond its title/scope: literal owner globs + predicate keywords + class.
func standingLawKeywords(l Lifecycle) []string {
	out := append([]string(nil), l.FreezeKeywords...)
	out = append(out, l.FreezeOwnerGlobs...)
	if l.AutoAdmitClass != "" {
		out = append(out, l.AutoAdmitClass)
	}
	return out
}

func resolveSuperseded(ids []string, byDecisionID map[string]mem.Claim) []StandingLawSupersededRecord {
	var out []StandingLawSupersededRecord
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		entry := StandingLawSupersededRecord{DecisionID: id}
		if c, ok := byDecisionID[id]; ok {
			l := LifecycleFromClaim(c)
			entry.ClaimID = c.ID
			entry.Kind = l.Kind
			entry.Title = strings.TrimSpace(c.Object)
			entry.Date = recordDate(c)
		}
		out = append(out, entry)
	}
	return out
}

func resolveSourcePath(c mem.Claim, evidenceSource map[string]string) string {
	for _, ref := range c.EvidenceRefs {
		src := evidenceSource[ref]
		if strings.HasPrefix(src, "policy-decision:") {
			return strings.TrimPrefix(src, "policy-decision:")
		}
	}
	// Fall back to the generic source qualifier (e.g. "decisions-repo") so the
	// disclosure never renders a blank provenance line.
	return strings.TrimSpace(c.Qualifiers["source"])
}

// recordDate is the record's observation date (immutable), used for newest-first
// ordering and the row's date badge. Falls back to created_at.
func recordDate(c mem.Claim) string {
	if d := strings.TrimSpace(c.ValidFrom); d != "" {
		return d
	}
	return strings.TrimSpace(c.CreatedAt)
}

// conflictCandidate is the read projection of a harvested decision candidate
// (the write side lives in internal/harvest). Only the fields the contradiction
// card needs are decoded.
type conflictCandidate struct {
	ID         string            `json:"id"`
	Subject    string            `json:"subject"`
	Statement  string            `json:"statement"`
	Qualifiers map[string]string `json:"qualifiers"`
}

func loadConflictCandidates(homePath string, titleByClaimID map[string]string) ([]StandingLawConflict, []string) {
	out := []StandingLawConflict{}
	if strings.TrimSpace(homePath) == "" {
		return out, nil
	}
	page, err := candidates.ReadPage(homePath, candidates.Decision, "", 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return out, []string{"decision candidate projection unavailable: " + err.Error()}
	}
	var warnings []string
	for _, entry := range page.Entries {
		if entry.Corruption != nil {
			warnings = append(warnings, entry.Corruption.Error())
			continue
		}
		var cand conflictCandidate
		if err := json.Unmarshal(entry.Record.Body, &cand); err != nil {
			warnings = append(warnings, "candidate entry "+string(entry.EntryCursor)+" is corrupt: invalid_json ("+err.Error()+")")
			continue
		}
		q := cand.Qualifiers
		if q == nil || q["harvest_outcome"] != HarvestConflictOutcome {
			continue
		}
		conflictClaim := q["conflicts_with_claim"]
		restatement := strings.TrimSpace(cand.Subject)
		if restatement == "" {
			restatement = strings.TrimSpace(cand.Statement)
		}
		out = append(out, StandingLawConflict{
			CandidateID:           entry.Record.ID,
			Kind:                  q["kind"],
			Restatement:           restatement,
			Verbatim:              strings.TrimSpace(q["verbatim"]),
			Session:               q["session"],
			Transcript:            q["transcript"],
			Line:                  q["line"],
			ConflictsWithClaim:    conflictClaim,
			ConflictsWithDecision: q["conflicts_with_decision"],
			ConflictingTitle:      titleByClaimID[conflictClaim],
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CandidateID < out[j].CandidateID })
	return out, warnings
}

func countStandingLaw(view StandingLawView) StandingLawCounts {
	counts := StandingLawCounts{
		Records:   len(view.Records),
		Conflicts: len(view.Conflicts),
	}
	for _, r := range view.Records {
		switch r.Kind {
		case KindRuling:
			counts.Rulings++
		case KindGrant:
			counts.Grants++
		case KindFreeze:
			counts.Freezes++
		}
		counts.Superseded += len(r.Supersedes)
	}
	return counts
}
