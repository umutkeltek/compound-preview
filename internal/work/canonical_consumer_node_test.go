package work

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/authority/gitrepository"
)

const nodeRequired = `["checks.test.mjs","product checks","keeps the contract",["product checks"]]`

func nodeContract(t *testing.T, dir string) []byte {
	t.Helper()
	body, err := json.Marshal(canonicalConsumerContract{SchemaVersion: "compound.proof_consumers.v1", Profile: "node_npm_consumers_v1", Consumers: []CanonicalConsumer{{
		ID: "product-checks", Command: []string{"node", "proof.mjs"}, Dir: dir, TimeoutSeconds: 10,
		RequiredTools: []string{"node"}, ReportKind: "node_test_junit", RequiredTests: []string{strings.Replace(nodeRequired, "checks.test.mjs", filepath.ToSlash(filepath.Join(dir, "checks.test.mjs")), 1)},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func nodeGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Synthetic", "-c", "user.email=proof@example.invalid"}, args...)...)
	command.Dir = repo
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func nodeRepo(t *testing.T, contractPath string, extra map[string]string) (string, gitrepository.Snapshot) {
	t.Helper()
	repo := t.TempDir()
	files := map[string]string{
		contractPath: string(nodeContract(t, "app")), "app/package.json": `{"name":"synthetic","private":true}`,
		"app/package-lock.json": `{"name":"synthetic","lockfileVersion":3,"packages":{"":{"name":"synthetic"}}}`,
	}
	for name, body := range extra {
		files[name] = body
	}
	for name, body := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nodeGit(t, repo, "init", "-q")
	nodeGit(t, repo, "add", "--", ".")
	nodeGit(t, repo, "commit", "-qm", "synthetic fixture")
	head := nodeGit(t, repo, "rev-parse", "HEAD")
	snapshot, err := gitrepository.CaptureCleanSnapshot(repo, head)
	if err != nil {
		t.Fatal(err)
	}
	return repo, snapshot
}

func TestCanonicalNodeNestedDeclarationAndInventory(t *testing.T) {
	repo, source := nodeRepo(t, "app/compound-proof.json", nil)
	plan, err := ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID != "node_npm_consumers_v1" || len(plan.Steps) != 3 || plan.Steps[1].Name != "diff-check" || plan.Steps[2].Name != "redact-public" {
		t.Fatalf("wrong complete plan: %+v", plan)
	}
	if err := RefuseRustInCompoundOnlyProofMode(repo, "ci"); err == nil {
		t.Fatal("nested consumers borrowed Compound-only gates")
	}
	if _, _, err := ReadCanonicalConsumer(repo, source.HeadSHA, plan.Steps[0]); err != nil {
		t.Fatal(err)
	}
	// The same body at another location is not the admitted contract.
	if err := os.Rename(filepath.Join(repo, "app/compound-proof.json"), filepath.Join(repo, "compound-proof.json")); err != nil {
		t.Fatal(err)
	}
	nodeGit(t, repo, "add", "--", "app/compound-proof.json", "compound-proof.json")
	nodeGit(t, repo, "commit", "-qm", "relocate declaration")
	if _, _, err := ReadCanonicalConsumer(repo, "HEAD", plan.Steps[0]); err == nil {
		t.Fatal("relocated declaration retained admitted gate")
	}
}

func TestCanonicalNodeDeclarationFalsifiers(t *testing.T) {
	for _, scenario := range []string{"ambiguous", "missing lock", "symlink", "tree", "gitlink", "lock symlink", "rust", "go", "compound", "bun", "root package", "non-node nested"} {
		t.Run(scenario, func(t *testing.T) {
			extra := map[string]string{}
			switch scenario {
			case "ambiguous":
				extra["other/compound-proof.json"] = string(nodeContract(t, "app"))
			case "rust":
				extra["Cargo.toml"] = "[workspace]\n"
				extra["Cargo.lock"] = "version = 3\n"
			case "go":
				extra["go.mod"] = "module example.invalid/product\n"
			case "compound":
				extra["go.mod"] = "module compound\n"
			case "bun":
				extra["bun.lock"] = "{}"
			case "root package":
				extra["package.json"] = "{}"
			case "tree":
				extra["other/compound-proof.json/child"] = "not a declaration"
			case "non-node nested":
				extra["app/compound-proof.json"] = strings.ReplaceAll(string(nodeContract(t, "app")), "node_npm_consumers_v1", "xcode_consumers_v1")
			}
			repo, source := nodeRepo(t, "app/compound-proof.json", extra)
			if scenario == "gitlink" {
				if err := os.Mkdir(filepath.Join(repo, "other"), 0o700); err != nil {
					t.Fatal(err)
				}
				nodeGit(t, repo, "clone", "--quiet", "--shared", "--", repo, "other/compound-proof.json")
				nodeGit(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+source.HeadSHA+",other/compound-proof.json")
				nodeGit(t, repo, "commit", "-qm", "gitlink masquerade")
				var err error
				source, err = gitrepository.CaptureCleanSnapshot(repo, source.HeadSHA)
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing lock" || scenario == "symlink" || scenario == "lock symlink" {
				p := "app/package-lock.json"
				if scenario == "symlink" {
					p = "app/compound-proof.json"
				}
				if err := os.Remove(filepath.Join(repo, p)); err != nil {
					t.Fatal(err)
				}
				if scenario == "symlink" || scenario == "lock symlink" {
					if err := os.Symlink("package.json", filepath.Join(repo, p)); err != nil {
						t.Fatal(err)
					}
				}
				nodeGit(t, repo, "add", "--", p)
				nodeGit(t, repo, "commit", "-qm", "invalid fixture")
				var err error
				source, err = gitrepository.CaptureCleanSnapshot(repo, source.HeadSHA)
				if err != nil {
					t.Fatal(err)
				}
			}
			plan, err := ResolveCanonicalProofPlan(repo, source, func(string, ...string) ([]ProofCheckStep, error) {
				return []ProofCheckStep{{Name: "owned", Command: []string{"true"}, ProofRef: "owned"}}, nil
			})
			if scenario == "rust" || scenario == "compound" {
				if err != nil || plan.ID == "node_npm_consumers_v1" {
					t.Fatalf("canonical root inventory replaced: %+v %v", plan, err)
				}
			} else if err == nil {
				t.Fatalf("invalid nested declaration accepted: %+v", plan)
			}
		})
	}
}

// These are genuine local node:test reports, not PASS text converted to XML.
func nativeNodeReport(t *testing.T, source string) ([]byte, string) {
	t.Helper()
	node := nativeNode26(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checks.test.mjs"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "--test", "--test-reporter=junit", "checks.test.mjs")
	cmd.Dir = dir
	body, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatal(err)
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return body, real
}

func nativeNode26(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("native Node runtime unavailable; static captured fixtures still run")
	}
	version, err := exec.Command(node, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(version), "v26.") {
		t.Skipf("native execution fixture targets tested Node26 report shape; found %s", strings.TrimSpace(string(version)))
	}
	return node
}

func nodeEvidenceStep(t *testing.T, report []byte, root string) ProofCheckStep {
	t.Helper()
	body := nodeContract(t, ".")
	step := canonicalConsumerStep(bodyConsumer(t, body), body)
	step.ConsumerEvidence = &CanonicalConsumerEvidence{Contract: body, Report: report}
	// JSON exercises the evidence wire shape even before implementation exists.
	raw, err := json.Marshal(map[string]any{"contract": body, "report": report, "source_root": root})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, step.ConsumerEvidence); err != nil {
		t.Fatal(err)
	}
	return step
}

func bodyConsumer(t *testing.T, body []byte) CanonicalConsumer {
	t.Helper()
	var c canonicalConsumerContract
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	return c.Consumers[0]
}

func TestCanonicalNodeGenuineNativeOutcomes(t *testing.T) {
	for _, outcome := range []string{"pass", "fail", "todo", "skip", "duplicate", "nested"} {
		t.Run(outcome, func(t *testing.T) {
			statement := `test('keeps the contract', () => assert.equal(2+2,4));`
			switch outcome {
			case "fail":
				statement = `test('keeps the contract', () => assert.equal(2+2,5));`
			case "todo":
				statement = `test('keeps the contract', {todo:true}, () => {});`
			case "skip":
				statement = `test('keeps the contract', {skip:true}, () => {});`
			case "duplicate":
				statement += statement
			case "nested":
				statement += `describe('nested', () => {test('other',()=>{});test('another',()=>{});});`
			}
			body, root := nativeNodeReport(t, `import {test,describe} from 'node:test'; import assert from 'node:assert/strict'; console.log('native diagnostic'); describe('product checks',()=>{`+statement+`});`)
			err := ValidateCanonicalConsumerEvidence(nodeEvidenceStep(t, body, root))
			if outcome == "pass" || outcome == "nested" {
				if err != nil {
					t.Fatalf("genuine native report refused: %v\n%s", err, body)
				}
			} else if err == nil {
				t.Fatalf("native %s accepted", outcome)
			}
		})
	}
}

func TestCanonicalNodeNativeReportTampering(t *testing.T) {
	body, root := nativeNodeReport(t, `import {test,describe} from 'node:test'; describe('product checks',()=>{test('keeps the contract',()=>{});});`)
	for name, mutate := range map[string]func(string) string{
		"truncated":   func(s string) string { return s[:len(s)/2] },
		"count":       func(s string) string { return strings.Replace(s, `tests="1"`, `tests="2"`, 1) },
		"name":        func(s string) string { return strings.Replace(s, `name="keeps the contract"`, `name="unrelated"`, 1) },
		"escape":      func(s string) string { return strings.Replace(s, root, filepath.Dir(root), 1) },
		"second root": func(s string) string { return s + `<testsuites/>` },
		"namespace": func(s string) string {
			return strings.Replace(s, `<testsuites>`, `<testsuites xmlns="urn:example.invalid">`, 1)
		},
		"hidden status": func(s string) string { return strings.Replace(s, `<testcase `, `<testcase status="failed" `, 1) },
		"declaration":   func(s string) string { return `<!DOCTYPE testsuites>` + s },
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCanonicalConsumerEvidence(nodeEvidenceStep(t, []byte(mutate(string(body))), root)); err == nil {
				t.Fatal("tampered native report accepted")
			}
		})
	}
}

func TestCanonicalNodeRunnerUsesSealedDeclarationAndPrivateEnvironment(t *testing.T) {
	nativeNode26(t)
	proof := `import {spawnSync} from 'node:child_process'; import {readFileSync} from 'node:fs'; import assert from 'node:assert/strict'; assert.equal(process.env.NODE_OPTIONS,undefined); assert.equal(process.env.NPM_TOKEN,undefined); assert.equal(process.env.npm_config_registry,undefined); assert.ok(process.env.HOME.startsWith(process.env.TMPDIR)); for(const key of ['NPM_CONFIG_USERCONFIG','NPM_CONFIG_GLOBALCONFIG']){assert.ok(process.env[key].startsWith(process.env.TMPDIR));assert.equal(readFileSync(process.env[key],'utf8'),'');} assert.equal(process.env.NPM_CONFIG_OFFLINE,'true');assert.equal(process.env.NPM_CONFIG_IGNORE_SCRIPTS,'true');assert.equal(process.env.NPM_CONFIG_FETCH_RETRIES,'0');assert.ok(process.env.NPM_CONFIG_CACHE.startsWith(process.env.TMPDIR)); const r=spawnSync(process.execPath,['--test','--test-reporter=junit','--test-reporter-destination='+process.env.COMPOUND_PROOF_REPORT,'checks.test.mjs'],{stdio:'inherit',env:process.env}); process.exit(r.status ?? 1);`
	repo, source := nodeRepo(t, "app/compound-proof.json", map[string]string{"app/proof.mjs": proof, "app/checks.test.mjs": `import {test,describe} from 'node:test';describe('product checks',()=>{test('keeps the contract',()=>{});});`})
	plan, err := ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--invalid-inherited-option")
	t.Setenv("NPM_TOKEN", "synthetic-do-not-inherit")
	t.Setenv("npm_config_registry", "https://example.invalid")
	t.Setenv("NPM_CONFIG_USERCONFIG", "/untrusted/user.config")
	t.Setenv("NPM_CONFIG_GLOBALCONFIG", "/untrusted/global.config")
	t.Setenv("NPM_CONFIG_CACHE", "/untrusted/cache")
	output, evidence, err := RunCanonicalConsumer(context.Background(), repo, plan.Steps[0], []string{source.HeadSHA}, exec.CommandContext, exec.LookPath)
	if err != nil {
		t.Fatalf("runner failed: %v\n%s", err, output)
	}
	step := plan.Steps[0]
	step.ConsumerEvidence = evidence
	if err := ValidateCanonicalConsumerEvidence(step); err != nil {
		t.Fatal(err)
	}
}

// Captured from genuine Node26.9 execution. Only absolute root/hostname/time
// metadata was normalized in this portable fixture; test results/counts are native.
func nodeGoldenStep(t *testing.T) ProofCheckStep {
	t.Helper()
	report, err := os.ReadFile("testdata/node26.junit.xml")
	if err != nil {
		t.Fatal(err)
	}
	step := nodeEvidenceStep(t, report, "/proof")
	var contract canonicalConsumerContract
	if err := json.Unmarshal(step.ConsumerEvidence.Contract, &contract); err != nil {
		t.Fatal(err)
	}
	contract.Consumers[0].AllowedSkips = []string{`["checks.test.mjs","product checks.nested","optional",["product checks","nested"]]`}
	body, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	step.ProofRef = canonicalConsumerStep(contract.Consumers[0], body).ProofRef
	step.ConsumerEvidence.Contract = body
	return step
}

func TestCanonicalNodePortableNativeFixture(t *testing.T) {
	if err := ValidateCanonicalConsumerEvidence(nodeGoldenStep(t)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ProofCheckStep){
		"omitted root":  func(s *ProofCheckStep) { s.ConsumerEvidence.SourceRoot = "" },
		"tampered root": func(s *ProofCheckStep) { s.ConsumerEvidence.SourceRoot = "/different" },
		"alias root":    func(s *ProofCheckStep) { s.ConsumerEvidence.SourceRoot = "/alias/proof" },
		"unclean root":  func(s *ProofCheckStep) { s.ConsumerEvidence.SourceRoot = "/proof/../proof" },
		"outside file": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.ReplaceAll(string(s.ConsumerEvidence.Report), "/proof/", "/outside/"))
		},
		"prefix sibling": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.ReplaceAll(string(s.ConsumerEvidence.Report), "/proof/", "/proof-x/"))
		},
		"parent traversal": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.ReplaceAll(string(s.ConsumerEvidence.Report), "/proof/", "/proof/../proof/"))
		},
		"todo not skip": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.ReplaceAll(string(s.ConsumerEvidence.Report), `type="skipped"`, `type="todo"`))
		},
		"failure": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `<skipped `, `<failure `, 1))
		},
		"duplicate identity": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `</testsuite>`, `<testcase name="other" time="0.000000" classname="product checks.nested" file="/proof/checks.test.mjs"/></testsuite>`, 1))
		},
		"missing required": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.ReplaceAll(string(s.ConsumerEvidence.Report), "keeps the contract", "unrelated"))
		},
		"bad immediate count": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `tests="2"`, `tests="3"`, 1))
		},
		"truncation": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = s.ConsumerEvidence.Report[:len(s.ConsumerEvidence.Report)-30]
		},
		"hidden status": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `<testcase `, `<testcase status="failed" `, 1))
		},
		"duplicate attribute": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `<testcase `, `<testcase name="fake" `, 1))
		},
		"namespace": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = []byte(strings.Replace(string(s.ConsumerEvidence.Report), `<testsuites>`, `<testsuites xmlns="urn:example.invalid">`, 1))
		},
		"trailing root": func(s *ProofCheckStep) {
			s.ConsumerEvidence.Report = append(s.ConsumerEvidence.Report, []byte(`<testsuites/>`)...)
		},
		"changed bytes": func(s *ProofCheckStep) { s.ConsumerEvidence.Contract = append(s.ConsumerEvidence.Contract, ' ') },
		"changed path":  func(s *ProofCheckStep) { s.ConsumerEvidence.ContractPath = "nested/compound-proof.json" },
	} {
		t.Run(name, func(t *testing.T) {
			step := nodeGoldenStep(t)
			mutate(&step)
			if err := ValidateCanonicalConsumerEvidence(step); err == nil {
				t.Fatal("false native success")
			}
		})
	}
}

