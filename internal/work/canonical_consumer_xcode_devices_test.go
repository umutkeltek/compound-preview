package work

import (
	"strings"
	"testing"
)

func xcodeDeviceNode(id string, arguments ...any) map[string]any {
	return map[string]any{
		"nodeType": "Device", "nodeIdentifier": id, "name": "Synthetic phone",
		"result": "Passed", "children": arguments,
	}
}

func xcodeDeviceFixture(t *testing.T) (map[string]any, map[string]any, ProofResourceReservation, map[string]any, map[string]any) {
	t.Helper()
	report, evidence, reservation := xcodeArgumentsTree(t)
	tree := xcodeArgumentsDecode(t, evidence)
	tree["devices"] = []any{map[string]any{"deviceId": "device-a", "modelName": "Synthetic phone"}}
	device := xcodeDeviceNode("device-a", xcodeArgumentsNativeArg("1, one"), xcodeArgumentsNativeArg("2, two"))
	parent := xcodeArgumentsPassedCase([]map[string]any{device})
	parent["nodeIdentifier"] = "ActivationTests/testFirstAppearance()"
	replacePassedParent(t, tree, parent)
	return report, evidence, reservation, tree, parent
}

func TestCanonicalConsumerXcodeDevicesAcceptsNativeLayer(t *testing.T) {
	for _, variant := range []string{"simple", "parameterized", "multiple devices", "direct arguments sibling", "shared device across parents"} {
		t.Run(variant, func(t *testing.T) {
			report, evidence, reservation, tree, parent := xcodeDeviceFixture(t)
			switch variant {
			case "simple":
				parent["children"] = []any{xcodeDeviceNode("device-a")}
			case "multiple devices":
				tree["devices"] = append(tree["devices"].([]any), map[string]any{"deviceId": "device-b"})
				parent["children"] = append(parent["children"].([]any), xcodeDeviceNode("device-b", xcodeArgumentsNativeArg("1, one")))
			case "direct arguments sibling":
				parent["children"] = append(parent["children"].([]any), xcodeArgumentsNativeArg("1, one"))
			case "shared device across parents":
				sibling := xcodeArgumentsPassedCase([]map[string]any{xcodeDeviceNode("device-a", xcodeArgumentsNativeArg("1, one"))})
				sibling["nodeIdentifier"] = "SiblingTests/parameter()"
				tree["testNodes"] = append(tree["testNodes"].([]any), sibling)
				evidence["report"] = []byte(strings.Replace(string(evidence["report"].([]byte)), `"totalTestCount":2,"passedTests":1`, `"totalTestCount":3,"passedTests":2`, 1))
			}
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
				t.Fatalf("native Device diagnostics rejected: %v", err)
			}
		})
	}
}

