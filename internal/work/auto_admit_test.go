package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
	autonomyproof "compound/internal/routeevidence/autonomy"
)

// makeProposedDirectorWork creates a director-proposed work of the given autonomy class,
// in state `proposed`, carrying the director provenance + trigger evidence refs the
// kill-switch keys on.
func makeProposedDirectorWork(t *testing.T, s *Store, class, id string, now time.Time) string {
	t.Helper()
	plan := ensureJobHealthAutonomyPlan(t, s)
	w, err := s.Create(CreateInput{
		WorkID:           id,
		Owner:            "compound-authority",
		Actor:            "director",
		SuccessPredicate: "review " + class + " signal " + id,
		State:            "proposed",
		EvidenceRefs:     []string{"proposed_by:director", "director_trigger:" + class + ":" + id, "route_plan:" + plan.ID},
		RouteDecisionID:  plan.ID,
		ProposalKind:     envelope.ProposalKindOrdinary,
	}, now)
	if err != nil {
		t.Fatalf("create proposed %s work: %v", class, err)
	}
	if w.State != "proposed" {
		t.Fatalf("director work should start proposed, got %q", w.State)
	}
	return w.ID
}

// TestKillSwitchHaltsAutoAdmit is the headline W9-J4 proof: a simulated trust-dial
// auto-admit of an ELIGIBLE class is denied while the policy-owned halt is engaged, a
// per-class disable blocks only its own class, and an unset halt lets the admit proceed.
// It fails if any auto-admit path ignores the flag.
func TestKillSwitchHaltsAutoAdmit(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)

	// Phase A — halt UNSET: an eligible auto-admit proceeds to admitted.
	jobA := makeProposedDirectorWork(t, s, "job_health", "work_kill_a", fixedNow)
	resA, err := s.AdmitAuto(jobA, eligible, fixedNow)
	if err != nil {
		t.Fatalf("unset halt must let eligible auto-admit proceed: %v", err)
	}
	if !resA.Admitted || resA.WorkState != "admitted" {
		t.Fatalf("unset halt auto-admit should reach admitted: %+v", resA)
	}
	if got, _ := s.Get(jobA); got.State != "admitted" {
		t.Fatalf("unset halt work should be admitted on disk, got %q", got.State)
	}

	// Phase B — GLOBAL halt: the same eligible auto-admit is denied and stays proposed.
	if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{Global: true, Reason: "incident review"}); err != nil {
		t.Fatalf("engage global halt: %v", err)
	}
	jobB := makeProposedDirectorWork(t, s, "job_health", "work_kill_b", fixedNow)
	resB, err := s.AdmitAuto(jobB, eligible, fixedNow)
	if err == nil {
		t.Fatalf("global halt must deny an eligible auto-admit")
	}
	if resB.Admitted || !resB.Halted {
		t.Fatalf("global halt result should be halted, not admitted: %+v", resB)
	}
	if got, _ := s.Get(jobB); got.State != "proposed" {
		t.Fatalf("global halt must leave work proposed, got %q", got.State)
	}

	// Phase C — PER-CLASS disable blocks job_health. A sibling class also stays
	// proposed because this first evidence-autonomy slice is class-scoped; the
	// job_health artifact cannot be replayed against another class.
	if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{
		DisabledClasses: map[string]string{"job_health": "noisy class parked"},
	}); err != nil {
		t.Fatalf("engage per-class halt: %v", err)
	}
	jobC := makeProposedDirectorWork(t, s, "job_health", "work_kill_c", fixedNow)
	srcC := makeProposedDirectorWork(t, s, "source_connection_readiness", "work_kill_src", fixedNow)

	resC, err := s.AdmitAuto(jobC, eligible, fixedNow)
	if err == nil || resC.Admitted {
		t.Fatalf("per-class disable must block its own class: err=%v res=%+v", err, resC)
	}
	if got, _ := s.Get(jobC); got.State != "proposed" {
		t.Fatalf("disabled class must stay proposed, got %q", got.State)
	}

	resSrc, err := s.AdmitAuto(srcC, eligible, fixedNow)
	if err == nil || resSrc.Admitted || !strings.Contains(err.Error(), "no enabled evidence-autonomy slice") {
		t.Fatalf("job_health eligibility must not admit a sibling class: err=%v res=%+v", err, resSrc)
	}
	if got, _ := s.Get(srcC); got.State != "proposed" {
		t.Fatalf("sibling class should stay proposed, got %q", got.State)
	}
}

