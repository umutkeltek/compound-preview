// Package operatornotice is the single missing edge between the attention
// hold frontier (internal/attention) and the existing act post buzz transport
// (internal/act). It does not invent a new subsystem, a new scheduler, or a new
// transport — it wires the three existing surfaces together so an attention
// hold that needs a human emits a governed notification through buzz, exactly
// once per hold (idempotent on re-scan), with unnotifiable or transport-
// refused holds surfaced in a doctor count rather than silently swallowed.
//
// The package is the connective tissue promised by work_a4d2491c8358.
package operatornotice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/attention"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/finding"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/product"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

// SchemaVersion identifies the NoticeReport wire shape.
const SchemaVersion = "compound.operatornotice.v0.1"

// Notice outcomes recorded per hold. Only "emitted" occupies the operator
// channel; a refused or unnotifiable ask is nobody's pending decision.
const (
	outcomeEmitted      = "emitted"
	outcomeRefused      = "refused"
	outcomeUnnotifiable = "unnotifiable"
)

// MaxOpenHolds caps how many asks may be unanswered at once.
//
// One, deliberately. The scarce resource is not the operator's inbox, it is
// their decisions: this home has ~19,619 holds raised against 3 approvals, and
// the live frontier carries 18 needs-you groups. Emitting all 18 produces one
// ignored batch, not 18 decisions. Serving strictly one at a time turns the
// frontier into a queue that drains at the rate decisions are actually made.
var MaxOpenHolds = 1

// Channel occupancy is derived from the CURRENT frontier, never from a stored
// index: a hold is open exactly when it is still on the frontier and already
// has an emitted ask record. That needs no extra state and self-corrects — when
// the operator answers, the hold leaves the frontier and stops being counted.
//
// Closure evidence (how LONG an ask waited, and how many were answered) is a
// different question and is deliberately NOT implemented here. It requires
// seeing holds that have LEFT the frontier, which needs either a prefix Scan —
// "opnotice_" is not in eventstore's idPrefixFamilies, and an unmapped prefix
// disables routing and makes the scan parse every segment in the store, the
// exact failure the approval_ mapping exists to prevent — or a declared
// projection path under COMPOUND_HOME, which is another owner's decision.
// Both are real design calls; neither belongs in a change whose point is to
// make one ask decidable. Resolved/latency stay unmeasured and unclaimed
// rather than approximated.

// executeFn is the seam that drives the act.Execute call. Production routes
// to act.Execute directly; tests swap this to assert the once-and-only-once
// promise without standing up a full buzz transport config + standing grant.
// It's a per-package seam patterned after act's own recordEnvelopeFn — no
// new subsystem, no new transport.
var executeFn = func(homePath, transport, body string, workID string, now time.Time) (string, string, error) {
	ai := governedNoticeIntent(homePath, workID, transport, body, now)
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		return "", "", err
	}
	pd := policy.Evaluate(ai, cfg, now)
	if !policy.Allowed(pd) {
		return "", "", fmt.Errorf("policy: %s", pd.Outcome)
	}
	artifactStore, err := act.NewArtifactStore(homePath)
	if err != nil {
		return "", "", err
	}
	res, err := act.Execute(artifactStore, act.NewExecTransportAdapter(transport, homePath), ai, pd, []byte(body), now)
	if err != nil {
		return "", "", err
	}
	return res.Receipt.ID, res.Transition.ID, nil
}

// NoticeRecordSchemaVersion is the on-disk record id the operator notice
// emits through the event store to make re-scans idempotent. The id embeds
// "opnotice_" so it never collides with receipts, transitions, or other
// record kinds.
const NoticeRecordSchemaVersion = "compound.operatornotice_record.v0.1"