func TestCanonicalConsumerXcodeDevicesRejectsMalformed(t *testing.T) {
	type mutation func(tree, parent, device map[string]any)
	cases := map[string]mutation{
		"missing metadata":    func(tree, _, _ map[string]any) { delete(tree, "devices") },
		"empty metadata":      func(tree, _, _ map[string]any) { tree["devices"] = []any{} },
		"metadata missing ID": func(tree, _, _ map[string]any) { tree["devices"] = []any{map[string]any{}} },
		"metadata blank ID":   func(tree, _, _ map[string]any) { tree["devices"] = []any{map[string]any{"deviceId": " \t"}} },
		"duplicate metadata ID": func(tree, _, _ map[string]any) {
			tree["devices"] = append(tree["devices"].([]any), map[string]any{"deviceId": "device-a"})
		},
		"unknown device ID":     func(_, _, device map[string]any) { device["nodeIdentifier"] = "device-unknown" },
		"missing device ID":     func(_, _, device map[string]any) { delete(device, "nodeIdentifier") },
		"blank device ID":       func(_, _, device map[string]any) { device["nodeIdentifier"] = " \t" },
		"duplicate sibling ID":  func(_, parent, device map[string]any) { parent["children"] = []any{device, device} },
		"missing name":          func(_, _, device map[string]any) { delete(device, "name") },
		"blank name":            func(_, _, device map[string]any) { device["name"] = " \t" },
		"failed device":         func(_, _, device map[string]any) { device["result"] = "Failed" },
		"skipped device":        func(_, _, device map[string]any) { device["result"] = "Skipped" },
		"unknown device result": func(_, _, device map[string]any) { device["result"] = "Pending" },
		"missing device result": func(_, _, device map[string]any) { delete(device, "result") },
		"device testNodes":      func(_, _, device map[string]any) { device["testNodes"] = []any{xcodeArgumentsNativeArg("3, three")} },
		"nested device":         func(_, _, device map[string]any) { device["children"] = []any{xcodeDeviceNode("device-a")} },
		"nested Test Case":      func(_, _, device map[string]any) { device["children"] = []any{xcodeArgumentsPassedCase(nil)} },
		"nested Failure Message": func(_, _, device map[string]any) {
			device["children"] = []any{map[string]any{"nodeType": "Failure Message", "name": "reason"}}
		},
		"nested unknown node": func(_, _, device map[string]any) {
			device["children"] = []any{map[string]any{"nodeType": "Unknown", "result": "Passed"}}
		},
		"orphan root": func(tree, _, device map[string]any) { tree["testNodes"] = append(tree["testNodes"].([]any), device) },
		"orphan suite": func(tree, _, device map[string]any) {
			suite := tree["testNodes"].([]any)[0].(map[string]any)
			suite["children"] = append(suite["children"].([]any), device)
		},
		"device under allowed skip": func(tree, _, device map[string]any) {
			suite := tree["testNodes"].([]any)[0].(map[string]any)
			for _, node := range suite["children"].([]any) {
				if tc := node.(map[string]any); tc["result"] == "Skipped" {
					tc["children"] = []any{device}
				}
			}
		},
		"duplicate logical identity": func(tree, parent, _ map[string]any) { tree["testNodes"] = append(tree["testNodes"].([]any), parent) },
		"required identity absent":   func(_, parent, _ map[string]any) { parent["nodeIdentifier"] = "OtherTests/parameter()" },
		"blank argument name":        func(_, _, device map[string]any) { device["children"] = []any{xcodeArgumentsNativeArg(" \t")} },
		"failed argument": func(_, _, device map[string]any) {
			arg := xcodeArgumentsNativeArg("1, one")
			arg["result"] = "Failed"
			device["children"] = []any{arg}
		},
		"skipped argument": func(_, _, device map[string]any) {
			arg := xcodeArgumentsNativeArg("1, one")
			arg["result"] = "Skipped"
			device["children"] = []any{arg}
		},
		"unknown argument result": func(_, _, device map[string]any) {
			arg := xcodeArgumentsNativeArg("1, one")
			arg["result"] = "Pending"
			device["children"] = []any{arg}
		},
		"missing argument result": func(_, _, device map[string]any) {
			arg := xcodeArgumentsNativeArg("1, one")
			delete(arg, "result")
			device["children"] = []any{arg}
		},
		"argument identity": func(_, _, device map[string]any) {
			arg := xcodeArgumentsNativeArg("1, one")
			arg["nodeIdentifier"] = "ArgumentTests/identity()"
			device["children"] = []any{arg}
		},
		"argument children": func(_, _, device map[string]any) {
			arg := xcodeArgumentsNativeArg("1, one")
			arg["children"] = []any{xcodeArgumentsNativeArg("2, two")}
			device["children"] = []any{arg}
		},
		"argument testNodes": func(_, _, device map[string]any) {
			arg := xcodeArgumentsNativeArg("1, one")
			arg["testNodes"] = []any{xcodeArgumentsNativeArg("2, two")}
			device["children"] = []any{arg}
		},
		"duplicate argument name": func(_, _, device map[string]any) {
			device["children"] = []any{xcodeArgumentsNativeArg("1, one"), xcodeArgumentsNativeArg("1, one")}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			report, evidence, reservation, tree, parent := xcodeDeviceFixture(t)
			mutate(tree, parent, parent["children"].([]any)[0].(map[string]any))
			xcodeArgumentsReplaceTree(t, evidence, tree)
			if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
				t.Fatal("malformed Device evidence accepted")
			} else if name == "required identity absent" && !strings.Contains(err.Error(), "required Xcode test ActivationTests/testFirstAppearance() did not pass") {
				t.Fatalf("required-test fence was not reached: %v", err)
			}
		})
	}
}

func TestCanonicalConsumerXcodeDevicesRejectsExpandedSummaryCounts(t *testing.T) {
	report, evidence, reservation, tree, _ := xcodeDeviceFixture(t)
	xcodeArgumentsReplaceTree(t, evidence, tree)
	evidence["report"] = []byte(strings.Replace(string(evidence["report"].([]byte)), `"totalTestCount":2,"passedTests":1`, `"totalTestCount":3,"passedTests":2`, 1))
	if err := validateConsumerOwnedFixture(t, report, reservation); err == nil || !strings.Contains(err.Error(), "summary and test tree disagree") {
		t.Fatalf("expanded diagnostics must not count as logical tests: %v", err)
	}
}
