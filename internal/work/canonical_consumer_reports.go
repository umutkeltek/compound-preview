package work

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func validateConsumerGoReport(c CanonicalConsumer, body []byte) error {
	if !bytes.HasSuffix(body, []byte("\n")) {
		return fmt.Errorf("Go report has an incomplete final record")
	}
	type packageState struct {
		done  bool
		tests map[string]string
	}
	packages := map[string]*packageState{}
	observed := map[string]string{}
	allowed, _ := consumerUniqueStrings(c.AllowedSkips, false)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), CanonicalConsumerReportLimit)
	for scanner.Scan() {
		line := scanner.Bytes()
		if err := consumerUniqueJSON(line); err != nil {
			return fmt.Errorf("Go event: %w", err)
		}
		var event struct{ Action, Package, Test, Output string }
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("Go event: %w", err)
		}
		if event.Package == "" {
			return fmt.Errorf("Go event requires package identity")
		}
		p := packages[event.Package]
		if event.Test == "" {
			if event.Action == "start" {
				if p != nil {
					return fmt.Errorf("duplicate Go package start")
				}
				packages[event.Package] = &packageState{tests: map[string]string{}}
				continue
			}
			if p == nil || p.done {
				return fmt.Errorf("out-of-order Go package event")
			}
			switch event.Action {
			case "output":
				fields := strings.Fields(event.Output)
				if len(fields) == 3 && fields[0] == "ok" && fields[1] == event.Package && fields[2] == "(cached)" {
					return fmt.Errorf("cached Go output is not fresh native execution")
				}
			case "pass":
				for _, state := range p.tests {
					if state != "pass" && state != "skip" {
						return fmt.Errorf("Go package passed before tests finished")
					}
				}
				p.done = true
			default:
				return fmt.Errorf("Go package did not pass: %s", event.Action)
			}
			continue
		}
		if p == nil || p.done {
			return fmt.Errorf("Go test outside running package")
		}
		state := p.tests[event.Test]
		id := event.Package + "::" + event.Test
		switch event.Action {
		case "run":
			if state != "" {
				return fmt.Errorf("duplicate Go test %s", id)
			}
			p.tests[event.Test] = "run"
		case "pause":
			if state != "run" {
				return fmt.Errorf("out-of-order Go pause")
			}
			p.tests[event.Test] = "pause"
		case "cont":
			if state != "pause" {
				return fmt.Errorf("out-of-order Go continue")
			}
			p.tests[event.Test] = "run"
		case "output":
			if state != "run" && state != "pause" {
				return fmt.Errorf("Go output outside running test")
			}
		case "pass", "skip":
			if state != "run" {
				return fmt.Errorf("out-of-order Go test result for %s", id)
			}
			if event.Action == "skip" && !allowed[id] {
				return fmt.Errorf("unexpected skipped Go test %s", id)
			}
			p.tests[event.Test] = event.Action
			observed[id] = event.Action
		default:
			return fmt.Errorf("Go test did not pass: %s %s", id, event.Action)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(packages) == 0 || len(observed) == 0 {
		return fmt.Errorf("vacuous Go report")
	}
	for _, p := range packages {
		if !p.done {
			return fmt.Errorf("Go report missing package pass")
		}
	}
	for _, id := range c.RequiredTests {
		if observed[id] != "pass" {
			return fmt.Errorf("required Go test %s did not pass", id)
		}
	}
	return nil
}

type consumerXcodeNode struct {
	NodeType       string              `json:"nodeType"`
	NodeIdentifier string              `json:"nodeIdentifier"`
	Name           string              `json:"name"`
	Result         string              `json:"result"`
	Children       []consumerXcodeNode `json:"children"`
	TestNodes      []consumerXcodeNode `json:"testNodes"`
}

