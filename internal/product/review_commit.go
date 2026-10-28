package product

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/mem"
)

// Review intent and outcomes use the existing transition owner. The private
// artifact freezes one operation; it is not a second hypothesis authority.
type reviewSnapshot struct {
	SchemaVersion string `json:"schema_version"`
	Request       Review `json:"request"`
	Review        Review `json:"review"`
	SourcePath    string `json:"source_path"`
	SourceDigest  string `json:"source_digest,omitempty"`
	CreatedAt     string `json:"created_at"`
	Preimage      []byte `json:"preimage"`
	PreDigest     string `json:"preimage_digest"`
	PreCustody    string `json:"preimage_custody_token"`
	Postimage     []byte `json:"proposed_postimage"`
	PostDigest    string `json:"proposed_postimage_digest"`
}

const reviewSnapshotSchema = "compound.product_review_snapshot.v0.1"

// Persistence seams retain real storage behavior in production and allow
// deterministic failure/interleaving tests at the actual commit boundaries.
var replaceReviewCandidate = candidates.ReplaceCandidateIfCurrent
var publishReviewHistory = appendUniqueReview
var publishReviewArtifacts = recordReviewArtifactResult
var openReviewOperationFile = (*os.Root).OpenFile

// ApplyReviewFile admits the parsed file revision, even if the path is replaced
// afterward. The operator's reviewer override is frozen with that exact input.
func ApplyReviewFile(homePath string, store *mem.Store, sourcePath, reviewedBy string, now time.Time) (ReviewReport, error) {
	request, digest, err := loadReviewSource(sourcePath)
	if err != nil {
		return reviewBaseReport(request), err
	}
	request.ReviewedBy = strings.TrimSpace(reviewedBy)
	return applyReview(homePath, store, request, sourcePath, digest, now)
}

// ApplyReview accepts a programmatic request. Its source is a provenance label,
// not evidence that a file's bytes produced the typed value. File-backed callers
// use ApplyReviewFile to bind parsing and fingerprinting to one captured read.
func ApplyReview(homePath string, store *mem.Store, request Review, sourcePath string, now time.Time) (ReviewReport, error) {
	return applyReview(homePath, store, request, sourcePath, "", now)
}