// NoticeRecord is the event-store record that proves a hold was notified.
// The HoldID is the content-addressed id derived from the band-needs-you
// group, so a re-scan of the same hold reads the same record and skips.
//
// Outcomes: "emitted" (the act post buzz write succeeded), "refused" (the
// transport refused without a standing grant), "unnotifiable" (no candidate
// work id). Each sets the matching DoctorCount bucket, but every record is
// idempotent on re-scan — the predicate's once-and-only-once falsifier rides
// on this exact field.
//
// The ID field is the canonical eventstore record id (matches the put id).
// HoldID is the same value, surfaced under a domain-meaningful name for
// the operator reading the JSON.
type NoticeRecord struct {
	ID             string   `json:"id"`
	SchemaVersion  string   `json:"schema_version"`
	HoldID         string   `json:"hold_id"`
	GroupKey       string   `json:"group_key"`
	GroupArea      string   `json:"group_area"`
	GroupSource    string   `json:"group_source"`
	WorkID         string   `json:"work_id,omitempty"`
	ItemIDs        []string `json:"item_ids,omitempty"`
	OperatorAsk    string   `json:"operator_ask,omitempty"`
	ApproveCommand string   `json:"approve_command,omitempty"`
	Transport      string   `json:"transport"`
	Outcome        string   `json:"outcome"`
	RefusalReason  string   `json:"refusal_reason,omitempty"`
	ReceiptID      string   `json:"receipt_id,omitempty"`
	TransitionID   string   `json:"transition_id,omitempty"`
	NotifiedAt     string   `json:"notified_at"`
}

// NoticeReport is the public return value of Notify / NotifyFromInputs. All
// counts are integers; the schema is stable and exportable.
type NoticeReport struct {
	SchemaVersion   string `json:"schema_version"`
	GeneratedAt     string `json:"generated_at"`
	Transport       string `json:"transport"`
	Scanned         int    `json:"scanned"`
	AlreadyNotified int    `json:"already_notified"`
	Emitted         int    `json:"emitted"`
	Refused         int    `json:"refused"`
	Unnotifiable    int    `json:"unnotifiable"`
	DoctorCount     int    `json:"doctor_count"`

	// OpenHolds is how many asks on the current frontier are still unanswered
	// — the channel occupancy that MaxOpenHolds caps. It is derived from the
	// frontier, not stored, so it self-corrects when a hold is answered.
	OpenHolds int `json:"open_holds"`
	// Queued counts needs-you holds that were eligible but did not get the
	// channel this flush. They are deliberately left unrecorded so a later
	// flush can still reach them.
	Queued int `json:"queued"`

	// AwayThrough and ReturnQueued are set only inside an away window: the
	// window's end, and how many asks were newly added to the return queue.
	// Both are omitted while present so the report stays byte-identical.
	AwayThrough  string `json:"away_through,omitempty"`
	ReturnQueued int    `json:"return_queued,omitempty"`
}

// Notify is the public entry point: it builds the attention frontier from the
// home and emits governed notifications through the buzz transport. It is the
// operator-facing CLI's call site (compound notice-flush).
//
// The wire shape is identical to NotifyFromInputs — splits only so tests can
// drive the function off a prepared attention.Report without rebuilding the
// event store every call.
func Notify(homePath, transport string, now time.Time) (NoticeReport, error) {
	return NotifyWithReviews(homePath, transport, nil, now)
}

// NotifyWithReviews is Notify with the review queue supplied by the caller.
//
// The reviews report is an ARGUMENT rather than something this package builds,
// on purpose. Assembling it is reviewqueue.Build, which pulls in decisions,
// product, authority, director, runtimebinding and capgraph; importing that
// chain would turn a leaf whose whole job is "wire three existing surfaces
// together" into a package that depends on six organs. The CLI already has the
// assembled report (cmd/compound buildReviewQueueReport) and hands it over.
//
// This matters because it is the difference between the notifier seeing the
// operator's real frontier and seeing almost none of it: attention.Build only
// walks review groups when Reviews is non-nil, so with nil the notifier sees
// workbench and executor groups alone. That is exactly what produced the one
// flush this system has ever run — a single aggregate card reading "33 open
// work items are blocked on operator review", because the workbench group was
// the only thing it could see.
//
// A nil report stays valid: it degrades to the previous behaviour rather than
// failing, so a caller without a review queue still notifies workbench holds.
func NotifyWithReviews(homePath, transport string, reviews *reviewartifact.QueueReport, now time.Time) (NoticeReport, error) {
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return NoticeReport{}, fmt.Errorf("operatornotice: open work: %w", err)
	}
	wb, err := store.Workbench(now, work.WorkbenchOptions{RecentLimit: 8})
	if err != nil {
		return NoticeReport{}, fmt.Errorf("operatornotice: build workbench: %w", err)
	}
	execs, err := store.ExecutorsProjection(now, work.ExecutorsProjectionOptions{Workbench: &wb})
	if err != nil {
		return NoticeReport{}, fmt.Errorf("operatornotice: build executors: %w", err)
	}
	report := attention.Build(attention.Inputs{
		Reviews:   reviews,
		Workbench: &wb,
		Executors: &execs,
	}, now)
	return NotifyFromInputs(homePath, transport, report, now)
}