func TestCanonicalNodeOwnedFinalizerBindsDeclarationPathAndNativeEvidence(t *testing.T) {
	step := nodeGoldenStep(t)
	body := step.ConsumerEvidence.Contract
	step.ProofRef = canonicalConsumerStepAtPath(bodyConsumer(t, body), body, "app/compound-proof.json").ProofRef
	step.ConsumerEvidence.ContractPath = "app/compound-proof.json"
	report, _, reservation := consumerOwnedReportFixture(t, "go_test_json")
	profile, err := NewProofResourceProfile(ProofResourceProfileCanonicalFull, reservation.Workspace, "", []ProofResourceGateProfile{{Name: step.Name, Command: step.Command, Dir: step.Dir, ProofRef: step.ProofRef}})
	if err != nil {
		t.Fatal(err)
	}
	reservation.Profile = profile
	report["steps"] = []any{map[string]any{"name": step.Name, "command": step.Command, "dir": step.Dir, "proof_ref": step.ProofRef, "status": "ok", "consumer_evidence": step.ConsumerEvidence}}
	report["proof_refs"] = []string{step.ProofRef}
	if err := validateConsumerOwnedFixture(t, report, reservation); err != nil {
		t.Fatal(err)
	}
	step.ConsumerEvidence.ContractPath = "other/compound-proof.json"
	if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
		t.Fatal("finalizer accepted relocated declaration")
	}
	step.ConsumerEvidence.ContractPath = "app/compound-proof.json"
	step.ConsumerEvidence.Report = append(step.ConsumerEvidence.Report, []byte(`<testsuites/>`)...)
	if err := validateConsumerOwnedFixture(t, report, reservation); err == nil {
		t.Fatal("finalizer accepted tampered native evidence")
	}
}