func applyReview(homePath string, store *mem.Store, request Review, sourcePath, sourceDigest string, now time.Time) (report ReviewReport, resultErr error) {
	report = reviewBaseReport(request)
	if err := ValidateReview(request); err != nil {
		return report, err
	}
	if request.AppliedAt != "" || request.EvidenceID != "" || request.SourceRef != "" {
		return report, fmt.Errorf("review application provenance is owner-written, not request input")
	}
	// A per-operation transient lock prevents concurrent retries from mistaking
	// an in-flight CAS for an interrupted one. Contention returns immediately;
	// no global review/work lock is held across candidate publication.
	release, err := lockReviewOperation(homePath, request.ID)
	if err != nil {
		return reviewRecovery(report, err)
	}
	defer func() {
		if err := release(); err != nil {
			report.Status = "recovery_required"
			resultErr = errors.Join(resultErr, err)
		}
	}()
	events, err := eventstore.Open(homePath)
	if err != nil {
		return report, err
	}
	intentRecord, err := events.Read(reviewTransitionID("product_review_intent", request.ID))
	if err == nil {
		var intent envelope.Transition
		if err := json.Unmarshal(intentRecord.Bytes, &intent); err != nil {
			return reviewRecovery(report, err)
		}
		snapshot, ref, err := readReviewSnapshot(homePath, intent)
		if err != nil {
			return reviewRecovery(report, err)
		}
		report.SnapshotRef = ref
		if !equalReviewValue(request, snapshot.Request) || cleanReviewSource(sourcePath) != snapshot.SourcePath {
			return report, fmt.Errorf("review %s already has a frozen request; reviewer, payload, time and source cannot change", request.ID)
		}
		if err := checkReviewSource(snapshot, sourceDigest); err != nil {
			return report, err
		}
		return settleReview(homePath, snapshot, ref)
	}
	if !os.IsNotExist(err) {
		return reviewRecovery(report, err)
	}
	// Legacy rows remain historical telemetry. Without a frozen operation they
	// cannot be replayed as a new write or certified by a generic citation.
	reviews, err := loadReviews(homePath)
	if err != nil {
		return report, err
	}
	for _, existing := range reviews {
		if existing.ID == request.ID {
			return reviewRecovery(report, fmt.Errorf("legacy review has no frozen intent; candidate left unchanged"))
		}
	}
	before, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, request.HypothesisID)
	if err != nil {
		return report, err
	}
	var candidate HypothesisCandidate
	if err := json.Unmarshal(before.Body, &candidate); err != nil {
		return report, err
	}
	if candidate.ID != request.HypothesisID || candidate.ProductID != request.ProductID {
		return report, fmt.Errorf("review identity does not match the exact hypothesis record")
	}
	if slices.Contains(candidate.CommittedReviewIDs, request.ID) {
		return reviewRecovery(report, fmt.Errorf("surviving review witness has no frozen intent; do not mint a replacement operation"))
	}
	review := request
	if review.ReviewedAt == "" {
		review.ReviewedAt = now.UTC().Format(time.RFC3339)
	}
	if _, err := time.Parse(time.RFC3339Nano, review.ReviewedAt); err != nil {
		return report, fmt.Errorf("reviewed_at: %w", err)
	}
	review.AppliedAt = now.UTC().Format(time.RFC3339)
	review.SourceRef = "product-review:" + filepath.Base(sourcePath)
	evidence, err := store.RecordEvidence(mem.EvidenceItem{SourceKind: "repo", SourceID: review.SourceRef, QuoteText: review.Rationale, Actor: review.ReviewedBy}, now)
	if err != nil {
		return report, err
	}
	review.EvidenceID = evidence.ID
	report.EvidenceID = evidence.ID
	postimage, err := approvalCandidateImage(projectReview(candidate, review))
	if err != nil {
		return report, err
	}
	snapshot := reviewSnapshot{SchemaVersion: reviewSnapshotSchema, Request: request, Review: review, SourcePath: cleanReviewSource(sourcePath), SourceDigest: sourceDigest, CreatedAt: now.UTC().Format(time.RFC3339Nano), Preimage: before.Body, PreDigest: before.Digest, PreCustody: before.CustodyToken, Postimage: postimage, PostDigest: approvalCandidateDigest(postimage)}
	ref, err := freezeReview(homePath, snapshot)
	if err != nil {
		return report, err
	}
	report.SnapshotRef = ref
	intent, err := reviewSnapshotTransition(snapshot, ref, "product_review_intent")
	if err != nil {
		return report, err
	}
	if err := recordLoopTransition(homePath, intent); err != nil {
		return reviewRecovery(report, err)
	}
	var after HypothesisCandidate
	if err := json.Unmarshal(postimage, &after); err != nil {
		return reviewRecovery(report, err)
	}
	replaced, err := replaceReviewCandidate(homePath, before, after)
	if err != nil {
		// Publication may have won before sync/ack failed. Only the surviving
		// dedicated witness or exact postimage may settle this ambiguity.
		settled, settleErr := settleReview(homePath, snapshot, ref)
		if settleErr == nil {
			return settled, nil
		}
		return settled, errors.Join(err, settleErr)
	}
	if !replaced.Replaced {
		report.CandidateOutcome = "refused"
		refused, err := reviewSnapshotTransition(snapshot, ref, "product_review_refused")
		if err != nil {
			return reviewRecovery(report, err)
		}
		if err := recordLoopTransition(homePath, refused); err != nil {
			return reviewRecovery(report, err)
		}
		report.Status, report.CandidateOutcome, report.TransitionID = "refused", "refused", refused.ID
		return report, fmt.Errorf("review %s refused: hypothesis changed since its gate read; no review result was written", request.ID)
	}
	return settleReview(homePath, snapshot, ref)
}

func projectReview(candidate HypothesisCandidate, review Review) HypothesisCandidate {
	candidate.Result, candidate.NextAction = review.Result, review.NextAction
	candidate.CommittedReviewIDs = compact(append(candidate.CommittedReviewIDs, review.ID))
	candidate.EvidenceRefs = compact(append(append(candidate.EvidenceRefs, review.EvidenceID, "product_review:"+review.ID), review.EvidenceRefs...))
	return candidate
}

func reviewBaseReport(review Review) ReviewReport {
	return ReviewReport{Status: "refused", CandidateOutcome: "not_attempted", ProductID: review.ProductID, HypothesisID: review.HypothesisID, ReviewID: review.ID, Result: review.Result, ReviewedBy: review.ReviewedBy}
}