// NotifyFromInputs is the testable seam: it accepts the prepared attention
// report and emits one governed act post buzz record per *new* hold. Idempotent
// by content-addressed hold id; refused or unnotifiable holds are counted in
// the report and a doctor finding is emitted (never silently swallowed).
func NotifyFromInputs(homePath, transport string, in attention.Report, now time.Time) (NoticeReport, error) {
	homePath = strings.TrimSpace(homePath)
	transport = strings.TrimSpace(transport)
	if homePath == "" {
		return NoticeReport{}, fmt.Errorf("operatornotice: home path required")
	}
	if transport == "" {
		return NoticeReport{}, fmt.Errorf("operatornotice: transport required")
	}
	report := NoticeReport{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Transport:     transport,
	}

	// Open the event store first to verify the home is a real compound home
	// without writing anything. A bare temp dir without a manifest is a
	// typed error — never a silent success.
	if err := requireManifest(homePath); err != nil {
		return NoticeReport{}, fmt.Errorf("operatornotice: %w", err)
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return NoticeReport{}, fmt.Errorf("operatornotice: open event store: %w", err)
	}
	_ = store

	// Consider ONLY the BandNeedsYou groups. Other bands are operator-visible
	// but not blocking — wiring them to the buzz transport would create
	// noise; the predicate says "a hold that needs a human".
	//
	// Decidable groups are served first. When the channel holds one ask at a
	// time, an undecidable hold winning it starves every hold the operator
	// could actually have acted on.
	pending := make([]attention.Group, 0, len(in.Groups))
	for _, group := range in.Groups {
		if group.Band == attention.BandNeedsYou {
			pending = append(pending, group)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		return decideCommand(pending[i]) != "" && decideCommand(pending[j]) == ""
	})

	// Away mode: inside the operator's away window every ask is queued for
	// their return (and its work item parked through the governed hold) before
	// anything is served, so a hold already notified before the window opened
	// is queued too. Outside the window this is a no-op and the flush below is
	// unchanged. A queue that cannot be written fails the flush: an ask the
	// operator will never see is the failure this exists to prevent.
	through := awayThrough(homePath, now)
	if through != "" {
		report.AwayThrough = through
		queued, err := queueAsksForReturn(homePath, pending, through, now)
		if err != nil {
			return NoticeReport{}, fmt.Errorf("operatornotice: return queue: %w", err)
		}
		report.ReturnQueued = queued
	}

	// Pass A: classify against prior records without emitting anything, so
	// channel occupancy is known BEFORE the first emit decision. A one-pass
	// loop would let a hold already occupying the channel be counted after a
	// new emit had already been sent, overshooting the cap.
	type candidate struct {
		group  attention.Group
		holdID string
		workID string
	}
	fresh := make([]candidate, 0, len(pending))
	occupancy := 0
	for _, group := range pending {
		report.Scanned++
		holdID := HoldIDFor(group)
		workID := firstWorkID(group.ItemIDs)

		already, prior, err := findPriorNotification(homePath, holdID)
		if err != nil {
			report.Refused++
			recordDoctorReadFailure(homePath, group, holdID, err, now)
			continue
		}
		if already {
			report.AlreadyNotified++
			// Still on the frontier and already asked = still unanswered, so
			// it holds the channel. Refused and unnotifiable records do not:
			// nothing is waiting on the operator for those.
			if prior != nil && prior.Outcome == outcomeEmitted {
				occupancy++
			}
			continue
		}
		fresh = append(fresh, candidate{group: group, holdID: holdID, workID: workID})
	}

	// Pass B: serve the fresh holds, at most MaxOpenHolds unanswered at once.
	for _, c := range fresh {
		group, holdID, workID := c.group, c.holdID, c.workID

		// Unnotifiable: holds without a candidate work id cannot be
		// expressed as a writable notice. Count it; write the record so
		// re-scans are still idempotent; emit a doctor finding.
		if workID == "" {
			report.Unnotifiable++
			report.DoctorCount++
			recordDoctorUnnotifiable(homePath, group, holdID, now)
			persistNoticeRecord(homePath, holdID, group, workID, transport, "", "", "unnotifiable", "no actionable work id", now)
			continue
		}

		// The channel is full: this hold is eligible but does not get served
		// now. Deliberately write NOTHING — a record here would read as an
		// idempotency hit on the next flush and the hold could never be
		// notified at all. Leaving it unrecorded is what makes it a queue
		// rather than a silent drop.
		if occupancy >= MaxOpenHolds {
			report.Queued++
			continue
		}

		// Build the one-line card body and drive the existing act seam.
		// A PolicyDecision-gate refusal or a transport-exec failure is
		// counted in the doctor; the target stays byte-identical.
		card := cardBodyFor(group, holdID, workID, through)
		receiptID, transitionID, err := executeFn(homePath, transport, card, workID, now)
		if err != nil {
			report.Refused++
			report.DoctorCount++
			recordDoctorRefusal(homePath, group, holdID, err, now)
			persistNoticeRecord(homePath, holdID, group, workID, transport, "", "", "refused", err.Error(), now)
			continue
		}

		// Persist the notice record so the next scan is an idempotent hit.
		// This is the ONLY record NOT written through act.Execute — it is
		// the idempotency witness, not a Receipt/Transition.
		if err := persistNoticeRecord(homePath, holdID, group, workID, transport, receiptID, transitionID, "emitted", "", now); err != nil {
			// The act write succeeded but the idempotency record failed.
			// Count it in the doctor so the operator sees the impression; a
			// re-scan would re-emit (the predicate's exact-once promise is
			// only as strong as the witness).
			report.Refused++
			report.DoctorCount++
			recordDoctorRefusal(homePath, group, holdID, err, now)
			continue
		}
		report.Emitted++
		occupancy++
	}

	report.OpenHolds = occupancy
	report.DoctorCount = report.Refused + report.Unnotifiable
	return report, nil
}

