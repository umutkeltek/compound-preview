package work

import (
	"encoding/json"
	"strings"
	"testing"
)

// xcodeArgumentsTree builds a synthetic xcresult test tree with optional
// Arguments children. Mutations are applied to the returned fixture so each
// case carries its own clearly-labeled falsifier shape. No real product names
// or paths; every identity in this file is synthetic.
func xcodeArgumentsTree(t *testing.T) (map[string]any, map[string]any, ProofResourceReservation) {
	t.Helper()
	report, evidence, reservation := consumerOwnedReportFixture(t, "xcresult")
	if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
		t.Fatalf("baseline fixture invalid: %v", err)
	}
	return report, evidence, reservation
}

func xcodeArgumentsReplaceTree(t *testing.T, evidence map[string]any, tree map[string]any) {
	t.Helper()
	body, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	evidence["tests"] = body
}

func xcodeArgumentsDecode(t *testing.T, evidence map[string]any) map[string]any {
	t.Helper()
	var tree map[string]any
	if err := json.Unmarshal(evidence["tests"].([]byte), &tree); err != nil {
		t.Fatal(err)
	}
	return tree
}

// xcodeArgumentsPassedCase returns a synthetic Passed Test Case parent whose
// children are the supplied argument entries. Mutations always produce a
// single isolated parent so per-parent uniqueness is observable.
func xcodeArgumentsPassedCase(args []map[string]any) map[string]any {
	children := make([]any, len(args))
	for i, arg := range args {
		children[i] = arg
	}
	return map[string]any{
		"nodeType":       "Test Case",
		"nodeIdentifier": "ParentTests/sample()",
		"result":         "Passed",
		"children":       children,
	}
}

func xcodeArgumentsNativeArg(name string) map[string]any {
	return map[string]any{
		"nodeType": "Arguments",
		"name":     name,
		"result":   "Passed",
	}
}

// replacePassedParent swaps the existing required Passed Test Case in a clean
// baseline tree with the supplied replacement. It only mutates the tree; the
// caller is responsible for re-marshaling into evidence["tests"].
func replacePassedParent(t *testing.T, tree map[string]any, replacement map[string]any) {
	t.Helper()
	nodes := tree["testNodes"].([]any)
	suite := nodes[0].(map[string]any)["children"].([]any)
	for i, node := range suite {
		if tc, ok := node.(map[string]any); ok && tc["nodeType"] == "Test Case" && tc["result"] == "Passed" {
			suite[i] = replacement
			return
		}
	}
	t.Fatal("baseline tree missing required Passed Test Case")
}

// TestCanonicalConsumerXcodeArgumentsAcceptsGenuineParameterExpansion locks in
// the synthetic shape that mirrors a real native Swift Testing parameter
// expansion: the required Passed Test Case keeps its identity and summary
// count, and gains flat Arguments children. Without the fix, this fails with
// "unsupported or nested Xcode test diagnostic".
func TestCanonicalConsumerXcodeArgumentsAcceptsGenuineParameterExpansion(t *testing.T) {
	report, evidence, reservation := xcodeArgumentsTree(t)
	tree := xcodeArgumentsDecode(t, evidence)
	passing := xcodeArgumentsPassedCase([]map[string]any{
		xcodeArgumentsNativeArg("0, zero"),
		xcodeArgumentsNativeArg("1, one"),
		xcodeArgumentsNativeArg("2, two"),
		xcodeArgumentsNativeArg("3, three"),
	})
	// Preserve the required Test Case identity so the required_tests check
	// still resolves against the same identifier.
	passing["nodeIdentifier"] = "ActivationTests/testFirstAppearance()"
	// Replace the existing required Passed Test Case with one that carries
	// flat Arguments children. The unchanged skipped Test Case and the
	// unchanged summary keep counts honest.
	nodes := tree["testNodes"].([]any)
	suite := nodes[0].(map[string]any)["children"].([]any)
	for i, node := range suite {
		if tc, ok := node.(map[string]any); ok && tc["nodeType"] == "Test Case" && tc["result"] == "Passed" {
			suite[i] = passing
			break
		}
	}
	tree["testNodes"] = []any{map[string]any{"nodeType": "Test Suite", "children": suite}}
	xcodeArgumentsReplaceTree(t, evidence, tree)
	if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
		t.Fatalf("passed native parameter arguments rejected: %v", err)
	}
}

