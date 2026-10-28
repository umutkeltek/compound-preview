package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"compound/internal/reviewartifact"
)

const (
	CapabilityLifecycleDispositionSchemaVersion     = "compound.capability.lifecycle_disposition.v0.1"
	CapabilityLifecycleDispositionPlanSchemaVersion = "compound.capability.lifecycle_disposition_plan.v0.1"
	CapabilityLifecycleReservationSchemaVersion     = "compound.capability.lifecycle_disposition_reservation.v0.1"

	CapabilityLifecycleErrorStaleRevision       = "stale_revision"
	CapabilityLifecycleErrorDispositionConflict = "disposition_conflict"

	CapabilityLifecycleOpenWork = "open_work"
	CapabilityLifecycleDefer    = "defer"
	CapabilityLifecycleReject   = "reject"
)

type CapabilityLifecycleReviewError struct {
	Code       string
	ProposalID string
	Detail     string
}

func (e *CapabilityLifecycleReviewError) Error() string { return e.Detail }

func CapabilityLifecycleReviewErrorCode(err error) string {
	var target *CapabilityLifecycleReviewError
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func capabilityLifecycleReviewError(code, proposalID, format string, args ...any) error {
	return &CapabilityLifecycleReviewError{Code: code, ProposalID: proposalID, Detail: fmt.Sprintf(format, args...)}
}

type CapabilityLifecycleProposalSnapshot struct {
	Proposal CapabilityLifecycleProposal `json:"proposal"`
	Hash     string                      `json:"proposal_hash"`
	Path     string                      `json:"proposal_path,omitempty"`
}

type CapabilityLifecycleDispositionInput struct {
	ProposalID   string `json:"proposal_id"`
	ExpectedHash string `json:"expected_hash"`
	Decision     string `json:"decision"`
	Rationale    string `json:"rationale"`
	Actor        string `json:"actor"`
	DeferUntil   string `json:"defer_until,omitempty"`
}

type CapabilityLifecycleDispositionPlan struct {
	SchemaVersion string                              `json:"schema_version"`
	ID            string                              `json:"id"`
	GeneratedAt   string                              `json:"generated_at"`
	Status        string                              `json:"status"`
	Input         CapabilityLifecycleDispositionInput `json:"input"`
	Snapshot      CapabilityLifecycleProposalSnapshot `json:"snapshot"`
	BeforeBand    string                              `json:"before_band"`
	AfterBand     string                              `json:"after_band"`
	AfterStatus   string                              `json:"after_status"`
	StateDelta    string                              `json:"state_delta"`
	Warnings      []string                            `json:"warnings"`
}

type CapabilityLifecycleDispositionLinks struct {
	WorkID           string `json:"work_id,omitempty"`
	WorkState        string `json:"work_state,omitempty"`
	WorkPlanID       string `json:"work_plan_id,omitempty"`
	ActionIntentID   string `json:"action_intent_id,omitempty"`
	PolicyDecisionID string `json:"policy_decision_id,omitempty"`
}

type CapabilityLifecycleDisposition struct {
	SchemaVersion        string   `json:"schema_version"`
	ID                   string   `json:"id"`
	CreatedAt            string   `json:"created_at"`
	Status               string   `json:"status"`
	ProposalID           string   `json:"proposal_id"`
	ProposalHash         string   `json:"proposal_hash"`
	Decision             string   `json:"decision"`
	Rationale            string   `json:"rationale"`
	Actor                string   `json:"actor"`
	DeferUntil           string   `json:"defer_until,omitempty"`
	SupersedesDecisionID string   `json:"supersedes_decision_id,omitempty"`
	BeforeBand           string   `json:"before_band"`
	AfterBand            string   `json:"after_band"`
	BeforeStatus         string   `json:"before_status"`
	AfterStatus          string   `json:"after_status"`
	Changed              bool     `json:"changed"`
	StateDelta           string   `json:"state_delta"`
	WorkID               string   `json:"work_id,omitempty"`
	WorkState            string   `json:"work_state,omitempty"`
	WorkPlanID           string   `json:"work_plan_id,omitempty"`
	ActionIntentID       string   `json:"action_intent_id,omitempty"`
	PolicyDecisionID     string   `json:"policy_decision_id,omitempty"`
	EvidenceRefs         []string `json:"evidence_refs"`
	Warnings             []string `json:"warnings"`
}

type CapabilityLifecycleDispositionReservation struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	ProposalID    string `json:"proposal_id"`
	ProposalHash  string `json:"proposal_hash"`
	Generation    string `json:"generation"`
	DispositionID string `json:"disposition_id"`
	Decision      string `json:"decision"`
	Rationale     string `json:"rationale"`
	Actor         string `json:"actor"`
	DeferUntil    string `json:"defer_until,omitempty"`
}

type CapabilityLifecycleWorkCreator func() (CapabilityLifecycleDispositionLinks, error)

func LoadCapabilityLifecycleProposalSnapshot(homePath, proposalID string) (CapabilityLifecycleProposalSnapshot, error) {
	proposalID = strings.TrimSpace(proposalID)
	if !validCapabilityLifecycleProposalID(proposalID) {
		return CapabilityLifecycleProposalSnapshot{}, fmt.Errorf("invalid capability lifecycle proposal id %q", proposalID)
	}
	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		return CapabilityLifecycleProposalSnapshot{}, err
	}
	path := filepath.Join(dir, proposalID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return CapabilityLifecycleProposalSnapshot{}, fmt.Errorf("read capability lifecycle proposal %s: %w", proposalID, err)
	}
	var proposal CapabilityLifecycleProposal
	if err := json.Unmarshal(data, &proposal); err != nil {
		return CapabilityLifecycleProposalSnapshot{}, fmt.Errorf("parse capability lifecycle proposal %s: %w", proposalID, err)
	}
	if proposal.SchemaVersion != CapabilityLifecycleProposalSchemaVersion || proposal.ID != proposalID {
		return CapabilityLifecycleProposalSnapshot{}, fmt.Errorf("capability lifecycle proposal %s identity/schema mismatch", proposalID)
	}
	if strings.TrimSpace(proposal.Statement) == "" || len(reviewartifact.Unique(proposal.EvidenceRefs)) == 0 {
		return CapabilityLifecycleProposalSnapshot{}, fmt.Errorf("capability lifecycle proposal %s is incomplete", proposalID)
	}
	digest := sha256.Sum256(data)
	return CapabilityLifecycleProposalSnapshot{Proposal: proposal, Hash: hex.EncodeToString(digest[:]), Path: path}, nil
}