func RenderReviewReport(report ReviewReport) string {
	return fmt.Sprintf("# product review %s\n\n- status: %s\n- candidate_outcome: %s\n- product: %s\n- hypothesis: %s\n- result: %s\n- reviewed_by: %s\n- evidence: %s\n- evidence_refs: %d\n- transition: %s\n- snapshot: %s\n",
		report.ReviewID, report.Status, report.CandidateOutcome, report.ProductID, report.HypothesisID, report.Result, report.ReviewedBy, report.EvidenceID, len(report.EvidenceRefs), report.TransitionID, report.SnapshotRef)
}

func reviewRecovery(report ReviewReport, err error) (ReviewReport, error) {
	report.Status = "recovery_required"
	if report.CandidateOutcome == "not_attempted" {
		report.CandidateOutcome = "unknown"
	}
	return report, fmt.Errorf("review %s recovery_required: %w; retry the exact original review and reviewer to reconcile without reapplying; missing commit evidence or corrupt history requires evidence repair, not repeated retries", report.ReviewID, err)
}

func cleanReviewSource(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return filepath.Clean(path)
}

func checkReviewSource(snapshot reviewSnapshot, capturedDigest string) error {
	if capturedDigest != "" {
		if capturedDigest != snapshot.SourceDigest {
			return fmt.Errorf("original review source bytes changed")
		}
		return nil
	}
	if snapshot.SourceDigest == "" {
		return nil
	}
	body, err := os.ReadFile(snapshot.SourcePath)
	if err != nil {
		return fmt.Errorf("read original review source: %w", err)
	}
	if approvalCandidateDigest(body) != snapshot.SourceDigest {
		return fmt.Errorf("original review source bytes changed")
	}
	return nil
}

func equalReviewValue(a, b any) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	return err == nil && bytes.Equal(x, y)
}

func freezeReview(homePath string, snapshot reviewSnapshot) (string, error) {
	body, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		return "", err
	}
	return artifacts.PutPrivate(body)
}

func reviewTransitionID(kind, reviewID string) string {
	// Review IDs are globally unique in the existing review history, even when
	// a caller supplies another product/hypothesis under the same ID.
	return stableID("tr_", kind, reviewID)
}

func reviewSnapshotTransition(snapshot reviewSnapshot, ref, kind string) (envelope.Transition, error) {
	var before, after HypothesisCandidate
	if err := json.Unmarshal(snapshot.Preimage, &before); err != nil {
		return envelope.Transition{}, err
	}
	if err := json.Unmarshal(snapshot.Postimage, &after); err != nil {
		return envelope.Transition{}, err
	}
	refs := append([]string{"product_hypothesis:" + before.ID}, before.EvidenceRefs...)
	afterRefs := refs
	summary := "review intent; no committed review outcome until the exact-generation CAS settles"
	switch kind {
	case "product_review_intent":
	case "product_review_refused":
		summary = "review refused: candidate changed since the gate read; this review wrote no result"
	case "product_review":
		afterRefs = append([]string{"product_hypothesis:" + after.ID}, after.EvidenceRefs...)
		summary = fmt.Sprintf("review %s committed result %s in its recorded generation; later measurement unchanged; no product surface changed", snapshot.Review.ID, snapshot.Review.Result)
	default:
		return envelope.Transition{}, fmt.Errorf("unknown review transition %q", kind)
	}
	at, err := time.Parse(time.RFC3339Nano, snapshot.CreatedAt)
	if err != nil {
		return envelope.Transition{}, err
	}
	tr, err := BuildLoopTransition(LoopTransitionInput{Kind: kind, ProductID: before.ProductID, ObjectID: snapshot.Review.ID, Actor: snapshot.Review.ReviewedBy, SourceTrace: "product_review:" + snapshot.Review.ID, BeforeRefs: refs, AfterRefs: afterRefs, EvidenceRefs: compact(append(append([]string{}, snapshot.Review.EvidenceRefs...), snapshot.Review.EvidenceID, ref)), Summary: summary}, at)
	tr.ID = reviewTransitionID(kind, snapshot.Review.ID)
	tr.ActionRefs = append(tr.ActionRefs, ref)
	tr.SecondOrder.FutureBehaviorChanged = kind == "product_review"
	return tr, err
}