func validateConsumerXcodeReport(c CanonicalConsumer, summaryBody, testsBody []byte) error {
	if len(testsBody) == 0 {
		return fmt.Errorf("Xcode test tree required; summary alone is not proof")
	}
	for _, body := range [][]byte{summaryBody, testsBody} {
		if err := consumerUniqueJSON(body); err != nil {
			return fmt.Errorf("Xcode native report: %w", err)
		}
	}
	var summary struct {
		Result   string            `json:"result"`
		Total    *int              `json:"totalTestCount"`
		Passed   *int              `json:"passedTests"`
		Failed   *int              `json:"failedTests"`
		Skipped  *int              `json:"skippedTests"`
		Expected *int              `json:"expectedFailures"`
		Failures []json.RawMessage `json:"testFailures"`
	}
	if err := json.Unmarshal(summaryBody, &summary); err != nil {
		return err
	}
	if summary.Result != "Passed" || summary.Total == nil || summary.Passed == nil || summary.Failed == nil || summary.Skipped == nil || summary.Expected == nil || summary.Failures == nil {
		return fmt.Errorf("Xcode summary lacks complete passing counts")
	}
	if *summary.Total < 1 || *summary.Passed < 1 || *summary.Skipped < 0 || *summary.Failed != 0 || *summary.Expected != 0 || len(summary.Failures) != 0 || *summary.Passed+*summary.Skipped != *summary.Total {
		return fmt.Errorf("Xcode summary is failed, vacuous, or inconsistent")
	}
	var tree struct {
		TestNodes []consumerXcodeNode `json:"testNodes"`
		Devices   []struct {
			DeviceID string `json:"deviceId"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(testsBody, &tree); err != nil {
		return err
	}
	observed := map[string]string{}
	allowed, _ := consumerUniqueStrings(c.AllowedSkips, false)
	passed, skipped := 0, 0
	var deviceIDs map[string]bool
	validateDeviceMetadata := func() error {
		if deviceIDs != nil {
			return nil
		}
		deviceIDs = map[string]bool{}
		for _, device := range tree.Devices {
			if strings.TrimSpace(device.DeviceID) == "" || deviceIDs[device.DeviceID] {
				return fmt.Errorf("Xcode device metadata requires nonblank unique deviceId")
			}
			deviceIDs[device.DeviceID] = true
		}
		if len(deviceIDs) == 0 {
			return fmt.Errorf("Xcode Device diagnostic requires root devices metadata")
		}
		return nil
	}
	var visit func(consumerXcodeNode) error
	visit = func(n consumerXcodeNode) error {
		if n.NodeType == "Test Case" {
			if n.NodeIdentifier == "" || observed[n.NodeIdentifier] != "" || len(n.TestNodes) != 0 {
				return fmt.Errorf("invalid or duplicate Xcode test leaf")
			}
			// Native diagnostics never add logical tests. A merged report may
			// wrap flat Passed Arguments in one metadata-bound Device layer.
			// Skipped cases retain their direct Failure Message behavior.
			argumentNames := map[string]bool{}
			parentDeviceIDs := map[string]bool{}
			for _, diagnostic := range n.Children {
				switch diagnostic.NodeType {
				case "Failure Message":
					if n.Result != "Skipped" || diagnostic.NodeIdentifier != "" || diagnostic.Result != "" ||
						len(diagnostic.Children)+len(diagnostic.TestNodes) != 0 {
						return fmt.Errorf("unsupported or nested Xcode test diagnostic")
					}
				case "Arguments":
					if n.Result != "Passed" {
						return fmt.Errorf("unsupported or nested Xcode test diagnostic")
					}
					if err := validateConsumerXcodeArgument(diagnostic, argumentNames, n.NodeIdentifier); err != nil {
						return err
					}
				case "Device":
					if n.Result != "Passed" || diagnostic.Result != "Passed" ||
						strings.TrimSpace(diagnostic.Name) == "" || strings.TrimSpace(diagnostic.NodeIdentifier) == "" ||
						len(diagnostic.TestNodes) != 0 {
						return fmt.Errorf("invalid Xcode Device diagnostic under %s", n.NodeIdentifier)
					}
					if err := validateDeviceMetadata(); err != nil {
						return err
					}
					if !deviceIDs[diagnostic.NodeIdentifier] || parentDeviceIDs[diagnostic.NodeIdentifier] {
						return fmt.Errorf("unknown or duplicate Xcode Device %q under %s", diagnostic.NodeIdentifier, n.NodeIdentifier)
					}
					parentDeviceIDs[diagnostic.NodeIdentifier] = true
					deviceArgumentNames := map[string]bool{}
					for _, argument := range diagnostic.Children {
						if argument.NodeType != "Arguments" {
							return fmt.Errorf("unsupported or nested Xcode Device diagnostic")
						}
						if err := validateConsumerXcodeArgument(argument, deviceArgumentNames, n.NodeIdentifier); err != nil {
							return err
						}
					}
				default:
					return fmt.Errorf("unsupported or nested Xcode test diagnostic")
				}
			}
			switch n.Result {
			case "Passed":
				passed++
			case "Skipped":
				if !allowed[n.NodeIdentifier] {
					return fmt.Errorf("unexpected skipped Xcode test %s", n.NodeIdentifier)
				}
				skipped++
			default:
				return fmt.Errorf("Xcode test %s did not pass: %s", n.NodeIdentifier, n.Result)
			}
			observed[n.NodeIdentifier] = n.Result
			return nil
		}
		// Devices and Arguments are valid only in the explicit diagnostic
		// branches above. Never flatten one found at root or under a group.
		if n.NodeType == "Device" {
			return fmt.Errorf("orphan Xcode Device node %q outside Passed Test Case", n.NodeIdentifier)
		}
		if n.NodeType == "Arguments" {
			if strings.TrimSpace(n.Name) == "" {
				return fmt.Errorf("orphan Xcode argument node requires a nonblank name")
			}
			return fmt.Errorf("orphan Xcode argument node %q outside Passed Test Case", n.Name)
		}
		if n.Result == "Failed" {
			return fmt.Errorf("failed Xcode test group")
		}
		for _, nodes := range [][]consumerXcodeNode{n.Children, n.TestNodes} {
			for _, child := range nodes {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, node := range tree.TestNodes {
		if err := visit(node); err != nil {
			return err
		}
	}
	if passed != *summary.Passed || skipped != *summary.Skipped || len(observed) != *summary.Total {
		return fmt.Errorf("Xcode summary and test tree disagree")
	}
	for _, id := range c.RequiredTests {
		if observed[id] != "Passed" {
			return fmt.Errorf("required Xcode test %s did not pass", id)
		}
	}
	return nil
}

func validateConsumerXcodeArgument(n consumerXcodeNode, names map[string]bool, parentID string) error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("Xcode argument node requires a nonblank name")
	}
	if n.Result != "Passed" {
		return fmt.Errorf("Xcode argument node %q did not pass: %s", n.Name, n.Result)
	}
	if n.NodeIdentifier != "" {
		return fmt.Errorf("Xcode argument node %q must not carry a nodeIdentifier", n.Name)
	}
	if len(n.Children)+len(n.TestNodes) != 0 {
		return fmt.Errorf("Xcode argument node %q must be flat", n.Name)
	}
	if names[n.Name] {
		return fmt.Errorf("duplicate Xcode argument name %q under %s", n.Name, parentID)
	}
	names[n.Name] = true
	return nil
}