func PlanCapabilityLifecycleDisposition(homePath string, in CapabilityLifecycleDispositionInput, now time.Time) (CapabilityLifecycleDispositionPlan, error) {
	in = normalizeCapabilityLifecycleDispositionInput(in)
	snapshot, err := LoadCapabilityLifecycleProposalSnapshot(homePath, in.ProposalID)
	if err != nil {
		return CapabilityLifecycleDispositionPlan{}, err
	}
	if in.ExpectedHash == "" || in.ExpectedHash != snapshot.Hash {
		return CapabilityLifecycleDispositionPlan{}, capabilityLifecycleReviewError(
			CapabilityLifecycleErrorStaleRevision,
			in.ProposalID,
			"capability lifecycle proposal %s changed since preview; refresh before deciding",
			in.ProposalID,
		)
	}
	if !capabilityLifecycleDispositionOK(in.Decision) {
		return CapabilityLifecycleDispositionPlan{}, fmt.Errorf("capability lifecycle decision must be open_work|defer|reject")
	}
	if in.Rationale == "" {
		return CapabilityLifecycleDispositionPlan{}, fmt.Errorf("capability lifecycle rationale required")
	}
	if in.Actor == "" {
		return CapabilityLifecycleDispositionPlan{}, fmt.Errorf("capability lifecycle actor required")
	}
	if in.Decision == CapabilityLifecycleDefer {
		if in.DeferUntil == "" {
			return CapabilityLifecycleDispositionPlan{}, fmt.Errorf("defer_until required when decision=defer")
		}
		revisitAt, err := time.Parse(time.RFC3339, in.DeferUntil)
		if err != nil || !revisitAt.After(now) {
			return CapabilityLifecycleDispositionPlan{}, fmt.Errorf("defer_until must be a future RFC3339 timestamp")
		}
	} else if in.DeferUntil != "" {
		return CapabilityLifecycleDispositionPlan{}, fmt.Errorf("defer_until is only valid when decision=defer")
	}
	afterBand, afterStatus, delta := capabilityLifecycleDispositionDelta(in.Decision, snapshot.Proposal.ID, in.DeferUntil)
	plan := CapabilityLifecycleDispositionPlan{
		SchemaVersion: CapabilityLifecycleDispositionPlanSchemaVersion,
		GeneratedAt:   reviewartifact.Timestamp(now),
		Status:        "ready",
		Input:         in,
		Snapshot:      snapshot,
		BeforeBand:    reviewartifact.BandNeedsYou,
		AfterBand:     afterBand,
		AfterStatus:   afterStatus,
		StateDelta:    delta,
		Warnings: []string{
			"does not mutate CapabilityGraph, skill files, adapters, or routing defaults",
			"decision_brief command_argv remains advisory and is never executed",
		},
	}
	plan.ID = lifecycleDispositionPlanID(plan)
	return plan, nil
}

func RecordCapabilityLifecycleDisposition(homePath string, plan CapabilityLifecycleDispositionPlan, links CapabilityLifecycleDispositionLinks) (CapabilityLifecycleDisposition, error) {
	var out CapabilityLifecycleDisposition
	err := withCapabilityLifecycleDispositionLock(homePath, plan.Input.ProposalID, func() error {
		if err := validateCapabilityLifecycleDispositionPlan(homePath, plan); err != nil {
			return err
		}
		_, existingOK, err := existingCapabilityLifecycleDisposition(homePath, plan)
		if err != nil {
			return err
		}
		if plan.Input.Decision == CapabilityLifecycleOpenWork {
			if !existingOK {
				if _, err := reserveCapabilityLifecycleDisposition(homePath, plan); err != nil {
					return err
				}
			}
		}
		var recordErr error
		out, recordErr = recordCapabilityLifecycleDisposition(homePath, plan, links)
		return recordErr
	})
	return out, err
}

// ApplyCapabilityLifecycleDisposition is the commit boundary for a semantic
// owner decision. It serializes each proposal and durably reserves the winning
// open_work semantics before invoking any callback that can admit work. An
// exact retry may resume after a crash; a different decision cannot create
// follow-on work for the same proposal generation.
func ApplyCapabilityLifecycleDisposition(homePath string, plan CapabilityLifecycleDispositionPlan, createWork CapabilityLifecycleWorkCreator) (CapabilityLifecycleDisposition, error) {
	var out CapabilityLifecycleDisposition
	err := withCapabilityLifecycleDispositionLock(homePath, plan.Input.ProposalID, func() error {
		if err := validateCapabilityLifecycleDispositionPlan(homePath, plan); err != nil {
			return err
		}
		if existing, ok, err := existingCapabilityLifecycleDisposition(homePath, plan); err != nil {
			return err
		} else if ok {
			out = existing
			return nil
		}

		links := CapabilityLifecycleDispositionLinks{}
		if plan.Input.Decision == CapabilityLifecycleOpenWork {
			if createWork == nil {
				return fmt.Errorf("open_work disposition requires a work creator")
			}
			if _, err := reserveCapabilityLifecycleDisposition(homePath, plan); err != nil {
				return err
			}
			var err error
			links, err = createWork()
			if err != nil {
				return err
			}
		}

		var err error
		out, err = recordCapabilityLifecycleDisposition(homePath, plan, links)
		return err
	})
	return out, err
}

