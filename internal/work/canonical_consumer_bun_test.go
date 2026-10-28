package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
)

const bunIdentity = `["tests/example.test.ts","","does the thing",["example.test.ts","feature"]]`

func bunConsumerStep(t *testing.T, report string) ProofCheckStep {
	t.Helper()
	contract := canonicalConsumerContract{SchemaVersion: "compound.proof_consumers.v1", Profile: "bun_consumers_v1", Consumers: []CanonicalConsumer{{
		ID: "native-bun", Command: []string{"bun", "test"}, Dir: ".", TimeoutSeconds: 60,
		RequiredTools: []string{"bun"}, RequiredEnvironment: []string{"BUN_INSTALL_CACHE_DIR"},
		ReportKind: "bun_junit", RequiredTests: []string{bunIdentity},
	}}}
	body, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return ProofCheckStep{Name: "consumer:native-bun", Command: []string{"bun", "test"}, Dir: ".", ProofRef: "consumer:native-bun:sha256:" + hex.EncodeToString(sum[:]), ConsumerEvidence: &CanonicalConsumerEvidence{Contract: body, Report: []byte(report)}}
}

func TestCanonicalBunJUnitAcceptsNestedSuiteAndBlankClassname(t *testing.T) {
	report := `<testsuites tests="1" failures="0" errors="0" skipped="0"><testsuite name="example.test.ts" tests="1" failures="0" errors="0" skipped="0"><testsuite name="feature" tests="1" failures="0" errors="0" skipped="0"><testcase file="tests/example.test.ts" classname="" name="does the thing"/></testsuite></testsuite></testsuites>`
	if err := ValidateCanonicalConsumerEvidence(bunConsumerStep(t, report)); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalBunJUnitIdentityUsesEveryNativeField(t *testing.T) {
	valid := `<testsuites tests="1" failures="0" errors="0" skipped="0"><testsuite name="example.test.ts" tests="1" failures="0" errors="0" skipped="0"><testsuite name="feature" tests="1" failures="0" errors="0" skipped="0"><testcase file="tests/example.test.ts" classname="" name="does the thing"/></testsuite></testsuite></testsuites>`
	for name, report := range map[string]string{
		"file":         strings.Replace(valid, `file="tests/example.test.ts"`, `file="tests/other.test.ts"`, 1),
		"classname":    strings.Replace(valid, `classname=""`, `classname="feature"`, 1),
		"outer suite":  strings.Replace(valid, `name="example.test.ts"`, `name="other.test.ts"`, 1),
		"nested suite": strings.Replace(valid, `name="feature"`, `name="other feature"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCanonicalConsumerEvidence(bunConsumerStep(t, report)); err == nil || !strings.Contains(err.Error(), "absent") {
				t.Fatalf("wrong identity field admitted: %v", err)
			}
		})
	}
}

func TestCanonicalBunJUnitRejectsFalseGreenReports(t *testing.T) {
	valid := `<testsuites tests="1" failures="0" errors="0" skipped="0"><testsuite name="example.test.ts" tests="1" failures="0" errors="0" skipped="0"><testsuite name="feature" tests="1" failures="0" errors="0" skipped="0"><testcase file="tests/example.test.ts" classname="" name="does the thing"/></testsuite></testsuite></testsuites>`
	prefixedRoot := strings.Replace(valid, `<testsuites `, `<x:testsuites xmlns:x="urn:example.invalid" `, 1)
	prefixedRoot = strings.Replace(prefixedRoot, `</testsuites>`, `</x:testsuites>`, 1)
	undeclaredRoot := strings.Replace(valid, `<testsuites `, `<x:testsuites `, 1)
	undeclaredRoot = strings.Replace(undeclaredRoot, `</testsuites>`, `</x:testsuites>`, 1)
	longElement := strings.Replace(valid, `<testsuite name="feature"`, `<`+strings.Repeat("x", 257)+`/><testsuite name="feature"`, 1)
	longAttribute := strings.Replace(valid, `<testcase file=`, `<testcase `+strings.Repeat("x", 257)+`="" file=`, 1)
	cases := map[string]string{
		"missing required":    strings.Replace(valid, `name="does the thing"`, `name="other"`, 1),
		"duplicate identity":  strings.Replace(valid, `</testsuite></testsuite></testsuites>`, `<testcase file="tests/example.test.ts" classname="" name="does the thing"/></testsuite></testsuite></testsuites>`, 1),
		"failure":             strings.Replace(valid, `<testcase file=`, `<testcase><failure>boom</failure></testcase><testcase file=`, 1),
		"trailing root":       valid + `<testsuites/>`,
		"zero tests":          `<testsuites tests="0" failures="0" errors="0" skipped="0"></testsuites>`,
		"unknown wrapper":     strings.Replace(valid, `<testsuite name="example.test.ts"`, `<wrapper><testsuite name="example.test.ts"`, 1),
		"outcome outside":     strings.Replace(valid, `</testsuites>`, `<failure>boom</failure></testsuites>`, 1),
		"duplicate attr":      strings.Replace(valid, `<testsuites `, `<testsuites tests="1" `, 1),
		"namespace spoof":     strings.Replace(valid, `<testcase`, `<x:testcase xmlns:x="urn:example.invalid"`, 1),
		"prefixed root":       prefixedRoot,
		"undeclared root":     undeclaredRoot,
		"undeclared element":  strings.Replace(valid, `<testcase`, `<x:testcase`, 1),
		"undeclared attr":     strings.Replace(valid, ` file=`, ` x:file=`, 1),
		"default namespace":   strings.Replace(valid, `<testsuites `, `<testsuites xmlns="urn:example.invalid" `, 1),
		"namespaced attr":     strings.Replace(valid, `<testcase file=`, `<testcase xmlns:x="urn:example.invalid" x:file=`, 1),
		"long element name":   longElement,
		"long attribute name": longAttribute,
		"mismatched close":    strings.Replace(valid, `</testsuite>`, `</testsuites>`, 1),
		"nested outcome":      strings.Replace(valid, `<testcase file=`, `<testcase><failure><error/></failure></testcase><testcase file=`, 1),
		"nonspace outside":    "not xml" + valid,
		"second declaration":  `<?xml version="1.0"?><?xml version="1.0"?>` + valid,
		"missing counts":      strings.Replace(valid, ` tests="1" failures="0" errors="0" skipped="0"`, ``, 1),
		"empty suite":         strings.Replace(valid, `name="feature"`, `name=""`, 1),
		"hidden status":       strings.Replace(valid, `<testcase file=`, `<testcase status="failed" file=`, 1),
		"empty sibling suite": strings.Replace(valid, `</testsuites>`, `<testsuite name="empty" tests="0" failures="0" errors="0" skipped="0"/></testsuites>`, 1),
	}
	for name, report := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCanonicalConsumerEvidence(bunConsumerStep(t, report)); err == nil {
				t.Fatal("false-green JUnit report accepted")
			}
		})
	}
}

func TestBunXMLDecoderPreservesUndeclaredPrefixesInFullNames(t *testing.T) {
	d := xml.NewDecoder(strings.NewReader(`<x:testcase x:file="tests/example.test.ts"/>`))
	tok, err := d.Token()
	if err != nil {
		t.Fatal(err)
	}
	start, ok := tok.(xml.StartElement)
	if !ok {
		t.Fatalf("token = %T, want xml.StartElement", tok)
	}
	if start.Name != (xml.Name{Space: "x", Local: "testcase"}) {
		t.Fatalf("element name = %v, want unresolved prefix retained", start.Name)
	}
	if len(start.Attr) != 1 || start.Attr[0].Name != (xml.Name{Space: "x", Local: "file"}) {
		t.Fatalf("attribute names = %v, want unresolved prefix retained", start.Attr)
	}
	if bunAttrsKnown(start) {
		t.Fatal("namespaced alias matched namespace-free Bun vocabulary")
	}
}

func TestCanonicalBunJUnitOutcomeSemantics(t *testing.T) {
	caseXML := func(outcome string, tests, failures, errors, skipped int) string {
		return fmt.Sprintf(`<testsuites tests="%d" failures="%d" errors="%d" skipped="%d"><testsuite name="example.test.ts" tests="%d" failures="%d" errors="%d" skipped="%d"><testsuite name="feature" tests="%d" failures="%d" errors="%d" skipped="%d"><testcase file="tests/example.test.ts" classname="" name="does the thing">%s</testcase></testsuite></testsuite></testsuites>`, tests, failures, errors, skipped, tests, failures, errors, skipped, tests, failures, errors, skipped, outcome)
	}
	for name, report := range map[string]string{
		"failure":        caseXML(`<failure>boom</failure>`, 1, 1, 0, 0),
		"error":          caseXML(`<error>boom</error>`, 1, 0, 1, 0),
		"required skip":  caseXML(`<skipped/>`, 1, 0, 0, 1),
		"unlisted skip":  strings.Replace(caseXML(`<skipped/>`, 1, 0, 0, 1), `name="does the thing"`, `name="other"`, 1),
		"count mismatch": caseXML(``, 2, 0, 0, 0),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCanonicalConsumerEvidence(bunConsumerStep(t, report)); err == nil {
				t.Fatal("outcome false green accepted")
			}
		})
	}
	duplicate := `<testsuites tests="2" failures="0" errors="0" skipped="0"><testsuite name="example.test.ts" tests="2" failures="0" errors="0" skipped="0"><testsuite name="feature" tests="2" failures="0" errors="0" skipped="0"><testcase file="tests/example.test.ts" classname="" name="does the thing"/><testcase file="tests/example.test.ts" classname="" name="does the thing"/></testsuite></testsuite></testsuites>`
	if err := ValidateCanonicalConsumerEvidence(bunConsumerStep(t, duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate identity did not reach uniqueness check: %v", err)
	}
}

func TestCanonicalBunJUnitAllowsOnlyDeclaredNonrequiredSkip(t *testing.T) {
	allowed := `["tests/example.test.ts","","optional",["example.test.ts","feature"]]`
	step := bunConsumerStep(t, `<testsuites tests="2" failures="0" errors="0" skipped="1"><testsuite name="example.test.ts" tests="2" failures="0" errors="0" skipped="1"><testsuite name="feature" tests="2" failures="0" errors="0" skipped="1"><testcase file="tests/example.test.ts" classname="" name="does the thing"/><testcase file="tests/example.test.ts" classname="" name="optional"><skipped/></testcase></testsuite></testsuite></testsuites>`)
	var contract canonicalConsumerContract
	if err := json.Unmarshal(step.ConsumerEvidence.Contract, &contract); err != nil {
		t.Fatal(err)
	}
	contract.Consumers[0].AllowedSkips = []string{allowed}
	body, _ := json.Marshal(contract)
	sum := sha256.Sum256(body)
	step.ConsumerEvidence.Contract = body
	step.ProofRef = "consumer:native-bun:sha256:" + hex.EncodeToString(sum[:])
	if err := ValidateCanonicalConsumerEvidence(step); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalBunIdentityBoundsAndAllowedSkipGrammar(t *testing.T) {
	suites := make([]string, 33)
	for i := range suites {
		suites[i] = fmt.Sprintf("suite-%d", i)
	}
	body, _ := json.Marshal([]any{"tests/example.test.ts", "", "optional", suites})
	step := bunConsumerStep(t, `<testsuites tests="1" failures="0" errors="0" skipped="0"><testsuite name="example.test.ts" tests="1" failures="0" errors="0" skipped="0"><testcase file="tests/example.test.ts" classname="" name="does the thing"/></testsuite></testsuites>`)
	var contract canonicalConsumerContract
	_ = json.Unmarshal(step.ConsumerEvidence.Contract, &contract)
	contract.Consumers[0].AllowedSkips = []string{string(body)}
	updated, _ := json.Marshal(contract)
	sum := sha256.Sum256(updated)
	step.ConsumerEvidence.Contract = updated
	step.ProofRef = "consumer:native-bun:sha256:" + hex.EncodeToString(sum[:])
	if err := ValidateCanonicalConsumerEvidence(step); err == nil {
		t.Fatal("33-suite allowed skip identity admitted")
	}
	var xmlBody strings.Builder
	xmlBody.WriteString(`<testsuites tests="1" failures="0" errors="0" skipped="0">`)
	for i := 0; i < 33; i++ {
		fmt.Fprintf(&xmlBody, `<testsuite name="suite-%d" tests="1" failures="0" errors="0" skipped="0">`, i)
	}
	xmlBody.WriteString(`<testcase file="tests/example.test.ts" classname="" name="does the thing"/>`)
	for i := 0; i < 33; i++ {
		xmlBody.WriteString(`</testsuite>`)
	}
	xmlBody.WriteString(`</testsuites>`)
	if err := ValidateCanonicalConsumerEvidence(bunConsumerStep(t, xmlBody.String())); err == nil || !strings.Contains(err.Error(), "ancestry exceeds 32") {
		t.Fatalf("33-deep native suite ancestry did not reach bound: %v", err)
	}
}
