package reviewartifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ReviewWorkBindingSchemaVersion = "compound.review_work_binding.v0.1"

// ReviewSourceRun preserves the typed execution provenance that produced a
// review artifact. WorkID is empty for a legitimately unbound source run. When
// it is present, it must equal ReviewWorkBinding.SourceWorkID.
type ReviewSourceRun struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	WorkID string `json:"work_id,omitempty"`
}

// ReviewWorkBinding is an immutable companion to a sealed core review
// artifact. SourceWorkID is provenance first: a terminal source remains
// recorded here but can never become executable authority. FallbackWorkID is
// the explicit per-area owner that may supply current authority when the source
// is absent or terminal.
type ReviewWorkBinding struct {
	SchemaVersion       string           `json:"schema_version"`
	ID                  string           `json:"id"`
	CreatedAt           string           `json:"created_at"`
	Area                string           `json:"area"`
	ReviewID            string           `json:"review_id"`
	ReviewPath          string           `json:"review_path"`
	ReviewSHA256        string           `json:"review_sha256"`
	SourceWorkID        string           `json:"source_work_id,omitempty"`
	SourceRun           *ReviewSourceRun `json:"source_run,omitempty"`
	FallbackWorkID      string           `json:"fallback_work_id,omitempty"`
	SupersedesBindingID string           `json:"supersedes_binding_id,omitempty"`
	EvidenceRefs        []string         `json:"evidence_refs,omitempty"`
}

type WorkBindingTarget struct {
	Area           string
	ReviewID       string
	CoreReviewPath string
}

type RecordWorkBindingInput struct {
	Area                string
	ReviewID            string
	CoreReviewPath      string
	SourceWorkID        string
	SourceRun           *ReviewSourceRun
	FallbackWorkID      string
	SupersedesBindingID string
	EvidenceRefs        []string
}