func recordCapabilityLifecycleDisposition(homePath string, plan CapabilityLifecycleDispositionPlan, links CapabilityLifecycleDispositionLinks) (CapabilityLifecycleDisposition, error) {
	if err := validateCapabilityLifecycleDispositionPlan(homePath, plan); err != nil {
		return CapabilityLifecycleDisposition{}, err
	}
	if plan.Input.Decision == CapabilityLifecycleOpenWork {
		if strings.TrimSpace(links.WorkID) == "" || strings.TrimSpace(links.WorkState) != "proposed" || strings.TrimSpace(links.WorkPlanID) == "" || strings.TrimSpace(links.ActionIntentID) == "" || strings.TrimSpace(links.PolicyDecisionID) == "" {
			return CapabilityLifecycleDisposition{}, fmt.Errorf("open_work disposition requires proposed work plus action/policy refs")
		}
	} else if links != (CapabilityLifecycleDispositionLinks{}) {
		return CapabilityLifecycleDisposition{}, fmt.Errorf("%s disposition must not attach work refs", plan.Input.Decision)
	}

	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		return CapabilityLifecycleDisposition{}, err
	}
	report := capabilityLifecycleDispositionFromPlan(plan, links)
	history, err := capabilityLifecycleDispositionHistory(dir, plan.Input.ProposalID, report.ID)
	if err != nil {
		return CapabilityLifecycleDisposition{}, err
	}
	for _, existing := range history {
		if existing.ID == report.ID {
			return replayCapabilityLifecycleDisposition(existing, report)
		}
	}
	if len(history) > 0 {
		latest := history[len(history)-1]
		if latest.Decision != CapabilityLifecycleDefer {
			return CapabilityLifecycleDisposition{}, capabilityLifecycleReviewError(
				CapabilityLifecycleErrorDispositionConflict,
				report.ProposalID,
				"capability lifecycle proposal %s already has a different disposition",
				report.ProposalID,
			)
		}
		due, dueErr := time.Parse(time.RFC3339, latest.DeferUntil)
		plannedAt, planErr := time.Parse(time.RFC3339, plan.GeneratedAt)
		if dueErr != nil || planErr != nil || plannedAt.Before(due) {
			return CapabilityLifecycleDisposition{}, capabilityLifecycleReviewError(
				CapabilityLifecycleErrorDispositionConflict,
				report.ProposalID,
				"capability lifecycle proposal %s is deferred until %s",
				report.ProposalID,
				latest.DeferUntil,
			)
		}
		report.SupersedesDecisionID = latest.ID
		report.BeforeBand = reviewartifact.BandWatching
	}
	reservation, reserved, err := capabilityLifecycleReservationForHistory(dir, plan, history)
	if err != nil {
		return CapabilityLifecycleDisposition{}, err
	}
	if plan.Input.Decision == CapabilityLifecycleOpenWork {
		if !reserved {
			return CapabilityLifecycleDisposition{}, fmt.Errorf("open_work disposition has no durable winner reservation")
		}
		if !sameCapabilityLifecycleReservation(reservation, capabilityLifecycleReservationFromPlan(plan, reservation.Generation)) {
			return CapabilityLifecycleDisposition{}, capabilityLifecycleReviewError(
				CapabilityLifecycleErrorDispositionConflict,
				plan.Input.ProposalID,
				"capability lifecycle proposal %s reservation belongs to a different disposition",
				plan.Input.ProposalID,
			)
		}
	} else if reserved {
		return CapabilityLifecycleDisposition{}, capabilityLifecycleReviewError(
			CapabilityLifecycleErrorDispositionConflict,
			plan.Input.ProposalID,
			"capability lifecycle proposal %s has a reserved open_work disposition",
			plan.Input.ProposalID,
		)
	}
	path := filepath.Join(dir, plan.Input.ProposalID+"."+report.ID+".decision.json")
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return CapabilityLifecycleDisposition{}, err
	}
	return publishCapabilityLifecycleDisposition(path, report, append(data, '\n'))
}

// ExistingCapabilityLifecycleDisposition resolves idempotent replay and
// terminal/defer conflicts before a caller creates any follow-on work.
func ExistingCapabilityLifecycleDisposition(homePath string, plan CapabilityLifecycleDispositionPlan) (CapabilityLifecycleDisposition, bool, error) {
	return existingCapabilityLifecycleDisposition(homePath, plan)
}