// TestCanonicalConsumerXcodeArgumentsAllowsSharedNamesAcrossParents verifies
// that the same argument name under two distinct Passed Test Case parents is
// not rejected. Per-parent uniqueness is the rule; cross-parent collisions
// remain valid.
func TestCanonicalConsumerXcodeArgumentsAllowsSharedNamesAcrossParents(t *testing.T) {
	report, evidence, reservation := xcodeArgumentsTree(t)
	tree := xcodeArgumentsDecode(t, evidence)
	shared := []map[string]any{
		xcodeArgumentsNativeArg("0, alpha"),
		xcodeArgumentsNativeArg("1, beta"),
	}
	parentA := xcodeArgumentsPassedCase(shared)
	parentA["nodeIdentifier"] = "ActivationTests/testFirstAppearance()"
	parentB := xcodeArgumentsPassedCase(shared)
	parentB["nodeIdentifier"] = "SiblingTests/sharedArgCase()"
	// Add an extra Passed parent and rebalance the summary to 3 logical
	// tests (2 Passed + 1 Skipped) so the validator agrees.
	tree["testNodes"] = []any{map[string]any{"nodeType": "Test Suite", "children": []any{parentA, parentB,
		map[string]any{
			"nodeType":       "Test Case",
			"nodeIdentifier": "HardwareTests/testCamera()",
			"result":         "Skipped",
			"children":       []any{map[string]any{"nodeType": "Failure Message", "name": "Physical device required"}},
		}}}}
	xcodeArgumentsReplaceTree(t, evidence, tree)
	evidence["report"] = []byte(strings.Replace(string(evidence["report"].([]byte)), `"totalTestCount":2,"passedTests":1,"failedTests":0,"skippedTests":1`, `"totalTestCount":3,"passedTests":2,"failedTests":0,"skippedTests":1`, 1))
	if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
		t.Fatalf("shared argument names across parents rejected: %v", err)
	}
}

// TestCanonicalConsumerXcodeArgumentsPreservesLogicalSummary locks in that a
// Test Case with four flat Arguments children still counts as one logical
// Test Case against the summary. The summary stays at totalTestCount=2 with
// one Passed and one Skipped; a fourth argument child must not inflate the
// passed count.
func TestCanonicalConsumerXcodeArgumentsPreservesLogicalSummary(t *testing.T) {
	report, evidence, reservation := xcodeArgumentsTree(t)
	tree := xcodeArgumentsDecode(t, evidence)
	passing := xcodeArgumentsPassedCase([]map[string]any{
		xcodeArgumentsNativeArg("0, zero"),
		xcodeArgumentsNativeArg("1, one"),
		xcodeArgumentsNativeArg("2, two"),
		xcodeArgumentsNativeArg("3, three"),
	})
	// Keep the required identity.
	passing["nodeIdentifier"] = "ActivationTests/testFirstAppearance()"
	nodes := tree["testNodes"].([]any)
	suite := nodes[0].(map[string]any)["children"].([]any)
	for i, node := range suite {
		if tc, ok := node.(map[string]any); ok && tc["nodeType"] == "Test Case" && tc["result"] == "Passed" {
			suite[i] = passing
			break
		}
	}
	tree["testNodes"] = []any{map[string]any{"nodeType": "Test Suite", "children": suite}}
	xcodeArgumentsReplaceTree(t, evidence, tree)
	if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
		t.Fatalf("argument-expansion summary mismatch: %v", err)
	}
}