// PlanWorkBinding validates and materializes the exact immutable binding bytes
// without writing them. Work liveness and fallback-area ownership are checked
// by reviewqueue at the authority-resolution seam, not by this artifact owner.
func PlanWorkBinding(homePath string, in RecordWorkBindingInput, now time.Time) (ReviewWorkBinding, error) {
	target, reviewRel, digest, err := normalizeWorkBindingTarget(homePath, WorkBindingTarget{
		Area: in.Area, ReviewID: in.ReviewID, CoreReviewPath: in.CoreReviewPath,
	})
	if err != nil {
		return ReviewWorkBinding{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	sourceWorkID := strings.TrimSpace(in.SourceWorkID)
	fallbackWorkID := strings.TrimSpace(in.FallbackWorkID)
	if sourceWorkID != "" && !validDurableWorkID(sourceWorkID) {
		return ReviewWorkBinding{}, fmt.Errorf("review work binding source_work_id must be a bare durable work_ id")
	}
	if fallbackWorkID != "" && !validDurableWorkID(fallbackWorkID) {
		return ReviewWorkBinding{}, fmt.Errorf("review work binding fallback_work_id must be a bare durable work_ id")
	}
	if sourceWorkID == "" && fallbackWorkID == "" {
		return ReviewWorkBinding{}, fmt.Errorf("review work binding requires source_work_id or fallback_work_id")
	}
	sourceRun, err := normalizeReviewSourceRun(in.SourceRun, sourceWorkID)
	if err != nil {
		return ReviewWorkBinding{}, err
	}
	binding := ReviewWorkBinding{
		SchemaVersion:       ReviewWorkBindingSchemaVersion,
		CreatedAt:           Timestamp(now),
		Area:                target.Area,
		ReviewID:            target.ReviewID,
		ReviewPath:          reviewRel,
		ReviewSHA256:        digest,
		SourceWorkID:        sourceWorkID,
		SourceRun:           sourceRun,
		FallbackWorkID:      fallbackWorkID,
		SupersedesBindingID: strings.TrimSpace(in.SupersedesBindingID),
		EvidenceRefs:        UniqueSorted(in.EvidenceRefs),
	}
	if binding.SupersedesBindingID != "" && !validWorkBindingID(binding.SupersedesBindingID) {
		return ReviewWorkBinding{}, fmt.Errorf("review work binding supersedes_binding_id invalid")
	}
	binding.ID = workBindingID(binding)
	if err := validateReviewWorkBinding(binding, target, reviewRel); err != nil {
		return ReviewWorkBinding{}, err
	}
	return binding, nil
}

// RecordWorkBinding appends one immutable sidecar. Repeating the same semantic
// record is idempotent. Changing a current binding requires naming its exact
// head in SupersedesBindingID, preventing last-writer-wins authority drift.
func RecordWorkBinding(homePath string, in RecordWorkBindingInput, now time.Time) (ReviewWorkBinding, string, error) {
	target := WorkBindingTarget{Area: in.Area, ReviewID: in.ReviewID, CoreReviewPath: in.CoreReviewPath}
	current, err := LoadWorkBindingHead(homePath, target)
	if err != nil {
		return ReviewWorkBinding{}, "", err
	}
	planned, err := PlanWorkBinding(homePath, in, now)
	if err != nil {
		return ReviewWorkBinding{}, "", err
	}
	if current != nil && sameWorkBindingMeaning(*current, planned) {
		return *current, workBindingPath(in.CoreReviewPath, current.ID), nil
	}
	supersedes := strings.TrimSpace(in.SupersedesBindingID)
	switch {
	case current == nil && supersedes != "":
		return ReviewWorkBinding{}, "", fmt.Errorf("review work binding supersedes %s but no current binding exists", supersedes)
	case current != nil && supersedes != current.ID:
		return ReviewWorkBinding{}, "", fmt.Errorf("review work binding supersedes_binding_id must equal current head %s", current.ID)
	}
	path := workBindingPath(in.CoreReviewPath, planned.ID)
	data, err := json.MarshalIndent(planned, "", "  ")
	if err != nil {
		return ReviewWorkBinding{}, "", err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, loadErr := loadReviewWorkBindingFile(path)
			if loadErr == nil && sameWorkBindingRecord(existing, planned) {
				return existing, path, nil
			}
		}
		return ReviewWorkBinding{}, "", fmt.Errorf("append review work binding %s: %w", path, err)
	}
	removePartial := true
	defer func() {
		_ = file.Close()
		if removePartial {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return ReviewWorkBinding{}, "", fmt.Errorf("write review work binding %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return ReviewWorkBinding{}, "", fmt.Errorf("sync review work binding %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return ReviewWorkBinding{}, "", fmt.Errorf("close review work binding %s: %w", path, err)
	}
	removePartial = false
	return planned, path, nil
}

// LoadCurrentWorkBinding validates the complete append-only chain and returns
// its single unsuperseded head only when that head binds the current core
// review bytes. Historical bindings retain the digest of the review version
// they bound. Any corrupt member, fork, head digest drift, or path drift fails
// closed.
func LoadCurrentWorkBinding(homePath string, target WorkBindingTarget) (*ReviewWorkBinding, error) {
	head, digest, err := loadWorkBindingHead(homePath, target)
	if err != nil || head == nil {
		return head, err
	}
	if head.ReviewSHA256 != digest {
		return nil, fmt.Errorf("review work binding head %s review digest drift: got %q want %q", head.ID, head.ReviewSHA256, digest)
	}
	return head, nil
}

// LoadWorkBindingHead validates the complete append-only chain and returns its
// single unsuperseded structural head without requiring that head to bind the
// current core review bytes. Record and planning callers use this seam to name
// the exact stale head when a regenerated review needs explicit supersession.
// Queue authority must use LoadCurrentWorkBinding and remain fail closed until
// the superseding head exists.
func LoadWorkBindingHead(homePath string, target WorkBindingTarget) (*ReviewWorkBinding, error) {
	head, _, err := loadWorkBindingHead(homePath, target)
	return head, err
}

func loadWorkBindingHead(homePath string, target WorkBindingTarget) (*ReviewWorkBinding, string, error) {
	normalized, reviewRel, digest, err := normalizeWorkBindingTarget(homePath, target)
	if err != nil {
		return nil, "", err
	}
	pattern := strings.TrimSuffix(normalized.CoreReviewPath, ".json") + ".work-binding.*.json"
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, "", fmt.Errorf("scan review work bindings: %w", err)
	}
	if len(paths) == 0 {
		return nil, digest, nil
	}
	sort.Strings(paths)
	byID := make(map[string]ReviewWorkBinding, len(paths))
	childByParent := make(map[string]string, len(paths))
	for _, path := range paths {
		binding, err := loadReviewWorkBindingFile(path)
		if err != nil {
			return nil, "", err
		}
		if want := workBindingPath(normalized.CoreReviewPath, binding.ID); filepath.Clean(path) != filepath.Clean(want) {
			return nil, "", fmt.Errorf("review work binding path drift: %s does not match id %s", path, binding.ID)
		}
		if err := validateReviewWorkBinding(binding, normalized, reviewRel); err != nil {
			return nil, "", fmt.Errorf("review work binding %s invalid: %w", path, err)
		}
		if _, exists := byID[binding.ID]; exists {
			return nil, "", fmt.Errorf("duplicate review work binding id %s", binding.ID)
		}
		byID[binding.ID] = binding
		if parent := binding.SupersedesBindingID; parent != "" {
			if priorChild, fork := childByParent[parent]; fork {
				return nil, "", fmt.Errorf("review work binding chain fork at %s: %s and %s", parent, priorChild, binding.ID)
			}
			childByParent[parent] = binding.ID
		}
	}
	roots := 0
	heads := make([]string, 0, 1)
	for id, binding := range byID {
		if binding.SupersedesBindingID == "" {
			roots++
		} else if _, ok := byID[binding.SupersedesBindingID]; !ok {
			return nil, "", fmt.Errorf("review work binding %s supersedes missing binding %s", id, binding.SupersedesBindingID)
		}
		if _, superseded := childByParent[id]; !superseded {
			heads = append(heads, id)
		}
	}
	if roots != 1 || len(heads) != 1 {
		return nil, "", fmt.Errorf("review work binding chain requires one root and one head; roots=%d heads=%d", roots, len(heads))
	}
	seen := map[string]bool{}
	for id := heads[0]; id != ""; id = byID[id].SupersedesBindingID {
		if seen[id] {
			return nil, "", fmt.Errorf("review work binding chain cycle at %s", id)
		}
		seen[id] = true
	}
	if len(seen) != len(byID) {
		return nil, "", fmt.Errorf("review work binding chain is disconnected")
	}
	head := byID[heads[0]]
	return &head, digest, nil
}

func normalizeWorkBindingTarget(homePath string, target WorkBindingTarget) (WorkBindingTarget, string, string, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return WorkBindingTarget{}, "", "", fmt.Errorf("COMPOUND_HOME required")
	}
	area := strings.TrimSpace(target.Area)
	if err := validArea(area); err != nil {
		return WorkBindingTarget{}, "", "", err
	}
	reviewID := strings.TrimSpace(target.ReviewID)
	if reviewID == "" || filepath.Base(reviewID) != reviewID || strings.ContainsAny(reviewID, `/\`) {
		return WorkBindingTarget{}, "", "", fmt.Errorf("review work binding review_id must be a bare id")
	}
	reviewPath, err := filepath.Abs(strings.TrimSpace(target.CoreReviewPath))
	if err != nil || strings.TrimSpace(target.CoreReviewPath) == "" {
		return WorkBindingTarget{}, "", "", fmt.Errorf("review work binding core review path required")
	}
	if filepath.Ext(reviewPath) != ".json" || !coreReviewJSON(reviewPath) {
		return WorkBindingTarget{}, "", "", fmt.Errorf("review work binding requires a core review JSON artifact")
	}
	homeAbs, err := filepath.Abs(homePath)
	if err != nil {
		return WorkBindingTarget{}, "", "", err
	}
	expectedDir := filepath.Join(homeAbs, "reviews", filepath.FromSlash(area))
	if filepath.Clean(filepath.Dir(reviewPath)) != filepath.Clean(expectedDir) {
		return WorkBindingTarget{}, "", "", fmt.Errorf("review work binding path is outside reviews/%s", area)
	}
	resolvedHome, err := filepath.EvalSymlinks(homeAbs)
	if err != nil {
		return WorkBindingTarget{}, "", "", fmt.Errorf("resolve COMPOUND_HOME: %w", err)
	}
	resolvedReview, err := filepath.EvalSymlinks(reviewPath)
	if err != nil {
		return WorkBindingTarget{}, "", "", fmt.Errorf("resolve core review path: %w", err)
	}
	resolvedRel, err := filepath.Rel(resolvedHome, resolvedReview)
	if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
		return WorkBindingTarget{}, "", "", fmt.Errorf("review work binding core review resolves outside COMPOUND_HOME")
	}
	data, err := os.ReadFile(reviewPath)
	if err != nil {
		return WorkBindingTarget{}, "", "", fmt.Errorf("read core review artifact: %w", err)
	}
	reviewRel, err := filepath.Rel(homeAbs, reviewPath)
	if err != nil {
		return WorkBindingTarget{}, "", "", err
	}
	reviewRel = filepath.ToSlash(reviewRel)
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	return WorkBindingTarget{Area: area, ReviewID: reviewID, CoreReviewPath: reviewPath}, reviewRel, digest, nil
}

func normalizeReviewSourceRun(source *ReviewSourceRun, sourceWorkID string) (*ReviewSourceRun, error) {
	if source == nil {
		return nil, nil
	}
	out := *source
	out.Kind = strings.TrimSpace(out.Kind)
	out.ID = strings.TrimSpace(out.ID)
	out.WorkID = strings.TrimSpace(out.WorkID)
	if out.Kind == "" || out.ID == "" || filepath.Base(out.Kind) != out.Kind || filepath.Base(out.ID) != out.ID {
		return nil, fmt.Errorf("review work binding source_run kind and id must be bare typed identities")
	}
	if out.WorkID != "" && !validDurableWorkID(out.WorkID) {
		return nil, fmt.Errorf("review work binding source_run work_id must be a bare durable work_ id")
	}
	if out.WorkID != sourceWorkID {
		return nil, fmt.Errorf("review work binding source_run work_id %s does not match source_work_id %s", out.WorkID, sourceWorkID)
	}
	return &out, nil
}

func validateReviewWorkBinding(binding ReviewWorkBinding, target WorkBindingTarget, reviewRel string) error {
	if binding.SchemaVersion != ReviewWorkBindingSchemaVersion {
		return fmt.Errorf("schema_version = %q", binding.SchemaVersion)
	}
	if binding.Area != target.Area || binding.ReviewID != target.ReviewID {
		return fmt.Errorf("area/review identity drift")
	}
	if binding.ReviewPath != reviewRel {
		return fmt.Errorf("review path drift: got %q want %q", binding.ReviewPath, reviewRel)
	}
	if !validSHA256Digest(binding.ReviewSHA256) {
		return fmt.Errorf("review_sha256 must be a canonical sha256 digest")
	}
	if _, err := time.Parse(time.RFC3339, binding.CreatedAt); err != nil {
		return fmt.Errorf("created_at invalid: %w", err)
	}
	if binding.SourceWorkID != "" && !validDurableWorkID(binding.SourceWorkID) {
		return fmt.Errorf("source_work_id invalid")
	}
	if binding.FallbackWorkID != "" && !validDurableWorkID(binding.FallbackWorkID) {
		return fmt.Errorf("fallback_work_id invalid")
	}
	if binding.SourceWorkID == "" && binding.FallbackWorkID == "" {
		return fmt.Errorf("source_work_id or fallback_work_id required")
	}
	if _, err := normalizeReviewSourceRun(binding.SourceRun, binding.SourceWorkID); err != nil {
		return err
	}
	if binding.SupersedesBindingID != "" && !validWorkBindingID(binding.SupersedesBindingID) {
		return fmt.Errorf("supersedes_binding_id invalid")
	}
	if got := UniqueSorted(binding.EvidenceRefs); strings.Join(got, "\x00") != strings.Join(binding.EvidenceRefs, "\x00") {
		return fmt.Errorf("evidence_refs must be canonical unique order")
	}
	if !validWorkBindingID(binding.ID) || binding.ID != workBindingID(binding) {
		return fmt.Errorf("binding id does not match immutable content")
	}
	return nil
}

func validSHA256Digest(digest string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+sha256.Size*2 {
		return false
	}
	hexPart := strings.TrimPrefix(digest, prefix)
	if hexPart != strings.ToLower(hexPart) {
		return false
	}
	decoded, err := hex.DecodeString(hexPart)
	return err == nil && len(decoded) == sha256.Size
}

func loadReviewWorkBindingFile(path string) (ReviewWorkBinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReviewWorkBinding{}, fmt.Errorf("read review work binding %s: %w", path, err)
	}
	var binding ReviewWorkBinding
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&binding); err != nil {
		return ReviewWorkBinding{}, fmt.Errorf("parse review work binding %s: %w", path, err)
	}
	if err := ensureBindingJSONEOF(dec); err != nil {
		return ReviewWorkBinding{}, fmt.Errorf("parse review work binding %s: %w", path, err)
	}
	return binding, nil
}

func ensureBindingJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("multiple JSON values")
}

func workBindingID(binding ReviewWorkBinding) string {
	sourceRun := ""
	if binding.SourceRun != nil {
		sourceRun = strings.Join([]string{binding.SourceRun.Kind, binding.SourceRun.ID, binding.SourceRun.WorkID}, "\x00")
	}
	return ID("rwb_",
		binding.Area,
		binding.ReviewID,
		binding.ReviewPath,
		binding.ReviewSHA256,
		binding.SourceWorkID,
		sourceRun,
		binding.FallbackWorkID,
		binding.SupersedesBindingID,
		strings.Join(binding.EvidenceRefs, "\x00"),
	)
}

func workBindingPath(coreReviewPath, bindingID string) string {
	return strings.TrimSuffix(filepath.Clean(coreReviewPath), ".json") + ".work-binding." + bindingID + ".json"
}

func validWorkBindingID(id string) bool {
	return strings.HasPrefix(id, "rwb_") && len(id) == len("rwb_")+12 && filepath.Base(id) == id
}

func validDurableWorkID(id string) bool {
	return len(id) > len("work_") && strings.HasPrefix(id, "work_") &&
		!strings.HasPrefix(id, "work_live_") && filepath.Base(id) == id
}

func sameWorkBindingMeaning(a, b ReviewWorkBinding) bool {
	a.ID, b.ID = "", ""
	a.CreatedAt, b.CreatedAt = "", ""
	a.SupersedesBindingID, b.SupersedesBindingID = "", ""
	return sameWorkBindingRecord(a, b)
}

func sameWorkBindingRecord(a, b ReviewWorkBinding) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}
