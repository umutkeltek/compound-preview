package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/finding"
	"compound/internal/policy"
)

const (
	PublicRedactionReviewSchema     = "compound.public_redaction_review.v1"
	PublicRedactionReviewCapability = "proof.redaction.review_public"
	PublicRedactionReviewAction     = "classify_public_redaction_findings"
	publicRedactionReviewMarker     = "<compound-public-review-v1>"
	publicRedactionEvidenceLimit    = 4 << 20
)

// This is evidence, not an allowlist or policy. Every file in the complete
// committed scan inventory must be present, including clean files.
type PublicRedactionReview struct {
	SchemaVersion string                 `json:"schema_version"`
	WorkID        string                 `json:"work_id"`
	Source        gitrepository.Snapshot `json:"source_snapshot"`
	Files         []PublicRedactionFile  `json:"files"`
}

type PublicRedactionFile struct {
	PublicRedactionScan
	Approvals []PublicRedactionApproval `json:"approvals"`
}

type PublicRedactionScan struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Report   []byte `json:"report"`
	ExitCode int    `json:"exit_code"`
}

type PublicRedactionApproval struct {
	Finding        PublicRedactionFinding `json:"finding"`
	Classification string                 `json:"classification"`
	EvidenceRefs   []string               `json:"evidence_refs"`
}

// Keep the scanner's complete finding identity, including original positions.
// A masked preview alone is not an identity.
type PublicRedactionFinding struct {
	ID                string `json:"id"`
	Tier              string `json:"tier"`
	Severity          string `json:"severity"`
	Category          string `json:"category"`
	Description       string `json:"description"`
	Line              int    `json:"line"`
	Col               int    `json:"col"`
	Preview           string `json:"preview"`
	AutoRedactable    bool   `json:"autoRedactable"`
	RepoVisibility    string `json:"repoVisibility"`
	ToolFenceDegraded bool   `json:"toolFenceDegraded,omitempty"`
}

type publicRedactionCounts struct {
	High   *int `json:"HIGH"`
	Medium *int `json:"MEDIUM"`
	Low    *int `json:"LOW"`
	Warn   *int `json:"WARN"`
}

type publicRedactionReport struct {
	Findings       []PublicRedactionFinding `json:"findings"`
	Counts         *publicRedactionCounts   `json:"counts"`
	RepoVisibility string                   `json:"repoVisibility"`
	Oversize       *bool                    `json:"oversize"`
}

type PublicRedactionReviewBinding struct {
	ActionID  string `json:"action_id"`
	PolicyID  string `json:"policy_id"`
	FindingID string `json:"finding_id"`
	CID       string `json:"cid"`
}

// Exact raw review and scanner bytes travel inside the existing owned report.
// The reservation's ordered command identity seals the native authority tuple.
type PublicRedactionEvidence struct {
	Binding PublicRedactionReviewBinding `json:"binding"`
	Review  []byte                       `json:"review"`
	Scans   []PublicRedactionScan        `json:"scans"`
}

type PublicRedactionAuthority struct {
	Binding PublicRedactionReviewBinding
	Body    []byte
	Review  PublicRedactionReview
}

var publicReviewFindingID = regexp.MustCompile(`^find_[a-f0-9]{12}$`)
var publicReviewSHA256 = regexp.MustCompile(`^[a-f0-9]{64}$`)