// TestKillSwitchDoesNotBlockOperatorAdmit is the required negative: a manual operator
// admit through the admission seam is UNAFFECTED by the kill-switch, even under a global
// halt. Governance halts autonomy, not the operator.
func TestKillSwitchDoesNotBlockOperatorAdmit(t *testing.T) {
	s := newStore(t)
	if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{Global: true, Reason: "incident review"}); err != nil {
		t.Fatalf("engage global halt: %v", err)
	}
	id := makeProposedDirectorWork(t, s, "job_health", "work_op_admit", fixedNow)

	out, err := s.Advance(id, AdvanceInput{Actor: "operator-test", To: "admitted"}, fixedNow.Add(1*time.Minute))
	if err != nil {
		t.Fatalf("operator admit must not be blocked by autonomy halt: %v", err)
	}
	if out.State != "admitted" {
		t.Fatalf("operator admit should reach admitted under halt, got %q", out.State)
	}
}

// TestKillSwitchHaltsAdvanceSeamDefenseInDepth proves the generic Advance seam carries its
// own halt check: an autonomous (non-operator) admission is stopped BY THE HALT, not merely
// by the operator-provenance guard, so no auto-admit path can route around the flag.
func TestKillSwitchHaltsAdvanceSeamDefenseInDepth(t *testing.T) {
	s := newStore(t)
	if err := policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{Global: true, Reason: "incident review"}); err != nil {
		t.Fatalf("engage global halt: %v", err)
	}
	id := makeProposedDirectorWork(t, s, "job_health", "work_did", fixedNow)

	_, err := s.Advance(id, AdvanceInput{Actor: "auto-admit:director", To: "admitted"}, fixedNow.Add(1*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "autonomy halted") {
		t.Fatalf("generic seam must halt an autonomous admission with an autonomy-halt error, got %v", err)
	}
	if got, _ := s.Get(id); got.State != "proposed" {
		t.Fatalf("halted advance must leave work proposed, got %q", got.State)
	}
}

// TestAutoAdmitRequiresEligibilityGrant proves that the admission seam never
// infers authority: without a typed, content-addressed eligibility artifact,
// nothing auto-admits even with the halt unset.
func TestAutoAdmitRequiresEligibilityGrant(t *testing.T) {
	s := newStore(t)
	id := makeProposedDirectorWork(t, s, "job_health", "work_elig", fixedNow)

	res, err := s.AdmitAuto(id, AutoAdmitInput{By: "director"}, fixedNow.Add(1*time.Minute))
	if err == nil {
		t.Fatalf("auto-admit without a clean-window grant must be denied")
	}
	if res.Admitted {
		t.Fatalf("ineligible auto-admit must not admit: %+v", res)
	}
	if got, _ := s.Get(id); got.State != "proposed" {
		t.Fatalf("ineligible auto-admit must stay proposed, got %q", got.State)
	}
}