// persistNoticeRecord writes the idempotency record. The same id space is
// shared across emitted/refused/unnotifiable outcomes; the Outcome field
// disambiguates. A write failure is returned so the caller can record a
// doctor finding.
func persistNoticeRecord(homePath, holdID string, g attention.Group, workID, transport, receiptID, transitionID, outcome, refusalReason string, now time.Time) error {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return err
	}
	notice := NoticeRecord{
		ID:            holdID,
		SchemaVersion: NoticeRecordSchemaVersion,
		HoldID:        holdID,
		GroupKey:      g.Key,
		GroupArea:     g.Area,
		GroupSource:   g.Source,
		WorkID:        workID,
		ItemIDs:       append([]string{}, g.ItemIDs...),
		OperatorAsk:   g.OperatorSummary,
		// The decidable command alone, not the whole rendered card. The record
		// is the queryable evidence of what the operator was asked to run; a
		// field named ApproveCommand holding a four-line card body cannot be
		// joined against anything.
		ApproveCommand: decideCommand(g),
		Transport:      transport,
		Outcome:        outcome,
		RefusalReason:  refusalReason,
		ReceiptID:      receiptID,
		TransitionID:   transitionID,
		NotifiedAt:     now.UTC().Format(time.RFC3339),
	}
	_, err = store.PutJSON(holdID, &notice, " ")
	return err
}

// HoldIDFor derives a stable hold id from a band-needs-you group. The id is
// content-addressed over the group shape so the same hold across scans always
// resolves to the same record; the brief's once-and-only-once falsifier rides
// on this exact function.
//
// The id keys on WHAT THE OPERATOR MUST DECIDE — area, kind, source, and the
// action's identity — and deliberately NOT on the item set or its oldest
// timestamp. A hold is the same hold as items join and leave it: "resolve the
// source-control-cleanup queue" does not become a different question when the
// queue gains a row.
//
// Hashing ItemIDs and OldestCreatedAt (the original shape) made the id churn
// with the frontier, so findPriorNotification missed and the operator was
// re-notified about a hold they had already seen. That is tolerable on a verb a
// human runs by hand and a notification loop on a schedule, which is why this
// keying is a precondition for scheduling `notice flush` at all.
func HoldIDFor(g attention.Group) string {
	h := sha256.New()
	h.Write([]byte(g.Area))
	h.Write([]byte{0})
	h.Write([]byte(g.Kind))
	h.Write([]byte{0})
	h.Write([]byte(g.Source))
	h.Write([]byte{0})
	if g.Action != nil {
		h.Write([]byte(g.Action.ID))
	}
	return "opnotice_" + hex.EncodeToString(h.Sum(nil))[:24]
}