func existingCapabilityLifecycleDisposition(homePath string, plan CapabilityLifecycleDispositionPlan) (CapabilityLifecycleDisposition, bool, error) {
	if err := validateCapabilityLifecycleDispositionPlan(homePath, plan); err != nil {
		return CapabilityLifecycleDisposition{}, false, err
	}
	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		return CapabilityLifecycleDisposition{}, false, err
	}
	wantID := capabilityLifecycleDispositionID(plan)
	history, err := capabilityLifecycleDispositionHistory(dir, plan.Input.ProposalID, wantID)
	if err != nil {
		return CapabilityLifecycleDisposition{}, false, err
	}
	for _, existing := range history {
		if existing.ID == wantID {
			existing.Status = "already_recorded"
			existing.Changed = false
			existing.StateDelta = "no change; identical disposition already recorded"
			return existing, true, nil
		}
	}
	if len(history) == 0 {
		if reservation, ok, err := capabilityLifecycleReservationForHistory(dir, plan, history); err != nil {
			return CapabilityLifecycleDisposition{}, false, err
		} else if ok {
			requested := capabilityLifecycleReservationFromPlan(plan, reservation.Generation)
			if !sameCapabilityLifecycleReservation(reservation, requested) {
				return CapabilityLifecycleDisposition{}, false, capabilityLifecycleReviewError(
					CapabilityLifecycleErrorDispositionConflict,
					plan.Input.ProposalID,
					"capability lifecycle proposal %s has a reserved open_work disposition",
					plan.Input.ProposalID,
				)
			}
		}
		return CapabilityLifecycleDisposition{}, false, nil
	}
	latest := history[len(history)-1]
	if latest.Decision != CapabilityLifecycleDefer {
		return CapabilityLifecycleDisposition{}, false, capabilityLifecycleReviewError(
			CapabilityLifecycleErrorDispositionConflict,
			plan.Input.ProposalID,
			"capability lifecycle proposal %s already has a different disposition",
			plan.Input.ProposalID,
		)
	}
	due, dueErr := time.Parse(time.RFC3339, latest.DeferUntil)
	plannedAt, planErr := time.Parse(time.RFC3339, plan.GeneratedAt)
	if dueErr != nil || planErr != nil || plannedAt.Before(due) {
		return CapabilityLifecycleDisposition{}, false, capabilityLifecycleReviewError(
			CapabilityLifecycleErrorDispositionConflict,
			plan.Input.ProposalID,
			"capability lifecycle proposal %s is deferred until %s",
			plan.Input.ProposalID,
			latest.DeferUntil,
		)
	}
	if reservation, ok, err := capabilityLifecycleReservationForHistory(dir, plan, history); err != nil {
		return CapabilityLifecycleDisposition{}, false, err
	} else if ok {
		requested := capabilityLifecycleReservationFromPlan(plan, reservation.Generation)
		if !sameCapabilityLifecycleReservation(reservation, requested) {
			return CapabilityLifecycleDisposition{}, false, capabilityLifecycleReviewError(
				CapabilityLifecycleErrorDispositionConflict,
				plan.Input.ProposalID,
				"capability lifecycle proposal %s has a reserved open_work disposition",
				plan.Input.ProposalID,
			)
		}
	}
	return CapabilityLifecycleDisposition{}, false, nil
}

func withCapabilityLifecycleDispositionLock(homePath, proposalID string, fn func() error) error {
	if !validCapabilityLifecycleProposalID(proposalID) {
		return fmt.Errorf("invalid capability lifecycle proposal id %q", proposalID)
	}
	lockPath := filepath.Join(homePath, "lock", "capability-lifecycle-dispositions", proposalID+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func reserveCapabilityLifecycleDisposition(homePath string, plan CapabilityLifecycleDispositionPlan) (CapabilityLifecycleDispositionReservation, error) {
	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		return CapabilityLifecycleDispositionReservation{}, err
	}
	history, err := capabilityLifecycleDispositionHistory(dir, plan.Input.ProposalID, capabilityLifecycleDispositionID(plan))
	if err != nil {
		return CapabilityLifecycleDispositionReservation{}, err
	}
	generation, err := capabilityLifecycleReservationGeneration(plan, history)
	if err != nil {
		return CapabilityLifecycleDispositionReservation{}, err
	}
	requested := capabilityLifecycleReservationFromPlan(plan, generation)
	path := capabilityLifecycleReservationPath(dir, requested.ProposalID, generation)
	data, err := json.MarshalIndent(requested, "", "  ")
	if err != nil {
		return CapabilityLifecycleDispositionReservation{}, err
	}
	return publishCapabilityLifecycleReservation(path, requested, append(data, '\n'))
}

func publishCapabilityLifecycleDisposition(path string, requested CapabilityLifecycleDisposition, data []byte) (CapabilityLifecycleDisposition, error) {
	if existing, ok := loadCapabilityLifecycleDisposition(path); ok {
		return replayCapabilityLifecycleDisposition(existing, requested)
	}
	repair, err := capabilityLifecycleCanonicalNeedsRepair(path)
	if err != nil {
		return CapabilityLifecycleDisposition{}, err
	}
	if err := publishCapabilityLifecycleCanonical(path, data, repair); err != nil {
		if errors.Is(err, os.ErrExist) {
			if existing, ok := loadCapabilityLifecycleDisposition(path); ok {
				return replayCapabilityLifecycleDisposition(existing, requested)
			}
		}
		return CapabilityLifecycleDisposition{}, err
	}
	return requested, nil
}

func publishCapabilityLifecycleReservation(path string, requested CapabilityLifecycleDispositionReservation, data []byte) (CapabilityLifecycleDispositionReservation, error) {
	existing, loadErr := loadCapabilityLifecycleReservation(path)
	if loadErr == nil {
		if sameCapabilityLifecycleReservation(existing, requested) {
			return existing, nil
		}
		return CapabilityLifecycleDispositionReservation{}, capabilityLifecycleReviewError(
			CapabilityLifecycleErrorDispositionConflict,
			requested.ProposalID,
			"capability lifecycle proposal %s already has a different reserved disposition",
			requested.ProposalID,
		)
	}
	if !errors.Is(loadErr, os.ErrNotExist) {
		return CapabilityLifecycleDispositionReservation{}, loadErr
	}
	if err := publishCapabilityLifecycleCanonical(path, data, false); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, loadErr := loadCapabilityLifecycleReservation(path)
			if loadErr == nil {
				if sameCapabilityLifecycleReservation(existing, requested) {
					return existing, nil
				}
				return CapabilityLifecycleDispositionReservation{}, capabilityLifecycleReviewError(
					CapabilityLifecycleErrorDispositionConflict,
					requested.ProposalID,
					"capability lifecycle proposal %s already has a different reserved disposition",
					requested.ProposalID,
				)
			}
		}
		return CapabilityLifecycleDispositionReservation{}, err
	}
	return requested, nil
}

// capabilityLifecycleCanonicalNeedsRepair distinguishes a missing canonical
// path from a readable but invalid file. Read failures and non-regular paths
// remain hard errors; only a poison file left by the former direct writer may
// be replaced under the per-proposal lock.
func capabilityLifecycleCanonicalNeedsRepair(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("capability lifecycle canonical path %s is not a regular file", path)
	}
	if _, err := os.ReadFile(path); err != nil {
		return false, err
	}
	return true, nil
}

