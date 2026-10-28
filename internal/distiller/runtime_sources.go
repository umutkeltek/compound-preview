package distiller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

// LoadRuntimeSources builds distiller source material from a durable work chain
// already present in COMPOUND_HOME. It is read-only: work remains owned by the
// work store, and distiller only turns the chain into extraction input.
func LoadRuntimeSources(homePath, workID string) (LLMExtractionSources, error) {
	var src LLMExtractionSources
	homePath = strings.TrimSpace(homePath)
	workID = strings.TrimSpace(workID)
	if homePath == "" {
		return src, fmt.Errorf("home path is required")
	}
	if workID == "" {
		return src, fmt.Errorf("work_id is required")
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return src, err
	}

	workPath := filepath.Join(homePath, "work", "work", workID+".json")
	workEnv, err := readRuntimeEnvelope(workPath)
	if err != nil {
		return src, fmt.Errorf("load work %s: %w", workID, err)
	}
	w, ok := workEnv.(*envelope.WorkEnvelope)
	if !ok {
		return src, fmt.Errorf("%s is %s, want work", workPath, workEnv.Kind())
	}
	if w.ID != workID {
		return src, fmt.Errorf("work file id %q does not match requested %q", w.ID, workID)
	}
	src.Work = w

	if ctxID := strings.TrimSpace(w.IDs.ContextPackID); ctxID != "" {
		ctxPath := filepath.Join(homePath, "context", "packs", ctxID+".json")
		ctxEnv, err := readRuntimeEnvelope(ctxPath)
		if err != nil {
			return src, fmt.Errorf("load context pack %s: %w", ctxID, err)
		}
		ctx, ok := ctxEnv.(*envelope.ContextPack)
		if !ok {
			return src, fmt.Errorf("%s is %s, want context_pack", ctxPath, ctxEnv.Kind())
		}
		src.ContextPack = ctx
	}

	seenExtra := map[string]bool{w.ID: true}
	if src.ContextPack != nil {
		seenExtra[src.ContextPack.ID] = true
	}

	hookEvents, err := loadHookEvents(events, w.ID, w.IDs.ContextPackID)
	if err != nil {
		return src, err
	}
	for i := range hookEvents {
		if markEnvelopeSeen(&hookEvents[i], seenExtra) {
			src.Extra = append(src.Extra, &hookEvents[i])
		}
	}
	hookSideEffects, err := loadHookSideEffectEnvelopes(homePath, events, hookEvents, seenExtra)
	if err != nil {
		return src, err
	}
	src.Extra = append(src.Extra, hookSideEffects...)

	if terminalWork(w.State) && strings.TrimSpace(w.IDs.TransitionID) == "" {
		return src, fmt.Errorf("closed work %s missing transition_id", workID)
	}
	if trID := strings.TrimSpace(w.IDs.TransitionID); trID != "" {
		trEnv, err := readRuntimeEventEnvelope(events, trID)
		if err != nil {
			return src, fmt.Errorf("load transition %s: %w", trID, err)
		}
		tr, ok := trEnv.(*envelope.Transition)
		if !ok {
			return src, fmt.Errorf("event %s is %s, want transition", trID, trEnv.Kind())
		}
		if markEnvelopeSeen(tr, seenExtra) {
			src.Extra = append(src.Extra, tr)
		}

		recID := transitionReceiptID(*tr)
		if recID == "" {
			return src, fmt.Errorf("transition %s missing receipt ref in after_refs", trID)
		}
		recEnv, err := readRuntimeEventEnvelope(events, recID)
		if err != nil {
			return src, fmt.Errorf("load receipt %s: %w", recID, err)
		}
		rec, ok := recEnv.(*envelope.Receipt)
		if !ok {
			return src, fmt.Errorf("event %s is %s, want receipt", recID, recEnv.Kind())
		}
		src.Receipt = rec
	}

	return src, nil
}

// LoadTraceEnvelopes loads already-imported TraceEnvelope runtime events from
// COMPOUND_HOME/events. It is read-only: event streams remain evidence sources,
// and distiller callers may only turn them into extraction input.
func LoadTraceEnvelopes(homePath string, traceIDs []string) ([]envelope.Envelope, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return nil, fmt.Errorf("home path is required")
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return nil, err
	}
	var out []envelope.Envelope
	seen := map[string]bool{}
	for _, rawID := range traceIDs {
		value := strings.TrimSpace(rawID)
		if value == "" {
			continue
		}
		traceRef, err := eventstore.ParseReference(value)
		if err != nil {
			return nil, fmt.Errorf("load trace %s: %w", value, err)
		}
		if seen[traceRef.String()] {
			continue
		}
		seen[traceRef.String()] = true
		env, err := readRuntimeEventEnvelope(events, traceRef.String())
		if err != nil {
			return nil, fmt.Errorf("load trace %s: %w", traceRef.ID(), err)
		}
		if env.Kind() != "trace_envelope" {
			return nil, fmt.Errorf("event %s is %s, want trace_envelope", traceRef.ID(), env.Kind())
		}
		out = append(out, env)
	}
	return out, nil
}

