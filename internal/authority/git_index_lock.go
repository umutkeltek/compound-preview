package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/work"
	"golang.org/x/sys/unix"
)

const gitIndexLockArea = "git-index-lock"

type GitIndexLockPlan struct {
	ID        string                       `json:"id"`
	Digest    string                       `json:"digest"`
	WorkID    string                       `json:"work_id"`
	Actor     string                       `json:"actor"`
	CreatedAt time.Time                    `json:"created_at"`
	State     gitrepository.IndexLockState `json:"state"`
}

// GitIndexLockWindow records an actual writer-pause commitment, NOT general
// permission to recover stale locks. The caller must cite that distinct evidence.
type GitIndexLockWindow struct {
	ReviewedBy    string    `json:"reviewed_by"`
	EvidenceRef   string    `json:"evidence_ref"`
	WritersPaused bool      `json:"writers_paused"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type GitIndexLockReview struct {
	ID         string             `json:"id"`
	PlanID     string             `json:"plan_id"`
	PlanDigest string             `json:"plan_digest"`
	IssuedAt   time.Time          `json:"issued_at"`
	Window     GitIndexLockWindow `json:"window"`
}

func BuildGitIndexLockPlan(ctx context.Context, repo, workID, actor string, idle gitrepository.IndexLockLiveness, now time.Time) (GitIndexLockPlan, error) {
	var plan GitIndexLockPlan
	if !strings.HasPrefix(workID, "work_") || strings.TrimSpace(actor) == "" || now.IsZero() {
		return plan, fmt.Errorf("work, actor and clock required")
	}
	if idle == nil {
		idle = gitrepository.LSOFIndexLockLiveness
	}
	state, err := gitrepository.InspectIndexLock(ctx, repo, now, idle)
	if err != nil {
		return plan, err
	}
	plan = GitIndexLockPlan{WorkID: workID, Actor: actor, CreatedAt: now, State: state}
	plan.Digest = indexRecoveryDigest(plan)
	plan.ID = "gilp_" + plan.Digest[:24]
	return plan, nil
}

func RecordGitIndexLockPlan(home string, plan GitIndexLockPlan, now time.Time) error {
	if err := validateGitIndexLockPlan(plan); err != nil {
		return err
	}
	if err := authorizeIndexLockArtifact(home, plan, "plan", now); err != nil {
		return err
	}
	return writeIndexRecoveryArtifact(home, plan.ID+".plan.json", plan)
}

func ReviewGitIndexLockPlan(home, planID string, window GitIndexLockWindow, now time.Time) (GitIndexLockReview, error) {
	var review GitIndexLockReview
	plan, err := loadGitIndexLockPlan(home, planID)
	if err != nil {
		return review, err
	}
	review = GitIndexLockReview{PlanID: plan.ID, PlanDigest: plan.Digest, IssuedAt: now, Window: window}
	review.ID = "gilr_" + indexRecoveryDigest(review)[:24]
	if err := validateGitIndexLockReview(plan, review, now); err != nil {
		return review, err
	}
	if err := authorizeIndexLockArtifact(home, plan, review.ID, now); err != nil {
		return review, err
	}
	return review, writeIndexRecoveryArtifact(home, review.ID+".review.json", review)
}

func validateGitIndexLockPlan(plan GitIndexLockPlan) error {
	copy := plan
	copy.ID, copy.Digest = "", ""
	if plan.Digest != indexRecoveryDigest(copy) || plan.ID != "gilp_"+plan.Digest[:24] || plan.State.Snapshot.Repo == "" || !strings.HasPrefix(plan.WorkID, "work_") || plan.Actor == "" {
		return fmt.Errorf("index-lock plan identity mismatch")
	}
	return nil
}

func validateGitIndexLockReview(plan GitIndexLockPlan, review GitIndexLockReview, now time.Time) error {
	copy := review
	copy.ID = ""
	if review.ID != "gilr_"+indexRecoveryDigest(copy)[:24] || review.PlanID != plan.ID || review.PlanDigest != plan.Digest {
		return fmt.Errorf("review does not bind exact recovery plan")
	}
	w := review.Window
	if !policy.HasOperatorProvenance(w.ReviewedBy) || strings.TrimSpace(w.EvidenceRef) == "" || !w.WritersPaused {
		return fmt.Errorf("operator provenance and actual all-writer pause evidence required")
	}
	if review.IssuedAt.IsZero() || review.IssuedAt.After(now) || !w.ExpiresAt.After(now) || w.ExpiresAt.Sub(review.IssuedAt) > 5*time.Minute {
		return fmt.Errorf("exclusive recovery window must be current and at most five minutes")
	}
	return nil
}

func loadGitIndexLockPlan(home, id string) (GitIndexLockPlan, error) {
	var plan GitIndexLockPlan
	if err := readIndexRecoveryArtifact(home, id+".plan.json", &plan); err != nil {
		return plan, err
	}
	if id != plan.ID {
		return plan, fmt.Errorf("plan filename identity mismatch")
	}
	return plan, validateGitIndexLockPlan(plan)
}

func indexRecoveryDigest(value any) string {
	body, _ := json.Marshal(value) // All callers use concrete JSON-only structs.
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func indexRecoveryAction(plan GitIndexLockPlan, id, action, path string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_" + indexRecoveryDigest(id)[:24], CreatedAt: now.UTC().Format(time.RFC3339Nano), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: plan.WorkID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1", EvidenceRefs: []string{"git-index-lock-plan:" + plan.ID, "sha256:" + plan.Digest}, Uncertainty: envelope.Uncertainty{Level: "known"}},
		Actor:  plan.Actor, Capability: "source-control.git-index-lock", Action: action, Target: envelope.ActionTarget{Kind: "git-repository", ID: plan.ID, Path: path}, MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "repo", Preview: "exact reviewed stale index-lock recovery; preserve index and all tracked WIP", RollbackPlan: "retain private exact index, binary WIP patches and displaced lock; reconcile without overwriting any replacement",
	}
}

func authorizeIndexLockArtifact(home string, plan GitIndexLockPlan, suffix string, now time.Time) error {
	store, err := work.Open(home)
	if err != nil {
		return err
	}
	current, err := store.Get(plan.WorkID)
	if err != nil {
		return err
	}
	if current.State != "running" {
		return fmt.Errorf("running owner work required")
	}
	action := indexRecoveryAction(plan, plan.ID+suffix, "record_index_lock_review_artifact", filepath.Join(home, "reviews", gitIndexLockArea), now)
	result, err := store.EvaluateAndRecordAction(plan.WorkID, action, now)
	if err != nil {
		return err
	}
	if !policy.Allowed(result.Decision) {
		return fmt.Errorf("index-lock artifact policy %s: %s", result.Decision.ID, result.Decision.Outcome)
	}
	return nil
}

func indexRecoveryArtifactPath(home, name string) (string, error) {
	if strings.TrimSpace(home) == "" || name == "" || name != filepath.Base(name) || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid private recovery artifact path")
	}
	return filepath.Join(home, "reviews", gitIndexLockArea, name), nil
}

func writeIndexRecoveryArtifact(home, name string, value any) error {
	return writeIndexRecoveryArtifactUsing(home, name, value, func(f *os.File, body []byte) error { _, err := f.Write(body); return err })
}

func writeIndexRecoveryArtifactUsing(home, name string, value any, write func(*os.File, []byte) error) error {
	path, err := indexRecoveryArtifactPath(home, name)
	if err != nil {
		return err
	}
	dir, err := reviewartifact.Dir(home, gitIndexLockArea)
	if err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("recovery artifact directory must not be a symlink")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".index-lock-artifact-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := write(f, body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Link publishes a complete synced body exclusively: never overwrite an
	// existing final artifact and never expose a partially written final name.
	if err := os.Link(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readIndexRecoveryArtifact(home, name string, value any) error {
	path, err := indexRecoveryArtifactPath(home, name)
	if err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fmt.Errorf("private regular recovery artifact required")
	}
	body, err := io.ReadAll(io.LimitReader(f, 256<<20))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected trailing recovery artifact data")
	}
	return nil
}