// publishCapabilityLifecycleCanonical writes and syncs a same-directory temp
// file before publishing it. New canonical paths use link(2) for atomic
// no-replace semantics. A readable poison file may be atomically replaced only
// while the caller holds the proposal lock.
func publishCapabilityLifecycleCanonical(path string, data []byte, repair bool) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if repair {
		if err := os.Rename(tmpPath, path); err != nil {
			return err
		}
	} else {
		if err := os.Link(tmpPath, path); err != nil {
			return err
		}
		if err := os.Remove(tmpPath); err != nil {
			return err
		}
	}
	return syncCapabilityLifecycleDirectory(dir)
}

func syncCapabilityLifecycleDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

func capabilityLifecycleReservationForHistory(dir string, plan CapabilityLifecycleDispositionPlan, history []CapabilityLifecycleDisposition) (CapabilityLifecycleDispositionReservation, bool, error) {
	generation, err := capabilityLifecycleReservationGeneration(plan, history)
	if err != nil {
		return CapabilityLifecycleDispositionReservation{}, false, err
	}
	path := capabilityLifecycleReservationPath(dir, plan.Input.ProposalID, generation)
	existing, err := loadCapabilityLifecycleReservation(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CapabilityLifecycleDispositionReservation{}, false, nil
		}
		return CapabilityLifecycleDispositionReservation{}, false, err
	}
	return existing, true, nil
}

func capabilityLifecycleReservationGeneration(plan CapabilityLifecycleDispositionPlan, history []CapabilityLifecycleDisposition) (string, error) {
	if len(history) == 0 {
		return "root", nil
	}
	latest := history[len(history)-1]
	if latest.Decision != CapabilityLifecycleDefer {
		return "", capabilityLifecycleReviewError(
			CapabilityLifecycleErrorDispositionConflict,
			plan.Input.ProposalID,
			"capability lifecycle proposal %s already has a different disposition",
			plan.Input.ProposalID,
		)
	}
	due, dueErr := time.Parse(time.RFC3339, latest.DeferUntil)
	plannedAt, planErr := time.Parse(time.RFC3339, plan.GeneratedAt)
	if dueErr != nil || planErr != nil || plannedAt.Before(due) {
		return "", capabilityLifecycleReviewError(
			CapabilityLifecycleErrorDispositionConflict,
			plan.Input.ProposalID,
			"capability lifecycle proposal %s is deferred until %s",
			plan.Input.ProposalID,
			latest.DeferUntil,
		)
	}
	return latest.ID, nil
}

func capabilityLifecycleReservationFromPlan(plan CapabilityLifecycleDispositionPlan, generation string) CapabilityLifecycleDispositionReservation {
	dispositionID := capabilityLifecycleDispositionID(plan)
	reservation := CapabilityLifecycleDispositionReservation{
		SchemaVersion: CapabilityLifecycleReservationSchemaVersion,
		CreatedAt:     plan.GeneratedAt,
		ProposalID:    plan.Input.ProposalID,
		ProposalHash:  plan.Snapshot.Hash,
		Generation:    generation,
		DispositionID: dispositionID,
		Decision:      plan.Input.Decision,
		Rationale:     plan.Input.Rationale,
		Actor:         plan.Input.Actor,
		DeferUntil:    plan.Input.DeferUntil,
	}
	reservation.ID = reviewartifact.ID("cldr_", reservation.ProposalID, reservation.ProposalHash, reservation.Generation, reservation.DispositionID)
	return reservation
}

func capabilityLifecycleReservationPath(dir, proposalID, generation string) string {
	return filepath.Join(dir, proposalID+"."+generation+".reservation.decision.json")
}

func loadCapabilityLifecycleReservation(path string) (CapabilityLifecycleDispositionReservation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CapabilityLifecycleDispositionReservation{}, err
	}
	var reservation CapabilityLifecycleDispositionReservation
	if err := json.Unmarshal(data, &reservation); err != nil {
		return CapabilityLifecycleDispositionReservation{}, fmt.Errorf("parse capability lifecycle reservation: %w", err)
	}
	if !validCapabilityLifecycleReservation(reservation) {
		return CapabilityLifecycleDispositionReservation{}, fmt.Errorf("capability lifecycle reservation identity invalid")
	}
	return reservation, nil
}

func validCapabilityLifecycleReservation(reservation CapabilityLifecycleDispositionReservation) bool {
	wantID := reviewartifact.ID("cldr_", reservation.ProposalID, reservation.ProposalHash, reservation.Generation, reservation.DispositionID)
	return reservation.SchemaVersion == CapabilityLifecycleReservationSchemaVersion &&
		reservation.ID == wantID &&
		validCapabilityLifecycleProposalID(reservation.ProposalID) &&
		reservation.ProposalHash != "" &&
		reservation.Generation != "" &&
		strings.HasPrefix(reservation.DispositionID, "cld_") &&
		reservation.Decision == CapabilityLifecycleOpenWork &&
		strings.TrimSpace(reservation.Rationale) != "" &&
		strings.TrimSpace(reservation.Actor) != ""
}

func sameCapabilityLifecycleReservation(existing, requested CapabilityLifecycleDispositionReservation) bool {
	return existing.ID == requested.ID &&
		existing.ProposalID == requested.ProposalID &&
		existing.ProposalHash == requested.ProposalHash &&
		existing.Generation == requested.Generation &&
		existing.DispositionID == requested.DispositionID &&
		existing.Decision == requested.Decision &&
		existing.Rationale == requested.Rationale &&
		existing.Actor == requested.Actor &&
		existing.DeferUntil == requested.DeferUntil
}