func readReviewSnapshot(homePath string, intent envelope.Transition) (reviewSnapshot, string, error) {
	var snapshot reviewSnapshot
	ref := ""
	for _, action := range intent.ActionRefs {
		if strings.HasPrefix(action, home.ArtifactRefPrefix) {
			if ref != "" {
				return snapshot, ref, fmt.Errorf("review intent has multiple frozen payloads")
			}
			ref = action
		}
	}
	if ref == "" {
		return snapshot, ref, fmt.Errorf("review intent has no frozen payload")
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		return snapshot, ref, err
	}
	body, err := artifacts.GetPrivate(ref)
	if err != nil {
		return snapshot, ref, err
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return snapshot, ref, err
	}
	if snapshot.SchemaVersion != reviewSnapshotSchema || snapshot.PreCustody == "" || snapshot.PreDigest != approvalCandidateDigest(snapshot.Preimage) || snapshot.PostDigest != approvalCandidateDigest(snapshot.Postimage) {
		return snapshot, ref, fmt.Errorf("invalid frozen review generation evidence")
	}
	if err := ValidateReview(snapshot.Request); err != nil {
		return snapshot, ref, err
	}
	var before HypothesisCandidate
	if err := json.Unmarshal(snapshot.Preimage, &before); err != nil {
		return snapshot, ref, err
	}
	if slices.Contains(before.CommittedReviewIDs, snapshot.Request.ID) {
		return snapshot, ref, fmt.Errorf("review witness already existed before the frozen operation")
	}
	at, err := time.Parse(time.RFC3339Nano, snapshot.CreatedAt)
	if err != nil {
		return snapshot, ref, err
	}
	expectedReview := snapshot.Request
	if expectedReview.ReviewedAt == "" {
		expectedReview.ReviewedAt = at.UTC().Format(time.RFC3339)
	}
	expectedReview.AppliedAt = at.UTC().Format(time.RFC3339)
	expectedReview.SourceRef = "product-review:" + filepath.Base(snapshot.SourcePath)
	expectedReview.EvidenceID = snapshot.Review.EvidenceID
	if snapshot.Request.AppliedAt != "" || snapshot.Request.EvidenceID != "" || snapshot.Request.SourceRef != "" || snapshot.Review.EvidenceID == "" || before.ID != snapshot.Request.HypothesisID || before.ProductID != snapshot.Request.ProductID || !equalReviewValue(expectedReview, snapshot.Review) {
		return snapshot, ref, fmt.Errorf("frozen review request/provenance binding mismatch")
	}
	postimage, err := approvalCandidateImage(projectReview(before, snapshot.Review))
	if err != nil || !bytes.Equal(postimage, snapshot.Postimage) {
		return snapshot, ref, fmt.Errorf("frozen review postimage does not match its request")
	}
	expectedIntent, err := reviewSnapshotTransition(snapshot, ref, "product_review_intent")
	if err != nil || !equalReviewValue(intent, expectedIntent) {
		return snapshot, ref, fmt.Errorf("review intent differs from its frozen payload")
	}
	return snapshot, ref, nil
}

