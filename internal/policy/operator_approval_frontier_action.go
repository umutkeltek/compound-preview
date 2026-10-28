package policy

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
	"sync"
	"time"
)

// OperatorActionApprovalRequest is a durable action-approval entry minted by the
// PreToolUse hold path. It is filed under compound://registries/operator-approval-frontier
// alongside the read-only decision-family registry, so an agent that sees a hold can
// wait, take an alternative, or record blocked-with-ticket against a stable id.
type OperatorActionApprovalRequest struct {
	SchemaVersion   string `json:"schema_version"`
	ID              string `json:"id"`
	ActionIntentID  string `json:"action_intent_id"`
	Action          string `json:"action"`
	Capability      string `json:"capability,omitempty"`
	Target          string `json:"target,omitempty"`
	Harness         string `json:"harness"`
	MintedAt        string `json:"minted_at"`
	Status          string `json:"status"`
	OperatorGrantor string `json:"operator_grantor,omitempty"`
}

const OperatorActionApprovalRequestSchema = "compound.operator_action_approval_request.v0.1"

// ErrOperatorActionApprovalFrontierUnavailable signals the action-approval mint
// cannot read or write the frontier registry. The hook path keeps the original
// hold authoritative when this error surfaces so a downstream registry outage
// never breaks the gate.
var ErrOperatorActionApprovalFrontierUnavailable = errors.New("operator-approval-frontier registry unavailable")

// operatorActionApprovalFrontierLock serializes concurrent mints within one
// process. Cross-process safety comes from the atomic rename below.
var operatorActionApprovalFrontierLock sync.Mutex

// MintOperatorActionApprovalRequest records a new action-approval request in
// the operator-approval-frontier registry and returns the minted entry. The
// entry's id is the stable reference a held agent can wait on, take an
// alternative against, or record blocked-with-ticket against. The mint
// deliberately carries no resolve command: no CLI verb exists to approve
// these entries, and instructing one would convert a legible hold into a
// dead end. All-time count moves forward by exactly one on success; an
// existing seed of 3 therefore moves to 4 the first time a blocked action
// calls this function.
//
// The registry is loaded (or initialized when missing) and rewritten under a
// tmp-file + rename so a concurrent reader never observes a half-written file.
func MintOperatorActionApprovalRequest(
	homePath string,
	intentID, harness, action, capability, target string,
	now time.Time,
) (OperatorActionApprovalRequest, error) {
	if strings.TrimSpace(homePath) == "" {
		return OperatorActionApprovalRequest{}, fmt.Errorf("%w: compound home required", ErrOperatorActionApprovalFrontierUnavailable)
	}
	operatorActionApprovalFrontierLock.Lock()
	defer operatorActionApprovalFrontierLock.Unlock()

	path := OperatorApprovalFrontierPath(homePath)
	frontier, err := loadOrSeedOperatorApprovalFrontier(path)
	if err != nil {
		return OperatorActionApprovalRequest{}, err
	}

	requestID := "opactreq_" + shortOperatorActionApprovalHash(intentID, harness, action, target, now.UTC().Format(time.RFC3339Nano))
	request := OperatorActionApprovalRequest{
		SchemaVersion:  OperatorActionApprovalRequestSchema,
		ID:             requestID,
		ActionIntentID: strings.TrimSpace(intentID),
		Action:         strings.TrimSpace(action),
		Capability:     strings.TrimSpace(capability),
		Target:         strings.TrimSpace(target),
		Harness:        strings.TrimSpace(harness),
		MintedAt:       now.UTC().Format(time.RFC3339Nano),
		Status:         "pending",
	}

	frontier.ActionApprovalCount++
	frontier.ActionApprovals = append(frontier.ActionApprovals, request)
	if err := writeOperatorApprovalFrontierAtomic(path, frontier); err != nil {
		return OperatorActionApprovalRequest{}, err
	}
	return request, nil
}

// OperatorActionApprovalCount returns the all-time count of action-approval
// requests that have been minted in the frontier registry. When the registry is
// missing, the count is zero (no seed, no requests).
func OperatorActionApprovalCount(homePath string) (int, error) {
	if strings.TrimSpace(homePath) == "" {
		return 0, fmt.Errorf("%w: compound home required", ErrOperatorActionApprovalFrontierUnavailable)
	}
	path := OperatorApprovalFrontierPath(homePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	frontier, err := parseOperatorApprovalFrontier(data)
	if err != nil {
		return 0, err
	}
	return frontier.ActionApprovalCount, nil
}

// SeedOperatorActionApprovalFrontier primes the registry with a known count so
// a falsifier can demonstrate the count moves off that seed. Production callers
// never use this — it exists only for tests and operator onboarding scripts.
func SeedOperatorActionApprovalFrontier(homePath string, seed int, now time.Time) error {
	if strings.TrimSpace(homePath) == "" {
		return fmt.Errorf("%w: compound home required", ErrOperatorActionApprovalFrontierUnavailable)
	}
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		return err
	}
	path := OperatorApprovalFrontierPath(homePath)
	frontier, err := loadOrSeedOperatorApprovalFrontier(path)
	if err != nil {
		return err
	}
	if frontier.ActionApprovalCount < seed {
		frontier.ActionApprovalCount = seed
	}
	frontier.ActionApprovalsSeeded = true
	frontier.ActionApprovalsSeededAt = now.UTC().Format(time.RFC3339Nano)
	return writeOperatorApprovalFrontierAtomic(path, frontier)
}

// OperatorActionApprovalFrontierPath exposes the on-disk registry path so tests
// can inspect persisted state without re-deriving the layout.
func OperatorActionApprovalFrontierPath(homePath string) string {
	return OperatorApprovalFrontierPath(homePath)
}

func loadOrSeedOperatorApprovalFrontier(path string) (OperatorApprovalFrontier, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return OperatorApprovalFrontier{}, fmt.Errorf("%w: read %s: %v", ErrOperatorActionApprovalFrontierUnavailable, path, err)
		}
		return OperatorApprovalFrontier{
			SchemaVersion:       1,
			Status:              "operator_approval_frontier_ready",
			AuthorityBoundary:   "decision families remain evidence-only; action-approval requests are minted by the hook path",
			ActionApprovalCount: 0,
			ActionApprovals:     []OperatorActionApprovalRequest{},
		}, nil
	}
	return parseOperatorApprovalFrontier(data)
}

func writeOperatorApprovalFrontierAtomic(path string, frontier OperatorApprovalFrontier) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%w: mkdir %s: %v", ErrOperatorActionApprovalFrontierUnavailable, filepath.Dir(path), err)
	}
	encoded, err := json.MarshalIndent(frontier, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: marshal frontier: %v", ErrOperatorActionApprovalFrontierUnavailable, err)
	}
	tmp := path + ".tmp-" + shortOperatorActionApprovalHash(string(encoded), fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		return fmt.Errorf("%w: write tmp %s: %v", ErrOperatorActionApprovalFrontierUnavailable, tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: rename %s -> %s: %v", ErrOperatorActionApprovalFrontierUnavailable, tmp, path, err)
	}
	return nil
}

func shortOperatorActionApprovalHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// sortedOperatorActionApprovalIDs returns the registered request ids in a
// stable, sorted order so tests can assert exact sets without depending on
// slice order.
func sortedOperatorActionApprovalIDs(approvals []OperatorActionApprovalRequest) []string {
	ids := make([]string, 0, len(approvals))
	for _, a := range approvals {
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	return ids
}