func TestAutoAdmitRejectsAmbiguousDirectorTriggerIdentity(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	plan := ensureJobHealthAutonomyPlan(t, s)
	w, err := s.Create(CreateInput{
		WorkID:           "work_ambiguous_director_trigger",
		Owner:            "compound-authority",
		Actor:            "director",
		SuccessPredicate: "ambiguous trigger identity must not select an autonomy class",
		State:            "proposed",
		RouteDecisionID:  plan.ID,
		EvidenceRefs: []string{
			"proposed_by:director",
			"director_trigger:job_health:claimed",
			"director_trigger:source_connection_readiness:actual",
			"route_plan:" + plan.ID,
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AdmitAuto(w.ID, eligible, fixedNow)
	if err == nil || result.Admitted || !strings.Contains(err.Error(), "ambiguous director_trigger") {
		t.Fatalf("ambiguous trigger identity escaped: err=%v result=%+v", err, result)
	}
	if got, _ := s.Get(w.ID); got.State != "proposed" {
		t.Fatalf("ambiguous trigger identity changed work state: %q", got.State)
	}
}

func TestAutoAdmitRejectsCallerAssertedEligibilityString(t *testing.T) {
	s := newStore(t)
	id := makeProposedDirectorWork(t, s, "job_health", "work_asserted_eligibility", fixedNow)
	res, err := s.AdmitAuto(id, AutoAdmitInput{
		By:                "director",
		EligibilityReason: "clean-window: trust me",
		GrantRef:          "grant:asserted-only",
	}, fixedNow.Add(time.Minute))
	if err == nil || res.Admitted || !strings.Contains(err.Error(), "typed eligibility artifact required") {
		t.Fatalf("caller assertion must not establish eligibility: err=%v res=%+v", err, res)
	}
	if got, _ := s.Get(id); got.State != "proposed" {
		t.Fatalf("caller assertion changed work state: %q", got.State)
	}
}

func TestAutoAdmitOperatorOnlyRiskFrontierAlwaysWins(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	tests := []struct {
		name          string
		mutationClass string
		reversibility string
		blastRadius   string
	}{
		{name: "external", mutationClass: "external_side_effect", reversibility: "easy", blastRadius: "local"},
		{name: "public", mutationClass: "public", reversibility: "easy", blastRadius: "local"},
		{name: "destructive", mutationClass: "destructive", reversibility: "easy", blastRadius: "local"},
		{name: "financial", mutationClass: "financial", reversibility: "easy", blastRadius: "local"},
		{name: "security", mutationClass: "security", reversibility: "easy", blastRadius: "local"},
		{name: "irreversible", mutationClass: "local_reversible", reversibility: "irreversible", blastRadius: "local"},
		{name: "hard", mutationClass: "local_reversible", reversibility: "hard", blastRadius: "local"},
		{name: "none", mutationClass: "local_reversible", reversibility: "none", blastRadius: "local"},
		{name: "wide", mutationClass: "local_reversible", reversibility: "easy", blastRadius: "public"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			eligible := typedJobHealthEligibility(t, s, fixedNow)
			id := makeProposedDirectorWork(t, s, "job_health", "work_risk_"+tc.name, fixedNow)
			w, err := s.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			var action envelope.ActionIntent
			if err := s.readJSON(s.envPath(w.ActionIntentIDs[0]), &action); err != nil {
				t.Fatal(err)
			}
			action.MutationClass = tc.mutationClass
			action.Reversibility = tc.reversibility
			action.BlastRadius = tc.blastRadius
			if err := action.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := s.replaceEvent(action.ID, &action); err != nil {
				t.Fatal(err)
			}
			res, err := s.AdmitAuto(id, eligible, fixedNow)
			if err == nil || res.Admitted || !strings.Contains(err.Error(), "operator-only") {
				t.Fatalf("operator-only risk escaped: err=%v res=%+v", err, res)
			}
			if got, _ := s.Get(id); got.State != "proposed" {
				t.Fatalf("risk denial changed work state: %q", got.State)
			}
		})
	}
}

func TestAutoAdmitRevalidatesGrantAtAdmission(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	seedDecisionClaim(t, s, "grant", "job-health-autonomy-revoked", map[string]string{
		"supersedes": "job-health-autonomy-test",
	}, fixedNow.Add(-time.Hour))
	id := makeProposedDirectorWork(t, s, "job_health", "work_revoked_at_admit", fixedNow)
	res, err := s.AdmitAuto(id, eligible, fixedNow)
	if err == nil || res.Admitted || !strings.Contains(err.Error(), "missing or ambiguous") {
		t.Fatalf("revoked grant escaped admission revalidation: err=%v res=%+v", err, res)
	}
	if got, _ := s.Get(id); got.State != "proposed" {
		t.Fatalf("revoked grant changed work state: %q", got.State)
	}
}

func TestAutoAdmitRejectsTamperedEligibilityArtifact(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	id := strings.TrimPrefix(eligible.EligibilityRef, "eligibility:")
	path := filepath.Join(s.home, "artifacts", "autonomy-eligibility", id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	payload["failure_count"] = float64(99)
	tampered, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(tampered, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	workID := makeProposedDirectorWork(t, s, "job_health", "work_tampered_eligibility", fixedNow)
	res, err := s.AdmitAuto(workID, eligible, fixedNow)
	if err == nil || res.Admitted || !strings.Contains(err.Error(), "content digest mismatch") {
		t.Fatalf("tampered eligibility escaped: err=%v res=%+v", err, res)
	}
	if got, _ := s.Get(workID); got.State != "proposed" {
		t.Fatalf("tampered eligibility changed work state: %q", got.State)
	}
}

func TestAutoAdmitRejectsEligibilityFromEarlierEvaluationInstant(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	workID := makeProposedDirectorWork(t, s, "job_health", "work_stale_evaluation", fixedNow)

	res, err := s.AdmitAuto(workID, eligible, fixedNow.Add(31*24*time.Hour))
	if err == nil || res.Admitted || !strings.Contains(err.Error(), "eligibility evaluation instant") {
		t.Fatalf("stale evaluation instant escaped: err=%v res=%+v", err, res)
	}
	if got, _ := s.Get(workID); got.State != "proposed" {
		t.Fatalf("stale evaluation changed work state: %q", got.State)
	}
}

func TestAutoAdmitEventFailureLeavesWorkProposed(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	workID := makeProposedDirectorWork(t, s, "job_health", "work_event_failure", fixedNow)
	before, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	eventPath := s.eventsLog()
	eventBackup := eventPath + ".backup"
	eventBefore, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	sabotaged := false
	restoreEvents := func() {
		t.Helper()
		if !sabotaged {
			return
		}
		if err := os.RemoveAll(eventPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(eventBackup, eventPath); err != nil {
			t.Fatal(err)
		}
		sabotaged = false
	}

	previousHook := autoAdmitBeforeCommit
	autoAdmitBeforeCommit = func() {
		if err := os.Rename(eventPath, eventBackup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(eventPath, 0o755); err != nil {
			t.Fatal(err)
		}
		sabotaged = true
	}
	defer func() {
		autoAdmitBeforeCommit = previousHook
		restoreEvents()
	}()
	result, err := s.AdmitAuto(workID, eligible, fixedNow)
	autoAdmitBeforeCommit = previousHook
	restoreEvents()
	if err == nil || result.Admitted || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("event persistence failure admitted work: err=%v result=%+v", err, result)
	}
	after, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterJSON) != string(beforeJSON) {
		t.Fatalf("event persistence failure mutated work:\nbefore=%s\nafter=%s", beforeJSON, afterJSON)
	}
	eventAfter, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(eventAfter) != string(eventBefore) {
		t.Fatalf("event persistence failure changed lifecycle log:\nbefore=%s\nafter=%s", eventBefore, eventAfter)
	}
}

func TestAutoAdmitRejectsTruncatedEventTailWithoutMutatingWork(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	workID := makeProposedDirectorWork(t, s, "job_health", "work_truncated_event_tail", fixedNow)

	workPath := s.workPath(workID)
	before, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	envelopesBefore, err := os.ReadDir(filepath.Join(s.home, "events"))
	if err != nil {
		t.Fatal(err)
	}
	const truncatedTail = `{"ts":"torn"`

	previousHook := autoAdmitBeforeCommit
	autoAdmitBeforeCommit = func() {
		log, err := os.OpenFile(s.eventsLog(), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.WriteString(truncatedTail); err != nil {
			_ = log.Close()
			t.Fatal(err)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { autoAdmitBeforeCommit = previousHook }()

	result, err := s.AdmitAuto(workID, eligible, fixedNow)
	if err == nil || result.Admitted || !strings.Contains(err.Error(), "incomplete trailing record") {
		t.Fatalf("truncated lifecycle tail did not fail closed: err=%v result=%+v", err, result)
	}
	after, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("truncated lifecycle tail mutated work:\nbefore=%s\nafter=%s", before, after)
	}
	eventsAfter, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(eventsAfter), string(eventsBefore)+truncatedTail; got != want {
		t.Fatalf("truncated lifecycle tail was appended through:\nwant=%s\ngot=%s", want, got)
	}
	envelopesAfter, err := os.ReadDir(filepath.Join(s.home, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if len(envelopesAfter) != len(envelopesBefore) {
		t.Fatalf("malformed lifecycle log created %d envelope side effects, want none", len(envelopesAfter)-len(envelopesBefore))
	}
	if _, err := s.eventsFor(workID); err == nil || !strings.Contains(err.Error(), "incomplete trailing record") {
		t.Fatalf("lifecycle reader silently skipped malformed tail: %v", err)
	}
}

func TestAutoAdmitRetryFinalizesDurableEventExactlyOnce(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	workID := makeProposedDirectorWork(t, s, "job_health", "work_retry_durable_event", fixedNow)

	workPath := s.workPath(workID)
	workBackup := workPath + ".before-rename-failure"
	before, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeWork, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash, err := workEnvelopeSHA256(beforeWork)
	if err != nil {
		t.Fatal(err)
	}
	sabotaged := false
	restoreWork := func() {
		t.Helper()
		if !sabotaged {
			return
		}
		if err := os.RemoveAll(workPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(workBackup, workPath); err != nil {
			t.Fatal(err)
		}
		sabotaged = false
	}
	defer restoreWork()

	previousHook := advanceBeforeWorkCommit
	defer func() { advanceBeforeWorkCommit = previousHook }()
	advanceBeforeWorkCommit = func() {
		if err := os.Rename(workPath, workBackup); err != nil {
			t.Fatal(err)
		}
		sabotaged = true
		if err := os.Mkdir(workPath, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.AdmitAuto(workID, eligible, fixedNow)
	advanceBeforeWorkCommit = previousHook
	if err == nil || result.Admitted {
		t.Fatalf("rename failure unexpectedly admitted work: err=%v result=%+v", err, result)
	}
	restoreWork()
	afterFailure, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterFailure) != string(before) {
		t.Fatalf("failed rename changed persisted work:\nbefore=%s\nafter=%s", before, afterFailure)
	}

	type durableAdmissionEvent struct {
		WorkID             string `json:"work_id"`
		From               string `json:"from"`
		To                 string `json:"to"`
		PreviousWorkSHA256 string `json:"previous_work_sha256"`
		WorkSHA256         string `json:"work_sha256"`
	}
	eventBytes, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	var durableEvents []durableAdmissionEvent
	for _, line := range strings.Split(strings.TrimSpace(string(eventBytes)), "\n") {
		var event durableAdmissionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.WorkID == workID && event.From == "proposed" && event.To == "admitted" {
			durableEvents = append(durableEvents, event)
		}
	}
	if len(durableEvents) != 1 {
		t.Fatalf("first attempt persisted %d admission events, want exactly one: %+v", len(durableEvents), durableEvents)
	}
	durableEvent := durableEvents[0]
	if durableEvent.PreviousWorkSHA256 != beforeHash || durableEvent.WorkSHA256 == "" {
		t.Fatalf("durable admission did not bind both WorkEnvelope versions: before=%q want=%q after=%q", durableEvent.PreviousWorkSHA256, beforeHash, durableEvent.WorkSHA256)
	}
	pendingPath := workPath + ".advance-pending"
	pendingBody, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatalf("durable admission lost its pending WorkEnvelope: %v", err)
	}
	var pending envelope.WorkEnvelope
	if err := json.Unmarshal(pendingBody, &pending); err != nil {
		t.Fatal(err)
	}
	pendingHash, err := workEnvelopeSHA256(pending)
	if err != nil {
		t.Fatal(err)
	}
	if pendingHash != durableEvent.WorkSHA256 {
		t.Fatalf("pending WorkEnvelope hash %q does not match durable event %q", pendingHash, durableEvent.WorkSHA256)
	}

	result, err = s.AdmitAuto(workID, eligible, fixedNow.Add(time.Minute))
	if err != nil || !result.Admitted || result.WorkState != "admitted" {
		t.Fatalf("retry did not finalize the durable admission: err=%v result=%+v", err, result)
	}
	recovered, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	recoveredHash, err := workEnvelopeSHA256(recovered)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredHash != durableEvent.WorkSHA256 {
		t.Fatalf("recovered WorkEnvelope hash %q does not match durable event %q", recoveredHash, durableEvent.WorkSHA256)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("recovered admission left pending WorkEnvelope behind: %v", err)
	}
	afterEvents, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterEvents) != string(eventBytes) {
		t.Fatalf("recovery appended a duplicate lifecycle event:\nbefore=%s\nafter=%s", eventBytes, afterEvents)
	}
}

func TestAutoAdmitSerializesConcurrentHaltAtCommit(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	workID := makeProposedDirectorWork(t, s, "job_health", "work_halt_race", fixedNow)

	entered := make(chan struct{})
	release := make(chan struct{})
	previousHook := autoAdmitBeforeCommit
	autoAdmitBeforeCommit = func() {
		close(entered)
		<-release
	}
	defer func() { autoAdmitBeforeCommit = previousHook }()

	type admitOutcome struct {
		result AutoAdmitResult
		err    error
	}
	admitDone := make(chan admitOutcome, 1)
	go func() {
		result, err := s.AdmitAuto(workID, eligible, fixedNow)
		admitDone <- admitOutcome{result: result, err: err}
	}()
	<-entered

	haltStarted := make(chan struct{})
	haltDone := make(chan error, 1)
	go func() {
		close(haltStarted)
		haltDone <- policy.WriteAutonomyHalt(s.home, policy.AutonomyHalt{Global: true, Reason: "concurrent incident"})
	}()
	<-haltStarted
	select {
	case err := <-haltDone:
		t.Fatalf("halt writer crossed the admission critical section: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)

	admitted := <-admitDone
	if admitted.err != nil || !admitted.result.Admitted {
		t.Fatalf("admission holding the lock should linearize first: err=%v result=%+v", admitted.err, admitted.result)
	}
	if err := <-haltDone; err != nil {
		t.Fatal(err)
	}
	second := makeProposedDirectorWork(t, s, "job_health", "work_after_concurrent_halt", fixedNow)
	if result, err := s.AdmitAuto(second, eligible, fixedNow); err == nil || result.Admitted || !result.Halted {
		t.Fatalf("halt must govern the next admission after it linearizes: err=%v result=%+v", err, result)
	}
}

func TestAutoAdmitConcurrentAttemptsCommitExactlyOnce(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	workID := makeProposedDirectorWork(t, s, "job_health", "work_double_admit", fixedNow)

	start := make(chan struct{})
	type admitOutcome struct {
		result AutoAdmitResult
		err    error
	}
	outcomes := make(chan admitOutcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			result, err := s.AdmitAuto(workID, eligible, fixedNow)
			outcomes <- admitOutcome{result: result, err: err}
		}()
	}
	close(start)
	successes := 0
	failures := 0
	for i := 0; i < 2; i++ {
		outcome := <-outcomes
		if outcome.err == nil && outcome.result.Admitted {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent admission must commit exactly once: successes=%d failures=%d", successes, failures)
	}
	w, events, err := s.Show(workID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != "admitted" {
		t.Fatalf("winning admission did not persist: %+v", w)
	}
	admissionEvents := 0
	for _, event := range events {
		if event.From == "proposed" && event.To == "admitted" {
			admissionEvents++
		}
	}
	if admissionEvents != 1 {
		t.Fatalf("concurrent admission emitted %d admitted events, want 1: %+v", admissionEvents, events)
	}
}

func TestStaleCreatePlanCannotOverwriteAdmittedWork(t *testing.T) {
	s := newStore(t)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	routePlan := ensureJobHealthAutonomyPlan(t, s)
	const workID = "work_stale_create_after_admit"
	stale, err := s.CreatePlan(CreateInput{
		WorkID:           workID,
		Owner:            "compound-authority",
		Actor:            "director",
		SuccessPredicate: "stale create plan cannot overwrite admitted state",
		State:            "proposed",
		ProposalKind:     envelope.ProposalKindOrdinary,
		RouteDecisionID:  routePlan.ID,
		EvidenceRefs: []string{
			"proposed_by:director",
			"director_trigger:job_health:" + workID,
			"route_plan:" + routePlan.ID,
		},
	}, fixedNow, CreatePlanOptions{Actor: "director"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.admitWork(stale, fixedNow, "created", nil); err != nil {
		t.Fatalf("setup admission failed: err=%v", err)
	}
	if result, err := s.AdmitAuto(workID, eligible, fixedNow); err != nil || !result.Admitted {
		t.Fatalf("setup admission failed: err=%v result=%+v", err, result)
	}
	if _, err := s.admitWork(stale, fixedNow.Add(2*time.Minute), "stale replay", nil); err == nil || !strings.Contains(err.Error(), "refusing stale create-plan") {
		t.Fatalf("stale create plan was not refused: %v", err)
	}
	got, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "admitted" {
		t.Fatalf("stale create plan overwrote admitted state: %+v", got)
	}
}

func TestAutoAdmitSerializesConcurrentTerminalCreation(t *testing.T) {
	s := newStore(t)
	plan := ensureJobHealthAutonomyPlan(t, s)
	eligible := typedJobHealthEligibility(t, s, fixedNow)
	workID := makeProposedDirectorWork(t, s, "job_health", "work_terminal_create_race", fixedNow)
	terminalWork, err := s.Create(CreateInput{
		WorkID:           "work_concurrent_terminal_sample",
		Owner:            "compound-authority",
		Actor:            "operator-test",
		SuccessPredicate: "terminal sample created during admission",
		State:            "proposed",
		ProposalKind:     envelope.ProposalKindOrdinary,
		RouteDecisionID:  plan.ID,
		EvidenceRefs: []string{
			"proposed_by:director",
			"director_trigger:job_health:concurrent-terminal",
			"route_plan:" + plan.ID,
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(terminalWork.ID, AdvanceInput{Actor: "operator-test", To: "ready"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(terminalWork.ID, "sample-executor", fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(terminalWork.ID, AdvanceInput{Actor: "sample-executor", To: "running"}, fixedNow); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	previousHook := autoAdmitBeforeCommit
	autoAdmitBeforeCommit = func() {
		close(entered)
		<-release
	}
	defer func() { autoAdmitBeforeCommit = previousHook }()

	type admitOutcome struct {
		result AutoAdmitResult
		err    error
	}
	admitDone := make(chan admitOutcome, 1)
	go func() {
		result, err := s.AdmitAuto(workID, eligible, fixedNow)
		admitDone <- admitOutcome{result: result, err: err}
	}()
	<-entered

	createStarted := make(chan struct{})
	createDone := make(chan error, 1)
	go func() {
		close(createStarted)
		_, err := s.Close(terminalWork.ID, CloseInput{
			Outcome:   "failed",
			MoveType:  "control_plane_repair",
			Actor:     "sample-executor",
			Summary:   "terminal sample failed during admission",
			ProofRefs: []string{"failure:concurrent-terminal-sample"},
		}, fixedNow.Add(24*time.Hour))
		createDone <- err
	}()
	<-createStarted
	select {
	case err := <-createDone:
		t.Fatalf("terminal creation crossed the admission critical section: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)

	admitted := <-admitDone
	if admitted.err != nil || !admitted.result.Admitted {
		t.Fatalf("admission holding the lock should linearize first: err=%v result=%+v", admitted.err, admitted.result)
	}
	if err := <-createDone; err != nil {
		t.Fatal(err)
	}
	terminal, err := s.Get("work_concurrent_terminal_sample")
	if err != nil || terminal.State != "failed" {
		t.Fatalf("concurrent terminal sample was not persisted after the lock released: state=%q err=%v", terminal.State, err)
	}
	artifactID := strings.TrimPrefix(eligible.EligibilityRef, "eligibility:")
	artifactBody, err := os.ReadFile(filepath.Join(s.home, "artifacts", "autonomy-eligibility", artifactID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var sealed autonomyproof.Artifact
	if err := json.Unmarshal(artifactBody, &sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := autonomyproof.Compile(s.home, sealed.Grant, sealed.RoutePlan, fixedNow); err == nil || !strings.Contains(err.Error(), "terminal timestamp after eligibility evaluation") {
		t.Fatalf("post-generation terminal sample did not invalidate the frozen evidence set: terminal=%+v err=%v", terminal, err)
	}

	autoAdmitBeforeCommit = previousHook
	next := makeProposedDirectorWork(t, s, "job_health", "work_after_terminal_sample", fixedNow.Add(2*time.Minute))
	if result, err := s.AdmitAuto(next, eligible, fixedNow); err == nil || result.Admitted || !strings.Contains(err.Error(), "stale or source evidence changed") {
		t.Fatalf("new terminal evidence must invalidate the old artifact: err=%v result=%+v", err, result)
	}
}