func publicReviewStrictJSON(body []byte, target any) error {
	if len(body) == 0 || len(body) > finding.MaxBodyBytes {
		return fmt.Errorf("public redaction JSON exceeds the bounded body contract")
	}
	if err := consumerUniqueJSON(body); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

func ParsePublicRedactionReport(body []byte, exitCode int) ([]PublicRedactionFinding, error) {
	var report publicRedactionReport
	if err := publicReviewStrictJSON(body, &report); err != nil {
		return nil, err
	}
	if report.RepoVisibility != "public" || report.Findings == nil || report.Counts == nil || report.Counts.High == nil || report.Counts.Medium == nil || report.Counts.Low == nil || report.Counts.Warn == nil || report.Oversize == nil || *report.Oversize {
		return nil, fmt.Errorf("incomplete, nonpublic or oversized redaction report")
	}
	var high, medium, low, warn int
	seen := map[string]bool{}
	for _, f := range report.Findings {
		if f.ID == "" || f.Description == "" || f.Preview == "" || f.Line < 1 || f.Col < 1 || f.RepoVisibility != "public" {
			return nil, fmt.Errorf("invalid scanner finding identity")
		}
		if f.Tier != "HIGH" && f.Tier != "MEDIUM" && f.Tier != "LOW" {
			return nil, fmt.Errorf("invalid scanner finding tier")
		}
		key := fmt.Sprintf("%s:%d:%d", f.ID, f.Line, f.Col)
		if seen[key] {
			return nil, fmt.Errorf("duplicate scanner finding")
		}
		seen[key] = true
		switch f.Severity {
		case "HIGH":
			high++
		case "MEDIUM":
			medium++
		case "LOW":
			low++
		case "WARN":
			warn++
		default:
			return nil, fmt.Errorf("invalid scanner severity")
		}
		if (f.Severity != "WARN" && (f.Tier != f.Severity || f.ToolFenceDegraded)) || (f.Severity == "WARN" && !f.ToolFenceDegraded) {
			return nil, fmt.Errorf("scanner tier/severity mismatch")
		}
	}
	if high != *report.Counts.High || medium != *report.Counts.Medium || low != *report.Counts.Low || warn != *report.Counts.Warn {
		return nil, fmt.Errorf("scanner counts disagree with findings")
	}
	wantExit := 0
	if high > 0 {
		wantExit = 3
	} else if medium > 0 {
		wantExit = 2
	}
	if wantExit != exitCode {
		return nil, fmt.Errorf("scanner exit/count mismatch")
	}
	return report.Findings, nil
}

func ParsePublicRedactionReview(body []byte) (PublicRedactionReview, error) {
	var review PublicRedactionReview
	if err := publicReviewStrictJSON(body, &review); err != nil {
		return review, err
	}
	if review.SchemaVersion != PublicRedactionReviewSchema || !strings.HasPrefix(review.WorkID, "work_") || len(review.Files) == 0 || len(review.Files) > 4096 {
		return review, fmt.Errorf("invalid public redaction review identity or inventory")
	}
	if err := gitrepository.ValidateSnapshotShape(review.Source); err != nil {
		return review, err
	}
	if !review.Source.Clean {
		return review, fmt.Errorf("review source must be clean")
	}
	previous := ""
	approved := 0
	for _, file := range review.Files {
		if file.Path == "" || len(file.Path) > 4096 || path.IsAbs(file.Path) || path.Clean(file.Path) != file.Path || file.Path == ".." || strings.HasPrefix(file.Path, "../") || strings.ContainsAny(file.Path, "\\\x00\r\n") || file.Path <= previous || !publicReviewSHA256.MatchString(file.SHA256) {
			return review, fmt.Errorf("review inventory must be unique, ordered regular source paths and complete hashes")
		}
		previous = file.Path
		findings, err := ParsePublicRedactionReport(file.Report, file.ExitCode)
		if err != nil {
			return review, fmt.Errorf("review report %s: %w", file.Path, err)
		}
		remaining := append([]PublicRedactionApproval(nil), file.Approvals...)
		for _, f := range findings {
			if f.Severity == "HIGH" {
				return review, fmt.Errorf("HIGH findings cannot be reviewed away")
			}
			if f.Severity != "MEDIUM" {
				continue
			}
			if f.Tier != "MEDIUM" || f.Category != "pii" || f.ToolFenceDegraded || !f.AutoRedactable || (f.ID != "pii.phone.e164" && f.ID != "pii.email") {
				return review, fmt.Errorf("only supported MEDIUM privacy findings are reviewable")
			}
			match := -1
			for i, approval := range remaining {
				if reflect.DeepEqual(approval.Finding, f) {
					match = i
					break
				}
			}
			if match < 0 {
				return review, fmt.Errorf("unreviewed MEDIUM finding")
			}
			a := remaining[match]
			if (f.ID == "pii.email" && a.Classification != "public_support_contact") || (f.ID == "pii.phone.e164" && a.Classification != "public_provider_identifier") || len(a.EvidenceRefs) == 0 || len(a.EvidenceRefs) > 8 {
				return review, fmt.Errorf("unsupported classification or missing evidence")
			}
			for _, ref := range a.EvidenceRefs {
				if strings.TrimSpace(ref) != ref || ref == "" || len(ref) > 4096 {
					return review, fmt.Errorf("invalid classification evidence ref")
				}
			}
			remaining = slices.Delete(remaining, match, match+1)
			approved++
		}
		if len(remaining) != 0 {
			return review, fmt.Errorf("duplicate or unmatched classification")
		}
	}
	if approved == 0 {
		return review, fmt.Errorf("review has no supported blocking finding")
	}
	return review, nil
}

func ValidatePublicRedactionScans(review PublicRedactionReview, scans []PublicRedactionScan) error {
	if len(scans) != len(review.Files) {
		return fmt.Errorf("complete redaction inventory mismatch")
	}
	for i, scan := range scans {
		file := review.Files[i]
		if file.Path != scan.Path || file.SHA256 != scan.SHA256 {
			return fmt.Errorf("committed redaction source drift")
		}
		expected, err := ParsePublicRedactionReport(file.Report, file.ExitCode)
		if err != nil {
			return err
		}
		actual, err := ParsePublicRedactionReport(scan.Report, scan.ExitCode)
		if err != nil {
			return err
		}
		if scan.ExitCode != file.ExitCode || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("new or changed scanner finding")
		}
	}
	return nil
}

