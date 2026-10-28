// Package harvest turns operator utterances spoken in ANY session into
// decision-record candidates through the EXISTING machinery. It builds NO new
// extraction (compound.session_distill_source is binding): it consumes the
// hook transcript reader for attributed operator utterances, classifies the
// ruling/grant/freeze-shaped ones deterministically (no LLM in the gate path),
// and reconciles each against the current accepted decision records using the
// mem cluster machinery —
//
//   - restatement of an accepted record  -> corroboration evidence appended (no
//     card, no duplicate);
//   - reversal of an accepted record      -> a loud conflict candidate (needs-you);
//   - novel ruling                        -> a proposed-only decision candidate
//     through the decision-candidate door, carrying the verbatim quote + session
//     provenance.
//
// Nothing here ever auto-accepts a grant/freeze/ruling: a novel candidate lands
// with verdict=park and only the operator ratification path (`compound decisions
// candidate-plan` -> a decision source -> import) can make it accepted
// (ruling-harvest falsifier e).
package harvest

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/decisions"
	"compound/internal/hook"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

const ReportSchemaVersion = "compound.ruling_harvest_report.v0.1"

// Options controls one harvest run over a single transcript.
type Options struct {
	TranscriptPath string
	DryRun         bool
	Now            time.Time
}

// Item is one classified utterance's reconciled outcome. In dry-run mode the
// side-effect ids (EvidenceID/CandidateID) are left empty because nothing was
// written; the Outcome/Quote/Match still describe what WOULD happen.
type Item struct {
	Outcome       Outcome  `json:"outcome"`
	Kind          Kind     `json:"kind"`
	Quote         string   `json:"quote"`
	Restatement   string   `json:"restatement"`
	LineNo        int      `json:"line_no"`
	Timestamp     string   `json:"timestamp,omitempty"`
	Match         *Match   `json:"match,omitempty"`
	EvidenceID    string   `json:"evidence_id,omitempty"`
	CandidateID   string   `json:"candidate_id,omitempty"`
	Signals       []string `json:"signals,omitempty"`
	NeedsOperator bool     `json:"needs_operator,omitempty"`
}

// Report is what `compound sessions harvest` returns: per-utterance outcomes
// plus the counts a meter reads (corroborations = re-litigation avoided; novel =
// genuinely new law to ratify; conflicts = loud contradictions).
type Report struct {
	SchemaVersion  string   `json:"schema_version"`
	Status         string   `json:"status"`
	GeneratedAt    string   `json:"generated_at"`
	TranscriptPath string   `json:"transcript_path"`
	SessionID      string   `json:"session_id"`
	DryRun         bool     `json:"dry_run"`
	Utterances     int      `json:"utterances"`
	Classified     int      `json:"classified"`
	Corroborations int      `json:"corroborations"`
	Conflicts      int      `json:"conflicts"`
	Novel          int      `json:"novel"`
	Items          []Item   `json:"items"`
	ProofRequired  []string `json:"proof_required"`
}

// decisionCandidate is the harvester's projection of a novel/conflict ruling onto
// the decision-candidate inbox record. Its json shape is the cross-package
// contract read by internal/decisions.readDecisionCandidate /
// BuildDecisionCandidatePlan — it MUST carry kind="decision" and verdict="park"
// so the candidate stays proposed-only (falsifier e).
type decisionCandidate struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	Entity       string            `json:"entity"`
	Predicate    string            `json:"predicate"`
	Object       string            `json:"object"`
	Subject      string            `json:"subject"`
	Statement    string            `json:"statement"`
	Detail       string            `json:"detail"`
	Confidence   float64           `json:"confidence"`
	Qualifiers   map[string]string `json:"qualifiers"`
	EvidenceRefs []string          `json:"evidence_refs"`
	Verdict      string            `json:"verdict"`
}

// Run executes one harvest over opts.TranscriptPath against the store's home.
// A nil store is only valid with DryRun (classification/reconciliation still run,
// but there are no accepted records to reconcile against and nothing is written).
func Run(store *mem.Store, homePath string, opts Options) (Report, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	report := Report{
		SchemaVersion:  ReportSchemaVersion,
		Status:         "ok",
		GeneratedAt:    now.UTC().Format(time.RFC3339),
		TranscriptPath: opts.TranscriptPath,
		SessionID:      sessionIDFromPath(opts.TranscriptPath),
		DryRun:         opts.DryRun,
		ProofRequired: []string{
			"restatements of accepted records append corroboration evidence, never a duplicate record",
			"novel rulings land as verdict=park decision candidates; only operator ratification can accept them",
			"no grant/freeze/ruling is ever auto-accepted by the harvester",
		},
	}
	if strings.TrimSpace(opts.TranscriptPath) == "" {
		return report, fmt.Errorf("harvest: transcript path required")
	}

	utterances, err := hook.OperatorUtterances(opts.TranscriptPath)
	if err != nil {
		return report, fmt.Errorf("harvest: read transcript: %w", err)
	}
	report.Utterances = len(utterances)

	var active []mem.Claim
	if store != nil {
		claims, err := store.AllCurrent()
		if err != nil {
			return report, fmt.Errorf("harvest: load current claims: %w", err)
		}
		active = decisions.AllActiveDecisions(claims)
	}

	for _, u := range utterances {
		cls, ok := Classify(u.Text)
		if !ok {
			continue
		}
		report.Classified++
		outcome, match := Reconcile(u.Text, cls.Kind, active)
		item := Item{
			Outcome:     outcome,
			Kind:        cls.Kind,
			Quote:       u.Text,
			Restatement: restate(cls.Kind, u.Text),
			LineNo:      u.LineNo,
			Timestamp:   u.Timestamp,
			Signals:     cls.Signals,
		}
		if match.ClaimID != "" {
			m := match
			item.Match = &m
		}
		switch outcome {
		case OutcomeCorroboration:
			report.Corroborations++
			if !opts.DryRun && store != nil {
				evID, err := applyCorroboration(store, match.ClaimID, u, report.SessionID, opts.TranscriptPath, now)
				if err != nil {
					return report, err
				}
				item.EvidenceID = evID
			}
		case OutcomeConflict:
			report.Conflicts++
			item.NeedsOperator = true
			if !opts.DryRun && store != nil {
				candID, err := applyDecisionCandidate(store, homePath, cls, outcome, match, u, report.SessionID, opts.TranscriptPath, now)
				if err != nil {
					return report, err
				}
				item.CandidateID = candID
			}
		default: // novel
			report.Novel++
			if !opts.DryRun && store != nil {
				candID, err := applyDecisionCandidate(store, homePath, cls, outcome, match, u, report.SessionID, opts.TranscriptPath, now)
				if err != nil {
					return report, err
				}
				item.CandidateID = candID
			}
		}
		report.Items = append(report.Items, item)
	}
	return report, nil
}