// TestCanonicalConsumerXcodeArgumentsPreservesSkippedFailureMessage locks in
// that an allowed-skipped Test Case with a Failure Message child is still
// accepted, even though the new code path inspects Test Case children.
func TestCanonicalConsumerXcodeArgumentsPreservesSkippedFailureMessage(t *testing.T) {
	report, _, reservation := xcodeArgumentsTree(t)
	if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
		t.Fatalf("allowed-skipped Test Case with Failure Message rejected: %v", err)
	}
}

// TestCanonicalConsumerXcodeArgumentsRejectsMalformed is a table-driven
// falsifier that drives every malformed-shape rule from the implementation
// packet through the production validator. Each entry must fail closed.
func TestCanonicalConsumerXcodeArgumentsRejectsMalformed(t *testing.T) {
	// Each falsifier builds its own clean tree from the baseline fixture so
	// failures are isolated to the mutation, never inherited state.
	cases := map[string]func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation){
		"orphan argument at root": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			tree["testNodes"] = append(tree["testNodes"].([]any), map[string]any{
				"nodeType": "Arguments", "name": "orphan", "result": "Passed",
			})
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("orphan argument accepted as native proof")
			}
		},
		"orphan argument under group": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			nodes := tree["testNodes"].([]any)
			suite := nodes[0].(map[string]any)
			suite["children"] = append(suite["children"].([]any), map[string]any{
				"nodeType": "Arguments", "name": "orphan-under-group", "result": "Passed",
			})
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("orphan argument under group accepted as proof")
			}
		},
		"nested argument children": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			nested := xcodeArgumentsPassedCase([]map[string]any{
				map[string]any{
					"nodeType": "Arguments", "name": "0, zero", "result": "Passed",
					"children": []any{map[string]any{"nodeType": "Arguments", "name": "deeper", "result": "Passed"}},
				},
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, nested)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("nested argument child accepted")
			}
		},
		"mixed Failure Message and Arguments": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			mixed := xcodeArgumentsPassedCase([]map[string]any{
				xcodeArgumentsNativeArg("0, zero"),
				map[string]any{"nodeType": "Failure Message", "name": "diagnostic"},
			})
			replacePassedParent(t, tree, mixed)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("mixed Failure Message + Arguments accepted")
			}
		},
		"empty argument name": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			empty := xcodeArgumentsPassedCase([]map[string]any{
				map[string]any{"nodeType": "Arguments", "name": "", "result": "Passed"},
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, empty)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("empty argument name accepted")
			}
		},
		"whitespace argument name": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			ws := xcodeArgumentsPassedCase([]map[string]any{
				map[string]any{"nodeType": "Arguments", "name": "   ", "result": "Passed"},
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, ws)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("whitespace argument name accepted")
			}
		},
		"duplicate same-parent argument name": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			dup := xcodeArgumentsPassedCase([]map[string]any{
				xcodeArgumentsNativeArg("0, alpha"),
				xcodeArgumentsNativeArg("0, alpha"),
			})
			replacePassedParent(t, tree, dup)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("duplicate same-parent argument accepted")
			}
		},
		"argument nodeIdentifier set": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			withID := xcodeArgumentsPassedCase([]map[string]any{
				map[string]any{"nodeType": "Arguments", "name": "0, zero", "nodeIdentifier": "ParentTests/sample()", "result": "Passed"},
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, withID)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("argument with nodeIdentifier accepted")
			}
		},
		"argument Failed result": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			failed := xcodeArgumentsPassedCase([]map[string]any{
				map[string]any{"nodeType": "Arguments", "name": "0, zero", "result": "Failed"},
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, failed)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("Failed argument accepted")
			}
		},
		"argument Skipped result": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			skipped := xcodeArgumentsPassedCase([]map[string]any{
				map[string]any{"nodeType": "Arguments", "name": "0, zero", "result": "Skipped"},
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, skipped)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("Skipped argument accepted")
			}
		},
		"argument missing result": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			missing := xcodeArgumentsPassedCase([]map[string]any{
				map[string]any{"nodeType": "Arguments", "name": "0, zero"},
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, missing)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("missing argument result accepted")
			}
		},
		"argument unknown result": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			unknown := xcodeArgumentsPassedCase([]map[string]any{
				map[string]any{"nodeType": "Arguments", "name": "0, zero", "result": "Pending"},
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, unknown)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("unknown-result argument accepted")
			}
		},
		"argument under Skipped Test Case": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			nodes := tree["testNodes"].([]any)
			suite := nodes[0].(map[string]any)["children"].([]any)
			skippedWithArgs := map[string]any{
				"nodeType":       "Test Case",
				"nodeIdentifier": "HardwareTests/testCamera()",
				"result":         "Skipped",
				"children": []any{
					map[string]any{"nodeType": "Failure Message", "name": "Physical device required"},
					xcodeArgumentsNativeArg("0, zero"),
				},
			}
			for i, node := range suite {
				if tc, ok := node.(map[string]any); ok && tc["nodeType"] == "Test Case" && tc["result"] == "Skipped" {
					suite[i] = skippedWithArgs
					break
				}
			}
			tree["testNodes"] = []any{map[string]any{"nodeType": "Test Suite", "children": suite}}
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("argument under Skipped Test Case accepted")
			}
		},
		"unknown diagnostic child type": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			unknown := xcodeArgumentsPassedCase([]map[string]any{
				map[string]any{"nodeType": "Mystery", "name": "0, zero", "result": "Passed"},
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, unknown)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("unknown diagnostic child type accepted")
			}
		},
		"summary passedTests mismatch": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			passing := xcodeArgumentsPassedCase([]map[string]any{
				xcodeArgumentsNativeArg("0, zero"),
				xcodeArgumentsNativeArg("1, one"),
			})
			replacePassedParent(t, tree, passing)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			evidence["report"] = []byte(strings.Replace(string(evidence["report"].([]byte)), `"passedTests":1`, `"passedTests":2`, 1))
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("summary passedTests mismatch accepted")
			}
		},
		"required test missing under argument parent": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			passing := xcodeArgumentsPassedCase([]map[string]any{
				xcodeArgumentsNativeArg("0, zero"),
				xcodeArgumentsNativeArg("1, one"),
			})
			// Rename the required Test Case so it no longer satisfies the
			// required_tests check; arguments must not stand in for it.
			passing["nodeIdentifier"] = "OtherTests/argumentParent()"
			replacePassedParent(t, tree, passing)
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("required test silently dropped under argument parent")
			}
		},
		"disallowed skip under argument parent path": func(t *testing.T, report map[string]any, evidence map[string]any, reservation ProofResourceReservation) {
			tree := xcodeArgumentsDecode(t, evidence)
			nodes := tree["testNodes"].([]any)
			suite := nodes[0].(map[string]any)["children"].([]any)
			for i, node := range suite {
				if tc, ok := node.(map[string]any); ok && tc["nodeType"] == "Test Case" && tc["result"] == "Skipped" {
					// Change the skipped Test Case identity so it is not in
					// allowed_skips; the Skipped branch must still enforce.
					disallowed := map[string]any{
						"nodeType":       "Test Case",
						"nodeIdentifier": "OtherTests/testNotAllowedSkip()",
						"result":         "Skipped",
						"children":       []any{map[string]any{"nodeType": "Failure Message", "name": "diagnostic"}},
					}
					suite[i] = disallowed
					break
				}
			}
			tree["testNodes"] = []any{map[string]any{"nodeType": "Test Suite", "children": suite}}
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("disallowed skip accepted")
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			report, evidence, reservation := xcodeArgumentsTree(t)
			mutate(t, report, evidence, reservation)
		})
	}
}