// The descriptor is a Compound logical gate, not additional flags for redact.
// Existing profile digests already seal every ordered command argument. No
// second policy directory, replacement profile or value-wide exception exists.
func BindPublicRedactionReview(plan CanonicalProofPlan, binding PublicRedactionReviewBinding) (CanonicalProofPlan, error) {
	if !validPublicRedactionBinding(binding) {
		return plan, fmt.Errorf("invalid native review binding")
	}
	plan = cloneCanonicalProofPlan(plan)
	found := false
	for i := range plan.Steps {
		step := &plan.Steps[i]
		if step.Name != "redact-public" {
			continue
		}
		if found || step.ProofRef != "redact-public-clean" || len(step.Command) != 4 || step.Command[0] != "redact" || step.Command[1] != "--repo-visibility" || step.Command[2] != "public" {
			return plan, fmt.Errorf("review requires one unchanged canonical redaction gate")
		}
		found = true
		step.Command = append(step.Command, publicRedactionReviewMarker, binding.ActionID, binding.PolicyID, binding.FindingID, binding.CID)
		step.ProofRef = "redact-public-reviewed"
	}
	if !found {
		return plan, fmt.Errorf("canonical redaction gate missing")
	}
	return plan, nil
}

func validPublicRedactionBinding(b PublicRedactionReviewBinding) bool {
	return strings.HasPrefix(b.ActionID, "act_") && !strings.ContainsAny(b.ActionID, "/\\\x00\r\n ") && strings.HasPrefix(b.PolicyID, "pol_") && !strings.ContainsAny(b.PolicyID, "/\\\x00\r\n ") && publicReviewFindingID.MatchString(b.FindingID) && strings.HasPrefix(b.CID, "sha256:") && publicReviewSHA256.MatchString(strings.TrimPrefix(b.CID, "sha256:"))
}

func PublicRedactionBindingForStep(step ProofCheckStep) (PublicRedactionReviewBinding, bool, error) {
	marker := slices.Index(step.Command, publicRedactionReviewMarker)
	if marker < 0 {
		if step.ProofRef == "redact-public-reviewed" || step.RedactionEvidence != nil {
			return PublicRedactionReviewBinding{}, false, fmt.Errorf("unbound redaction review evidence")
		}
		return PublicRedactionReviewBinding{}, false, nil
	}
	if step.Name != "redact-public" || step.ProofRef != "redact-public-reviewed" || marker != 4 || len(step.Command) != 9 || step.Command[0] != "redact" || step.Command[1] != "--repo-visibility" || step.Command[2] != "public" {
		return PublicRedactionReviewBinding{}, false, fmt.Errorf("invalid reviewed redaction gate identity")
	}
	b := PublicRedactionReviewBinding{step.Command[5], step.Command[6], step.Command[7], step.Command[8]}
	if !validPublicRedactionBinding(b) {
		return b, false, fmt.Errorf("invalid review tuple")
	}
	return b, true, nil
}