func capabilityLifecycleReservationFromExtras(paths []string) (CapabilityLifecycleDispositionReservation, bool) {
	var found CapabilityLifecycleDispositionReservation
	for _, path := range paths {
		if !strings.HasSuffix(path, ".reservation.decision.json") {
			continue
		}
		reservation, err := loadCapabilityLifecycleReservation(path)
		if err == nil && (found.ID == "" || reservation.CreatedAt > found.CreatedAt || (reservation.CreatedAt == found.CreatedAt && reservation.ID > found.ID)) {
			found = reservation
		}
	}
	return found, found.ID != ""
}

func capabilityLifecycleDispositionFromExtras(paths []string) (CapabilityLifecycleDisposition, bool) {
	var found CapabilityLifecycleDisposition
	for _, path := range paths {
		if !strings.HasSuffix(path, ".decision.json") {
			continue
		}
		report, ok := loadCapabilityLifecycleDisposition(path)
		if ok && (found.ID == "" || report.CreatedAt > found.CreatedAt || (report.CreatedAt == found.CreatedAt && report.ID > found.ID)) {
			found = report
		}
	}
	return found, found.ID != ""
}

func capabilityLifecycleDispositionFromExtrasByID(paths []string, dispositionID string) (CapabilityLifecycleDisposition, bool) {
	for _, path := range paths {
		if !strings.HasSuffix(path, ".decision.json") {
			continue
		}
		report, ok := loadCapabilityLifecycleDisposition(path)
		if ok && report.ID == dispositionID {
			return report, true
		}
	}
	return CapabilityLifecycleDisposition{}, false
}

func capabilityLifecycleDecisionCard(snapshot CapabilityLifecycleProposalSnapshot, disposition *CapabilityLifecycleDisposition) *reviewartifact.DecisionCard {
	p := snapshot.Proposal
	meta := map[string]string{
		"proposal_hash":  snapshot.Hash,
		"proposal_kind":  p.ProposalKind,
		"capability_ref": p.Draft.CapabilityRef,
		"risk":           "local_reversible",
		"reversibility":  "easy_before_work_execution",
	}
	if disposition != nil {
		meta["decision_id"] = disposition.ID
		meta["decision"] = disposition.Decision
		meta["decision_actor"] = disposition.Actor
		meta["decision_rationale"] = disposition.Rationale
		meta["state_delta"] = disposition.StateDelta
		meta["work_id"] = disposition.WorkID
		meta["defer_until"] = disposition.DeferUntil
	}
	card := &reviewartifact.DecisionCard{
		Claim: p.Statement,
		Why: []reviewartifact.DecisionCardReason{
			{Label: "proposal", Question: "What would change?", Detail: reviewartifact.FirstNonEmpty(p.Detail, p.Statement)},
			{Label: "evidence", Question: "What supports it?", Detail: fmt.Sprintf("%d evidence refs; %.0f%% source confidence", len(p.EvidenceRefs), p.Confidence*100)},
			{Label: "boundary", Question: "What cannot happen here?", Detail: p.Draft.Guardrail},
		},
		Choices: []reviewartifact.DecisionCardChoice{
			{ID: CapabilityLifecycleOpenWork, Label: "Turn into tracked work", Effect: "Records this disposition and creates one proposed WorkEnvelope. It does not mutate capability authority."},
			{ID: CapabilityLifecycleDefer, Label: "Defer with a trigger", Effect: "Moves the proposal to Watching until the named revisit time; creates no work."},
			{ID: CapabilityLifecycleReject, Label: "Reject proposal", Effect: "Files the proposal with your rationale; creates no work and changes no capability authority."},
		},
		OptionalRationale:   false,
		CanonicalActionHint: "compound capability lifecycle-review --proposal <id> --expected-hash <sha256> --decision <open_work|defer|reject> --rationale <why>",
		Boundary: []string{
			"no CapabilityGraph, skill, adapter, or route mutation",
			"open_work creates proposed work only; lifecycle advance, claim, execution, and close remain separate",
			"decision brief commands are never execution authority",
		},
		Meta: meta,
	}
	if disposition != nil {
		card.Why = append(card.Why, reviewartifact.DecisionCardReason{
			Label:    "recorded decision",
			Question: "What did the owner record?",
			Detail:   fmt.Sprintf("%s recorded %s: %s", disposition.Actor, disposition.Decision, disposition.Rationale),
		})
	}
	return card
}

func capabilityLifecycleReservationDecisionCard(snapshot CapabilityLifecycleProposalSnapshot, reservation CapabilityLifecycleDispositionReservation) *reviewartifact.DecisionCard {
	card := capabilityLifecycleDecisionCard(snapshot, nil)
	card.Choices = []reviewartifact.DecisionCardChoice{{
		ID:             "resume_open_work",
		Label:          "Resume tracked work",
		Effect:         "Retries the exact reserved disposition and repairs or completes its proposed WorkEnvelope; no alternate decision is allowed.",
		RequiresReview: true,
	}}
	card.Why = append(card.Why, reviewartifact.DecisionCardReason{
		Label:    "reserved decision",
		Question: "What is safe to resume?",
		Detail:   fmt.Sprintf("%s reserved open_work with rationale: %s", reservation.Actor, reservation.Rationale),
	})
	card.Meta["reservation_id"] = reservation.ID
	card.Meta["reserved_disposition_id"] = reservation.DispositionID
	card.Meta["decision"] = reservation.Decision
	card.Meta["reserved_actor"] = reservation.Actor
	card.CanonicalActionHint = "compound capability lifecycle-review resumes the exact server-owned reservation"
	return card
}