// governedNoticeIntent builds the act post buzz ActionIntent for one hold.
// The capability is fixed (product.ContentPostCapability) so the standing
// grant for product.post_content is the single authority; the target path
// mirrors the act_post convention.
//
// During development NO real notifications are sent (the act seam is dry-run
// by default and the buzz transport config is intentionally absent in tests).
// The intent shape is identical to the live act post so the operator's
// --confirm and the existing grant both apply unchanged.
func governedNoticeIntent(homePath, workID, transport, body string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            noticeActionID(workID, transport, body, now),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:      "operator",
		Capability: product.ContentPostCapability,
		Action:     "post_content",
		Target: envelope.ActionTarget{
			Kind: "transport",
			ID:   transport,
			Path: "actuator/transports/" + transport + ".json",
		},
		MutationClass: "external_side_effect",
		Reversibility: "irreversible",
		BlastRadius:   "local",
		Preview:       "notify operator of attention hold via " + transport,
		RollbackPlan:  "none guaranteed by the transport adapter",
	}
}

// noticeActionID is the act intent id for one notice. It hashes the work id,
// transport, body, and timestamp so two distinct holds get distinct intents.
func noticeActionID(workID, transport, body string, now time.Time) string {
	h := sha256.Sum256([]byte(strings.Join([]string{workID, transport, body, now.UTC().Format(time.RFC3339Nano)}, "|")))
	return "act_opnotice_" + hex.EncodeToString(h[:])[:16]
}

// cardBody builds the card body the operator receives: hold id + work id + a
// one-line ask +, when the group carries one, the exact command that DECIDES
// the hold.
//
// The decide line is g.Action.Argv verbatim — the policy-stamped argv that
// review groups already carry (attention.groupsFromReviews, sourced from
// reviewartifact.QueueItem.DecisionCard). It is reproduced, never synthesized:
// a command this function invents is a command no authority has approved.
//
// It previously emitted `compound act post --from-work <id> ... --confirm`
// regardless of the group. That line re-posts the notification to the same
// transport that just carried it, so the one flush this system has ever run
// (2026-08-06) delivered a card the operator could not act on. A group with no
// action now says so plainly and carries no command at all, which lets the
// caller count it as unnotifiable instead of shipping a dead end.
func cardBody(g attention.Group, holdID, workID, transport string) string {
	return cardBodyFor(g, holdID, workID, "")
}

// cardBodyFor is cardBody with the away window's end. Inside a window
// (through non-empty) the decide line is replaced by where the ask now
// waits: the card no longer asks, it says the ask is queued for the operator's
// return and names its return-queue entry.
func cardBodyFor(g attention.Group, holdID, workID, through string) string {
	lines := []string{
		fmt.Sprintf("🛎 hold=%s", holdID),
		fmt.Sprintf("work=%s", workID),
		fmt.Sprintf("ask=%s", askSummary(g)),
	}
	if through != "" {
		lines = append(lines, fmt.Sprintf("queued for your return (through %s): compound notice return-queue show %s",
			through, ReturnQueueIDFor(workID, g.Kind)))
		return strings.Join(lines, "\n")
	}
	if cmd := decideCommand(g); cmd != "" {
		lines = append(lines, "decide="+cmd)
	} else {
		lines = append(lines, "decide=(none available; this hold carries no adjudicating command)")
	}
	return strings.Join(lines, "\n")
}