func TestCanonicalConsumerLegacyEvidenceSerializationUnchanged(t *testing.T) {
	body, err := json.Marshal(&CanonicalConsumerEvidence{Contract: []byte("a"), Report: []byte("b")})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"contract":"YQ==","report":"Yg=="}` {
		t.Fatalf("legacy evidence bytes changed: %s", body)
	}
}

func TestCanonicalNodeRunnerRefusesPostReportSourceMutationAndTimeout(t *testing.T) {
	nativeNode26(t)
	for _, scenario := range []string{"source mutation", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			finish := `await import('node:fs').then(fs=>fs.appendFileSync('checks.test.mjs','\n// changed'));`
			if scenario == "timeout" {
				finish = `setInterval(()=>{},100);`
			}
			proof := `import {spawnSync} from 'node:child_process';const r=spawnSync(process.execPath,['--test','--test-reporter=junit','--test-reporter-destination='+process.env.COMPOUND_PROOF_REPORT,'checks.test.mjs'],{stdio:'inherit',env:process.env});if(r.status!==0)process.exit(r.status??1);` + finish
			var c canonicalConsumerContract
			if err := json.Unmarshal(nodeContract(t, "app"), &c); err != nil {
				t.Fatal(err)
			}
			c.Consumers[0].TimeoutSeconds = 1
			body, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			repo, source := nodeRepo(t, "app/compound-proof.json", map[string]string{"app/compound-proof.json": string(body), "app/proof.mjs": proof, "app/checks.test.mjs": `import {test,describe} from 'node:test';describe('product checks',()=>{test('keeps the contract',()=>{});});`})
			plan, err := ResolveCanonicalProofPlan(repo, source, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, evidence, err := RunCanonicalConsumer(context.Background(), repo, plan.Steps[0], []string{source.HeadSHA}, exec.CommandContext, exec.LookPath)
			if err == nil {
				t.Fatal("unsafe run accepted despite native success report")
			}
			if evidence == nil || len(evidence.Report) == 0 {
				t.Fatal("native partial evidence not retained")
			}
			if scenario == "source mutation" && !strings.Contains(err.Error(), "changed sealed tracked source") {
				t.Fatalf("wrong mutation refusal: %v", err)
			}
		})
	}
}