func capabilityLifecycleResumeAction(reservation CapabilityLifecycleDispositionReservation) reviewartifact.Action {
	argv := []string{
		"compound", "capability", "lifecycle-review",
		"--proposal", reservation.ProposalID,
		"--expected-hash", reservation.ProposalHash,
		"--decision", CapabilityLifecycleOpenWork,
		"--rationale", reservation.Rationale,
		"--actor", reservation.Actor,
		"--json",
	}
	return reviewartifact.Action{
		ID:             "resume_open_work",
		Label:          "Resume tracked work",
		Command:        "compound capability lifecycle-review --proposal " + reservation.ProposalID + " --decision open_work",
		Argv:           argv,
		RequiresReview: true,
		PolicyNote:     "Resumes only the exact hash, actor, rationale, and open_work semantics already held by the durable reservation.",
	}
}

func capabilityLifecycleDecisionActions() []reviewartifact.Action {
	return []reviewartifact.Action{
		{
			ID:             CapabilityLifecycleOpenWork,
			Label:          "Turn into tracked work",
			Command:        "compound capability lifecycle-review --decision open_work",
			RequiresReview: true,
			PolicyNote:     "Requires the rendered proposal hash and operator rationale before creating proposed work.",
		},
		{
			ID:             CapabilityLifecycleDefer,
			Label:          "Defer with a trigger",
			Command:        "compound capability lifecycle-review --decision defer",
			RequiresReview: true,
			PolicyNote:     "Requires the rendered proposal hash, operator rationale, and a future revisit time.",
		},
		{
			ID:             CapabilityLifecycleReject,
			Label:          "Reject proposal",
			Command:        "compound capability lifecycle-review --decision reject",
			RequiresReview: true,
			PolicyNote:     "Requires the rendered proposal hash and operator rationale; changes review state only.",
		},
	}
}

func normalizeCapabilityLifecycleDispositionInput(in CapabilityLifecycleDispositionInput) CapabilityLifecycleDispositionInput {
	in.ProposalID = strings.TrimSpace(in.ProposalID)
	in.ExpectedHash = strings.ToLower(strings.TrimSpace(in.ExpectedHash))
	in.Decision = strings.TrimSpace(in.Decision)
	in.Rationale = strings.TrimSpace(in.Rationale)
	in.Actor = strings.TrimSpace(in.Actor)
	in.DeferUntil = strings.TrimSpace(in.DeferUntil)
	return in
}

func capabilityLifecycleDispositionOK(decision string) bool {
	return decision == CapabilityLifecycleOpenWork || decision == CapabilityLifecycleDefer || decision == CapabilityLifecycleReject
}

