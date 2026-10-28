package work

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/runattempt"
)

// These falsifiers pin the 2026-10-15 proof-contention repair: the global
// admission lock must not pay for history it does not own. A corrupt record in
// an UNRELATED family (or an unrelated RunAttempt's launch evidence) is invisible
// to an exact owner-scoped lookup, so proof completion/refresh and ordinary
// lifecycle mutations must keep working while that corruption stays on disk for
// the doctor. Before the repair the broad scans parsed every family and
// validated every RunAttempt under the lock, so each test below failed.

// corruptUnrelatedEventSegment plants an unparseable sealed segment in a family
// the proof refresh chain never owns. Scans that walk every family fail closed
// on it; exact and family-restricted lookups never read it.
func corruptUnrelatedEventSegment(t *testing.T, s *Store, family string) {
	t.Helper()
	dir := filepath.Join(s.home, "events", "segments", family)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2020-01-01.jsonl")
	// Two invalid lines: a lone final line would be tolerated as a torn tail of
	// a hot segment, and this falsifier must fail EVERY scan that reads it.
	if err := os.WriteFile(path, []byte("this line is not an event frame\nneither is this one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func proofScanSeesCorruption(t *testing.T, s *Store, prefixes ...string) {
	t.Helper()
	if _, err := s.events.Scan(eventstore.ScanOptions{Prefixes: prefixes, RegularOnly: true}); err == nil {
		t.Fatalf("scan with prefixes %v tolerated the planted corruption; the falsifier is not armed", prefixes)
	}
}

func reservedRefreshedProofWork(t *testing.T, s *Store, predicate string, ttl time.Duration) (string, ProofResourceReserveResult, ProofResourceReservation) {
	t.Helper()
	w := claimedProofWork(t, s, predicate, proofActorAlpha, fixedNow)
	w, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: proofActorAlpha}, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := s.RefreshProofResource(
		w.ID,
		acquired.Reservation.ID,
		proofActorAlpha,
		ttl,
		fixedNow.Add(20*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	return w.ID, acquired, refreshed
}

func TestCompleteProofResourceIgnoresUnrelatedEventFamilies(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	workID, acquired, _ := reservedRefreshedProofWork(t, s, "hotpath completion ignores unrelated families", 30*time.Second)
	// Routed receipt history is a bystander: completion must not
	// parse it to learn one reservation's refresh history.
	corruptUnrelatedEventSegment(t, s, "receipt")
	proofScanSeesCorruption(t, s, "rec_")

	report := canonicalOwnedProofReport()
	report["base_ref"] = ""
	report["generated_at"] = fixedNow.Add(22 * time.Second).Format(time.RFC3339)
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteProofResource(workID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(23*time.Second))
	if err != nil {
		t.Fatalf("proof completion scanned unrelated event history under the admission lock: %v", err)
	}
	if completed.Result.Status != ProofResourceVerified {
		t.Fatalf("completion status = %q, want verified", completed.Result.Status)
	}
}

func TestProofRefreshLookupStaysInsideReservationFamily(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, acquired, refreshed := reservedRefreshedProofWork(t, s, "hotpath fallback stays in reservation family", 45*time.Second)
	corruptUnrelatedEventSegment(t, s, "receipt")
	proofScanSeesCorruption(t, s, "rec_")

	// Exact enumeration must find the reservation-scoped outcome without
	// touching the receipt family.
	latest, err := s.proofReservationWithLatestRefresh(acquired.Reservation)
	if err != nil {
		t.Fatalf("fallback refresh lookup left the reservation family: %v", err)
	}
	if latest.ExpiresAt != refreshed.ExpiresAt {
		t.Fatalf("fallback lookup expiry = %s, want the refreshed %s", latest.ExpiresAt, refreshed.ExpiresAt)
	}
}

func corruptUnrelatedRunAttemptLaunchEvidence(t *testing.T, s *Store, workID string) string {
	t.Helper()
	attempts := runattempt.Open(s.home)
	ra, err := attempts.Mint(workID, "codex:unrelated", "claude-code", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"schema_version":"compound.runattempt.launch_evidence.v0.1","attempt_id":%q,"kind":"committed","at":"%s"}`+"\n",
		ra.ID, fixedNow.Format(time.RFC3339Nano))
	path := filepath.Join(s.home, "run_attempts", "launch", ra.ID+".committed.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ra.ID
}

func TestLifecycleMutationIgnoresUnrelatedRunAttemptLaunchEvidence(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	bystander := claimedProofWork(t, s, "unrelated broken launch evidence", "codex:unrelated", fixedNow)
	corruptUnrelatedRunAttemptLaunchEvidence(t, s, bystander.ID)

	w := claimedProofWork(t, s, "ordinary lifecycle mutation", proofActorAlpha, fixedNow.Add(time.Second))
	if _, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: proofActorAlpha}, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatalf("lifecycle mutation validated an unrelated RunAttempt's launch evidence under the admission lock: %v", err)
	}
}

func TestRefreshProofResourceIgnoresUnrelatedRunAttemptLaunchEvidence(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	bystander := claimedProofWork(t, s, "unrelated broken launch evidence for refresh", "codex:unrelated", fixedNow)
	corruptUnrelatedRunAttemptLaunchEvidence(t, s, bystander.ID)

	w := claimedProofWork(t, s, "refresh without unrelated validation", proofActorAlpha, fixedNow.Add(time.Second))
	w, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: proofActorAlpha}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor:     proofActorAlpha,
		TTL:       30 * time.Second,
		Workspace: "/synthetic/repo-alpha",
		Command:   "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshProofResource(
		w.ID,
		acquired.Reservation.ID,
		proofActorAlpha,
		45*time.Second,
		fixedNow.Add(20*time.Second),
	); err != nil {
		t.Fatalf("proof refresh validated an unrelated RunAttempt's launch evidence under the admission lock: %v", err)
	}
}

func TestPreparedCloseFenceStillHoldsExactIntent(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "prepared close fence stays exact", proofActorAlpha, fixedNow)

	attempts := runattempt.Open(s.home)
	ra, err := attempts.Mint(w.ID, proofActorAlpha, "claude-code", fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	intent := runattempt.TerminalizationIntent{
		SchemaVersion:             runattempt.TerminalizationIntentSchemaVersion,
		Owner:                     runattempt.TerminalizationOwnerWorkClose,
		WorkID:                    w.ID,
		AuthorityActionIntentID:   "act_fence0000000000000000000000001",
		AuthorityPolicyDecisionID: "pol_fence000000000000000000000001",
		ExpectedWorkState:         "claimed",
		ExpectedWorkSHA256:        "sha256:" + strings.Repeat("0", 64),
		CloseInputSHA256:          "sha256:" + strings.Repeat("1", 64),
		CloseActionIntentID:       "act_fence0000000000000000000000002",
		ClosePolicyDecisionID:     "pol_fence000000000000000000000002",
		Outcome:                   "completed",
		TargetStatus:              runattempt.StatusSucceeded,
		ReceiptID:                 "rec_fence000000000000000000000001",
		TransitionID:              "tr_fence000000000000000000000001",
		PreparedAt:                fixedNow.Add(2 * time.Second),
	}
	if err := intent.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.PrepareTerminalization(ra.ID, intent); err != nil {
		t.Fatal(err)
	}

	_, err = s.Advance(w.ID, AdvanceInput{To: "running", Actor: proofActorAlpha}, fixedNow.Add(3*time.Second))
	if err == nil {
		t.Fatal("dangling prepared close no longer fences a lifecycle mutation")
	}
	if !strings.Contains(err.Error(), "has prepared close") {
		t.Fatalf("fence error = %v, want prepared-close refusal", err)
	}
}

func TestCompleteProofResourceWithoutHeartbeatIgnoresUnrelatedHistory(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "zero heartbeat completion", proofActorAlpha, fixedNow)
	w, err := s.Advance(w.ID, AdvanceInput{To: "running", Actor: proofActorAlpha}, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor: proofActorAlpha, TTL: 30 * time.Second, Workspace: "/synthetic/repo-alpha",
		Command: "compound proof-check --out /synthetic/report-alpha.json",
	}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	workID := w.ID
	// Routed receipt history is a bystander: completion must not
	// parse it to learn one reservation's refresh history.
	corruptUnrelatedEventSegment(t, s, "receipt")
	proofScanSeesCorruption(t, s, "rec_")

	report := canonicalOwnedProofReport()
	report["base_ref"] = ""
	report["generated_at"] = fixedNow.Add(22 * time.Second).Format(time.RFC3339)
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reportRef, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteProofResource(workID, ProofResourceCompleteInput{
		Actor:             proofActorAlpha,
		ReservationID:     acquired.Reservation.ID,
		ReportArtifactRef: reportRef,
		ProducerExitKnown: true,
		ProducerExitCode:  0,
	}, fixedNow.Add(23*time.Second))
	if err != nil {
		t.Fatalf("proof completion scanned unrelated event history under the admission lock: %v", err)
	}
	if completed.Result.Status != ProofResourceVerified {
		t.Fatalf("completion status = %q, want verified", completed.Result.Status)
	}
}

func TestProofRefreshRefsRetainLegacyLifecycleAttachments(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	w := claimedProofWork(t, s, "legacy refresh attachment discovery", proofActorAlpha, fixedNow)
	legacyID := "pfrh_legacy_attachment"
	if err := s.writeEventJSON(legacyID, map[string]string{
		"id": legacyID, "schema_version": ProofResourceRefreshSchemaVersion,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.appendEvent(Event{TS: ts(fixedNow.Add(time.Second)), WorkID: w.ID,
		From: w.State, To: w.State, By: proofActorAlpha, Attached: []string{legacyID},
	}); err != nil {
		t.Fatal(err)
	}
	refs, scoped, err := s.proofRefreshRefs(ProofResourceReservation{ID: "pfr_legacy_reservation", WorkID: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	if scoped || len(refs) != 1 || refs[0] != legacyID {
		t.Fatalf("legacy refs lost: %v scoped=%v", refs, scoped)
	}
}
