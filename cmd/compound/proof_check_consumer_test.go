package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"compound/internal/authority/gitrepository"
	"compound/internal/work"
)

func consumerContractFixture(profile string) map[string]any {
	kind, id := "go_test_json", "example.invalid/product/integration::TestBuyerPath"
	if profile == "xcode_consumers_v1" {
		kind, id = "xcresult", "ActivationTests/testFirstAppearance()"
	} else if profile == "bun_consumers_v1" {
		kind, id = "bun_junit", `["tests/example.test.ts","","does the thing",["example.test.ts","feature"]]`
	}
	return map[string]any{
		"schema_version": "compound.proof_consumers.v1", "profile": profile,
		"consumers": []any{map[string]any{
			"id": "buyer-path", "command": []string{"make", "test-buyer-path"}, "dir": ".",
			"timeout_seconds": 240, "required_tools": []string{"make"},
			"report_kind": kind, "required_tests": []string{id},
		}},
	}
}

func consumerContractJSON(t *testing.T, contract map[string]any) string {
	t.Helper()
	body, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestCanonicalConsumerPlanSelectsSealedContract(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"go_consumers_v1", "xcode_consumers_v1", "bun_consumers_v1"} {
		t.Run(profile, func(t *testing.T) {
			files := map[string]string{"compound-proof.json": consumerContractJSON(t, consumerContractFixture(profile))}
			if profile == "go_consumers_v1" {
				files["go.mod"] = "module example.invalid/product\n\ngo 1.26.4\n"
			} else if profile == "bun_consumers_v1" {
				files["package.json"] = "{}\n"
				files["bun.lock"] = "lockfileVersion = 1\n"
			}
			repo := initCanonicalProofRepo(t, files)
			source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := work.ResolveCanonicalProofPlan(repo, source, nil)
			if err != nil {
				t.Fatalf("required native consumer is not admitted: %v", err)
			}
			if plan.ID != profile || len(plan.Steps) != 3 {
				t.Fatalf("plan = %+v", plan)
			}
			step := plan.Steps[0]
			if step.Name != "consumer:buyer-path" || !reflect.DeepEqual(step.Command, []string{"make", "test-buyer-path"}) || step.Dir != "." || step.SkipIfMissing != "" || !strings.Contains(step.ProofRef, "sha256:") {
				t.Fatalf("consumer identity = %+v", step)
			}
			resource, err := work.ProofResourceProfileForCanonicalPlan(plan)
			if err != nil {
				t.Fatal(err)
			}
			if publishes, err := work.CanonicalProofProfilePublishesCompoundRuntimeArtifact(plan, resource, nil); publishes || err != nil {
				t.Fatalf("external runtime claim = %v, %v", publishes, err)
			}
			writeCanonicalProofFile(t, repo, "compound-proof.json", "broken ambient replacement")
			again, err := work.ResolveCanonicalProofPlan(repo, source, nil)
			if err != nil || !reflect.DeepEqual(plan, again) {
				t.Fatalf("working copy replaced sealed contract: %+v, %v", again, err)
			}
		})
	}
}

func TestCanonicalBunConsumerRequiresBothPackageMarkers(t *testing.T) {
	for _, missing := range []string{"package.json", "bun.lock"} {
		t.Run(missing, func(t *testing.T) {
			files := map[string]string{
				"package.json": "{}\n", "bun.lock": "lockfileVersion = 1\n",
				"compound-proof.json": consumerContractJSON(t, consumerContractFixture("bun_consumers_v1")),
			}
			delete(files, missing)
			repo := initCanonicalProofRepo(t, files)
			source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := work.ResolveCanonicalProofPlan(repo, source, nil); err == nil {
				t.Fatalf("Bun contract admitted without %s", missing)
			}
		})
	}
}

func TestCanonicalGoConsumerAllowsRootPackageJSONWithoutBunLock(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.invalid/product\n", "package.json": "{}\n",
		"compound-proof.json": consumerContractJSON(t, consumerContractFixture("go_consumers_v1")),
	}
	repo := initCanonicalProofRepo(t, files)
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := work.ResolveCanonicalProofPlan(repo, source, nil); err != nil {
		t.Fatalf("root JS tooling broke Go consumer: %v", err)
	}
}

func TestCanonicalConsumerRejectsCrossProfileRootMarkers(t *testing.T) {
	cases := []struct {
		name, profile string
		extra         map[string]string
	}{
		{"bun-with-go", "bun_consumers_v1", map[string]string{"package.json": "{}\n", "bun.lock": "lockfileVersion = 1\n", "go.mod": "module example.invalid/product\n"}},
		{"bun-with-cargo", "bun_consumers_v1", map[string]string{"package.json": "{}\n", "bun.lock": "lockfileVersion = 1\n", "Cargo.toml": "[workspace]\n", "Cargo.lock": "version = 4\n"}},
		{"go-with-bun-lock", "go_consumers_v1", map[string]string{"go.mod": "module example.invalid/product\n", "bun.lock": "lockfile exact\n"}},
		{"xcode-with-bun-lock", "xcode_consumers_v1", map[string]string{"bun.lock": "lockfile exact\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"compound-proof.json": consumerContractJSON(t, consumerContractFixture(tc.profile))}
			for name, body := range tc.extra {
				files[name] = body
			}
			repo := initCanonicalProofRepo(t, files)
			source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := work.ResolveCanonicalProofPlan(repo, source, nil); err == nil {
				t.Fatal("cross-profile root markers admitted")
			}
		})
	}
}

