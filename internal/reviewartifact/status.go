package reviewartifact

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// terminalStatuses is the ONE owner of "this review artifact is finished".
//
// Before this existed the same six-word literal was copied into three
// identically-bodied private predicates under three different names —
// internal/home's terminalReviewStatus, internal/director's
// reviewStatusTerminal, and this package's terminalResolutionStatus — plus
// several inline switches. Every copy omitted "completed", so every consumer
// independently billed finished work to the operator as a pending decision.
//
// The measured cost of that omission on a live home: 107 of 191
// review_required rows were `status: completed` worktree-retirement RESULTS,
// i.e. governed work that had already run to completion. The operator's
// backlog read 56% larger than it was.
//
// A status word belongs here when some producer in this repo writes it to mean
// "there is nothing left to decide". Add to this set rather than to a caller;
// a seventh copy of the literal is how the sixth one drifted.
var terminalStatuses = map[string]bool{
	"ok":                     true,
	"applied":                true,
	"resolved":               true,
	"accepted":               true,
	"recorded":               true,
	"candidate_recorded":     true,
	"authorization_recorded": true,
	"completed":              true,
}

// TerminalStatus reports whether a review artifact status means the work is
// finished and carries no remaining operator decision. Callers in other
// packages must use this instead of re-listing the words.
func TerminalStatus(status string) bool {
	return terminalStatuses[strings.TrimSpace(status)]
}

// OwnerResult is an owner-written `<stem>.result.json` sibling of a core
// review artifact whose status is terminal. It is the owner saying "this plan
// ran to completion" in its own schema, outside the generic
// `<stem>.reviewed.json` resolution sidecar.
type OwnerResult struct {
	ID     string
	Status string
	Path   string
}

// ownerTerminalResult reads `<dir>/<stem>.result.json` and returns it only
// when its status is terminal. A missing or non-terminal result returns nil.
//
// Why this exists: a plan artifact freezes its birth status (`review_required`)
// into the file; the owner later records the outcome in a sibling result. The
// result is itself counted OK, but nothing carried that verdict back onto the
// plan, so 651 completed worktree retirements kept surfacing as decisions the
// operator owed. The rule: a terminal owner result on the same stem
// terminalizes the plan. A result that is not terminal (still running) leaves
// the plan open — the fold must never hide a retirement that has not happened.
func ownerTerminalResult(dir, stem string) (*OwnerResult, error) {
	path := filepath.Join(dir, stem+".result.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var payload struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil // an unreadable result is no verdict; leave the plan open
	}
	if !TerminalStatus(payload.Status) {
		return nil, nil
	}
	return &OwnerResult{ID: strings.TrimSpace(payload.ID), Status: strings.TrimSpace(payload.Status), Path: path}, nil
}