// Resolve consumes native finding evidence and an exact performed public action.
// It never grants authority, consumes another approval, or records a review.
func (s *Store) ResolvePublicRedactionReview(workID, actor, actionID, repo string, source gitrepository.Snapshot, now time.Time) (PublicRedactionAuthority, error) {
	return s.resolvePublicRedactionReview(workID, actor, actionID, repo, source, now, true)
}

// Historical binding is not permission to execute. Only plan reconstruction
// uses requireCurrent=false so an exact sealed completion can reach the existing
// recovery door. Admission, execution and new green finalization require true.
func (s *Store) resolvePublicRedactionReview(workID, actor, actionID, repo string, source gitrepository.Snapshot, now time.Time, requireCurrent bool) (PublicRedactionAuthority, error) {
	var out PublicRedactionAuthority
	w, err := s.Get(workID)
	if err != nil {
		return out, err
	}
	if w.State != "claimed" && w.State != "running" && w.State != "verifying" {
		return out, fmt.Errorf("privacy review needs active work")
	}
	lease, err := s.readLease(workID)
	if err != nil || lease.WorkID != workID || lease.By != actor || actor == "" {
		return out, fmt.Errorf("privacy review actor does not hold this work")
	}
	if w.GitDelivery == nil || w.GitDelivery.RepositoryFingerprint != source.RepositoryFingerprint || !slices.Contains(w.ActionIntentIDs, actionID) {
		return out, fmt.Errorf("review is not performed on this source-owned work")
	}
	var ai envelope.ActionIntent
	if err := s.readEventJSON(actionID, &ai); err != nil {
		return out, err
	}
	if err := ai.Validate(); err != nil {
		return out, err
	}
	if ai.ID != actionID || ai.IDs.WorkID != workID || ai.Actor != actor || ai.Capability != PublicRedactionReviewCapability || ai.Action != PublicRedactionReviewAction || ai.MutationClass != "public" || ai.SecurityFloorOutcome != "" || ai.Target.Kind != "repository" || ai.Target.ID != source.RepositoryFingerprint || ai.Target.Path != repo {
		return out, fmt.Errorf("review needs an exact release-class action, not local editing permission")
	}
	for _, ref := range ai.EvidenceRefs {
		if strings.HasPrefix(ref, "finding:") {
			if out.Binding.FindingID != "" {
				return out, fmt.Errorf("ambiguous review finding")
			}
			out.Binding.FindingID = strings.TrimPrefix(ref, "finding:")
		}
	}
	if !publicReviewFindingID.MatchString(out.Binding.FindingID) {
		return out, fmt.Errorf("native review finding missing")
	}
	f, err := s.readPublicRedactionFinding(out.Binding.FindingID)
	if err != nil {
		return out, err
	}
	if f.WorkID != workID || f.ProducedBy != actor || f.CID != ai.ArgumentsHash {
		return out, fmt.Errorf("review finding provenance or action digest mismatch")
	}
	out.Body = []byte(f.Body)
	out.Review, err = ParsePublicRedactionReview(out.Body)
	if err != nil {
		return out, err
	}
	if out.Review.WorkID != workID || !reflect.DeepEqual(out.Review.Source, source) {
		return out, fmt.Errorf("review work or exact source snapshot mismatch")
	}
	events, err := s.eventsFor(workID)
	if err != nil {
		return out, err
	}
	for _, event := range events {
		if !slices.Contains(event.Attached, actionID) {
			continue
		}
		id, err := oneAttachedWithPrefix(event.Attached, "pol_")
		if err != nil || out.Binding.PolicyID != "" {
			return out, fmt.Errorf("review has no unique native policy transaction")
		}
		out.Binding.PolicyID = id
	}
	var pd envelope.PolicyDecision
	if out.Binding.PolicyID == "" {
		return out, fmt.Errorf("review policy transaction missing")
	}
	if err := s.readEventJSON(out.Binding.PolicyID, &pd); err != nil {
		return out, err
	}
	if err := pd.Validate(); err != nil {
		return out, err
	}
	if pd.ID != out.Binding.PolicyID || pd.IDs.WorkID != workID || pd.ActionIntentID != actionID || !policy.Allowed(pd) {
		return out, fmt.Errorf("review decision is not allowing or exact")
	}
	if pd.ExpiresAt != "" {
		expiry, err := time.Parse(time.RFC3339Nano, pd.ExpiresAt)
		if err != nil || (requireCurrent && !now.Before(expiry)) {
			return out, fmt.Errorf("review decision expired")
		}
	}
	out.Binding.ActionID, out.Binding.CID = actionID, f.CID
	if !requireCurrent {
		return out, nil
	}
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		return out, err
	}
	for _, class := range []string{"build-implement", PublicRedactionReviewCapability} {
		halted, _, err := policy.AutonomyHalted(s.home, class)
		if err != nil || halted {
			return out, fmt.Errorf("review authority halted or unavailable")
		}
	}
	fresh := policy.Evaluate(ai, cfg, now)
	if fresh.Outcome == "deny" {
		return out, fmt.Errorf("current policy denies review")
	}
	if !(policy.Allowed(fresh) && fresh.StandingGrantID == pd.StandingGrantID) {
		if pd.StandingGrantID != "" || fresh.Outcome != "hold" || !s.hasConsumedPublicReviewApproval(ai, pd, cfg, now) {
			return out, fmt.Errorf("current public-review authority unavailable")
		}
	}
	return out, nil
}

