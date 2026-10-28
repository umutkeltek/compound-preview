package product

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/mem"
	"gopkg.in/yaml.v3"
)

func reviewCommitFixture(t *testing.T) (string, *mem.Store, Review) {
	t.Helper()
	h := t.TempDir()
	id := mintHypothesisDraftFixture(t, h, "product:review-fixture", "synthetic change", []string{"activation"})
	s, err := mem.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	return h, s, Review{SchemaVersion: ReviewSchemaVersion, ID: "review_a", ProductID: "product:review-fixture", HypothesisID: id, Result: "supported", Rationale: "Synthetic measurement supports the hypothesis.", Confidence: .8, NextAction: "retain", EvidenceRefs: []string{"product_signal:synthetic"}, ReviewedBy: "synthetic-reviewer"}
}

func TestReviewParsedSourceReplacementDoesNotStrandRecovery(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	path := filepath.Join(t.TempDir(), "review.yaml")
	bodyA, err := yaml.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.Result, b.Rationale = "refuted", "Different source revision."
	bodyB, err := yaml.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bodyA, 0o600); err != nil {
		t.Fatal(err)
	}
	oldRead := readReviewSourceFile
	t.Cleanup(func() { readReviewSourceFile = oldRead })
	// The real reader captures A, then the source changes before admission.
	readReviewSourceFile = func(path string) ([]byte, error) {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, bodyB, 0o600); err != nil {
			return nil, err
		}
		return body, nil
	}
	oldHistory := publishReviewHistory
	t.Cleanup(func() { publishReviewHistory = oldHistory })
	publishReviewHistory = func(string, Review) error { return errors.New("postcommit history failure") }
	failed, err := ApplyReviewFile(h, s, path, a.ReviewedBy, tProduct)
	readReviewSourceFile = oldRead
	publishReviewHistory = oldHistory
	if err == nil || failed.Status != "recovery_required" || failed.CandidateOutcome != "committed" {
		t.Fatalf("lost committed partial result: %+v %v", failed, err)
	}
	artifacts, err := home.NewArtifactStore(h)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := artifacts.GetPrivate(failed.SnapshotRef)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot reviewSnapshot
	if err := json.Unmarshal(frozen, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !equalReviewValue(snapshot.Request, a) || snapshot.SourceDigest != approvalCandidateDigest(bodyA) || snapshot.SourceDigest == approvalCandidateDigest(bodyB) {
		t.Fatalf("frozen input mixed source revisions: %+v", snapshot)
	}
	before, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyReviewFile(h, s, path, a.ReviewedBy, tProduct.Add(time.Hour)); err == nil {
		t.Fatal("replacement source B recovered request A")
	}
	if err := os.WriteFile(path, bodyA, 0o600); err != nil {
		t.Fatal(err)
	}
	// On replay, matching parsed fields are insufficient: compare the captured
	// bytes, not a later reread of the path restored to A.
	readReviewSourceFile = func(string) ([]byte, error) {
		return append(bytes.Clone(bodyA), []byte("\n# another source revision\n")...), nil
	}
	if _, err := ApplyReviewFile(h, s, path, a.ReviewedBy, tProduct.Add(time.Hour)); err == nil {
		t.Fatal("different captured bytes recovered from a later matching path read")
	}
	readReviewSourceFile = oldRead
	recovered, err := ApplyReviewFile(h, s, path, a.ReviewedBy, tProduct.Add(time.Hour))
	if err != nil || recovered.Status != "updated" || recovered.CandidateOutcome != "committed" {
		t.Fatalf("restored exact source A cannot recover: %+v %v", recovered, err)
	}
	if recovered.TransitionID != failed.TransitionID || recovered.EvidenceID != failed.EvidenceID {
		t.Fatalf("recovery changed operation: %+v -> %+v", failed, recovered)
	}
	after, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil || !bytes.Equal(before.Body, after.Body) {
		t.Fatal("recovery reapplied the candidate")
	}
}

