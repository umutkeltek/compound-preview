package hook

// Per-session, delta-triggered memory injection.
//
// The live user-prompt hook re-materializes the full selected memory on EVERY
// prompt. Without a per-session memory the same claim://position:// refs are
// re-injected turn after turn — the model already holds them in its context
// window, so every repeat is wasted budget. This file makes injection delta-based:
// a small rewrite-in-place state file per session (custodied under
// COMPOUND_HOME/observed/injection-sessions/) remembers which refs were already
// injected and the classification that was in play, so a later prompt injects
// only NEW refs and records each suppression as an Excluded entry (the pack stays
// honest about what it withheld and why).
//
// Two re-opening triggers let a suppressed ref inject again: (1) a position turned
// contested since it was injected, and (2) the task-class / topic-seed
// classification changed versus the session's last prompt.
//
// FAIL-OPEN is load-bearing: an absent, corrupt, foreign, or unwritable state file
// yields the pack UNCHANGED — byte-for-byte identical to the no-delta path — so a
// state-layer fault can never silently starve the model of memory. The delta is a
// pure post-materialization transform in the hook (the client), never in contract's
// pack build, so the ContextPack itself stays a deterministic function of its input.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/contract"
	"compound/internal/envelope"
)

const (
	injectionSessionSchema  = "compound.injection_session.v0.1"
	injectionSessionDir     = "observed/injection-sessions"
	injectionSuppressReason = "already-injected this session"
)

// injectionStateStatus separates the three read outcomes because only a healthy
// prior state (OK) may drive suppression; absent and corrupt both fail open.
type injectionStateStatus int

const (
	injectionStateAbsent injectionStateStatus = iota
	injectionStateCorrupt
	injectionStateOK
)

// injectedRefRecord is one already-injected memory ref. Contested pins the
// position's contested flag as of the moment it was (re)injected, so a later flip
// from calm to contested re-opens it exactly once.
type injectedRefRecord struct {
	Kind      string `json:"kind"` // "claim" | "position"
	Contested bool   `json:"contested,omitempty"`
}

// injectionSessionState is the whole per-session record. SessionKey is the HASHED
// session id (raw session ids never touch disk here, matching the hook's other
// session-scoped state). Injected is keyed by ref; Go marshals map keys sorted, so
// the file is reproducible.
type injectionSessionState struct {
	Schema         string                       `json:"schema"`
	SessionKey     string                       `json:"session_key"`
	Classification string                       `json:"classification"`
	Injected       map[string]injectedRefRecord `json:"injected"`
	UpdatedAt      string                       `json:"updated_at"`
}

// applyInjectionDelta suppresses memory/position refs already injected earlier in
// this session and injects only NEW (or re-opened) refs, recording each suppression
// as an Excluded entry. It returns the pack UNCHANGED whenever it cannot safely
// suppress (no session scope, absent/corrupt/unwritable state, or a changed
// classification), so those paths are byte-for-byte identical to today.
func applyInjectionDelta(pack envelope.ContextPack, in contract.Input, homePath, sessionID string, now time.Time) envelope.ContextPack {
	sessionID = strings.TrimSpace(sessionID)
	if strings.TrimSpace(homePath) == "" || sessionID == "" {
		// No session scope → nothing to diff against. Full injection (fail open).
		return pack
	}

	prior, status := readInjectionSessionState(homePath, sessionID)
	classification := injectionClassificationKey(pack, in)

	// Only a healthy prior state whose classification still matches may suppress.
	// A changed classification re-opens everything for this prompt (context pivoted).
	suppressing := status == injectionStateOK && prior.Classification == classification

	// Current contested flag per position ref, read from the freshly materialized pack.
	contested := make(map[string]bool, len(pack.ActivePositions))
	for _, p := range pack.ActivePositions {
		contested[p.Ref] = p.Contested
	}

	next := injectionSessionState{
		Schema:         injectionSessionSchema,
		SessionKey:     hashStr(sessionID),
		Classification: classification,
		Injected:       make(map[string]injectedRefRecord),
		UpdatedAt:      now.UTC().Format(time.RFC3339Nano),
	}
	if status == injectionStateOK {
		// Carry forward the full injected set; refs seen earlier but absent from this
		// prompt's pack stay recorded so they remain suppressed if they reappear.
		for ref, rec := range prior.Injected {
			next.Injected[ref] = rec
		}
	}

	suppressed := make(map[string]bool)
	if suppressing {
		for _, ref := range pack.ActiveMemory {
			kind := injectionRefKind(ref)
			if kind == "" {
				continue
			}
			rec, seen := prior.Injected[ref]
			if !seen {
				continue // new ref → inject
			}
			if kind == "position" && contested[ref] && !rec.Contested {
				continue // contested flip since injection → re-open
			}
			suppressed[ref] = true
		}
	}

	// Record every memory/position ref in play. Refs actually (re)injected this prompt
	// capture their current contested flag; suppressed refs keep their recorded flag so
	// a genuine later flip still re-opens them.
	for _, ref := range pack.ActiveMemory {
		kind := injectionRefKind(ref)
		if kind == "" {
			continue
		}
		rec := next.Injected[ref]
		rec.Kind = kind
		if !suppressed[ref] {
			rec.Contested = contested[ref]
		}
		next.Injected[ref] = rec
	}

	// Best-effort persist. A write failure is swallowed: this prompt already fully
	// injected (nothing suppressed when status != OK), so the model is never starved.
	writeInjectionSessionState(homePath, sessionID, next)

	if len(suppressed) == 0 {
		return pack // byte-for-byte identical to the no-delta path
	}
	return suppressPackRefs(pack, suppressed)
}