func (s *Store) hasConsumedPublicReviewApproval(ai envelope.ActionIntent, pd envelope.PolicyDecision, cfg policy.Config, now time.Time) bool {
	want := policy.ActionBindingDigest(policy.ActionBindingForIntent(ai, cfg))
	for _, ref := range pd.EvidenceRefs {
		id := strings.TrimPrefix(ref, "compound://events/")
		if id == ref || !strings.HasPrefix(id, "approval_") || strings.HasPrefix(id, "approval_plan_") || strings.HasPrefix(id, "approval_use_") {
			continue
		}
		status, err := policy.NewExactActionApprovalStore(s.home).Check(id, now)
		if err == nil && status.Status == policy.ExactApprovalStatusConsumed && status.BindingDigest == want && status.ExpiresAt == pd.ExpiresAt {
			return true
		}
	}
	return false
}

// Pin the native owner's path, reject symlinks, and bound before decoding. The
// finding API's ordinary inspection reader is not a deciding-proof validator.
func (s *Store) readPublicRedactionFinding(id string) (finding.Finding, error) {
	var f finding.Finding
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return f, err
	}
	defer root.Close()
	for _, name := range []string{"observed", finding.Subdir} {
		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return f, fmt.Errorf("review finding directory is unavailable or linked")
		}
	}
	name := filepath.Join(finding.Subdir, id+".json")
	before, err := root.Lstat(name)
	if err != nil {
		return f, err
	}
	const limit = 8 * finding.MaxBodyBytes
	if !before.Mode().IsRegular() || before.Size() > limit {
		return f, fmt.Errorf("review finding is not bounded regular evidence")
	}
	file, err := root.Open(name)
	if err != nil {
		return f, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return f, fmt.Errorf("review finding identity changed")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return f, err
	}
	after, err := root.Lstat(name)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || len(body) > limit {
		return f, fmt.Errorf("review finding changed or exceeded budget")
	}
	if err := consumerUniqueJSON(body); err != nil {
		return f, err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return f, err
	}
	digest := sha256.Sum256([]byte(f.Topic + "|" + f.Body))
	if f.ID != id || f.ID != "find_"+hex.EncodeToString(digest[:])[:12] || f.CID != finding.CID(f.Body) || len(f.Body) > finding.MaxBodyBytes || f.SourceClass != "evidence" || f.Privacy != finding.PrivacyPrivate || f.Visibility != finding.VisibilityOperational {
		return f, fmt.Errorf("review finding custody or classification invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, f.CreatedAt); err != nil {
		return f, fmt.Errorf("review finding timestamp invalid")
	}
	return f, nil
}

