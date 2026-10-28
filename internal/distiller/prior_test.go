package distiller

import (
	"testing"

	"compound/internal/capgraph"
)

func TestLoadPriorStateFromHomeCoversTypedSignals(t *testing.T) {
	home, store := setup(t)
	raw := `{"trace_ref":"prior_trace","candidates":[
		{"kind":"memory","entity":"project:api","predicate":"launch_decision","object":"use go","confidence":0.9,"claim_type":"decision"},
{"kind":"memory","entity":"project:api","predicate":"runtime_status","object":"active","confidence":0.9,"claim_type":"status"},
{"kind":"skill","subject":"frontend performance lens","statement":"review React rendering paths","confidence":0.9},
{"kind":"decision","subject":"decision:route-free-model-judging","statement":"use paid frontier lanes only after cheap lanes produce evidence","confidence":0.9},
{"kind":"failure_pattern","subject":"distiller","statement":"misses relative dates","confidence":0.9},
{"kind":"taste","subject":"ui","statement":"calm utility over dashboard clutter","confidence":0.9},
{"kind":"product_hypothesis","subject":"product:focus-notes","statement":"screenshot may improve conversion","confidence":0.9},
{"kind":"route_observation","subject":"route:auto-extract","statement":"cheap lane worked for candidate-only extraction","confidence":0.9},
{"kind":"capability_observation","subject":"skill:paywall-cro","statement":"paywall","detail":"missed_when","confidence":0.9},
{"kind":"eval_case","subject":"distill.skill","statement":"missed an obvious skill","confidence":1.0},
{"kind":"method_upgrade","subject":"distill.memory","statement":"resolve relative dates absolute at extraction","confidence":0.9},
{"kind":"policy_patch","subject":"distill.policy","statement":"require source refs for all capability feedback","confidence":0.9}
]}`
	run(t, home, store, raw)
	run(t, home, store, `{"trace_ref":"prior_duplicate","candidates":[
		{"kind":"memory","entity":"project:api","predicate":"launch_decision","object":"use go","confidence":0.9}
	]}`)

	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatalf("capgraph open: %v", err)
	}
	if err := graph.Add(capgraph.Node{ID: "skill:paywall-cro", Kind: "skill", Name: "Paywall CRO", Triggers: []string{"paywall"}, Weight: 1}); err != nil {
		t.Fatalf("capgraph add: %v", err)
	}

	prior, err := LoadPriorStateFromHome(home, store, DefaultPolicy())
	if err != nil {
		t.Fatalf("load prior: %v", err)
	}
	cov := prior.Coverage()
	if prior.PolicyVersion != "default.v1" {
		t.Fatalf("policy version = %q, want default.v1", prior.PolicyVersion)
	}
	if cov.CurrentClaims != 2 || cov.Decisions != 1 || cov.Statuses != 1 {
		t.Fatalf("claim coverage current/decisions/statuses = %d/%d/%d, want 2/1/1", cov.CurrentClaims, cov.Decisions, cov.Statuses)
	}
	if cov.FailurePatterns != 1 || cov.TasteObservations != 1 || cov.ProductHypotheses != 1 {
		t.Fatalf("candidate coverage failure/taste/product = %d/%d/%d, want 1/1/1", cov.FailurePatterns, cov.TasteObservations, cov.ProductHypotheses)
	}
	if cov.SkillCandidates != 1 || cov.DecisionCandidates != 1 || cov.RouteObservations != 1 {
		t.Fatalf("candidate coverage skill/decision/route = %d/%d/%d, want 1/1/1", cov.SkillCandidates, cov.DecisionCandidates, cov.RouteObservations)
	}
	if cov.CapabilityObs != 1 || cov.EvalCases != 1 || cov.MethodUpgrades != 1 || cov.PolicyPatches != 1 {
		t.Fatalf("candidate coverage capability/eval/method/policy = %d/%d/%d/%d, want 1/1/1/1", cov.CapabilityObs, cov.EvalCases, cov.MethodUpgrades, cov.PolicyPatches)
	}
	if cov.CapabilityNodes != 1 {
		t.Fatalf("capability nodes = %d, want 1", cov.CapabilityNodes)
	}
	if cov.NoPromoteDecisions != 1 {
		t.Fatalf("no-promote decisions = %d, want 1", cov.NoPromoteDecisions)
	}
	if prior.CandidatesByKind[KindCapabilityObs][0].Detail != "missed_when" {
		t.Fatalf("capability observation detail not preserved: %+v", prior.CandidatesByKind[KindCapabilityObs][0])
	}
	if prior.CandidatesByKind[KindDecision][0].Subject != "decision:route-free-model-judging" {
		t.Fatalf("decision candidate not preserved: %+v", prior.CandidatesByKind[KindDecision][0])
	}
}

func TestLoadPriorStateWithoutHomeStaysMemoryOnly(t *testing.T) {
	home, store := setup(t)
	run(t, home, store, `{"trace_ref":"prior_memory_only","candidates":[
		{"kind":"memory","entity":"project:api","predicate":"architecture","object":"compound spine","confidence":0.9,"claim_type":"architecture"},
		{"kind":"method_upgrade","subject":"distill.memory","statement":"better method","confidence":0.9}
	]}`)

	prior, err := LoadPriorState(store)
	if err != nil {
		t.Fatalf("load prior: %v", err)
	}
	cov := prior.Coverage()
	if cov.CurrentClaims != 1 || cov.Architectures != 1 {
		t.Fatalf("memory-only coverage current/architecture = %d/%d, want 1/1", cov.CurrentClaims, cov.Architectures)
	}
	if cov.MethodUpgrades != 0 || cov.CapabilityNodes != 0 {
		t.Fatalf("without home, non-memory coverage should be zero; got method=%d capability_nodes=%d", cov.MethodUpgrades, cov.CapabilityNodes)
	}
}