// decideCommand renders the group's policy-stamped argv as a runnable line, or
// "" when the group carries no action. Empty is a real answer: it is what makes
// an undecidable hold countable rather than shippable.
func decideCommand(g attention.Group) string {
	if g.Action == nil || len(g.Action.Argv) == 0 {
		return ""
	}
	parts := make([]string, 0, len(g.Action.Argv))
	for _, a := range g.Action.Argv {
		if s := strings.TrimSpace(a); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// findPriorNotification returns true if a NoticeRecord with the given hold id
// already exists in the home's event store. A read error is returned so the
// caller can count it in the doctor (best-effort: a read failure does NOT
// block the emit, because the predicate is exactly-one-notification).
func findPriorNotification(homePath, holdID string) (bool, *NoticeRecord, error) {
	store, err := eventstore.Open(homePath)
	if err != nil {
		return false, nil, err
	}
	rec, err := store.Read(holdID)
	if err != nil {
		// A missing record is the steady-state hit; the event store frames
		// ENOENT as either "not found" or "no such file" depending on the
		// engine phase. Both are the absence path — surface only non-ENOENT
		// errors as the doctor signal.
		msg := err.Error()
		if strings.Contains(msg, "not found") || strings.Contains(msg, "no such file") {
			return false, nil, nil
		}
		return false, nil, err
	}
	var notice NoticeRecord
	if err := json.Unmarshal(rec.Bytes, &notice); err != nil {
		return false, nil, err
	}
	return true, &notice, nil
}

// firstWorkID picks the first non-empty ItemIDs entry. It favors the canonical
// surface (the work id itself) over a derived item id (e.g. executor-row ids)
// so the emitted approve command matches the operator's mental model.
func firstWorkID(ids []string) string {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		// Skip the executor-context:* composite that carries a hook event id —
		// it is not a work id and the approve command would be invalid.
		if strings.HasPrefix(id, "executor-context:") {
			continue
		}
		// Skip non-work ids such as activity-block:* and lease-hygiene entries.
		// Only true work_* ids are valid candidates for the approve command.
		if strings.HasPrefix(id, "activity-block:") || strings.HasPrefix(id, "lease-") {
			continue
		}
		return id
	}
	return ""
}

// doctorFinding is the small struct the doctor count routine emits. It is
// purely an evidence-class record — never a ledger byte — and is surfaced
// by compound doctor without a separate seam.
type doctorFinding struct {
	Kind       string
	GroupKey   string
	HoldID     string
	Detail     string
	ProducedAt string
}

func recordDoctorUnnotifiable(homePath string, g attention.Group, holdID string, now time.Time) {
	emitDoctorFinding(homePath, doctorFinding{
		Kind: "opnotice_unnotifiable", GroupKey: g.Key, HoldID: holdID,
		Detail:     "band-needs-you group has no actionable work id; cannot emit a notice",
		ProducedAt: now.UTC().Format(time.RFC3339),
	}, now)
}

func recordDoctorRefusal(homePath string, g attention.Group, holdID string, cause error, now time.Time) {
	emitDoctorFinding(homePath, doctorFinding{
		Kind: "opnotice_refused", GroupKey: g.Key, HoldID: holdID,
		Detail:     fmt.Sprintf("notice flush refused: %v", cause),
		ProducedAt: now.UTC().Format(time.RFC3339),
	}, now)
}

func recordDoctorReadFailure(homePath string, g attention.Group, holdID string, cause error, now time.Time) {
	emitDoctorFinding(homePath, doctorFinding{
		Kind: "opnotice_read_failure", GroupKey: g.Key, HoldID: holdID,
		Detail:     fmt.Sprintf("notice record read failed: %v", cause),
		ProducedAt: now.UTC().Format(time.RFC3339),
	}, now)
}

// emitDoctorFinding writes a finding-class record so the doctor count
// surfaces the refusal/unnotifiable. The home is best-effort: a finding
// failure logs nothing — the NoticeReport doctor count is the operator-visible
// witness regardless.
func emitDoctorFinding(homePath string, f doctorFinding, now time.Time) {
	body, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return
	}
	_, _ = finding.Record(homePath, finding.Finding{
		Topic:      f.Kind,
		Body:       string(body),
		ProducedBy: "internal/operatornotice",
		WorkID:     "",
	}, now)
}

// requireManifest is the cheap read-only check that the home is a real
// compound home (not a bare temp dir). It races with the eventstore's own
// validation, but it fails fast at the seam — the predicate's
// read-only-failure test rides on this check.
func requireManifest(homePath string) error {
	clean := strings.TrimSpace(homePath)
	if clean == "" {
		return fmt.Errorf("home path required")
	}
	manifest := filepath.Join(clean, home.ManifestName)
	if _, err := os.Stat(manifest); err != nil {
		return fmt.Errorf("home %s is not a compound home (missing %s)", clean, home.ManifestName)
	}
	return nil
}

// Unused but exported so a downstream caller can validate the schema
// without instantiating the report.
var _ = home.Resolve