type publicRedactionContext struct{ Home, WorkID, Actor string }
type publicRedactionContextKey struct{}

func ContextWithPublicRedactionReview(ctx context.Context, homePath, workID, actor string) context.Context {
	return context.WithValue(ctx, publicRedactionContextKey{}, publicRedactionContext{homePath, workID, actor})
}

func PublicRedactionReviewForExecution(ctx context.Context, step ProofCheckStep, now time.Time) (PublicRedactionAuthority, error) {
	var out PublicRedactionAuthority
	binding, reviewed, err := PublicRedactionBindingForStep(step)
	if err != nil || !reviewed {
		return out, fmt.Errorf("reviewed execution binding missing or invalid")
	}
	identity, ok := ctx.Value(publicRedactionContextKey{}).(publicRedactionContext)
	plan, planned := ctx.Value(canonicalProofPlanContextKey{}).(CanonicalProofPlan)
	if !ok || !planned {
		return out, fmt.Errorf("reviewed execution needs owned work and canonical plan")
	}
	store, err := OpenReadOnly(identity.Home)
	if err != nil {
		return out, err
	}
	out, err = store.ResolvePublicRedactionReview(identity.WorkID, identity.Actor, binding.ActionID, plan.Repo, plan.Source, now)
	if err != nil {
		return out, err
	}
	if out.Binding != binding {
		return out, fmt.Errorf("reviewed execution authority drift")
	}
	return out, nil
}

func ValidatePublicRedactionEvidence(step ProofCheckStep, source *gitrepository.Snapshot, workID string) error {
	binding, reviewed, err := PublicRedactionBindingForStep(step)
	if err != nil {
		return err
	}
	if !reviewed {
		return nil
	}
	if step.Status != "ok" && step.RedactionEvidence == nil {
		return nil
	}
	e := step.RedactionEvidence
	if e == nil || e.Binding != binding || finding.CID(string(e.Review)) != binding.CID {
		return fmt.Errorf("review evidence missing or not profile-bound")
	}
	size := len(e.Review)
	for _, scan := range e.Scans {
		size += len(scan.Report) + len(scan.Path) + len(scan.SHA256)
	}
	if size > publicRedactionEvidenceLimit {
		return fmt.Errorf("redaction evidence exceeds bounded report budget")
	}
	review, err := ParsePublicRedactionReview(e.Review)
	if err != nil {
		return err
	}
	if source == nil || !reflect.DeepEqual(review.Source, *source) || review.WorkID != workID {
		return fmt.Errorf("review evidence is source or work mismatched")
	}
	if step.Status == "ok" {
		return ValidatePublicRedactionScans(review, e.Scans)
	}
	return nil
}

// This is checked inside proof admission and immediately before a new green
// finalizer transaction. Completed certificates retain their historical proof;
// replay of an already sealed finalizer does not execute another public action.
func (s *Store) validatePublicRedactionProfileAuthority(workID, actor string, profile ProofResourceProfile, now time.Time) error {
	for _, gate := range profile.Gates {
		step := ProofCheckStep{Name: gate.Name, Command: gate.Command, ProofRef: gate.ProofRef}
		binding, reviewed, err := PublicRedactionBindingForStep(step)
		if err != nil {
			return err
		}
		if !reviewed {
			continue
		}
		if profile.SourceSnapshot == nil {
			return fmt.Errorf("reviewed proof needs exact source identity")
		}
		authority, err := s.ResolvePublicRedactionReview(workID, actor, binding.ActionID, profile.Repo, *profile.SourceSnapshot, now)
		if err != nil {
			return err
		}
		if authority.Binding != binding {
			return fmt.Errorf("review authority differs from sealed proof profile")
		}
	}
	return nil
}