func settleReview(homePath string, snapshot reviewSnapshot, ref string) (ReviewReport, error) {
	report := reviewBaseReport(snapshot.Review)
	report.SnapshotRef, report.EvidenceID = ref, snapshot.Review.EvidenceID
	current, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, snapshot.Review.HypothesisID)
	if err != nil {
		return reviewRecovery(report, err)
	}
	var candidate, after HypothesisCandidate
	if err := json.Unmarshal(current.Body, &candidate); err != nil {
		return reviewRecovery(report, err)
	}
	if err := json.Unmarshal(snapshot.Postimage, &after); err != nil {
		return reviewRecovery(report, err)
	}
	if candidate.ID != snapshot.Review.HypothesisID || candidate.ProductID != snapshot.Review.ProductID {
		return reviewRecovery(report, fmt.Errorf("current candidate identity changed"))
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return reviewRecovery(report, err)
	}
	committed, err := reviewSnapshotTransition(snapshot, ref, "product_review")
	if err != nil {
		return reviewRecovery(report, err)
	}
	commitRecord, commitErr := events.Read(committed.ID)
	refused, err := reviewSnapshotTransition(snapshot, ref, "product_review_refused")
	if err != nil {
		return reviewRecovery(report, err)
	}
	refusalRecord, refusalErr := events.Read(refused.ID)
	if commitErr != nil && !errors.Is(commitErr, os.ErrNotExist) {
		return reviewRecovery(report, commitErr)
	}
	if refusalErr != nil && !errors.Is(refusalErr, os.ErrNotExist) {
		return reviewRecovery(report, refusalErr)
	}
	witness := slices.Contains(candidate.CommittedReviewIDs, snapshot.Review.ID)
	if refusalErr == nil {
		var actual envelope.Transition
		if json.Unmarshal(refusalRecord.Bytes, &actual) != nil || !equalReviewValue(actual, refused) || commitErr == nil || witness {
			return reviewRecovery(report, fmt.Errorf("conflicting or invalid review refusal evidence"))
		}
		report.Status, report.CandidateOutcome, report.TransitionID = "refused", "refused", refused.ID
		return report, fmt.Errorf("review %s was refused; candidate unchanged by replay", snapshot.Review.ID)
	}
	if commitErr == nil {
		var actual envelope.Transition
		if json.Unmarshal(commitRecord.Bytes, &actual) != nil || !equalReviewValue(actual, committed) {
			return reviewRecovery(report, fmt.Errorf("committed review outcome differs from the frozen operation"))
		}
	} else if current.Digest != snapshot.PostDigest && !witness {
		return reviewRecovery(report, fmt.Errorf("no committed outcome or surviving exact replacement witness; do not reapply the old review"))
	}
	report.CandidateOutcome = "committed"
	if err := recordLoopTransition(homePath, committed); err != nil {
		return reviewRecovery(report, err)
	}
	report.TransitionID, report.EvidenceRefs = committed.ID, after.EvidenceRefs
	if err := publishReviewHistory(homePath, snapshot.Review); err != nil {
		return reviewRecovery(report, err)
	}
	at, err := time.Parse(time.RFC3339Nano, snapshot.CreatedAt)
	if err != nil {
		return reviewRecovery(report, err)
	}
	// Preserve the established success value; candidate_outcome carries the
	// stronger commit evidence independently of downstream completion status.
	report.Status = "updated"
	if err := publishReviewArtifacts(homePath, snapshot.Review, snapshot.SourcePath, report, at); err != nil {
		return reviewRecovery(report, err)
	}
	return report, nil
}

func lockReviewOperation(homePath, reviewID string) (func() error, error) {
	root, err := os.OpenRoot(homePath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// The home habitat catalog owns lock/ as transient, placement-allowed state.
	if err := root.Mkdir("lock", 0o755); err != nil && !os.IsExist(err) {
		return nil, err
	}
	directory, err := root.Lstat("lock")
	if err != nil || !directory.IsDir() {
		return nil, fmt.Errorf("review lock directory is not a regular directory: %v", err)
	}
	path := filepath.Join("lock", stableID("product-review-", reviewID)+".lock")
	f, err := openReviewOperationLock(root, path)
	if err != nil {
		return nil, err
	}
	verify := func() error {
		currentDirectory, directoryErr := root.Lstat("lock")
		currentFile, fileErr := root.Lstat(path)
		opened, statErr := f.Stat()
		if err := errors.Join(directoryErr, fileErr, statErr); err != nil {
			return err
		}
		if !currentDirectory.IsDir() || !os.SameFile(directory, currentDirectory) {
			return fmt.Errorf("review lock directory changed while opening")
		}
		if !currentFile.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(currentFile, opened) {
			return fmt.Errorf("review lock file changed while opening")
		}
		return nil
	}
	if err := verify(); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("review operation busy or unavailable: %w", err), f.Close())
	}
	if err := verify(); err != nil {
		return nil, errors.Join(err, syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
	}
	return func() error { return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close()) }, nil
}

func openReviewOperationLock(root *os.Root, path string) (*os.File, error) {
	// Match the existing candidate/run-attempt owners: exclusive creation, then
	// open the exact existing regular file if a concurrent creator won. A single
	// O_CREATE open returned ENOENT during simultaneous creation in the
	// Go 1.27.1 Darwin reproducer; this does not identify a kernel/runtime cause.
	// Do not retry missing/replaced authority or classify it as lock contention.
	before, err := root.Lstat(path)
	var f *os.File
	if os.IsNotExist(err) {
		f, err = openReviewOperationFile(root, path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
		if os.IsExist(err) {
			before, err = root.Lstat(path)
		}
	}
	if err != nil {
		return nil, err
	}
	if f == nil {
		if !before.Mode().IsRegular() {
			return nil, fmt.Errorf("review lock is not a regular file")
		}
		f, err = openReviewOperationFile(root, path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
	}
	opened, err := f.Stat()
	if err == nil && (!opened.Mode().IsRegular() || (before != nil && !os.SameFile(before, opened))) {
		err = fmt.Errorf("review lock file changed while opening")
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