func TestCanonicalConsumerContractRejectsInvalidRequirements(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(map[string]any, map[string]any)
	}{
		{"unknown field", func(c, _ map[string]any) { c["unexpected"] = true }},
		{"unsupported profile", func(c, _ map[string]any) { c["profile"] = "arbitrary" }},
		{"no consumers", func(c, _ map[string]any) { c["consumers"] = []any{} }},
		{"duplicate consumer", func(c, s map[string]any) { c["consumers"] = []any{s, s} }},
		{"no tests", func(_, s map[string]any) { s["required_tests"] = []string{} }},
		{"duplicate tests", func(_, s map[string]any) { s["required_tests"] = []string{"p::TestA", "p::TestA"} }},
		{"required skip", func(_, s map[string]any) { s["allowed_skips"] = s["required_tests"] }},
		{"escaping cwd", func(_, s map[string]any) { s["dir"] = "../outside" }},
		{"absolute cwd", func(_, s map[string]any) { s["dir"] = "/outside" }},
		{"empty command", func(_, s map[string]any) { s["command"] = []string{} }},
		{"no deadline", func(_, s map[string]any) { s["timeout_seconds"] = 0 }},
		{"wrong report", func(_, s map[string]any) { s["report_kind"] = "xcresult" }},
		{"secret environment", func(_, s map[string]any) { s["required_environment"] = []string{"DATABASE_URL"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contract := consumerContractFixture("go_consumers_v1")
			tc.change(contract, contract["consumers"].([]any)[0].(map[string]any))
			repo := initCanonicalProofRepo(t, map[string]string{"go.mod": "module example.invalid/product\n", "compound-proof.json": consumerContractJSON(t, contract)})
			source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := work.ResolveCanonicalProofPlan(repo, source, nil); err == nil {
				t.Fatal("invalid required consumer admitted")
			}
		})
	}
}

func TestCanonicalConsumerContractCannotReplaceCompoundOrRust(t *testing.T) {
	t.Parallel()
	for _, marker := range []string{"go.mod", "Cargo.toml"} {
		t.Run(marker, func(t *testing.T) {
			files := map[string]string{"compound-proof.json": consumerContractJSON(t, consumerContractFixture("go_consumers_v1"))}
			if marker == "go.mod" {
				files[marker] = "module compound\n"
			} else {
				files[marker] = "[workspace]\n"
				files["Cargo.lock"] = "version = 4\n"
			}
			repo := initCanonicalProofRepo(t, files)
			source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps); err == nil {
				t.Fatal("external contract replaced native canonical inventory")
			}
		})
	}
}

func TestCanonicalConsumerNonCertifyingModeRefusesCompoundInventory(t *testing.T) {
	repo := initCanonicalProofRepo(t, map[string]string{"compound-proof.json": consumerContractJSON(t, consumerContractFixture("xcode_consumers_v1"))})
	if err := work.RefuseRustInCompoundOnlyProofMode(repo, "development"); err == nil {
		t.Fatal("external consumer repository received Compound-only development inventory")
	}
}

func TestCanonicalConsumerMalformedModuleCannotSelectCompoundInventory(t *testing.T) {
	for _, body := range []string{
		"module \"compound\"junk\n",
		"module \"compound\" junk\n",
		"module compound junk\n",
		"module \"compound\" /junk\n",
	} {
		t.Run(body, func(t *testing.T) {
			repo := initCanonicalProofRepo(t, map[string]string{"go.mod": body})
			source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps); err == nil {
				t.Fatal("malformed module declaration selected Compound inventory")
			}
		})
	}
}

func TestCanonicalConsumerCannotReclassifyCompoundModule(t *testing.T) {
	for _, body := range []string{
		"module compound // valid comment\n",
		"module \"compound\"\n",
		"module \"compound\" // valid comment\n",
		"module \"compound\"// valid comment\n",
		"module \"compo\\u0075nd\"\n",
		"go 1.26.4\n\nmodule compound\n",
	} {
		t.Run(body, func(t *testing.T) {
			repo := initCanonicalProofRepo(t, map[string]string{"go.mod": body, "compound-proof.json": consumerContractJSON(t, consumerContractFixture("go_consumers_v1"))})
			source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := work.ResolveCanonicalProofPlan(repo, source, proofCheckSteps); err == nil {
				t.Fatal("Compound module spelling replaced full Compound proof with external inventory")
			}
		})
	}
}
