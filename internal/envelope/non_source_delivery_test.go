package envelope

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Use the wire seam so the regression fails on a dropped declaration, not on a
// missing Go symbol. A binding is per action; it never replaces git_delivery.
func TestWorkEnvelopeNonSourceDeliveryRoundTrip(t *testing.T) {
	for _, kind := range []string{"private_runtime", "worktree_retirement", "installed_package"} {
		t.Run(kind, func(t *testing.T) {
			binding := nonSourceDeliveryWireFixture()
			binding["kind"] = kind
			work := decodeNonSourceDeliveryWork(t, []map[string]any{binding})
			if err := work.Validate(); err != nil {
				t.Fatalf("valid exact-action binding: %v", err)
			}
			body, err := json.Marshal(work)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			var bindings []map[string]any
			if err := json.Unmarshal(got["non_source_delivery"], &bindings); err != nil {
				t.Fatalf("exact-action delivery declaration was lost: %s (%v)", body, err)
			}
			if !reflect.DeepEqual(bindings, []map[string]any{binding}) {
				t.Fatalf("binding changed: got %+v, want %+v", bindings, binding)
			}
		})
	}
}

func TestWorkEnvelopeNonSourceDeliveryRejectsAmbiguousBindings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unknown kind", func(b map[string]any) { b["kind"] = "ignore_git" }},
		{"unowned action", func(b map[string]any) { b["action_intent_id"] = "act_other" }},
		{"unowned binding action", func(b map[string]any) { b["binding_action_intent_id"] = "act_other" }},
		{"self binding", func(b map[string]any) { b["binding_action_intent_id"] = "act_runtime" }},
		{"missing evidence", func(b map[string]any) { delete(b, "evidence_ref") }},
		{"prose evidence", func(b map[string]any) { b["evidence_ref"] = "live:looks-good" }},
		{"noncanonical digest", func(b map[string]any) { b["evidence_ref"] = "artifact:sha256:" + strings.Repeat("A", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := nonSourceDeliveryWireFixture()
			tc.mutate(binding)
			work := decodeNonSourceDeliveryWork(t, []map[string]any{binding})
			if err := work.Validate(); err == nil {
				t.Fatal("ambiguous non-source binding must be refused, not ignored")
			}
		})
	}
	work := decodeNonSourceDeliveryWork(t, []map[string]any{nonSourceDeliveryWireFixture(), nonSourceDeliveryWireFixture()})
	if err := work.Validate(); err == nil {
		t.Fatal("duplicate coverage for one action must be refused")
	}
}

func TestWorkEnvelopeBeforeNonSourceDeliveryKeepsExactBytes(t *testing.T) {
	const legacy = `{"schema_version":"compound.work.v0.1","id":"work_legacy_delivery","created_at":"2026-07-13T22:00:00Z","producer":{"kind":"cli","name":"compound","version":""},"ids":{},"scope":{},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"owner":"codex","state":"running","success_predicate":"legacy delivery remains readable without changing its hash","non_goals":[],"run_ids":[],"artifact_ids":[],"verification_ids":[],"memory_candidate_ids":[]}`
	var work WorkEnvelope
	if err := json.Unmarshal([]byte(legacy), &work); err != nil {
		t.Fatal(err)
	}
	if err := work.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != legacy {
		t.Fatalf("legacy Work hash preimage changed: got %s, want %s", body, legacy)
	}
}

func nonSourceDeliveryWireFixture() map[string]any {
	return map[string]any{
		"action_intent_id":         "act_runtime",
		"kind":                     "private_runtime",
		"evidence_ref":             "artifact:sha256:" + strings.Repeat("a", 64),
		"binding_action_intent_id": "act_bind",
	}
}

func decodeNonSourceDeliveryWork(t *testing.T, bindings []map[string]any) WorkEnvelope {
	t.Helper()
	work := WorkEnvelope{
		Header: hdr(SVWork, "work_action_delivery"), Owner: "codex", State: "running",
		SuccessPredicate: "close only exact proven non-source effects",
		ActionIntentIDs:  []string{"act_runtime", "act_bind"},
	}
	body, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	wire["non_source_delivery"] = bindings
	body, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var decoded WorkEnvelope
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}
