package mem

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	homepkg "compound/internal/home"
)

func TestEngineDoctorProvesLedgerUnchanged(t *testing.T) {
	s := storeWith(t, "product:focus", "positioning", "private capture")
	before, err := s.LedgerDigest()
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.EngineDoctor("private capture", 10, &LexicalEngine{})
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.LedgerDigest()
	if err != nil {
		t.Fatal(err)
	}
	if !report.LedgerUnchanged || report.LedgerHashBefore != before || report.LedgerHashAfter != after || before != after {
		t.Fatalf("ledger should be unchanged: before=%s after=%s report=%+v", before, after, report)
	}
	if report.Engine.Name != "lexical" || report.Engine.CorpusGen != report.LedgerGen {
		t.Fatalf("engine generation should match ledger generation: %+v", report)
	}
	if len(report.Hits) != 1 || report.Hits[0].Entity != "product:focus" {
		t.Fatalf("hits = %+v", report.Hits)
	}
}

func TestEngineDoctorDetectsEngineLedgerMutation(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	report, err := s.EngineDoctor("ledger", 10, mutatingEngine{s: s})
	if err == nil || !strings.HasPrefix(err.Error(), "rented engine mutated owned ledger") {
		t.Fatalf("expected ledger mutation failure, got %v", err)
	}
	// The invariant must stay red AND name the offending ledger path the engine wrote.
	if !strings.Contains(err.Error(), "engine-owned-view") {
		t.Fatalf("engine mutation must name the offending ledger path: %v", err)
	}
	if len(report.EngineViolations) == 0 || !strings.Contains(strings.Join(report.EngineViolations, "; "), "engine-owned-view") {
		t.Fatalf("expected report.EngineViolations to name the engine-written path: %+v", report)
	}
}