// suppressPackRefs drops the suppressed refs from the injected surfaces (ActiveMemory,
// Included, ActivePositions) and appends a matching Excluded entry for each, in the
// deterministic order the refs appeared in ActiveMemory.
func suppressPackRefs(pack envelope.ContextPack, suppressed map[string]bool) envelope.ContextPack {
	origActiveMemory := pack.ActiveMemory

	activeMemory := make([]string, 0, len(origActiveMemory))
	for _, ref := range origActiveMemory {
		if !suppressed[ref] {
			activeMemory = append(activeMemory, ref)
		}
	}
	pack.ActiveMemory = activeMemory

	included := make([]envelope.IncludedRef, 0, len(pack.Included))
	for _, ref := range pack.Included {
		if !suppressed[ref.Ref] {
			included = append(included, ref)
		}
	}
	pack.Included = included

	positions := make([]envelope.ActivePosition, 0, len(pack.ActivePositions))
	for _, p := range pack.ActivePositions {
		if !suppressed[p.Ref] {
			positions = append(positions, p)
		}
	}
	pack.ActivePositions = positions

	for _, ref := range origActiveMemory {
		if suppressed[ref] {
			pack.Excluded = append(pack.Excluded, envelope.ExcludedRef{Ref: ref, Reason: injectionSuppressReason})
		}
	}
	return pack
}

// injectionClassificationKey is the coarse task-class / topic-seed signature the
// re-opening trigger compares against. It is derived purely from the fresh pack plus
// the input: the explicit topic seed, the repo kind (the closest task-class proxy
// available to the hook without re-running contract's probe classifier), and the set
// of topics the selected positions carry. When the operator's prompt pivots to a
// different topic mix or narrows to a new topic seed, this key changes and every
// suppressed ref re-opens for that prompt.
func injectionClassificationKey(pack envelope.ContextPack, in contract.Input) string {
	topicSet := make(map[string]bool)
	for _, p := range pack.ActivePositions {
		if t := strings.TrimSpace(p.Topic); t != "" {
			topicSet[t] = true
		}
	}
	topics := make([]string, 0, len(topicSet))
	for t := range topicSet {
		topics = append(topics, t)
	}
	sort.Strings(topics)
	body := struct {
		TopicSeed string   `json:"topic_seed"`
		RepoKind  string   `json:"repo_kind"`
		Topics    []string `json:"topics"`
	}{
		TopicSeed: strings.TrimSpace(in.Topic),
		RepoKind:  strings.TrimSpace(in.RepoKind),
		Topics:    topics,
	}
	data, _ := json.Marshal(body)
	return "sha256:" + hashStr(string(data))
}

// injectionRefKind classifies a ref as a tracked memory surface, or "" for refs the
// delta must leave alone (hooks, decisions, capability offers, etc.).
func injectionRefKind(ref string) string {
	switch {
	case strings.HasPrefix(ref, "claim://"):
		return "claim"
	case strings.HasPrefix(ref, "position://"):
		return "position"
	default:
		return ""
	}
}

func injectionSessionStatePath(homePath, sessionID string) string {
	return filepath.Join(homePath, filepath.FromSlash(injectionSessionDir), hashStr(strings.TrimSpace(sessionID))+".json")
}

// readInjectionSessionState loads the prior state. Any read/parse fault, a foreign
// schema, a session-key mismatch, or a missing injected map is reported as corrupt so
// the caller fails open (and rewrites a fresh record to self-heal the session).
func readInjectionSessionState(homePath, sessionID string) (injectionSessionState, injectionStateStatus) {
	data, err := os.ReadFile(injectionSessionStatePath(homePath, sessionID))
	if err != nil {
		return injectionSessionState{}, injectionStateAbsent
	}
	var st injectionSessionState
	if err := json.Unmarshal(data, &st); err != nil {
		return injectionSessionState{}, injectionStateCorrupt
	}
	if st.Schema != injectionSessionSchema || st.SessionKey != hashStr(strings.TrimSpace(sessionID)) || st.Injected == nil {
		return injectionSessionState{}, injectionStateCorrupt
	}
	return st, injectionStateOK
}

// writeInjectionSessionState persists the record atomically (temp + rename). Every
// failure is swallowed — the caller has already decided injection for this prompt and
// must not fault on a bookkeeping write.
func writeInjectionSessionState(homePath, sessionID string, st injectionSessionState) {
	dir := filepath.Join(homePath, filepath.FromSlash(injectionSessionDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	path := injectionSessionStatePath(homePath, sessionID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