func applyCorroboration(store *mem.Store, claimID string, u hook.OperatorUtterance, sessionID, transcript string, now time.Time) (string, error) {
	ev := mem.EvidenceItem{
		SourceKind: "session-transcript",
		SourceID:   evidenceSourceID(transcript, u.LineNo),
		SessionID:  sessionID,
		QuoteText:  u.Text,
		EventTime:  strings.TrimSpace(u.Timestamp),
		Actor:      "user",
		Visibility: "operational",
	}
	recorded, _, err := store.CorroborateClaim(claimID, ev, 1.0, now)
	if err != nil {
		return "", fmt.Errorf("harvest: corroborate %s: %w", claimID, err)
	}
	return recorded.ID, nil
}

func applyDecisionCandidate(store *mem.Store, homePath string, cls Classification, outcome Outcome, match Match, u hook.OperatorUtterance, sessionID, transcript string, now time.Time) (string, error) {
	if strings.TrimSpace(homePath) == "" {
		return "", fmt.Errorf("harvest: COMPOUND_HOME required to file a decision candidate")
	}
	// Record the verbatim utterance as citable evidence first, so the candidate
	// carries a real evidence ref (not just a synthetic string).
	ev := mem.EvidenceItem{
		SourceKind: "session-transcript",
		SourceID:   evidenceSourceID(transcript, u.LineNo),
		SessionID:  sessionID,
		QuoteText:  u.Text,
		EventTime:  strings.TrimSpace(u.Timestamp),
		Actor:      "user",
		Visibility: "operational",
	}
	recorded, err := store.RecordEvidence(ev, now)
	if err != nil {
		return "", fmt.Errorf("harvest: record evidence: %w", err)
	}

	id := candidateID(cls.Kind, u.Text, sessionID)
	decisionID := "session-" + string(cls.Kind) + "-" + reviewartifact.Digest12(u.Text+"|"+sessionID)
	q := map[string]string{
		"kind":             string(cls.Kind),
		"source":           "sessions_harvest",
		"lifecycle_status": "proposed",
		"session":          sessionID,
		"transcript":       transcript,
		"line":             fmt.Sprintf("%d", u.LineNo),
		"verbatim":         clip(u.Text, 400),
		"decision_id":      decisionID,
		"harvest_outcome":  string(outcome),
	}
	if outcome == OutcomeConflict && match.ClaimID != "" {
		q["conflicts_with_claim"] = match.ClaimID
		q["conflicts_with_decision"] = match.DecisionID
		q["needs_operator"] = "true"
	}
	rec := decisionCandidate{
		ID:           id,
		Kind:         string(candidates.Decision),
		Entity:       "decision:" + decisionID,
		Predicate:    "decided",
		Object:       clip(rulingTitle(u.Text), 200),
		Subject:      restate(cls.Kind, u.Text),
		Statement:    restate(cls.Kind, u.Text),
		Detail:       u.Text,
		Confidence:   0.6,
		Qualifiers:   q,
		EvidenceRefs: []string{recorded.ID, evidenceSourceID(transcript, u.LineNo)},
		Verdict:      candidates.Park,
	}
	if err := candidates.WriteCandidate(homePath, candidates.Decision, id, &rec); err != nil {
		return "", fmt.Errorf("harvest: write decision candidate: %w", err)
	}
	return id, nil
}

func candidateID(kind Kind, text, sessionID string) string {
	return "harvest-" + string(kind) + "-" + reviewartifact.Digest12(text+"|"+sessionID)
}

func evidenceSourceID(transcript string, line int) string {
	return fmt.Sprintf("transcript:%s#L%d", transcript, line)
}

func sessionIDFromPath(path string) string {
	base := filepath.Base(strings.TrimSpace(path))
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// restate produces a short plain-language restatement of the ruling. Deterministic
// (no LLM): it prefixes the kind and clips the operator's own words.
func restate(kind Kind, text string) string {
	label := map[Kind]string{KindRuling: "Ruling", KindGrant: "Grant", KindFreeze: "Freeze"}[kind]
	if label == "" {
		label = "Ruling"
	}
	return label + ": " + clip(rulingTitle(text), 160)
}

// rulingTitle takes the first sentence-ish span of the utterance as the record
// title/choice, so the candidate reads as one decision rather than a paragraph.
func rulingTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	for _, sep := range []string{". ", "; ", " — ", " - "} {
		if i := strings.Index(text, sep); i > 20 {
			return strings.TrimSpace(text[:i])
		}
	}
	return text
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	if n <= 3 {
		return string(r[:n])
	}
	return strings.TrimSpace(string(r[:n-3])) + "..."
}