// TestEngineDoctorFailsWhenAppendedRowLacksProvenance proves the retry does NOT weaken
// the invariant: a claim row appended to the ledger WITHOUT producer provenance (empty
// derivation) is engine-attributed, not a legitimate governed write, so the hop stays red
// and names the row.
func TestEngineDoctorFailsWhenAppendedRowLacksProvenance(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	report, err := s.EngineDoctor("ledger", 10, &provenancelessWriterEngine{store: s})
	if err == nil || !strings.HasPrefix(err.Error(), "rented engine mutated owned ledger") {
		t.Fatalf("a provenance-less appended row must stay a hard failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "without producer provenance") {
		t.Fatalf("failure must name the missing-provenance row: %v", err)
	}
	if len(report.EngineViolations) == 0 {
		t.Fatalf("expected an engine violation for the provenance-less row: %+v", report)
	}
}

// TestEngineDoctorDoesNotBlameEngineForConcurrentGovernedWrite is the real-world
// falsifier: a SEPARATE compound process appends a governed claim to the owned ledger
// while the doctor's breadth query runs, and the rented engine never re-indexes to the
// new generation mid-query (the realistic shape the pre-fix retry condition missed).
// The doctor must attribute the delta to a legitimate producer, retry once against the
// fresh gen, and pass with an INFO note — never blame the rented engine.
func TestEngineDoctorDoesNotBlameEngineForConcurrentGovernedWrite(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	eng := &externalWriterEngine{store: s}
	report, err := s.EngineDoctor("ledger", 10, eng)
	if err != nil {
		t.Fatalf("concurrent governed write must not be blamed on the rented engine: %v; report=%+v", err, report)
	}
	if !report.LedgerUnchanged || report.RetryCount != 1 || !report.OwnerLedgerAdvanced {
		t.Fatalf("expected one stable-snapshot retry after a concurrent governed write: %+v", report)
	}
	if report.AdvancedRows < 1 {
		t.Fatalf("expected advanced-row accounting for the concurrent governed write: %+v", report)
	}
	if !strings.Contains(report.Note, "concurrent legit") || !strings.Contains(report.Note, "fresh gen") {
		t.Fatalf("expected an INFO note naming concurrent legit writers verified against a fresh gen: %+v", report)
	}
	if len(report.EngineViolations) != 0 {
		t.Fatalf("a legit concurrent write must produce no engine violations: %+v", report)
	}
}

func TestEngineDoctorDegradesExplicitlyWhenUnavailable(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	report, err := s.EngineDoctor("ledger", 10, unavailableEngine{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Degraded || report.Error == "" || !report.LedgerUnchanged {
		t.Fatalf("expected explicit degraded unchanged report, got %+v", report)
	}
}

func TestEngineDoctorRetriesWhenOwnedLedgerAdvancesDuringProbe(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	eng := &ownerAdvancingEngine{store: s}
	report, err := s.EngineDoctor("ledger", 10, eng)
	if err != nil {
		t.Fatalf("owner advancement must not be blamed on rented engine: %v; report=%+v", err, report)
	}
	if !report.LedgerUnchanged || report.RetryCount != 1 || !report.OwnerLedgerAdvanced {
		t.Fatalf("expected one stable-snapshot retry after owner advancement: %+v", report)
	}
	if report.Engine.CorpusGen != report.LedgerGen || report.LedgerGenAfter != report.LedgerGen {
		t.Fatalf("stable report generations differ: %+v", report)
	}
}

func TestEngineDoctorReportsBusyUnderSustainedGovernedWrites(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	eng := &sustainedExternalWriterEngine{store: s}
	report, err := s.EngineDoctor("ledger", 10, eng)
	if !errors.Is(err, ErrLedgerBusy) {
		t.Fatalf("sustained governed writes must return typed busy result, got %v; report=%+v", err, report)
	}
	if report.LedgerUnchanged {
		t.Fatalf("hot-ledger proof must not claim a byte-stable window: %+v", report)
	}
	if !report.OwnerLedgerAdvanced || report.RetryCount != 1 || report.AdvancedRows < 2 {
		t.Fatalf("expected attributed owner activity across both attempts: %+v", report)
	}
	if len(report.EngineViolations) != 0 {
		t.Fatalf("governed appends are not engine violations: %+v", report)
	}
	if !strings.Contains(report.Note, "ownership preserved") || !strings.Contains(report.Note, "current synchronization not certified") {
		t.Fatalf("busy result must state exactly what was and was not proved: %+v", report)
	}
}

func TestDoctorEngineHopTreatsGovernedActivityAsSkippedNotFailed(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	var got Hop
	s.doctorEngineHop(&sustainedExternalWriterEngine{store: s}, func(name, status, detail string, extras ...hopExtras) {
		got = Hop{Name: name, Status: status, Detail: detail}
	})
	if got.Name != "sync/embedding" || got.Status != "skipped" {
		t.Fatalf("normal governed activity should be non-failing but not current-ok: %+v", got)
	}
	if !strings.Contains(got.Detail, "owner_ledger_advanced=true") || !strings.Contains(got.Detail, "current synchronization not certified") {
		t.Fatalf("skipped hop must explain the active-owner attribution: %+v", got)
	}
}

func TestEngineDoctorPreservesRetrySnapshotError(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	calls := 0
	s.ledgerDoctorSnapshotter = func() (ledgerDoctorSnapshot, error) {
		calls++
		if calls == 3 {
			return ledgerDoctorSnapshot{}, errors.New("retry snapshot unavailable")
		}
		return s.ledgerDoctorSnapshotFromDisk()
	}
	report, err := s.EngineDoctor("ledger", 10, &externalWriterEngine{store: s})
	if err == nil || !strings.Contains(err.Error(), "retry snapshot unavailable") {
		t.Fatalf("retry snapshot failure must survive unchanged, got err=%v report=%+v", err, report)
	}
	if len(report.EngineViolations) != 0 {
		t.Fatalf("snapshot failure is not an engine mutation: %+v", report)
	}
}

func TestEngineDoctorRejectsMutationDuringRetry(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	report, err := s.EngineDoctor("ledger", 10, &retryMutatingEngine{store: s})
	if err == nil || !strings.HasPrefix(err.Error(), "rented engine mutated owned ledger") {
		t.Fatalf("retry mutation must remain a hard failure, got %v; report=%+v", err, report)
	}
	if len(report.EngineViolations) == 0 || !strings.Contains(strings.Join(report.EngineViolations, "; "), "retry-engine-owned-view") {
		t.Fatalf("retry mutation must name the offending path: %+v", report)
	}
}

func TestLedgerDigestRejectsPathReplacementDuringRootedRead(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	const rel = "path-replacement-probe"
	target := filepath.Join(s.home, "ledger", rel)
	replacement := filepath.Join(s.home, "ledger", "path-replacement-candidate")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := ledgerDoctorAfterEntryLstat
	replaced := false
	ledgerDoctorAfterEntryLstat = func(observed string) {
		if observed != rel || replaced {
			return
		}
		replaced = true
		if err := os.Rename(replacement, target); err != nil {
			t.Errorf("replace ledger path: %v", err)
		}
	}
	t.Cleanup(func() { ledgerDoctorAfterEntryLstat = previous })

	if _, err := s.LedgerDigest(); err == nil || !strings.Contains(err.Error(), "changed during rooted read") {
		t.Fatalf("path replacement must fail rooted snapshot read, got %v", err)
	}
}

func TestLedgerDigestRejectsAncestorReplacementDuringRootedRead(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	const rel = "ancestor-replacement/probe"
	ledgerPath := filepath.Join(s.home, "ledger")
	ancestorPath := filepath.Join(ledgerPath, "ancestor-replacement")
	displacedPath := filepath.Join(ledgerPath, "ancestor-displaced")
	replacementPath := filepath.Join(ledgerPath, "ancestor-candidate")
	if err := os.Mkdir(ancestorPath, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ancestorPath, "probe")
	if err := os.WriteFile(target, []byte("stable inode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacementPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(replacementPath, "probe")); err != nil {
		t.Fatal(err)
	}
	previous := ledgerDoctorAfterEntryLstat
	replaced := false
	ledgerDoctorAfterEntryLstat = func(observed string) {
		if observed != rel || replaced {
			return
		}
		replaced = true
		if err := os.Rename(ancestorPath, displacedPath); err != nil {
			t.Errorf("displace ledger ancestor: %v", err)
			return
		}
		if err := os.Rename(replacementPath, ancestorPath); err != nil {
			t.Errorf("replace ledger ancestor: %v", err)
		}
	}
	t.Cleanup(func() { ledgerDoctorAfterEntryLstat = previous })

	if _, err := s.LedgerDigest(); err == nil || !strings.Contains(err.Error(), "path changed during rooted") {
		t.Fatalf("ancestor replacement must fail rooted snapshot read, got %v", err)
	}
}

func TestLedgerDigestRejectsRootReplacementDuringRootedRead(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	const rel = "root-replacement-probe"
	ledgerPath := filepath.Join(s.home, "ledger")
	displacedPath := filepath.Join(s.home, "ledger-displaced")
	if err := os.WriteFile(filepath.Join(ledgerPath, rel), []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := ledgerDoctorAfterEntryLstat
	replaced := false
	ledgerDoctorAfterEntryLstat = func(observed string) {
		if observed != rel || replaced {
			return
		}
		replaced = true
		if err := os.Rename(ledgerPath, displacedPath); err != nil {
			t.Errorf("displace ledger root: %v", err)
			return
		}
		if err := os.Mkdir(ledgerPath, 0o755); err != nil {
			t.Errorf("replace ledger root: %v", err)
		}
	}
	t.Cleanup(func() { ledgerDoctorAfterEntryLstat = previous })

	if _, err := s.LedgerDigest(); err == nil || !strings.Contains(err.Error(), "ledger root changed during rooted snapshot") {
		t.Fatalf("root replacement must fail rooted snapshot read, got %v", err)
	}
}

func TestLedgerDigestRejectsAppendAfterFileReadBeforeFinalLstat(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	const rel = "late-append-probe.jsonl"
	target := filepath.Join(s.home, "ledger", rel)
	if err := os.WriteFile(target, []byte("{\"row\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := ledgerDoctorBeforeFinalEntryLstat
	appended := false
	ledgerDoctorBeforeFinalEntryLstat = func(observed string) {
		if observed != rel || appended {
			return
		}
		appended = true
		file, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open late append target: %v", err)
			return
		}
		if _, err := file.Write([]byte("{\"row\":2}\n")); err != nil {
			t.Errorf("append after file stat: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Errorf("close late append target: %v", err)
		}
	}
	t.Cleanup(func() { ledgerDoctorBeforeFinalEntryLstat = previous })

	if _, err := s.LedgerDigest(); err == nil || !strings.Contains(err.Error(), "changed during rooted read") {
		t.Fatalf("append after fd stat must fail rooted snapshot read, got %v", err)
	}
}

func TestLedgerDigestRejectsEarlierFileAppendWhileLaterEntryIsRead(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	const earlierRel = "aa-earlier-read.jsonl"
	const laterRel = "zz-later-read.jsonl"
	earlierPath := filepath.Join(s.home, "ledger", earlierRel)
	if err := os.WriteFile(earlierPath, []byte("{\"row\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "ledger", laterRel), []byte("{\"row\":2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := ledgerDoctorAfterEntryLstat
	appended := false
	ledgerDoctorAfterEntryLstat = func(observed string) {
		if observed != laterRel || appended {
			return
		}
		appended = true
		file, err := os.OpenFile(earlierPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open earlier ledger file for append: %v", err)
			return
		}
		if _, err := file.Write([]byte("{\"row\":3}\n")); err != nil {
			t.Errorf("append earlier ledger file: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Errorf("close earlier ledger file: %v", err)
		}
	}
	t.Cleanup(func() { ledgerDoctorAfterEntryLstat = previous })

	if _, err := s.LedgerDigest(); err == nil || !strings.Contains(err.Error(), "changed during rooted enumeration") {
		t.Fatalf("later-entry work must not hide an append to an already-read file, got %v", err)
	}
}

func TestLedgerDoctorSnapshotRejectsReleaseAndReacquireAcrossInventory(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- homepkg.WithAutonomyAdmissionLock(s.home, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered
	held, firstEpoch, err := homepkg.AutonomyAdmissionLockState(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if !held || firstEpoch == "" {
		t.Fatalf("first acquisition has no epoch: held=%t epoch=%q", held, firstEpoch)
	}

	secondEntered := make(chan struct{})
	releaseSecond := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- homepkg.WithAutonomyAdmissionLock(s.home, func() error {
			close(secondEntered)
			<-releaseSecond
			return nil
		})
	}()
	previous := ledgerDoctorDirectoryRead
	reacquired := false
	ledgerDoctorDirectoryRead = func(label, phase string) {
		if label != "." || phase != "before" || reacquired {
			return
		}
		reacquired = true
		close(releaseFirst)
		if err := <-firstDone; err != nil {
			t.Errorf("release first acquisition: %v", err)
			return
		}
		<-secondEntered
	}
	t.Cleanup(func() { ledgerDoctorDirectoryRead = previous })

	snapshot, snapshotErr := s.ledgerDoctorSnapshotFromDisk()
	close(releaseSecond)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	if !reacquired {
		t.Fatal("snapshot did not cross the release/reacquire seam")
	}
	if snapshot.ownerLockEpoch != "" {
		t.Fatalf("two acquisitions were misreported as one continuous epoch %q", snapshot.ownerLockEpoch)
	}
}

func TestLedgerDigestPinsOriginalFileSetAcrossAmbientReplaceAndRestore(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	ledgerPath := filepath.Join(s.home, "ledger")
	if err := os.WriteFile(filepath.Join(ledgerPath, "original-file-set"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := s.LedgerDigest()
	if err != nil {
		t.Fatal(err)
	}
	parkedPath := filepath.Join(s.home, "ledger-pinned-original")
	decoyPath := filepath.Join(s.home, "ledger-decoy")
	if err := os.Mkdir(decoyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyPath, "decoy-file-set"), []byte("decoy"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := ledgerDoctorDirectoryRead
	swapped := false
	completed := false
	ledgerDoctorDirectoryRead = func(label, phase string) {
		if label != "." || completed {
			return
		}
		switch phase {
		case "before":
			if err := os.Rename(ledgerPath, parkedPath); err != nil {
				t.Errorf("park pinned ledger root: %v", err)
				return
			}
			if err := os.Rename(decoyPath, ledgerPath); err != nil {
				t.Errorf("install decoy ledger root: %v", err)
				return
			}
			swapped = true
		case "after":
			if !swapped {
				return
			}
			if err := os.Rename(ledgerPath, decoyPath); err != nil {
				t.Errorf("remove decoy ledger root: %v", err)
				return
			}
			if err := os.Rename(parkedPath, ledgerPath); err != nil {
				t.Errorf("restore pinned ledger root: %v", err)
				return
			}
			swapped = false
			completed = true
		}
	}
	t.Cleanup(func() { ledgerDoctorDirectoryRead = previous })

	got, err := s.LedgerDigest()
	if err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("ambient replace/restore seam did not execute")
	}
	if got != want {
		t.Fatalf("ambient decoy changed pinned ledger file set: got %s want %s", got, want)
	}
}

func TestUnresolvedArtifactProbePinsOriginalRootAcrossAmbientReplaceAndRestore(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	const rel = ".execution-route-evidence.jsonl.1.aaaaaaaaaaaa.bbbbbbbbbbbb.swap"
	ledgerPath := filepath.Join(s.home, "ledger")
	if err := os.WriteFile(filepath.Join(ledgerPath, rel), []byte("pending"), 0o644); err != nil {
		t.Fatal(err)
	}
	parkedPath := filepath.Join(s.home, "ledger-pending-original")
	decoyPath := filepath.Join(s.home, "ledger-pending-decoy")
	if err := os.Mkdir(decoyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := ledgerDoctorArtifactProbe
	swapped := false
	ledgerDoctorArtifactProbe = func(observed, phase string) {
		if observed != rel {
			return
		}
		switch phase {
		case "before":
			if err := os.Rename(ledgerPath, parkedPath); err != nil {
				t.Errorf("park pending ledger root: %v", err)
				return
			}
			if err := os.Rename(decoyPath, ledgerPath); err != nil {
				t.Errorf("install pending decoy root: %v", err)
				return
			}
			swapped = true
		case "after":
			if !swapped {
				return
			}
			if err := os.Rename(ledgerPath, decoyPath); err != nil {
				t.Errorf("remove pending decoy root: %v", err)
				return
			}
			if err := os.Rename(parkedPath, ledgerPath); err != nil {
				t.Errorf("restore pending ledger root: %v", err)
				return
			}
			swapped = false
		}
	}
	t.Cleanup(func() { ledgerDoctorArtifactProbe = previous })

	violations := s.unresolvedConcurrentLedgerArtifacts([]string{rel})
	if !strings.Contains(strings.Join(violations, "; "), "remained unresolved") {
		t.Fatalf("rooted artifact probe missed pending sibling behind ambient decoy: %+v", violations)
	}
}

func TestQueryBreadthRefusesEngineGenerationAheadOfOwnedLedger(t *testing.T) {
	s := storeWith(t, "system:compound", "owner", "ledger")
	eng := &aheadEngine{gen: s.currentGen() + 1}
	_, err := s.QueryBreadth("ledger", 10, eng)
	if !errors.Is(err, ErrEngineAhead) {
		t.Fatalf("expected engine-ahead refusal, got %v", err)
	}
	if eng.indexed {
		t.Fatal("owner boundary must not rebuild an ahead engine backwards")
	}
}

type mutatingEngine struct {
	s *Store
}

func (e mutatingEngine) Capabilities() EngineCapabilities {
	return EngineCapabilities{Name: "bad-ledger-writer", Breadth: true}
}

func (e mutatingEngine) Index([]Claim, int64) error {
	return os.WriteFile(filepath.Join(e.s.home, "ledger", "engine-owned-view"), []byte("bad"), 0o644)
}

func (e mutatingEngine) Query(string, string, []string, int) ([]Hit, error) {
	return nil, nil
}

type unavailableEngine struct{}

func (unavailableEngine) Capabilities() EngineCapabilities {
	return EngineCapabilities{Name: "down", Breadth: true}
}

func (unavailableEngine) Index([]Claim, int64) error {
	return ErrEngineUnavailable
}

func (unavailableEngine) Query(string, string, []string, int) ([]Hit, error) {
	return nil, errors.New("should not query after unavailable index")
}

type ownerAdvancingEngine struct {
	store    *Store
	base     LexicalEngine
	advanced bool
}

func (e *ownerAdvancingEngine) Capabilities() EngineCapabilities {
	caps := e.base.Capabilities()
	caps.Name = "owner-advancement-fixture"
	return caps
}

func (e *ownerAdvancingEngine) Index(claims []Claim, gen int64) error {
	return e.base.Index(claims, gen)
}

func (e *ownerAdvancingEngine) Query(text, gate string, allowlist []string, k int) ([]Hit, error) {
	if !e.advanced {
		e.advanced = true
		err := e.store.withLock(func() error {
			seq, err := e.store.nextSeq()
			if err != nil {
				return err
			}
			return appendJSONL(e.store.claimsLog(), Claim{
				ID: "claim_owner_advanced", Seq: seq, Entity: "owner", Predicate: "advanced",
				Object: "concurrent legitimate write", ValidFrom: t0.Format("2006-01-02T15:04:05Z07:00"),
				Confidence: 1, Derivation: "deterministic", Visibility: "operational",
			})
		})
		if err != nil {
			return nil, err
		}
		_ = e.base.Index(append(e.base.claims, Claim{ID: "claim_owner_advanced"}), e.store.currentGen())
	}
	return e.base.Query(text, gate, allowlist, k)
}

// externalWriterEngine models the REAL concurrent-write shape: a separate compound
// process (NOT the rented engine) appends a governed claim to the owned ledger while the
// doctor's breadth query runs. Unlike ownerAdvancingEngine, it deliberately does NOT
// re-index itself to the new generation — the writer is a different actor, so the engine
// stays at the pre-query gen. This is exactly the case the old
// `report.Engine.CorpusGen == afterGen` retry gate could never fire for.
type externalWriterEngine struct {
	store *Store
	base  LexicalEngine
	wrote bool
}

// sustainedExternalWriterEngine models a legitimately hot ledger: a different
// Compound owner appends a governed row during every rented-engine query.
type sustainedExternalWriterEngine struct {
	store  *Store
	base   LexicalEngine
	writes int
}

func (e *sustainedExternalWriterEngine) Capabilities() EngineCapabilities {
	caps := e.base.Capabilities()
	caps.Name = "sustained-external-writer-fixture"
	return caps
}

func (e *sustainedExternalWriterEngine) Index(claims []Claim, gen int64) error {
	return e.base.Index(claims, gen)
}

func (e *sustainedExternalWriterEngine) Query(text, gate string, allowlist []string, k int) ([]Hit, error) {
	e.writes++
	id := fmt.Sprintf("claim_external_legit_%d", e.writes)
	if err := appendGovernedDoctorClaim(e.store, id); err != nil {
		return nil, err
	}
	return e.base.Query(text, gate, allowlist, k)
}

// retryMutatingEngine first creates the legitimate owner-activity condition,
// then writes an engine-owned artifact during the bounded retry.
type retryMutatingEngine struct {
	store   *Store
	base    LexicalEngine
	queries int
}

func (e *retryMutatingEngine) Capabilities() EngineCapabilities {
	caps := e.base.Capabilities()
	caps.Name = "retry-mutating-fixture"
	return caps
}

func (e *retryMutatingEngine) Index(claims []Claim, gen int64) error {
	return e.base.Index(claims, gen)
}

func (e *retryMutatingEngine) Query(text, gate string, allowlist []string, k int) ([]Hit, error) {
	e.queries++
	if e.queries == 1 {
		if err := appendGovernedDoctorClaim(e.store, "claim_retry_legit"); err != nil {
			return nil, err
		}
	} else {
		if err := os.WriteFile(filepath.Join(e.store.home, "ledger", "retry-engine-owned-view"), []byte("bad"), 0o644); err != nil {
			return nil, err
		}
	}
	return e.base.Query(text, gate, allowlist, k)
}

func appendGovernedDoctorClaim(store *Store, id string) error {
	return store.withLock(func() error {
		seq, err := store.nextSeq()
		if err != nil {
			return err
		}
		return appendJSONL(store.claimsLog(), Claim{
			ID: id, Seq: seq, Entity: "other-session", Predicate: "closed",
			Object: "governed sustained concurrent write", ValidFrom: t0.Format("2006-01-02T15:04:05Z07:00"),
			Confidence: 1, Derivation: "deterministic", Visibility: "operational",
		})
	})
}

func (e *externalWriterEngine) Capabilities() EngineCapabilities {
	caps := e.base.Capabilities()
	caps.Name = "external-writer-fixture"
	return caps
}

func (e *externalWriterEngine) Index(claims []Claim, gen int64) error {
	return e.base.Index(claims, gen)
}

func (e *externalWriterEngine) Query(text, gate string, allowlist []string, k int) ([]Hit, error) {
	if !e.wrote {
		e.wrote = true
		err := e.store.withLock(func() error {
			seq, err := e.store.nextSeq()
			if err != nil {
				return err
			}
			return appendJSONL(e.store.claimsLog(), Claim{
				ID: "claim_external_legit", Seq: seq, Entity: "other-session", Predicate: "closed",
				Object: "governed concurrent write", ValidFrom: t0.Format("2006-01-02T15:04:05Z07:00"),
				Confidence: 1, Derivation: "deterministic", Visibility: "operational",
			})
		})
		if err != nil {
			return nil, err
		}
		// Deliberately NOT re-indexing the base engine to the new gen: the external writer
		// is a different actor; the rented engine remains at the pre-query generation.
	}
	return e.base.Query(text, gate, allowlist, k)
}

// provenancelessWriterEngine appends a claim row that skips the governed door's producer
// provenance (empty derivation). It stands in for a rented engine masquerading a write as
// a legit concurrent one — the doctor must still refuse it.
type provenancelessWriterEngine struct {
	store *Store
	base  LexicalEngine
	wrote bool
}

func (e *provenancelessWriterEngine) Capabilities() EngineCapabilities {
	caps := e.base.Capabilities()
	caps.Name = "provenanceless-writer-fixture"
	return caps
}

func (e *provenancelessWriterEngine) Index(claims []Claim, gen int64) error {
	return e.base.Index(claims, gen)
}

func (e *provenancelessWriterEngine) Query(text, gate string, allowlist []string, k int) ([]Hit, error) {
	if !e.wrote {
		e.wrote = true
		err := e.store.withLock(func() error {
			seq, err := e.store.nextSeq()
			if err != nil {
				return err
			}
			return appendJSONL(e.store.claimsLog(), Claim{
				ID: "claim_no_provenance", Seq: seq, Entity: "engine", Predicate: "wrote",
				Object: "row without provenance", ValidFrom: t0.Format("2006-01-02T15:04:05Z07:00"),
				Confidence: 1, Derivation: "", Visibility: "operational",
			})
		})
		if err != nil {
			return nil, err
		}
	}
	return e.base.Query(text, gate, allowlist, k)
}

type aheadEngine struct {
	gen     int64
	indexed bool
}

func (e *aheadEngine) Capabilities() EngineCapabilities {
	return EngineCapabilities{Name: "ahead", Breadth: true, CorpusGen: e.gen}
}
func (e *aheadEngine) Index([]Claim, int64) error                         { e.indexed = true; return nil }
func (e *aheadEngine) Query(string, string, []string, int) ([]Hit, error) { return nil, nil }