func loadHookEvents(events *eventstore.Store, workID, contextPackID string) ([]envelope.HookEvent, error) {
	// Read the authoritative hook_ records through the Scan seam. The prior
	// hook-events.ndjson stream is frozen once migration completes; Scan is the
	// deduped store view (executorlens doctrine), and the deterministic
	// (created_at, id) sort below reproduces the previous order byte-for-byte.
	entries, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"hook_"}, RegularOnly: true})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read hook events: %w", err)
	}
	var out []envelope.HookEvent
	seen := map[string]bool{}
	for _, entry := range entries {
		rec, err := entry.Read()
		if err != nil {
			return nil, fmt.Errorf("read hook event %s: %w", entry.ID, err)
		}
		var ev envelope.HookEvent
		if err := json.Unmarshal(rec.Bytes, &ev); err != nil {
			return nil, fmt.Errorf("parse hook event %s: %w", entry.ID, err)
		}
		if err := ev.Validate(); err != nil {
			return nil, fmt.Errorf("hook event %s invalid: %w", entry.ID, err)
		}
		if seen[ev.ID] {
			continue
		}
		if ev.IDs.WorkID == workID || (contextPackID != "" && ev.IDs.ContextPackID == contextPackID) {
			seen[ev.ID] = true
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func loadHookSideEffectEnvelopes(homePath string, store *eventstore.Store, events []envelope.HookEvent, seen map[string]bool) ([]envelope.Envelope, error) {
	var out []envelope.Envelope
	for _, ev := range events {
		for _, ref := range ev.SideEffects {
			eventRef, isEvent, err := hookSideEffectEventReference(ref)
			if err != nil {
				return nil, fmt.Errorf("hook event %s side effect %q: %w", ev.ID, ref, err)
			}
			if isEvent {
				env, err := readRuntimeEventEnvelope(store, eventRef.String())
				if err != nil {
					return nil, fmt.Errorf("load hook event %s side effect %s: %w", ev.ID, ref, err)
				}
				if markEnvelopeSeen(env, seen) {
					out = append(out, env)
				}
				continue
			}
			path, ok, err := hookSideEffectJSONPath(homePath, ref)
			if err != nil {
				return nil, fmt.Errorf("hook event %s side effect %q: %w", ev.ID, ref, err)
			}
			if !ok {
				continue
			}
			env, err := readRuntimeEnvelope(path)
			if err != nil {
				return nil, fmt.Errorf("load hook event %s side effect %s: %w", ev.ID, ref, err)
			}
			if markEnvelopeSeen(env, seen) {
				out = append(out, env)
			}
		}
	}
	return out, nil
}

func hookSideEffectEventReference(raw string) (eventstore.Reference, bool, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return eventstore.Reference{}, false, nil
	}
	if strings.HasPrefix(ref, "compound://events/") || strings.HasPrefix(ref, "events/") {
		parsed, err := eventstore.ParseReference(ref)
		return parsed, true, err
	}
	// A .json suffix is the established shape for a generic home-relative hook
	// side effect. New event side effects are logical refs, so keep ambiguous
	// bare values on the generic path rather than appending another .json in the
	// loose event engine.
	if strings.HasSuffix(ref, ".json") {
		return eventstore.Reference{}, false, nil
	}
	for _, prefix := range []string{"act_", "pol_", "rec_", "tr_", "trace_", "hook_"} {
		if strings.HasPrefix(ref, prefix) {
			parsed, err := eventstore.ParseReference(ref)
			return parsed, true, err
		}
	}
	return eventstore.Reference{}, false, nil
}

func hookSideEffectJSONPath(homePath, ref string) (string, bool, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || !strings.HasSuffix(ref, ".json") {
		return "", false, nil
	}
	if filepath.IsAbs(ref) {
		return "", false, fmt.Errorf("absolute path is not allowed")
	}
	clean := filepath.Clean(ref)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("path escapes COMPOUND_HOME")
	}
	return filepath.Join(homePath, clean), true, nil
}

func markEnvelopeSeen(e envelope.Envelope, seen map[string]bool) bool {
	id := runtimeEnvelopeID(e)
	if id == "" {
		return true
	}
	if seen[id] {
		return false
	}
	seen[id] = true
	return true
}

func runtimeEnvelopeID(e envelope.Envelope) string {
	switch v := e.(type) {
	case *envelope.EffectiveContract:
		return v.ID
	case *envelope.ContextRequest:
		return v.ID
	case *envelope.ContextPack:
		return v.ID
	case *envelope.WorkEnvelope:
		return v.ID
	case *envelope.ActionIntent:
		return v.ID
	case *envelope.PolicyDecision:
		return v.ID
	case *envelope.HookEvent:
		return v.ID
	case *envelope.TraceEnvelope:
		return v.ID
	case *envelope.Receipt:
		return v.ID
	case *envelope.Transition:
		return v.ID
	default:
		return ""
	}
}

func readRuntimeEnvelope(path string) (envelope.Envelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodeRuntimeEnvelope(data)
}

func readRuntimeEventEnvelope(store *eventstore.Store, ref string) (envelope.Envelope, error) {
	record, err := store.Read(ref)
	if err != nil {
		return nil, err
	}
	return decodeRuntimeEnvelope(record.Bytes)
}

func decodeRuntimeEnvelope(data []byte) (envelope.Envelope, error) {
	var probe struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse probe: %w", err)
	}
	e := envelope.New(probe.SchemaVersion)
	if e == nil {
		return nil, fmt.Errorf("unknown schema_version %q", probe.SchemaVersion)
	}
	if err := json.Unmarshal(data, e); err != nil {
		return nil, fmt.Errorf("parse envelope: %w", err)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}

func transitionReceiptID(tr envelope.Transition) string {
	for _, raw := range tr.AfterRefs {
		ref, err := eventstore.ParseReference(strings.TrimSpace(raw))
		if err == nil && strings.HasPrefix(ref.ID(), "rec_") {
			return ref.ID()
		}
	}
	return ""
}

func terminalWork(state string) bool {
	switch state {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}