func TestReviewProgrammaticSourceRemainsAProvenanceLabel(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	path := filepath.Join(t.TempDir(), "review.yaml")
	if err := os.WriteFile(path, []byte("not the bytes that produced the typed request"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := ApplyReview(h, s, a, path, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(h)
	if err != nil {
		t.Fatal(err)
	}
	body, err := artifacts.GetPrivate(report.SnapshotRef)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot reviewSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceDigest != "" || snapshot.SourcePath != path || !equalReviewValue(snapshot.Request, a) {
		t.Fatalf("programmatic request claimed unrelated file provenance: %+v", snapshot)
	}
}

func TestReviewReplayDoesNotOverwriteLaterMeasurement(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	first, err := ApplyReview(h, s, a, "review.yaml", tProduct)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.ID = "review_b"
	b.Result = "refuted"
	b.NextAction = "retire"
	if _, err := ApplyReview(h, s, b, "later.yaml", tProduct.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := ApplyReview(h, s, a, "review.yaml", tProduct.Add(2*time.Hour))
	if err != nil {
		t.Errorf("same operation must replay from original clock: %v", err)
	}
	after, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil || !bytes.Equal(before.Body, after.Body) {
		t.Errorf("replay overwrote later measurement: %v", err)
	}
	if replay.TransitionID != first.TransitionID {
		t.Errorf("replay changed transition: %+v -> %+v", first, replay)
	}
	events, err := eventstore.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	intentRecord, err := events.Read(reviewTransitionID("product_review_intent", a.ID))
	if err != nil {
		t.Fatal(err)
	}
	var intent envelope.Transition
	if err := json.Unmarshal(intentRecord.Bytes, &intent); err != nil {
		t.Fatal(err)
	}
	if intent.SecondOrder.FutureBehaviorChanged {
		t.Error("intent claims changed future behavior before commit")
	}
}

func TestReviewSameIDRejectsChangedReviewer(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	if _, err := ApplyReview(h, s, a, "review.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	a.ReviewedBy = "different-reviewer"
	before, _ := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if _, err := ApplyReview(h, s, a, "review.yaml", tProduct); err == nil {
		t.Fatal("same review ID accepted another reviewer")
	}
	after, _ := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if !bytes.Equal(before.Body, after.Body) {
		t.Fatal("changed reviewer mutated candidate before refusing")
	}
}

func TestReviewConcurrentReviewPreservesWinner(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	b := a
	b.ID = "review_b"
	b.Result = "refuted"
	injected := false
	restore := swapRecordLoopTransition(func(h string, tr envelope.Transition) error {
		if tr.ID == reviewTransitionID("product_review_intent", a.ID) && !injected {
			injected = true
			if _, err := ApplyReview(h, s, b, "later.yaml", tProduct.Add(time.Minute)); err != nil {
				return err
			}
		}
		return RecordTransition(h, tr)
	})
	defer restore()
	report, err := ApplyReview(h, s, a, "review.yaml", tProduct)
	if err == nil || report.Status != "refused" {
		t.Fatalf("stale review not refused: %+v %v", report, err)
	}
	c, err := loadHypothesisCandidate(h, a.HypothesisID)
	if err != nil || c.Result != b.Result || !reflect.DeepEqual(c.CommittedReviewIDs, []string{b.ID}) {
		t.Fatalf("lost winner: %+v %v", c, err)
	}
	reviews, err := loadReviews(h)
	if err != nil || len(reviews) != 1 || reviews[0].ID != b.ID {
		t.Fatalf("false accepted history: %+v %v", reviews, err)
	}
}

func TestReviewEveryPersistenceBoundary(t *testing.T) {
	for _, boundary := range []string{"intent_before", "intent_after", "candidate_before", "candidate_after", "outcome_before", "outcome_after", "history_before", "history_after", "artifacts_before", "artifacts_after"} {
		t.Run(boundary, func(t *testing.T) {
			h, s, a := reviewCommitFixture(t)
			injected := errors.New("injected " + boundary)
			restore := swapRecordLoopTransition(func(h string, tr envelope.Transition) error {
				isIntent := tr.ID == reviewTransitionID("product_review_intent", a.ID)
				isOutcome := tr.ID == reviewTransitionID("product_review", a.ID)
				if (boundary == "intent_before" && isIntent) || (boundary == "outcome_before" && isOutcome) {
					return injected
				}
				if err := RecordTransition(h, tr); err != nil {
					return err
				}
				if (boundary == "intent_after" && isIntent) || (boundary == "outcome_after" && isOutcome) {
					return injected
				}
				return nil
			})
			oldReplace, oldHistory, oldArtifacts := replaceReviewCandidate, publishReviewHistory, publishReviewArtifacts
			replaceReviewCandidate = func(h string, record candidates.Record, payload any) (candidates.ReplaceCandidateResult, error) {
				if boundary == "candidate_before" {
					return candidates.ReplaceCandidateResult{}, injected
				}
				r, err := candidates.ReplaceCandidateIfCurrent(h, record, payload)
				if err == nil && boundary == "candidate_after" {
					return r, injected
				}
				return r, err
			}
			publishReviewHistory = func(h string, review Review) error {
				if boundary == "history_before" {
					return injected
				}
				if err := appendUniqueReview(h, review); err != nil {
					return err
				}
				if boundary == "history_after" {
					return injected
				}
				return nil
			}
			publishReviewArtifacts = func(h string, review Review, path string, report ReviewReport, at time.Time) error {
				if boundary == "artifacts_before" {
					return injected
				}
				if err := recordReviewArtifactResult(h, review, path, report, at); err != nil {
					return err
				}
				if boundary == "artifacts_after" {
					return injected
				}
				return nil
			}
			report, applyErr := ApplyReview(h, s, a, "review.yaml", tProduct)
			restore()
			replaceReviewCandidate, publishReviewHistory, publishReviewArtifacts = oldReplace, oldHistory, oldArtifacts
			if boundary == "candidate_after" {
				if applyErr != nil || report.Status != "updated" || report.CandidateOutcome != "committed" {
					t.Fatalf("postrename ambiguity was not settled: %+v %v", report, applyErr)
				}
			} else if applyErr == nil || report.Status != "recovery_required" || report.ReviewID != a.ID {
				t.Fatalf("lost boundary result: %+v %v", report, applyErr)
			}
			candidate, err := loadHypothesisCandidate(h, a.HypothesisID)
			if err != nil {
				t.Fatal(err)
			}
			committed := strings.HasPrefix(boundary, "outcome") || strings.HasPrefix(boundary, "history") || strings.HasPrefix(boundary, "artifacts") || boundary == "candidate_after"
			if (candidate.Result == a.Result) != committed {
				t.Fatalf("wrong candidate outcome at %s: %+v", boundary, candidate)
			}
			before, _ := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
			recovered, err := ApplyReview(h, s, a, "review.yaml", tProduct.Add(time.Hour))
			if committed || boundary == "intent_before" {
				if err != nil || recovered.Status != "updated" || recovered.CandidateOutcome != "committed" {
					t.Errorf("recoverable fanout did not finish: %+v %v", recovered, err)
				}
			} else if err == nil || recovered.Status != "recovery_required" {
				t.Errorf("ambiguous precommit intent was silently retried: %+v %v", recovered, err)
			}
			after, _ := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
			if boundary != "intent_before" && !bytes.Equal(before.Body, after.Body) {
				t.Error("recovery reapplied candidate")
			}
		})
	}
}

func TestReviewSnapshotAndWitnessValidation(t *testing.T) {
	for _, tamper := range []string{"postimage", "request", "clock", "provenance", "generic_citation", "missing_witness"} {
		t.Run(tamper, func(t *testing.T) {
			h, s, a := reviewCommitFixture(t)
			restore := swapRecordLoopTransition(func(h string, tr envelope.Transition) error {
				if tr.ID == reviewTransitionID("product_review", a.ID) {
					return errors.New("defer outcome")
				}
				return RecordTransition(h, tr)
			})
			_, _ = ApplyReview(h, s, a, "review.yaml", tProduct)
			restore()
			events, _ := eventstore.Open(h)
			record, err := events.Read(reviewTransitionID("product_review_intent", a.ID))
			if err != nil {
				t.Fatal(err)
			}
			var intent envelope.Transition
			if err := json.Unmarshal(record.Bytes, &intent); err != nil {
				t.Fatal(err)
			}
			snapshot, _, err := readReviewSnapshot(h, intent)
			if err != nil {
				t.Fatal(err)
			}
			if tamper == "generic_citation" || tamper == "missing_witness" {
				candidate, _ := loadHypothesisCandidate(h, a.HypothesisID)
				candidate.CommittedReviewIDs = nil
				candidate.NextAction = "newer measurement"
				if tamper == "missing_witness" {
					candidate.EvidenceRefs = nil
				}
				if err := writeHypothesisCandidate(h, candidate); err != nil {
					t.Fatal(err)
				}
				report, err := ApplyReview(h, s, a, "review.yaml", tProduct.Add(time.Hour))
				if err == nil || report.Status != "recovery_required" {
					t.Fatalf("citation/no witness certified commit: %+v %v", report, err)
				}
				return
			}
			switch tamper {
			case "postimage":
				snapshot.Postimage = snapshot.Preimage
				snapshot.PostDigest = snapshot.PreDigest
			case "request":
				snapshot.Request.ReviewedBy = "forged"
			case "clock":
				snapshot.CreatedAt = "bad-time"
			case "provenance":
				snapshot.Review.SourceRef = "forged"
			}
			body, _ := json.Marshal(snapshot)
			artifacts, _ := home.NewArtifactStore(h)
			ref, err := artifacts.PutPrivate(body)
			if err != nil {
				t.Fatal(err)
			}
			intent.ActionRefs[len(intent.ActionRefs)-1] = ref
			if _, _, err := readReviewSnapshot(h, intent); err == nil {
				t.Fatal("tampered frozen operation accepted")
			}
		})
	}
}

func TestReviewRequestIdentityIsFrozen(t *testing.T) {
	for _, field := range []string{"result", "reviewer", "time", "source", "source_bytes", "hypothesis", "product"} {
		t.Run(field, func(t *testing.T) {
			h, s, a := reviewCommitFixture(t)
			path := filepath.Join(t.TempDir(), "review.yaml")
			body, err := yaml.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyReviewFile(h, s, path, a.ReviewedBy, tProduct); err != nil {
				t.Fatal(err)
			}
			before, _ := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
			originalID := a.HypothesisID
			switch field {
			case "result":
				a.Result = "refuted"
			case "reviewer":
				a.ReviewedBy = "another-reviewer"
			case "time":
				a.ReviewedAt = tProduct.Format(time.RFC3339)
			case "source":
				path += ".other"
			case "source_bytes":
				if err := os.WriteFile(path, []byte("changed source"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "hypothesis":
				a.HypothesisID = "other"
			case "product":
				a.ProductID = "product:other"
			}
			if report, err := ApplyReview(h, s, a, path, tProduct.Add(time.Hour)); err == nil || report.Status != "refused" {
				t.Errorf("changed identity accepted: %+v %v", report, err)
			}
			after, _ := candidates.ReadRecord(h, candidates.ProductHypothesis, originalID)
			if !bytes.Equal(before.Body, after.Body) {
				t.Error("identity refusal mutated candidate")
			}
		})
	}
}

func TestReviewConcurrentSameIDFreshProcesses(t *testing.T) {
	h, _, a := reviewCommitFixture(t)
	commands := make([]*exec.Cmd, 4)
	stdout, stderr := make([]bytes.Buffer, len(commands)), make([]bytes.Buffer, len(commands))
	started := 0
	defer func() {
		// A failed child must not leave its siblings writing into a removed fixture.
		for i := 0; i < started; i++ {
			if commands[i].ProcessState == nil {
				if err := commands[i].Wait(); err != nil {
					t.Errorf("unreaped child %d: %v\nstdout:\n%s\nstderr:\n%s", i, err, stdout[i].String(), stderr[i].String())
				}
			}
		}
	}()
	for i := range commands {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReviewCommitProcessHelper$")
		cmd.Env = append(os.Environ(), "COMPOUND_TEST_REVIEW_HOME="+h, "COMPOUND_TEST_REVIEW_HYPOTHESIS="+a.HypothesisID)
		cmd.Stdout, cmd.Stderr = &stdout[i], &stderr[i]
		commands[i] = cmd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		started++
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v\nstdout:\n%s\nstderr:\n%s", i, err, stdout[i].String(), stderr[i].String())
		}
	}
	s, err := mem.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyReview(h, s, a, "review.yaml", tProduct.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	reviews, err := loadReviews(h)
	if err != nil || len(reviews) != 1 || reviews[0].ReviewedAt != tProduct.Format(time.RFC3339) {
		t.Fatalf("concurrent replay duplicated or changed original review: %+v %v", reviews, err)
	}
}

func TestReviewCommitProcessHelper(t *testing.T) {
	h := os.Getenv("COMPOUND_TEST_REVIEW_HOME")
	if h == "" {
		return
	}
	s, err := mem.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	a := Review{SchemaVersion: ReviewSchemaVersion, ID: "review_a", ProductID: "product:review-fixture", HypothesisID: os.Getenv("COMPOUND_TEST_REVIEW_HYPOTHESIS"), Result: "supported", Rationale: "Synthetic measurement supports the hypothesis.", Confidence: .8, NextAction: "retain", EvidenceRefs: []string{"product_signal:synthetic"}, ReviewedBy: "synthetic-reviewer"}
	report, err := ApplyReview(h, s, a, "review.yaml", tProduct)
	if err != nil && !(report.Status == "recovery_required" && strings.Contains(err.Error(), "busy")) {
		t.Fatalf("same-ID process: %+v %v", report, err)
	}
}

func TestReviewRecoveryRejectsConflictingHistoryProvenance(t *testing.T) {
	for _, field := range []string{"reviewer", "time", "evidence", "partial_row"} {
		t.Run(field, func(t *testing.T) {
			h, s, a := reviewCommitFixture(t)
			if _, err := ApplyReview(h, s, a, "review.yaml", tProduct); err != nil {
				t.Fatal(err)
			}
			rows, err := loadReviews(h)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "reviewer":
				rows[0].ReviewedBy = "forged"
			case "time":
				rows[0].ReviewedAt = tProduct.Add(time.Hour).Format(time.RFC3339)
			case "evidence":
				rows[0].EvidenceID = "ev_forged"
			}
			body, err := json.Marshal(rows[0])
			if err != nil {
				t.Fatal(err)
			}
			if field == "partial_row" {
				body = []byte(`{"id":"review_a"`)
			}
			if err := os.WriteFile(reviewsLog(h), append(body, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			report, err := ApplyReview(h, s, a, "review.yaml", tProduct.Add(time.Hour))
			if err == nil || report.Status != "recovery_required" || report.CandidateOutcome != "committed" {
				t.Fatalf("conflicting committed history silently accepted: %+v %v", report, err)
			}
		})
	}
}

func TestReviewReplayPreservesLaterPlanStatus(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	plan := ReviewPlan{SchemaVersion: ReviewPlanSchemaVersion, ID: "review_plan_fixture", ProductID: a.ProductID, HypothesisID: a.HypothesisID, Status: "review_required", MetricName: "activation", Window: "seven days", SignalID: "sig_synthetic", ReviewDraft: ReviewPlanDraft{ReviewID: a.ID}}
	paths, err := RecordReviewPlan(h, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyReview(h, s, a, paths.ReviewYAMLPath, tProduct); err != nil {
		t.Fatal(err)
	}
	plan, err = loadReviewPlanArtifact(h, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan.Status = "archived"
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.PlanPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyReview(h, s, a, paths.ReviewYAMLPath, tProduct.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(paths.PlanPath)
	if err != nil || !bytes.Equal(body, after) {
		t.Fatal("old review replay rewrote a later plan status")
	}
}

func TestReviewRecoveryRepairsMarkdownAfterPlanJSONCommitted(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	plan := ReviewPlan{SchemaVersion: ReviewPlanSchemaVersion, ID: "review_plan_fixture", ProductID: a.ProductID, HypothesisID: a.HypothesisID, Status: "review_required", MetricName: "activation", Window: "seven days", SignalID: "sig_synthetic", ReviewDraft: ReviewPlanDraft{ReviewID: a.ID}}
	paths, err := RecordReviewPlan(h, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(paths.ReviewPath, paths.ReviewPath+".before"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.ReviewPath, 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := ApplyReview(h, s, a, paths.ReviewYAMLPath, tProduct)
	if err == nil || report.CandidateOutcome != "committed" || report.Status != "recovery_required" {
		t.Fatalf("lost partial plan publication: %+v %v", report, err)
	}
	if err := os.Remove(paths.ReviewPath); err != nil {
		t.Fatal(err)
	} // only the exact empty synthetic obstruction
	if _, err := ApplyReview(h, s, a, paths.ReviewYAMLPath, tProduct.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	current, err := loadReviewPlanArtifact(h, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(paths.ReviewPath)
	if err != nil || string(body) != RenderReviewPlan(current) {
		t.Errorf("Markdown fanout not repaired from current plan: %v", err)
	}
}

func TestReviewLegacyRowsRemainHistoricalWithoutReapplication(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	legacy := a
	legacy.ReviewedAt = tProduct.Format(time.RFC3339)
	legacy.AppliedAt = legacy.ReviewedAt
	legacy.EvidenceID = "ev_legacy"
	legacy.SourceRef = "legacy-source"
	if err := appendUniqueReview(h, legacy); err != nil {
		t.Fatal(err)
	}
	before, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	historyBefore, err := os.ReadFile(reviewsLog(h))
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyReview(h, s, a, "review.yaml", tProduct.Add(time.Hour))
	if err == nil || report.Status != "recovery_required" || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy row was re-certified: %+v %v", report, err)
	}
	after, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	historyAfter, err := os.ReadFile(reviewsLog(h))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Body, after.Body) || !bytes.Equal(historyBefore, historyAfter) {
		t.Fatal("legacy replay mutated historical facts or candidate")
	}
	rows, err := loadReviews(h)
	if err != nil || len(rows) != 1 || !sameReview(rows[0], legacy) {
		t.Fatalf("historical telemetry was hidden: %+v %v", rows, err)
	}
}

func TestReviewAppendPreservesValidLegacyRowWithoutNewline(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	legacy := a
	legacy.ID = "legacy_review"
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewsLog(h), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyReview(h, s, a, "review.yaml", tProduct); err != nil {
		t.Fatal(err)
	}
	rows, err := loadReviews(h)
	if err != nil || len(rows) != 2 || !sameReview(rows[0], legacy) {
		t.Fatalf("append corrupted valid historical bytes: %+v %v", rows, err)
	}
	after, err := os.ReadFile(reviewsLog(h))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, body) {
		t.Fatal("append rewrote legacy prefix")
	}
}

func TestReviewRejectsEmbeddedCandidateIdentityAndPreservesUnrelated(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	otherID := mintHypothesisDraftFixture(t, h, "product:other-fixture", "unrelated change", []string{"retention"})
	other, err := candidates.ReadRecord(h, candidates.ProductHypothesis, otherID)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := loadHypothesisCandidate(h, a.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	candidate.ID = otherID
	body, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h, candidates.InboxDir(candidates.ProductHypothesis), a.HypothesisID+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := ApplyReview(h, s, a, "review.yaml", tProduct)
	if err == nil || report.Status != "refused" {
		t.Fatalf("mismatched embedded identity accepted: %+v %v", report, err)
	}
	after, err := candidates.ReadRecord(h, candidates.ProductHypothesis, otherID)
	if err != nil || !bytes.Equal(other.Body, after.Body) {
		t.Fatal("identity refusal changed unrelated hypothesis")
	}
}

func TestReviewSurvivingWitnessWithoutIntentCannotBecomeNewOperation(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	c, err := loadHypothesisCandidate(h, a.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	c.CommittedReviewIDs = []string{a.ID}
	c.Result = "refuted"
	c.NextAction = "preserve prior decision"
	if err := writeHypothesisCandidate(h, c); err != nil {
		t.Fatal(err)
	}
	before, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyReview(h, s, a, "review.yaml", tProduct)
	if err == nil || report.Status != "recovery_required" {
		t.Errorf("surviving old witness allowed a new operation: %+v %v", report, err)
	}
	after, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil || !bytes.Equal(before.Body, after.Body) {
		t.Fatal("missing intent recovery overwrote prior result")
	}
}

func TestReviewSnapshotFailurePreservesEvidenceAndDoesNotAttemptCandidate(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	parent := filepath.Join(h, "private", "artifacts")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(parent, "sha256")); err != nil {
		t.Fatal(err)
	}
	before, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ApplyReview(h, s, a, "review.yaml", tProduct)
	if err == nil || report.Status != "refused" || report.CandidateOutcome != "not_attempted" || report.EvidenceID == "" {
		t.Errorf("snapshot failure lost already-recorded evidence or overstated outcome: %+v %v", report, err)
	}
	after, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil || !bytes.Equal(before.Body, after.Body) {
		t.Fatal("failed snapshot changed candidate")
	}
	events, err := eventstore.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Read(reviewTransitionID("product_review_intent", a.ID)); !os.IsNotExist(err) {
		t.Fatalf("failed snapshot published intent: %v", err)
	}
}

func TestReviewRefusalRecordingFailureRetainsKnownCandidateOutcome(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	restore := swapRecordLoopTransition(func(h string, tr envelope.Transition) error {
		if tr.ID == reviewTransitionID("product_review_intent", a.ID) {
			if _, err := HypothesisApprove(h, HypothesisApproveInput{CandidateID: a.HypothesisID, ExpectedEffect: "A concurrent approved expectation.", ReviewedBy: "synthetic-operator"}, tProduct.Add(time.Minute)); err != nil {
				return err
			}
		}
		if tr.ID == reviewTransitionID("product_review_refused", a.ID) {
			return errors.New("injected refusal publication failure")
		}
		return RecordTransition(h, tr)
	})
	defer restore()
	report, err := ApplyReview(h, s, a, "review.yaml", tProduct)
	if err == nil || report.Status != "recovery_required" || report.CandidateOutcome != "refused" {
		t.Fatalf("lost known CAS refusal on downstream failure: %+v %v", report, err)
	}
	reviews, err := loadReviews(h)
	if err != nil || len(reviews) != 0 {
		t.Fatalf("refused operation published accepted review: %+v %v", reviews, err)
	}
}

func TestReviewRefusesConcurrentApprovalWithoutPublishingReview(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	injected := false
	restore := swapRecordLoopTransition(func(h string, tr envelope.Transition) error {
		if strings.Contains(tr.StateDelta, "review intent") && !injected {
			injected = true
			if _, err := HypothesisApprove(h, HypothesisApproveInput{CandidateID: a.HypothesisID, ExpectedEffect: "A newer approved expectation.", ReviewedBy: "synthetic-operator"}, tProduct.Add(time.Minute)); err != nil {
				return err
			}
		}
		return RecordTransition(h, tr)
	})
	defer restore()
	report, err := ApplyReview(h, s, a, "review.yaml", tProduct)
	if !injected {
		t.Error("review never exposed its intent before candidate mutation")
	}
	if err == nil || report.Status != "refused" {
		t.Errorf("stale review must refuse: %+v err=%v", report, err)
	}
	c, err := loadHypothesisCandidate(h, a.HypothesisID)
	if err != nil || c.ExpectedEffect != "A newer approved expectation." || c.Result != "unknown" {
		t.Errorf("stale review lost approval: %+v err=%v", c, err)
	}
	reviews, err := loadReviews(h)
	if err != nil || len(reviews) != 0 {
		t.Errorf("refused review entered accepted history: %+v err=%v", reviews, err)
	}
}

func TestReviewCommittedOutcomeFailureRecoversWithoutReapplying(t *testing.T) {
	h, s, a := reviewCommitFixture(t)
	restore := swapRecordLoopTransition(func(h string, tr envelope.Transition) error {
		if tr.SourceTraceID == "product_review:"+a.ID && containsRef(tr.AfterRefs, "product_review:"+a.ID) {
			return errors.New("injected review outcome failure")
		}
		return RecordTransition(h, tr)
	})
	report, err := ApplyReview(h, s, a, "review.yaml", tProduct)
	restore()
	if err == nil || report.Status != "recovery_required" || report.ReviewID != a.ID {
		t.Errorf("lost committed operation on error: %+v err=%v", report, err)
	}
	c, err := loadHypothesisCandidate(h, a.HypothesisID)
	if err != nil || c.Result != "supported" {
		t.Fatalf("candidate did not commit: %+v err=%v", c, err)
	}
	reviews, err := loadReviews(h)
	if err != nil || len(reviews) != 0 {
		t.Errorf("unsettled review published accepted history: %+v err=%v", reviews, err)
	}
	b := a
	b.ID = "review_b"
	b.Result = "refuted"
	if _, err := ApplyReview(h, s, b, "later.yaml", tProduct.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil {
		t.Fatal(err)
	}
	report, err = ApplyReview(h, s, a, "review.yaml", tProduct.Add(2*time.Hour))
	if err != nil || report.Status != "updated" || report.CandidateOutcome != "committed" {
		t.Errorf("recovery failed: %+v err=%v", report, err)
	}
	after, err := candidates.ReadRecord(h, candidates.ProductHypothesis, a.HypothesisID)
	if err != nil || !bytes.Equal(before.Body, after.Body) {
		t.Error("recovery changed later measurement")
	}
	reviews, err = loadReviews(h)
	if err != nil || len(reviews) != 2 {
		t.Errorf("recovery did not repair history once: %+v err=%v", reviews, err)
	}
}