func validCapabilityLifecycleProposalID(id string) bool {
	if !strings.HasPrefix(id, "clp_") || len(id) <= len("clp_") {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func capabilityLifecycleDispositionDelta(decision, proposalID, deferUntil string) (string, string, string) {
	switch decision {
	case CapabilityLifecycleOpenWork:
		return reviewartifact.BandFiled, "tracked_work", fmt.Sprintf("proposal %s: needs_you -> filed; proposed work created", proposalID)
	case CapabilityLifecycleDefer:
		return reviewartifact.BandWatching, "deferred", fmt.Sprintf("proposal %s: needs_you -> watching until %s", proposalID, deferUntil)
	default:
		return reviewartifact.BandFiled, "rejected", fmt.Sprintf("proposal %s: needs_you -> filed as rejected", proposalID)
	}
}

func lifecycleDispositionPlanID(plan CapabilityLifecycleDispositionPlan) string {
	return reviewartifact.ID("cldp_", plan.GeneratedAt, plan.Input.ProposalID, plan.Snapshot.Hash, plan.Input.Decision, plan.Input.Rationale, plan.Input.Actor, plan.Input.DeferUntil)
}

func validateCapabilityLifecycleDispositionPlan(homePath string, plan CapabilityLifecycleDispositionPlan) error {
	if plan.SchemaVersion != CapabilityLifecycleDispositionPlanSchemaVersion || plan.ID != lifecycleDispositionPlanID(plan) {
		return fmt.Errorf("capability lifecycle disposition plan identity mismatch")
	}
	normalized := normalizeCapabilityLifecycleDispositionInput(plan.Input)
	if normalized != plan.Input || !capabilityLifecycleDispositionOK(plan.Input.Decision) || plan.Input.Rationale == "" || plan.Input.Actor == "" {
		return fmt.Errorf("capability lifecycle disposition plan semantic inputs invalid")
	}
	current, err := LoadCapabilityLifecycleProposalSnapshot(homePath, plan.Input.ProposalID)
	if err != nil {
		return err
	}
	if current.Hash != plan.Snapshot.Hash || current.Hash != plan.Input.ExpectedHash {
		return capabilityLifecycleReviewError(
			CapabilityLifecycleErrorStaleRevision,
			plan.Input.ProposalID,
			"capability lifecycle proposal %s changed since preview; refresh before deciding",
			plan.Input.ProposalID,
		)
	}
	afterBand, afterStatus, delta := capabilityLifecycleDispositionDelta(plan.Input.Decision, plan.Input.ProposalID, plan.Input.DeferUntil)
	if plan.Snapshot.Proposal.ID != plan.Input.ProposalID || plan.BeforeBand != reviewartifact.BandNeedsYou || plan.AfterBand != afterBand || plan.AfterStatus != afterStatus || plan.StateDelta != delta {
		return fmt.Errorf("capability lifecycle disposition plan state delta invalid")
	}
	return nil
}

func capabilityLifecycleDispositionFromPlan(plan CapabilityLifecycleDispositionPlan, links CapabilityLifecycleDispositionLinks) CapabilityLifecycleDisposition {
	id := capabilityLifecycleDispositionID(plan)
	refs := reviewartifact.Unique(append(append([]string{}, plan.Snapshot.Proposal.EvidenceRefs...),
		"capability_lifecycle_proposal:"+plan.Input.ProposalID,
		"sha256:"+plan.Snapshot.Hash,
	))
	return CapabilityLifecycleDisposition{
		SchemaVersion: CapabilityLifecycleDispositionSchemaVersion,
		ID:            id, CreatedAt: plan.GeneratedAt, Status: "recorded",
		ProposalID: plan.Input.ProposalID, ProposalHash: plan.Snapshot.Hash,
		Decision: plan.Input.Decision, Rationale: plan.Input.Rationale, Actor: plan.Input.Actor, DeferUntil: plan.Input.DeferUntil,
		BeforeBand: plan.BeforeBand, AfterBand: plan.AfterBand, BeforeStatus: plan.Snapshot.Proposal.Status, AfterStatus: plan.AfterStatus,
		Changed: true, StateDelta: plan.StateDelta,
		WorkID: links.WorkID, WorkState: links.WorkState, WorkPlanID: links.WorkPlanID,
		ActionIntentID: links.ActionIntentID, PolicyDecisionID: links.PolicyDecisionID,
		EvidenceRefs: refs, Warnings: append([]string{}, plan.Warnings...),
	}
}

func capabilityLifecycleDispositionID(plan CapabilityLifecycleDispositionPlan) string {
	return reviewartifact.ID("cld_", plan.Input.ProposalID, plan.Snapshot.Hash, plan.Input.Decision, plan.Input.Rationale, plan.Input.Actor, plan.Input.DeferUntil)
}

func loadCapabilityLifecycleDisposition(path string) (CapabilityLifecycleDisposition, bool) {
	report, err := readCapabilityLifecycleDisposition(path)
	return report, err == nil
}

func readCapabilityLifecycleDisposition(path string) (CapabilityLifecycleDisposition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CapabilityLifecycleDisposition{}, err
	}
	var report CapabilityLifecycleDisposition
	if err := json.Unmarshal(data, &report); err != nil {
		return CapabilityLifecycleDisposition{}, err
	}
	if !validCapabilityLifecycleDisposition(report) {
		return CapabilityLifecycleDisposition{}, fmt.Errorf("capability lifecycle disposition invalid")
	}
	return report, nil
}

func validCapabilityLifecycleDisposition(report CapabilityLifecycleDisposition) bool {
	if report.SchemaVersion != CapabilityLifecycleDispositionSchemaVersion || report.ID == "" || report.Status != "recorded" || !validCapabilityLifecycleProposalID(report.ProposalID) || report.ProposalHash == "" || !capabilityLifecycleDispositionOK(report.Decision) || strings.TrimSpace(report.Rationale) == "" || strings.TrimSpace(report.Actor) == "" {
		return false
	}
	wantID := reviewartifact.ID("cld_", report.ProposalID, report.ProposalHash, report.Decision, report.Rationale, report.Actor, report.DeferUntil)
	afterBand, afterStatus, delta := capabilityLifecycleDispositionDelta(report.Decision, report.ProposalID, report.DeferUntil)
	if report.ID != wantID || report.AfterBand != afterBand || report.AfterStatus != afterStatus || report.StateDelta != delta {
		return false
	}
	if report.BeforeBand != reviewartifact.BandNeedsYou && report.BeforeBand != reviewartifact.BandWatching {
		return false
	}
	if report.Decision == CapabilityLifecycleOpenWork {
		return report.WorkID != "" && report.WorkState == "proposed" && report.WorkPlanID != "" && report.ActionIntentID != "" && report.PolicyDecisionID != ""
	}
	return report.WorkID == "" && report.WorkState == "" && report.WorkPlanID == "" && report.ActionIntentID == "" && report.PolicyDecisionID == ""
}

func capabilityLifecycleDispositionHistory(dir, proposalID, repairableDispositionID string) ([]CapabilityLifecycleDisposition, error) {
	matches, err := filepath.Glob(filepath.Join(dir, proposalID+".*.decision.json"))
	if err != nil {
		return nil, err
	}
	out := make([]CapabilityLifecycleDisposition, 0, len(matches))
	for _, path := range matches {
		if strings.HasSuffix(path, ".reservation.decision.json") {
			continue
		}
		report, err := readCapabilityLifecycleDisposition(path)
		if err != nil {
			// The requested ID commits to the plan semantics, so only that exact
			// canonical path is eligible for the existing torn-write repair path.
			if repairableDispositionID != "" && filepath.Base(path) == proposalID+"."+repairableDispositionID+".decision.json" {
				continue
			}
			return nil, capabilityLifecycleReviewError(
				CapabilityLifecycleErrorDispositionConflict,
				proposalID,
				"capability lifecycle proposal %s has unreadable disposition evidence %s",
				proposalID,
				filepath.Base(path),
			)
		}
		if report.ProposalID != proposalID {
			return nil, capabilityLifecycleReviewError(
				CapabilityLifecycleErrorDispositionConflict,
				proposalID,
				"capability lifecycle proposal %s has mismatched disposition evidence %s",
				proposalID,
				filepath.Base(path),
			)
		}
		out = append(out, report)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func replayCapabilityLifecycleDisposition(existing, requested CapabilityLifecycleDisposition) (CapabilityLifecycleDisposition, error) {
	if existing.ID != requested.ID || existing.ProposalHash != requested.ProposalHash || existing.Decision != requested.Decision || existing.Rationale != requested.Rationale || existing.Actor != requested.Actor || existing.DeferUntil != requested.DeferUntil || existing.WorkID != requested.WorkID || existing.WorkState != requested.WorkState || existing.WorkPlanID != requested.WorkPlanID || existing.ActionIntentID != requested.ActionIntentID || existing.PolicyDecisionID != requested.PolicyDecisionID {
		return CapabilityLifecycleDisposition{}, capabilityLifecycleReviewError(
			CapabilityLifecycleErrorDispositionConflict,
			requested.ProposalID,
			"capability lifecycle proposal %s already has a different disposition",
			requested.ProposalID,
		)
	}
	existing.Status = "already_recorded"
	existing.Changed = false
	existing.StateDelta = "no change; identical disposition already recorded"
	return existing, nil
}
