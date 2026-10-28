package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot relative to internal/arch/ (where the test runs).
const repoRoot = "../.."

func newDocLeakGitRepo(t *testing.T) (string, func(...string)) {
	t.Helper()
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	return repo, runGit
}

func hasDocLeakFailContaining(findings []Finding, parts ...string) bool {
	for _, finding := range findings {
		if finding.Check != "doc-leak" || finding.Severity != Fail {
			continue
		}
		matched := true
		for _, part := range parts {
			if !strings.Contains(finding.Message, part) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func doctorDocLeakFindings(repo string, strict bool) []Finding {
	var findings []Finding
	for _, finding := range DoctorSource(repo, strict).Findings {
		if finding.Check == "doc-leak" {
			findings = append(findings, finding)
		}
	}
	return findings
}

// The doctor must be GREEN against the real repo: candidate map intact, every internal/ package
// classified, public docs clean. If someone adds an unclassified package or leaks a private path
// into a tracked doc, THIS test fails in CI — that is the enforcement.
func TestDoctorGreenAgainstRealRepo(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	if !r.OK {
		for _, f := range r.Findings {
			if f.Severity == Fail {
				t.Errorf("doctor FAIL [%s] %s", f.Check, f.Message)
			}
		}
	}
}

func TestPostgresADRAuthorityParity(t *testing.T) {
	read := func(rel string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(body)
	}

	accepted := read("docs/adr/0005-postgres-pgvector-storage-experiment.md")
	if !strings.Contains(accepted, "**Status:** Accepted for additive, opt-in experimentation; production authority cutover is not ratified") {
		t.Fatal("bounded Postgres experiment is not identified as the one accepted runtime ADR")
	}
	consolidation := read("docs/adr/0006-consolidation-freeze-protocol.md")
	for rel, body := range map[string]string{
		"docs/adr/0005-postgres-pgvector-storage-experiment.md": accepted,
		"docs/adr/0006-consolidation-freeze-protocol.md":        consolidation,
	} {
		for _, want := range []string{
			"process-memory-only DSN",
			"unknown/partial configuration",
			"without file or breadth fallback",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s does not preserve experimental Postgres boundary %q", rel, want)
			}
		}
	}
	for _, rel := range []string{
		"docs/adr/0005-storage-interface-extraction.md",
		"docs/adr/0006-postgres-pgvector-adoption.md",
	} {
		body := read(rel)
		if !strings.Contains(body, "**Status:** Historical proposal — never ratified; not the current runtime decision") {
			t.Fatalf("%s is not classified as historical and non-authoritative", rel)
		}
	}
	for _, stale := range []string{
		"remains independently proposed and deferred",
		"ADR-0006 (Postgres+pgvector adoption — DEFERRED)",
		"ADR-0006 (Package Consolidation Freeze Protocol)",
		"ADR-0006 defines the operator freeze protocol",
		"Package consolidation — ADR-0006 only",
	} {
		if strings.Contains(accepted+read("docs/adr/0005-storage-interface-extraction.md"), stale) {
			t.Fatalf("stale Postgres ADR authority label remains: %q", stale)
		}
	}
}

// Physical custody is the retirement ratchet: if future edits drift back to
// "the path and hash are enough," legacy note trees and databases become
// permanent co-owners again. Pin the focused owner doc, its served build-guide
// consumer, and the migration-only source-in-place boundary together.
func TestInformationCustodyDoctrineIsServedAndRetirementGrade(t *testing.T) {
	read := func(rel string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(body)
	}
	custody := read("docs/architecture/information-custody.md")
	for _, want := range []string{
		"Physical custody",
		"Semantic authority",
		"Projection",
		"Index",
		"source_in_place` remains readable only as a migration bridge",
		"isolated restore",
		"real consumer",
		"negative control",
		"quiet_window_proven",
		"One-owner matrix",
		"Preference and setting flow",
		"LifecyclePlan",
		"Failure-case contract",
		"Compound and execution-gateway boundary",
	} {
		if !strings.Contains(custody, want) {
			t.Errorf("information custody contract missing %q", want)
		}
	}
	agents := read("AGENTS.md")
	if !strings.Contains(agents, "docs/architecture/information-custody.md") {
		t.Fatal("served build guide does not route executors to the custody owner")
	}
	runtime := read("docs/architecture/runtime.md")
	for _, want := range []string{"lifecycle intention", "native`, `fallback`, `degraded`, or `unsupported`", "Installation observation is\nseparate evidence"} {
		if !strings.Contains(runtime, want) {
			t.Errorf("runtime hook contract missing %q", want)
		}
	}
	execution := read("docs/architecture/execution-plane.md")
	for _, want := range []string{"The execution handshake", "Requested and effective values are both durable", "no_account_or_quota", "unknown_effective_parameters"} {
		if !strings.Contains(execution, want) {
			t.Errorf("execution-plane contract missing %q", want)
		}
	}
	adr := read("docs/adr/0002-compound-owned-information-custody.md")
	if !strings.Contains(adr, "Status: accepted") || !strings.Contains(adr, "physically owns") {
		t.Fatal("accepted physical-custody decision is missing or weakened")
	}
	legacyADR := read("docs/adr/0001-local-monolith-information-kernel.md")
	if strings.Contains(legacyADR, "Sources can stay in place while derived knowledge") {
		t.Fatal("ADR 0001 regressed to permanent source-in-place ownership")
	}
}

// The candidate-map check is pure (no fs) — every kind has one home dir, created by home, with a
// consumer. This locks the B1 invariant.
func TestDocLeakUsesSourceLeakTokensForPublicSourceSurfaces(t *testing.T) {
	repo := t.TempDir()
	for _, dir := range []string{"docs", "docs_private", "fixtures", "tools", filepath.Join("console", "src")} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "docs_private", "leak-tokens.txt"), []byte("docs-only-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs_private", "source-leak-tokens.txt"), []byte("private-product\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "fixtures", "example.yaml"), []byte("name: private-product\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tools", "edge.test.ts"), []byte(`const product = "private-product";`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "console", "src", "page.tsx"), []byte(`export const label = "private-product";`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkDocLeak(repo, false)
	if len(findings) != 3 {
		t.Fatalf("findings = %+v, want fixture, tools, and console source leak failures", findings)
	}
	for _, want := range []string{
		"fixtures/example.yaml",
		"tools/edge.test.ts",
		filepath.Join("console", "src", "page.tsx"),
	} {
		var found bool
		for _, finding := range findings {
			if finding.Severity == Fail && strings.Contains(finding.Message, want) && strings.Contains(finding.Message, "private-product") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("findings should name %s token leak: %+v", want, findings)
		}
	}
}

// TestDocLeakArmedByEmbeddedBaselineWhenOperatorOverlayAbsent is the F2 falsifier:
// a fresh clone/worktree carries no gitignored docs_private/leak-tokens.txt overlay,
// yet the doc-leak gate must NOT fail closed under --strict. The tracked embedded
// baseline arms the gate by default, so a stranger's `proof-check` (which runs the
// doctor --strict) passes instead of tripping the old DISARMED failure.
func TestDocLeakArmedByEmbeddedBaselineWhenOperatorOverlayAbsent(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No docs_private/ overlay at all — the fresh-clone condition, under --strict.
	for _, f := range checkDocLeak(repo, true) {
		if f.Severity == Fail {
			t.Fatalf("fresh clone (no operator overlay) must not fail closed under --strict; got Fail: %+v", f)
		}
	}
}

// TestDocLeakBaselineCatchesSecretShapeWithoutOverlay proves the baseline is armed
// with TEETH, not merely present: an obvious secret-token shape in a tracked public
// doc is caught even when no operator overlay exists. The generic /Users/·email regex
// would NOT match this shape, so only the baseline token list can.
func TestDocLeakBaselineCatchesSecretShapeWithoutOverlay(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "leak.md"), []byte("example token ghp_0123456789abcdefABCD in a doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var caught bool
	for _, f := range checkDocLeak(repo, false) {
		if f.Severity == Fail && strings.Contains(f.Message, "ghp_") {
			caught = true
		}
	}
	if !caught {
		t.Fatalf("baseline gate must catch a secret token shape (ghp_) with no operator overlay")
	}
}

// TestFindGenericLeakBoundaryGuardAndAllowlists is the pure (no fs/git) unit falsifier
// for findGenericLeak's shape logic — F3 (RFC2606 email allow), F4 (synthetic path
// segment allow), and F5 (boundary guard against mid-word/mid-path false positives).
// Real-shaped disallowed cases are built by string concatenation so this test's own
// source stays clean under the very gate it is testing.
func TestFindGenericLeakBoundaryGuardAndAllowlists(t *testing.T) {
	realEmail := "real" + "person@gm" + "ail.com"
	realPath := "/Us" + "ers/real" + "person"
	notExampleEmail := "person@not" + "example.com"
	exampleSuffixEmail := "person@example.com." + "evil"
	subdomainExampleEmail := "person@sub." + "example.com"
	examplePrefixPath := "/Us" + "ers/example2"
	cases := []struct {
		name       string
		line       string
		wantLeak   bool
		wantSubstr string
	}{
		// F5: boundary guard — "/compound/home/work" (internal/work/land_check_test.go's
		// proof_path field) and a bare relative file-path string "internal/home/catalog.go"
		// (a Go source-file path literal, internal/arch/eventstore_guard_test.go) must NOT
		// match: neither slash is preceded by a real path boundary.
		{"boundary guard: /compound/home/work", `"proof_path":"/compound/home/work/land-checks/x.json"`, false, ""},
		{"boundary guard: bare relative file-path string", `mustWriteArchFixture(t, repo, "internal/home/catalog.go", body)`, false, ""},
		// F3: RFC2606/reserved email domains are allowed.
		{"RFC2606 allow: .invalid", `"debug_contact": "person@example.invalid"`, false, ""},
		{"RFC2606 allow: example.com", `"debug_contact": "alice@example.com"`, false, ""},
		{"RFC2606 allow: example.org", `"debug_contact": "bob@example.org"`, false, ""},
		{"RFC2606 allow: example.net", `"debug_contact": "bob@example.net"`, false, ""},
		{"RFC2606 allow: .test", `"debug_contact": "person@example.test"`, false, ""},
		{"RFC2606 allow: .example", `"debug_contact": "person@` + `fixture.example"`, false, ""},
		{"RFC2606 allow: .localhost", `"debug_contact": "person@` + `fixture.localhost"`, false, ""},
		// F4: synthetic /Users|/home user segments are allowed.
		{"synthetic segment allow: faketester", `body := "/Users/faketester/brain/note.md"`, false, ""},
		{"synthetic segment allow: tester", `t.Setenv("HOME", "/home/tester")`, false, ""},
		{"synthetic segment allow: testoperator", `"/Users/testoperator/private-notes"`, false, ""},
		{"synthetic segment allow: example", `"/Users/example/.claude/projects/x.jsonl"`, false, ""},
		// Hidden directories directly under /home are not user-home shapes: the user
		// segment must begin with a letter before exact synthetic-segment filtering.
		{"hidden product home is not a user segment", `"/home/.compound/reviews/area/item.json"`, false, ""},
		{"hidden cache is not a user segment", `"/home/.cache/item.json"`, false, ""},
		// Real-shaped, disallowed cases MUST fail.
		{"disallowed email", "contact " + realEmail + " directly", true, realEmail},
		{"example-name prefix is not allowlisted", "contact " + notExampleEmail, true, notExampleEmail},
		{"example domain with real suffix is not allowlisted", "contact " + exampleSuffixEmail, true, exampleSuffixEmail},
		{"example subdomain is not allowlisted", "contact " + subdomainExampleEmail, true, subdomainExampleEmail},
		{"disallowed path segment", realPath + "/secret.txt leaked", true, realPath},
		{"synthetic segment prefix is not allowlisted", examplePrefixPath + "/secret.txt", true, examplePrefixPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findGenericLeak(tc.line)
			if tc.wantLeak && got == "" {
				t.Fatalf("findGenericLeak(%q) = \"\", want a non-allowlisted leak", tc.line)
			}
			if !tc.wantLeak && got != "" {
				t.Fatalf("findGenericLeak(%q) = %q, want no leak (allowlisted or guarded)", tc.line, got)
			}
			if tc.wantSubstr != "" && got != tc.wantSubstr {
				t.Fatalf("findGenericLeak(%q) = %q, want %q", tc.line, got, tc.wantSubstr)
			}
		})
	}
}

// TestCheckDocLeakScansTrackedRootAndSourceFilesWithNoOperatorOverlay is the
// end-to-end falsifier for F1 (root-file coverage), F2 (source coverage with no
// docs_private overlay present at all), F6 (tracked-only scope), and F7 (fresh-clone
// armed: no operator overlay anywhere, under --strict). Before this packet: root files
// (README.md, AGENTS.md, go.mod, .github/**) were never scanned by the generic pass at
// all (docLeakScanRoots was docs/+skills/ only), and the source-dir generic pass never
// ran without the gitignored docs_private/source-leak-tokens.txt overlay present — a
// fresh public clone was blind on both counts. Real-shaped leaks are built by string
// concatenation so this test's own source stays clean under the gate it is testing.
func TestCheckDocLeakScansTrackedRootAndSourceFilesWithNoOperatorOverlay(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "user.name", "test")

	// F1: a ROOT file (never scanned pre-packet) with a real-shaped, non-RFC2606 email.
	rootEmail := "real" + "person@gm" + "ail.com"
	rootRel := "README.md"
	if err := os.WriteFile(filepath.Join(repo, rootRel), []byte("contact: "+rootEmail+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// F2: a SOURCE file with a boundary-guarded, real-shaped /Users/ path — no
	// docs_private/source-leak-tokens.txt overlay exists anywhere in this repo.
	sourceRel := filepath.Join("internal", "x", "x.go")
	realPath := "/Us" + "ers/real" + "person" + "/secret.txt"
	if err := os.MkdirAll(filepath.Join(repo, "internal", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, sourceRel), []byte("package x\n\n// path: "+realPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", rootRel, sourceRel)

	// F6: the SAME real-shaped leak, but in an UNTRACKED file — must stay invisible.
	untrackedRel := filepath.Join("internal", "x", "untracked.go")
	if err := os.WriteFile(filepath.Join(repo, untrackedRel), []byte("package x\n\n// path: "+realPath+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// F7: no docs_private/ overlay anywhere in this repo — the fresh-clone condition —
	// exercised under --strict.
	var fails []Finding
	for _, f := range checkDocLeak(repo, true) {
		if f.Severity == Fail {
			fails = append(fails, f)
		}
	}
	if len(fails) != 2 {
		t.Fatalf("want exactly 2 Fail findings (root email + source path, no double-report), got %+v", fails)
	}
	var sawRoot, sawSource bool
	for _, f := range fails {
		if strings.Contains(f.Message, rootRel+":") {
			sawRoot = true
		}
		if strings.Contains(f.Message, filepath.ToSlash(sourceRel)+":") {
			sawSource = true
		}
		if strings.Contains(f.Message, "untracked.go") {
			t.Fatalf("F6: untracked file leaked into findings (scope leak): %+v", f)
		}
	}
	if !sawRoot {
		t.Fatalf("F1: root file leak not caught: %+v", fails)
	}
	if !sawSource {
		t.Fatalf("F2: source file leak not caught with no operator overlay present: %+v", fails)
	}
}

func TestDoctorDocLeakIgnoresRepositoryShapingGitEnvironment(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	realEmail := "real" + "person@gm" + "ail.com"
	mustWriteArchFixture(t, repo, "README.md", "contact: "+realEmail+"\n")
	mustWriteArchFixture(t, repo, "decoy.txt", "clean\n")
	runGit("add", "--", "README.md")

	decoy, runDecoyGit := newDocLeakGitRepo(t)
	mustWriteArchFixture(t, decoy, "decoy.txt", "clean\n")
	runDecoyGit("add", "--", "decoy.txt")

	decoyGitDir := filepath.Join(decoy, ".git")
	t.Setenv("GIT_DIR", decoyGitDir)
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_COMMON_DIR", decoyGitDir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoyGitDir, "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(decoyGitDir, "objects"))
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(decoyGitDir, "objects"))

	docLeakFindings := doctorDocLeakFindings(repo, true)
	if !hasDocLeakFailContaining(docLeakFindings, "README.md:", realEmail) {
		t.Fatalf("repository-shaping Git environment redirected tracked enumeration: %+v", docLeakFindings)
	}
}

func TestDoctorDocLeakMatchesEmailIndependentlyFromHomePath(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	realEmail := "example@gm" + "ail.com"
	rel := "overlapping-shapes.txt"
	mustWriteArchFixture(t, repo, rel, "home: /Users/"+realEmail+"/notes\n")
	runGit("add", "--", rel)

	findings := doctorDocLeakFindings(repo, true)
	if !hasDocLeakFailContaining(findings, rel+":", "leaks "+strconv(realEmail)) {
		t.Fatalf("home-path match suppressed the overlapping email: %+v", findings)
	}
}

func TestDoctorDocLeakRequiresWholeHomeSegmentBeforeAllowlist(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	segment := "example+" + "actual"
	realPath := "/Users/" + segment
	rel := "whole-home-segment.txt"
	mustWriteArchFixture(t, repo, rel, "home: "+realPath+"/notes\n")
	runGit("add", "--", rel)

	findings := doctorDocLeakFindings(repo, true)
	if !hasDocLeakFailContaining(findings, rel+":", "leaks "+strconv(realPath)) {
		t.Fatalf("synthetic allowlist accepted only a prefix of the home segment: %+v", findings)
	}
}

func TestCheckDocLeakScansEveryTrackedTextBlobAndSkipsBinary(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	realEmail := "real" + "person@gm" + "ail.com"
	textFiles := []string{
		"go.mod",
		"go.sum",
		"Makefile",
		"LICENSE",
		".gitignore",
		"install.sh",
		filepath.Join("src", "main.swift"),
		filepath.Join("src", "site.css"),
		filepath.Join("fixtures", "events.ndjson"),
		filepath.Join("ops", "job.plist"),
		filepath.Join("console", "bun.lock"),
	}
	for _, rel := range textFiles {
		mustWriteArchFixture(t, repo, rel, "contact: "+realEmail+"\n")
	}
	binaryRel := "binary-with-text-extension.md"
	binaryBody := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0}, []byte("contact: "+realEmail+"\n")...)
	mustWriteArchFixture(t, repo, binaryRel, string(binaryBody))
	tracked := append(append([]string{}, textFiles...), binaryRel)
	runGit(append([]string{"add", "--"}, tracked...)...)

	findings := checkDocLeak(repo, true)
	for _, rel := range textFiles {
		if !hasDocLeakFailContaining(findings, filepath.ToSlash(rel)+":") {
			t.Errorf("tracked text blob %s escaped the generic identity scan: %+v", rel, findings)
		}
	}
	if hasDocLeakFailContaining(findings, binaryRel+":") {
		t.Fatalf("binary blob must not be treated as text: %+v", findings)
	}
}

func TestDoctorDocLeakScansTrackedUTF8ContainingNUL(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	realEmail := "real" + "person@gm" + "ail.com"
	rel := "nul-containing.txt"
	mustWriteArchFixture(t, repo, rel, "metadata\x00contact: "+realEmail+"\n")
	runGit("add", "--", rel)

	findings := doctorDocLeakFindings(repo, true)
	if !hasDocLeakFailContaining(findings, rel+":", realEmail) {
		t.Fatalf("valid UTF-8 containing NUL escaped the tracked-text scan: %+v", findings)
	}
}

func TestDoctorDocLeakScansTrackedUTF16LEWithBOM(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	realEmail := "real" + "person@gm" + "ail.com"
	rel := "utf16le.txt"
	body := []byte{0xff, 0xfe}
	for _, r := range "contact: " + realEmail + "\n" {
		body = append(body, byte(r), byte(r>>8))
	}
	if err := os.WriteFile(filepath.Join(repo, rel), body, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "--", rel)

	findings := doctorDocLeakFindings(repo, true)
	if !hasDocLeakFailContaining(findings, rel+":", realEmail) {
		t.Fatalf("BOM-marked UTF-16LE escaped the tracked-text scan: %+v", findings)
	}
}

func TestDoctorDocLeakScansTrackedUTF16BEWithBOM(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	realEmail := "real" + "person@gm" + "ail.com"
	rel := "utf16be.txt"
	body := []byte{0xfe, 0xff}
	for _, r := range "contact: " + realEmail + "\n" {
		body = append(body, byte(r>>8), byte(r))
	}
	if err := os.WriteFile(filepath.Join(repo, rel), body, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "--", rel)

	findings := doctorDocLeakFindings(repo, true)
	if !hasDocLeakFailContaining(findings, rel+":", realEmail) {
		t.Fatalf("BOM-marked UTF-16BE escaped the tracked-text scan: %+v", findings)
	}
}

func TestCheckDocLeakTokenPassesUseTrackedTextBlobs(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	mustWriteArchFixture(t, repo, filepath.Join("docs_private", "leak-tokens.txt"), "docs-only-token\n")
	mustWriteArchFixture(t, repo, filepath.Join("docs_private", "source-leak-tokens.txt"), "source-only-token\n")
	docRel := filepath.Join("docs", "guide.rst")
	sourceRel := filepath.Join("internal", "x", "schema.proto")
	binaryRel := filepath.Join("docs", "binary.rst")
	mustWriteArchFixture(t, repo, docRel, "docs-only-token\n")
	mustWriteArchFixture(t, repo, sourceRel, "source-only-token\n")
	binaryBody := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0}, []byte("docs-only-token\n")...)
	mustWriteArchFixture(t, repo, binaryRel, string(binaryBody))
	runGit("add", "--", docRel, sourceRel, binaryRel)

	findings := checkDocLeak(repo, true)
	if !hasDocLeakFailContaining(findings, filepath.ToSlash(docRel)+":", "docs-only-token") {
		t.Fatalf("docs token pass skipped an extension-independent text blob: %+v", findings)
	}
	if !hasDocLeakFailContaining(findings, filepath.ToSlash(sourceRel)+":", "source-only-token") {
		t.Fatalf("source token pass skipped an extension-independent text blob: %+v", findings)
	}
	if hasDocLeakFailContaining(findings, filepath.ToSlash(binaryRel)+":") {
		t.Fatalf("token pass must skip binary blobs: %+v", findings)
	}
}

func TestCheckDocLeakPlainDirectoryFallbackScansTextContent(t *testing.T) {
	repo := t.TempDir()
	realEmail := "real" + "person@gm" + "ail.com"
	mustWriteArchFixture(t, repo, "NOTICE", "contact: "+realEmail+"\n")

	findings := checkDocLeak(repo, true)
	if !hasDocLeakFailContaining(findings, "NOTICE:") {
		t.Fatalf("plain-directory fallback skipped extensionless text: %+v", findings)
	}
}

func TestCheckDocLeakFailsClosedWhenGitEnumerationFails(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	mustWriteArchFixture(t, repo, "README.md", "clean\n")
	runGit("add", "README.md")
	if err := os.WriteFile(filepath.Join(repo, ".git", "index"), []byte("corrupt-index"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkDocLeak(repo, true)
	if !hasDocLeakFailContaining(findings, "tracked-file enumeration") {
		t.Fatalf("git enumeration failure must fail closed: %+v", findings)
	}
}

func TestCheckDocLeakFailsClosedWhenTrackedPathIsMissing(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	mustWriteArchFixture(t, repo, "README.md", "clean\n")
	runGit("add", "README.md")
	if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
		t.Fatal(err)
	}

	findings := checkDocLeak(repo, true)
	if !hasDocLeakFailContaining(findings, "README.md", "cannot read tracked text") {
		t.Fatalf("missing tracked path must fail closed: %+v", findings)
	}
}

func TestCheckDocLeakScansTrackedSymlinkTargetWithoutFollowing(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	linkRel := "private-path.md"
	expectedLeak := "/Us" + "ers/real" + "person"
	realTarget := expectedLeak + "/private.md"
	if err := os.Symlink(realTarget, filepath.Join(repo, linkRel)); err != nil {
		t.Fatal(err)
	}
	runGit("add", linkRel)

	findings := checkDocLeak(repo, true)
	if !hasDocLeakFailContaining(findings, linkRel+":", expectedLeak) {
		t.Fatalf("tracked symlink target string escaped the scan: %+v", findings)
	}
}

func TestCheckDocLeakCleanNoOverlayReportsWidenedCoverage(t *testing.T) {
	repo, runGit := newDocLeakGitRepo(t)
	mustWriteArchFixture(t, repo, "README.md", "clean\n")
	runGit("add", "README.md")

	findings := checkDocLeak(repo, true)
	for _, finding := range findings {
		if finding.Check == "doc-leak" && finding.Severity == Info &&
			strings.Contains(finding.Message, "all tracked text blobs") &&
			strings.Contains(finding.Message, "root + docs + skills + source") {
			return
		}
	}
	t.Fatalf("clean fresh clone did not report widened tracked-text coverage: %+v", findings)
}

// TestPrivateSourceBodyContractCatchesPlantedBodyAndClearsWhenRemoved is J16's
// falsifier. checkDocLeak's generic identity pass covers every tracked text file (root
// files, docs, skills, and every source root — see checkDocLeak), and its two TOKEN
// passes only match an operator-configured literal list — neither ever reaches a
// semantic "this body came from brain" marker, nor a brain/operating-system path SHAPE
// landing in internal/cmd/fixtures/tools/console. This guard closes both gaps. The
// planted fixture uses a synthetic username (never the operator's real one) since it is
// written to a plain fixture file, not a _test.go file — the guard exempts _test.go from
// both checks so its own test source can safely describe the sentinel shapes without
// ever being live evidence against the real repo.
func TestPrivateSourceBodyContractCatchesPlantedBodyAndClearsWhenRemoved(t *testing.T) {
	repo := t.TempDir()
	srcDir := filepath.Join(repo, "internal", "planted")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plantPath := filepath.Join(srcDir, "body.md")
	planted := "source_of_truth: brain\n/Users/faketester/brain/private-note.md\n"
	if err := os.WriteFile(plantPath, []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkPrivateSourceBodyContract(repo)
	var failCount int
	for _, f := range findings {
		if f.Check == "private-source-body" && f.Severity == Fail {
			failCount++
		}
	}
	if failCount != 2 {
		t.Fatalf("expected 2 failures (source_of_truth marker + brain path prefix), got %+v", findings)
	}

	if err := os.Remove(plantPath); err != nil {
		t.Fatal(err)
	}
	clean := checkPrivateSourceBodyContract(repo)
	for _, f := range clean {
		if f.Severity == Fail {
			t.Fatalf("expected green after removing the plant, got %+v", clean)
		}
	}
}

// TestPrivateSourceBodyContractExemptsTestFiles proves the _test.go exemption
// is real: the same sentinel content in a _test.go file must not fail, since
// this guard's own tests need to describe/plant these shapes safely.
func TestPrivateSourceBodyContractExemptsTestFiles(t *testing.T) {
	repo := t.TempDir()
	srcDir := filepath.Join(repo, "internal", "planted")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plantPath := filepath.Join(srcDir, "fixture_test.go")
	planted := "source_of_truth: brain\n/Users/faketester/operating-system/notes.md\n"
	if err := os.WriteFile(plantPath, []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkPrivateSourceBodyContract(repo)
	for _, f := range findings {
		if f.Severity == Fail {
			t.Fatalf("expected _test.go files to be exempt, got %+v", findings)
		}
	}
}

// TestScanPrivateSourceBodySignatures is the pure (no-git) unit falsifier for the
// J16 signature set: each new marker/path shape must produce a Fail, and the
// deliberately-excluded shapes (compound's own public ~/.compound home, bare
// ~/brain pointer references, compound.gbrain_db_export schema naming) must NOT —
// precision over recall, so the doctor stays green on legitimate content.
func TestScanPrivateSourceBodySignatures(t *testing.T) {
	fails := func(content string) int {
		n := 0
		for _, f := range scanPrivateSourceBody("x.md", []byte(content)) {
			if f.Severity == Fail {
				n++
			}
		}
		return n
	}
	// MUST flag (copied-body signatures across brain / operating-system / gbrain / infra).
	mustFail := map[string]int{
		"source_of_truth: brain\n":                     1,
		"source_of_truth: operating-system\n":          1,
		"source_of_truth: decisions\n":                 1,
		"source_of_truth: infra\n":                     1,
		"  source_of_truth:   gbrain\n":                1,
		"/Users/faketester/brain/note.md\n":            1,
		"/Users/faketester/operating-system/dec.md\n":  1,
		"/Users/faketester/.gbrain/corpus.jsonl\n":     1,
		"ref ~/.gbrain/export.json here\n":             1,
		"see ~/Projects/operating-system/decisions/\n": 1,
	}
	for content, want := range mustFail {
		if got := fails(content); got != want {
			t.Errorf("scanPrivateSourceBody(%q) fails=%d, want %d", content, got, want)
		}
	}
	// MUST NOT flag (legitimate public references — the precision line).
	mustPass := []string{
		"COMPOUND_HOME defaults to ~/.compound\n",         // compound's own public home
		"private ids under ~/.compound/bench-suites/\n",   // ditto, subpath
		"const V = \"compound.gbrain_db_export.v0.1\"\n",  // compound's own gbrain-export schema, not a ~/.gbrain path
		"carry the source_of_truth handoff into memory\n", // mid-prose mention, not a frontmatter marker
		"meaning_store path is ~/brain for the fixture\n", // bare ~/brain pointer, deliberately not matched
	}
	for _, content := range mustPass {
		if got := fails(content); got != 0 {
			t.Errorf("scanPrivateSourceBody(%q) fails=%d, want 0 (false positive)", content, got)
		}
	}
}

// TestPrivateSourceBodyContractScopesToTrackedFiles is J16's end-to-end
// falsifier: a body-copy planted in a TRACKED file makes the doctor red, the
// SAME shape left UNTRACKED stays invisible (proving the scope is git ls-files,
// not the worktree), and removing the tracked file returns green.
func TestPrivateSourceBodyContractScopesToTrackedFiles(t *testing.T) {
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "user.name", "test")

	srcDir := filepath.Join(repo, "internal", "absorb")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	trackedRel := filepath.Join("internal", "absorb", "pasted.md")
	body := "source_of_truth: brain\n/Users/faketester/.gbrain/corpus.md\nref ~/.gbrain/export.json\n"
	if err := os.WriteFile(filepath.Join(repo, trackedRel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", trackedRel)

	// Same violation shape, but NEVER git-added: an untracked private body is
	// local scratch, not a public leak, and must stay invisible to the guard.
	untrackedRel := filepath.Join("internal", "absorb", "untracked.md")
	if err := os.WriteFile(filepath.Join(repo, untrackedRel), []byte("source_of_truth: brain\n/Users/faketester/.gbrain/x.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var fails []Finding
	for _, f := range checkPrivateSourceBodyContract(repo) {
		if f.Severity == Fail {
			fails = append(fails, f)
		}
	}
	if len(fails) == 0 {
		t.Fatalf("expected the tracked body-copy to fail the doctor, got green")
	}
	for _, f := range fails {
		if !strings.Contains(f.Message, "pasted.md") {
			t.Fatalf("a failure named a file other than the tracked plant (scope leak): %+v", f)
		}
		if strings.Contains(f.Message, "untracked.md") {
			t.Fatalf("untracked file was flagged — scope is not tracked-only: %+v", f)
		}
	}

	// Remove the tracked plant → green, even though the untracked violation
	// still sits in the worktree (proves git ls-files scope, not a walk).
	runGit("rm", "-f", trackedRel)
	for _, f := range checkPrivateSourceBodyContract(repo) {
		if f.Severity == Fail {
			t.Fatalf("expected green after removing the tracked plant, got %+v", f)
		}
	}
}

func TestCandidateMapInvariantPure(t *testing.T) {
	r := DoctorSource("", false)
	for _, f := range r.Findings {
		if f.Severity == Fail {
			t.Errorf("pure candidate-map FAIL: %s", f.Message)
		}
	}
}

// Every internal/ package the doctor sees is classified — i.e. classification is total. (Belt to
// the real-repo test's suspenders: assert the Info line, not just absence of Fail.)
func TestClassificationIsTotal(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var classified bool
	for _, f := range r.Findings {
		if f.Check == "classification" && f.Severity == Fail {
			t.Errorf("unclassified package: %s", f.Message)
		}
		if f.Check == "classification" && f.Severity == Info && strings.Contains(f.Message, "classified") {
			classified = true
		}
	}
	if !classified {
		t.Error("expected a 'all packages classified' info finding")
	}
}

// Spine is one envelope grammar plus object families the spine processes (LAYERS.md), NOT every
// package. If everything were spine, agents would treat everything as equally authoritative. This
// pins the split: exactly the ten spine packages, and the ten reclassified object families, so a
// silent re-widening of the spine fails here.
func TestSpineAndObjectFamilySplit(t *testing.T) {
	wantSpine := []string{"arch", "contract", "envelope", "harness", "home", "hook", "policy", "schema", "work", "writeback"}
	for _, pkg := range wantSpine {
		if Classification[pkg] != Spine {
			t.Errorf("%q must be classified Spine, got %q", pkg, Classification[pkg])
		}
		if !spineCore[pkg] {
			t.Errorf("%q must be in the spineCore allowlist", pkg)
		}
	}
	wantObjectFamily := []string{"capgraph", "director", "distiller", "localmodel", "observation", "panel", "product", "projection", "routeevidence", "runtimebinding"}
	for _, pkg := range wantObjectFamily {
		if Classification[pkg] != ObjectFamily {
			t.Errorf("%q must be classified ObjectFamily (a family the spine processes), got %q", pkg, Classification[pkg])
		}
		if spineCore[pkg] {
			t.Errorf("%q is an object family and must NOT be in the spineCore allowlist", pkg)
		}
	}
	var spineCount int
	for _, c := range Classification {
		if c == Spine {
			spineCount++
		}
	}
	if spineCount != len(wantSpine) {
		t.Errorf("Classification has %d Spine packages, want exactly %d (the envelope grammar); new packages are object-family/leaf/importer", spineCount, len(wantSpine))
	}
	if len(spineCore) != len(wantSpine) {
		t.Errorf("spineCore has %d entries, want exactly %d", len(spineCore), len(wantSpine))
	}
}

// The spine allowlist is a ratchet: spineCore and the Spine classification MUST name the same
// packages. Adding a package to spineCore without classifying it Spine (or classifying a package
// Spine without an allowlist entry) is drift and must FAIL the doctor. This exercises both
// directions against synthetic maps plus lockstep against the real maps.
func TestSpineCoreAllowlistRatchetCatchesDrift(t *testing.T) {
	if findings := checkSpineCoreDrift(Classification, spineCore); len(findings) != 0 {
		t.Fatalf("real spineCore and Classification must be in lockstep, got drift %+v", findings)
	}

	// Direction 1: a package is classified Spine but never allowlisted.
	findings := checkSpineCoreDrift(map[string]Class{"newpkg": Spine, "home": Spine}, map[string]bool{"home": true})
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one spine-allowlist failure", findings)
	}
	if !strings.Contains(findings[0].Message, "spineCore allowlist") ||
		!strings.Contains(findings[0].Message, "object-family/leaf/importer") {
		t.Fatalf("failure should point the author at the allowlist and object-family classes: %+v", findings)
	}

	// Direction 2: a spineCore entry is not classified Spine (map drift).
	findings = checkSpineCoreDrift(map[string]Class{"ghostcore": ObjectFamily}, map[string]bool{"ghostcore": true})
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one map-drift failure", findings)
	}
	if !strings.Contains(findings[0].Message, "map drift") || !strings.Contains(findings[0].Message, "lockstep") {
		t.Fatalf("failure should name the map drift and lockstep requirement: %+v", findings)
	}
}

// docs/architecture/status.md's package table must be SINGLE-SOURCED from arch.Classification (the
// package doc comment's own promise: "the package Classification below is the ENFORCED source of
// truth; docs/architecture/status.md narrates it"). This locks the mechanical side of that promise
// against three ways narration can drift from the map: a classified package missing from the table,
// a table row whose Class text is not the exact Class value (a paraphrase like "canonical" is not a
// substitute), and a table row naming a package the map no longer classifies (stale/retired).
func TestPackageClassificationTableMatchesArchClassification(t *testing.T) {
	// The real repo table must already match (this is the fix this test locks in).
	if findings := checkPackageClassificationTableContract(repoRoot); len(findings) != 1 || findings[0].Severity != Info {
		t.Fatalf("real status.md package table must match arch.Classification exactly, got %+v", findings)
	}

	writeStatus := func(t *testing.T, body string) string {
		t.Helper()
		repo := t.TempDir()
		statusDir := filepath.Join(repo, "docs", "architecture")
		if err := os.MkdirAll(statusDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(statusDir, "status.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return repo
	}

	classification := map[string]Class{
		"home": Spine,
		"mem":  MemTarget,
		"act":  Leaf,
	}
	restore := Classification
	Classification = classification
	defer func() { Classification = restore }()

	// A table naming every classified package with the exact Class string must pass clean.
	clean := "## Packages (`internal/`)\n\n| Package | Class | Role |\n|---|---|---|\n" +
		"| `home` | spine | runtime spine |\n" +
		"| `mem` | mem-target | memory |\n" +
		"| `act` | leaf | outbound seam |\n\n## Commands\n"
	if findings := checkPackageClassificationTableContract(writeStatus(t, clean)); len(findings) != 1 || findings[0].Severity != Info {
		t.Fatalf("exact-match table should pass clean, got %+v", findings)
	}

	// Missing package + stale "canonical" class text (the real-world drift this test was written
	// for) + an extra package the map no longer classifies, all in one row set.
	stale := "## Packages (`internal/`)\n\n| Package | Class | Role |\n|---|---|---|\n" +
		"| `home` | canonical | runtime spine |\n" +
		"| `act` | leaf | outbound seam |\n" +
		"| `retired` | leaf | gone |\n\n## Commands\n"
	findings := checkPackageClassificationTableContract(writeStatus(t, stale))
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("stale table should fail, got %+v", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{
		`internal/home is classified "spine" in arch.Classification but status.md's table says "canonical"`,
		"status.md package table is missing 1 classified package(s): mem",
		"status.md package table names internal/retired which arch.Classification does not classify",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should report %q, got: %s", want, msg)
		}
	}

	// A package listed on more than one row is flagged too.
	duplicate := "## Packages (`internal/`)\n\n| Package | Class | Role |\n|---|---|---|\n" +
		"| `home` | spine | runtime spine |\n" +
		"| `home` | spine | duplicate row |\n" +
		"| `mem` | mem-target | memory |\n" +
		"| `act` | leaf | outbound seam |\n\n## Commands\n"
	findings = checkPackageClassificationTableContract(writeStatus(t, duplicate))
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("duplicate-row table should fail, got %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "internal/home appears on more than one status.md package-table row") {
		t.Fatalf("failure should name the duplicated package: %+v", findings[0])
	}
}

func TestTransitionFieldContractMatchesEnvelopeStruct(t *testing.T) {
	// The live documentation must enumerate the exact direct fields, Go types, and JSON tags from
	// envelope.Transition. This deliberately reads source rather than importing envelope so the
	// architecture doctor remains a filesystem auditor.
	if findings := checkTransitionFieldContract(repoRoot); len(findings) != 1 || findings[0].Severity != Info {
		t.Fatalf("real transition.md field contract must match envelope.Transition, got %+v", findings)
	}

	writeFixture := func(t *testing.T, doc string) string {
		t.Helper()
		repo := t.TempDir()
		envelopeDir := filepath.Join(repo, "internal", "envelope")
		docDir := filepath.Join(repo, "docs", "architecture")
		if err := os.MkdirAll(envelopeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(docDir, 0o755); err != nil {
			t.Fatal(err)
		}
		source := "package envelope\n\n" +
			"type Header struct{}\n" +
			"type TransitionCause struct{}\n" +
			"type Transition struct {\n" +
			"\tHeader\n" +
			"\tBeforeRefs []string `json:\"before_refs\"`\n" +
			"\tCause *TransitionCause `json:\"cause,omitempty\"`\n" +
			"}\n"
		if err := os.WriteFile(filepath.Join(envelopeDir, "envelope.go"), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(docDir, "transition.md"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		return repo
	}

	clean := "| Go field | Go type | JSON tag |\n" +
		"|---|---|---|\n" +
		"| `Header` | `Header` | `-` |\n" +
		"| `BeforeRefs` | `[]string` | `json:\"before_refs\"` |\n" +
		"| `Cause` | `*TransitionCause` | `json:\"cause,omitempty\"` |\n"
	if findings := checkTransitionFieldContract(writeFixture(t, clean)); len(findings) != 1 || findings[0].Severity != Info {
		t.Fatalf("exact transition field contract should pass, got %+v", findings)
	}

	stale := "| Go field | Go type | JSON tag |\n" +
		"|---|---|---|\n" +
		"| `Header` | `UniversalHeader` | `-` |\n" +
		"| `BeforeRefs` | `[]string` | `json:\"before\"` |\n" +
		"| `Ghost` | `string` | `json:\"ghost\"` |\n"
	findings := checkTransitionFieldContract(writeFixture(t, stale))
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("drifted transition field contract should fail, got %+v", findings)
	}
	for _, want := range []string{
		"field Header has Go type",
		"field BeforeRefs has JSON tag",
		"docs missing envelope.Transition field Cause",
		"documents non-existent envelope.Transition field Ghost",
	} {
		if !strings.Contains(findings[0].Message, want) {
			t.Fatalf("failure should report %q, got %s", want, findings[0].Message)
		}
	}
}

// The convergence debt is reported (observable), not hidden — the coherent treatment of a duplicate
// that cannot yet be retired.
func TestStatusHotPathFailsAboveLineBudget(t *testing.T) {
	repo := t.TempDir()
	statusDir := filepath.Join(repo, "docs", "architecture")
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < MaxStatusHotPathNarrationLines+1; i++ {
		b.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(statusDir, "status.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkStatusHotPath(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one status-hot-path failure", findings)
	}
	if !strings.Contains(findings[0].Message, "docs/architecture/status.md") {
		t.Fatalf("failure should name status.md: %+v", findings[0])
	}
}

// writeStatusDoc lays a status.md down in a temp repo and returns the repo root.
func writeStatusDoc(t *testing.T, lines []string) string {
	t.Helper()
	repo := t.TempDir()
	statusDir := filepath.Join(repo, "docs", "architecture")
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(statusDir, "status.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// packageTableRows is the parity table checkPackageTableSingleSource mandates — one row per
// classified package, so this section's size is not discretionary.
func packageTableRows(n int) []string {
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, "| `pkg"+itoa(i)+"` | leaf | a classified package that must carry a parity row |")
	}
	return rows
}

// The regression that sent the ADR-0005 `persist` lane red on 2026-08-01: minting an organ REQUIRES
// a package-table row, so a budget that counts those rows fails the lane for doing the right thing.
// Narration sits exactly at budget here and the mandated rows push the file far past the old
// 250-line total cap — this must still pass, or the doctor is once again punishing correct minting.
func TestStatusHotPathIgnoresMandatedPackageRows(t *testing.T) {
	lines := []string{"# Package & Command Status", "", "## Packages (`internal/`)"}
	lines = append(lines, packageTableRows(120)...)
	for len(lines)-120 < MaxStatusHotPathNarrationLines {
		lines = append(lines, "narration that the budget does bound")
	}
	findings := checkStatusHotPath(writeStatusDoc(t, lines))
	if len(findings) != 1 || findings[0].Severity != Info {
		t.Fatalf("findings = %+v, want one status-hot-path Info: mandated package rows must not consume the narration budget", findings)
	}
	if !strings.Contains(findings[0].Message, "120 mandated package-table rows excluded") {
		t.Fatalf("Info should report the excluded row count: %+v", findings[0])
	}
}

// The exclusion must not become a loophole: rows buy no prose headroom whatsoever. One narration
// line past budget fails no matter how large the mandated table is.
func TestStatusHotPathPackageRowsCannotBuyProseHeadroom(t *testing.T) {
	lines := []string{"## Packages (`internal/`)"}
	lines = append(lines, packageTableRows(200)...)
	for len(lines)-200 < MaxStatusHotPathNarrationLines+1 {
		lines = append(lines, "narration that the budget does bound")
	}
	findings := checkStatusHotPath(writeStatusDoc(t, lines))
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want a Fail: a big parity table must not raise the prose budget", findings)
	}
	if !strings.Contains(findings[0].Message, "never raise the budget to fit") {
		t.Fatalf("failure should refuse the weaken-the-gate move: %+v", findings[0])
	}
}

// Narration cannot be smuggled past the ratchet by shaping it like a table row: only rows INSIDE the
// `## Packages` section are mandated, so identical rows under `## Commands` still count as prose.
func TestStatusHotPathCountsTableShapedProseOutsidePackages(t *testing.T) {
	lines := []string{"## Commands"}
	lines = append(lines, packageTableRows(MaxStatusHotPathNarrationLines)...)
	findings := checkStatusHotPath(writeStatusDoc(t, lines))
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want a Fail: table-shaped prose outside ## Packages is still narration", findings)
	}
}

func TestStatusHotPathFailsOnProofJournalEntries(t *testing.T) {
	repo := t.TempDir()
	statusDir := filepath.Join(repo, "docs", "architecture")
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Package & Command Status\n\n- 2026-07-05: go test ./... passed for latest proof run\n"
	if err := os.WriteFile(filepath.Join(statusDir, "status.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkStatusHotPath(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one status proof-journal failure", findings)
	}
	if !strings.Contains(findings[0].Message, "proof-journal") {
		t.Fatalf("failure should name proof-journal drift: %+v", findings[0])
	}
}

func TestConvergenceDebtIsObservable(t *testing.T) {
	r := DoctorSource("", false)
	for _, f := range r.Findings {
		if f.Check == "convergence" {
			return // present
		}
	}
	t.Error("convergence debt must be reported as a finding")
}

// The ratchet is honest: --strict does NOT fail now (debt is at budget, not over). If a future
// change adds a converge→retire package the count exceeds the budget and --strict fails; when a
// package converges, lower ConvergePendingBudget to lock the gain.
func TestConvergenceRatchetHoldsAtBudget(t *testing.T) {
	r := DoctorSource(repoRoot, true)
	for _, f := range r.Findings {
		if f.Check == "convergence" && f.Severity == Fail {
			t.Errorf("strict convergence FAIL (debt grew past budget %d): %s", ConvergePendingBudget, f.Message)
		}
	}
}

func TestRetiredSurfaceReferencesResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "retired-surface" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("retired surface check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected retired-surface finding")
	}
}

func TestRetiredSurfaceReferencesFailOnPendingSkillMatrix(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "roadmap.md"), []byte("pending old skill matrix\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkRetiredSurfaceReferences(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
}

func TestRetiredSurfaceReferencesRequireToolResolverRetirement(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "status.md"), []byte("capability graph replaced old matrix debt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkRetiredSurfaceReferences(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "tool-resolver") {
		t.Fatalf("failure should name tool-resolver retirement: %+v", findings)
	}

	text := "os-tool-resolver-json source_view_authority retired_as_authority to compound://registries/capability-graph; source remains evidence_only_input.\n"
	if err := os.WriteFile(filepath.Join(docs, "status.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings = checkRetiredSurfaceReferences(repo)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("findings = %+v, want non-failure", findings)
	}
}

// TestRetiredGen1PackagesStayDeadAgainstRealRepo proves the real worktree is clean: none of the
// nine Gen-1 names docs/architecture/status.md declares retired exist as internal/ directories.
func TestRetiredGen1PackagesStayDeadAgainstRealRepo(t *testing.T) {
	findings := checkRetiredGen1PackagesStayDead(repoRoot)
	var saw bool
	for _, f := range findings {
		if f.Check != "retired-gen1" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("retired-gen1 check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected a retired-gen1 finding")
	}
}

// TestRetiredGen1PackagesStayDeadFailsOnResurrection proves the check has teeth: resurrecting any
// one of the nine retired names as an internal/ directory reds the doctor, and removing it again
// goes back to green. This is the scratch-mutation proof the packet calls for since arch tests
// exercise real package names, not a synthetic package-tree.
func TestRetiredGen1PackagesStayDeadFailsOnResurrection(t *testing.T) {
	repo := t.TempDir()
	internalDir := filepath.Join(repo, "internal")
	if err := os.MkdirAll(internalDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Green: no retired package directories present.
	findings := checkRetiredGen1PackagesStayDead(repo)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("findings = %+v, want one non-failure finding on a clean tree", findings)
	}

	// Red: resurrect internal/recall.
	if err := os.MkdirAll(filepath.Join(internalDir, "recall"), 0o755); err != nil {
		t.Fatal(err)
	}
	findings = checkRetiredGen1PackagesStayDead(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want exactly one failure for resurrected internal/recall", findings)
	}
	if !strings.Contains(findings[0].Message, "internal/recall") {
		t.Fatalf("failure should name internal/recall: %+v", findings)
	}

	// Green again: remove the scratch mutation.
	if err := os.RemoveAll(filepath.Join(internalDir, "recall")); err != nil {
		t.Fatal(err)
	}
	findings = checkRetiredGen1PackagesStayDead(repo)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("findings = %+v, want one non-failure finding after removing the scratch mutation", findings)
	}
}

// TestRetiredGen1PackagesStayDeadFailsOnAllNames proves every one of the nine names is checked,
// not just the first, by resurrecting all of them at once and counting one failure per name.
func TestRetiredGen1PackagesStayDeadFailsOnAllNames(t *testing.T) {
	repo := t.TempDir()
	internalDir := filepath.Join(repo, "internal")
	for _, pkg := range retiredGen1Packages {
		if err := os.MkdirAll(filepath.Join(internalDir, pkg), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	findings := checkRetiredGen1PackagesStayDead(repo)
	if len(findings) != len(retiredGen1Packages) {
		t.Fatalf("findings = %+v, want exactly %d failures (one per retired name)", findings, len(retiredGen1Packages))
	}
	for _, f := range findings {
		if f.Severity != Fail {
			t.Fatalf("finding %+v should be Fail", f)
		}
	}
}

func TestPrivateAdaptationSteeringResolved(t *testing.T) {
	findings := checkPrivateAdaptationSteering(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("adaptation steering check failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "context-aware OS adaptation") {
		t.Fatalf("finding should name consumed adaptation steering: %+v", findings)
	}
}

func TestPrivateAdaptationSteeringFailsOnShallowRule(t *testing.T) {
	repo := writeAdaptationSteeringFixtureRepo(t,
		"# COMPOUND Goal Prompt\n\nMove everything from operating-system into Compound.\n",
		"# Operating-System Adaptation Rule\n\nCopy useful instructions.\n",
		"# Superseded Goal Prompt Pointer\n\nThis is not a second goal prompt. Use codex-goal-prompt.md. Superseded.\n",
	)
	findings := checkPrivateAdaptationSteering(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{"context-aware adaptation", "silence condition", "proof", "retirement"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should name missing %q: %+v", want, findings)
		}
	}
}

func TestPrivateAdaptationSteeringFailsOnDuplicatePromptBody(t *testing.T) {
	repo := writeAdaptationSteeringFixtureRepo(t,
		goodAdaptationGoalPromptFixture(),
		goodAdaptationRuleFixture(),
		"# COMPOUND Goal Prompt\n\n## Goal Lanes\n\nThis stale file is pretending to be active again.\n",
	)
	findings := checkPrivateAdaptationSteering(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "second active goal prompt") || !strings.Contains(findings[0].Message, "pointer-only") {
		t.Fatalf("failure should name duplicate prompt and pointer-only contract: %+v", findings)
	}
}

func TestPrivateAdaptationSteeringFailsOnStalePromptHistory(t *testing.T) {
	repo := writeAdaptationSteeringFixtureRepo(t,
		goodAdaptationGoalPromptFixture()+"\n\n## Latest Verified State (old)\nold state\n\n## Latest Verified State (older)\nolder state\n",
		goodAdaptationRuleFixture(),
		"# Superseded Goal Prompt Pointer\n\nThis not second goal prompt. Use codex-goal-prompt.md. Superseded.\n",
	)
	findings := checkPrivateAdaptationSteering(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "too many Latest Verified State") {
		t.Fatalf("failure should name stale prompt history: %+v", findings)
	}
}

func TestWorkbenchActionCardContractResolved(t *testing.T) {
	findings := checkWorkbenchActionCardContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("workbench action card contract failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "required inputs validation stop conditions") {
		t.Fatalf("finding should name action-card gates: %+v", findings)
	}
}

func TestWorkProofChainContractResolved(t *testing.T) {
	findings := checkWorkProofChainContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("work proof-chain contract failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "receipt/transition") {
		t.Fatalf("finding should describe terminal receipt/transition proof: %+v", findings)
	}
}

func TestGoStructFieldContractIgnoresFormattingButRejectsWrongTypeAndTag(t *testing.T) {
	expected := map[string]goStructFieldContract{
		"ReviewCards":  {Type: "[]WorkbenchReviewCard", Tag: `json:"review_cards,omitempty"`},
		"RunArtifacts": {Type: "[]WorkbenchRunArtifact", Tag: `json:"run_artifacts,omitempty"`},
	}
	text := `package fixture

type WorkbenchItem struct {
	ReviewCards []WorkbenchReviewCard ` + "`json:\"review_cards,omitempty\"`" + `
	AnUnrelatedMuchLongerSiblingField string ` + "`json:\"unrelated,omitempty\"`" + `
	RunArtifacts []WorkbenchRunArtifact ` + "`json:\"run_artifacts,omitempty\"`" + `
}
`
	if missing := missingGoStructFields("workbench.go", text, "WorkbenchItem", expected); len(missing) != 0 {
		t.Fatalf("gofmt-independent fields reported missing: %v", missing)
	}
	wrong := strings.Replace(text, "[]WorkbenchRunArtifact", "WorkbenchRunArtifact", 1)
	missing := missingGoStructFields("workbench.go", wrong, "WorkbenchItem", expected)
	if len(missing) != 1 || !strings.Contains(missing[0], "RunArtifacts []WorkbenchRunArtifact") {
		t.Fatalf("wrong field type must fail exactly: %v", missing)
	}
	wrongTag := strings.Replace(text, `json:"review_cards,omitempty"`, `json:"review_cards"`, 1)
	missing = missingGoStructFields("workbench.go", wrongTag, "WorkbenchItem", expected)
	if len(missing) != 1 || !strings.Contains(missing[0], `json:"review_cards,omitempty"`) {
		t.Fatalf("wrong field tag must fail exactly: %v", missing)
	}
}

func TestWorkbenchActionCardContractFailsOnShallowSurface(t *testing.T) {
	repo := writeWorkbenchActionCardFixtureRepo(t, map[string]string{
		"internal/work/workbench.go":      "type WorkbenchAdjacentAction struct { ID string; RequiresOperator bool }\n",
		"internal/work/workbench_test.go": "func TestAction(t *testing.T) { _ = \"operator required\" }\n",
		"cmd/compound/main.go":            "func render() { println(\"adjacent_non_packet_action\") }\n",
		"cmd/compound/main_test.go":       "func TestRender(t *testing.T) { _ = \"adjacent_non_packet_action\" }\n",
		"docs/architecture/status.md":     "Workbench shows operator-required adjacent actions.\n",
	})
	findings := checkWorkbenchActionCardContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{"required_inputs", "action_expected_validation", "stop_conditions"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should name missing %q: %+v", want, findings)
		}
	}
}

func TestConversationPreflightImportSafetyContractResolved(t *testing.T) {
	findings := checkConversationPreflightImportSafetyContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("conversation preflight import-safety contract failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "import_safety") {
		t.Fatalf("finding should name import_safety proof: %+v", findings)
	}
}

func TestConversationPreflightImportSafetyContractFailsOnShallowReport(t *testing.T) {
	repo := writeWorkbenchActionCardFixtureRepo(t, map[string]string{
		"tools/chatgpt_conversation_preflight.ts":      "type PreflightReport = { status: \"ok\" | \"fail\" };\n",
		"tools/chatgpt_conversation_preflight.test.ts": "test(\"preflight\", () => { expect(report.status).toBe(\"ok\"); });\n",
		"internal/work/workbench.go":                   "func action() { _ = \"conversation-preflight-private-export\" }\n",
		"docs/architecture/status.md":                  "ChatGPT capture edge note: conversation preflight exists.\n",
	})
	findings := checkConversationPreflightImportSafetyContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{
		"ImportSafetyReport",
		"report.import_safety.private_evidence_count",
		"import_safety proves private bounded evidence/candidates and zero temp promoted claims",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should name missing %q: %+v", want, findings)
		}
	}
}

func TestOperatorNeedsMeCandidateAuthorityContractResolved(t *testing.T) {
	findings := checkOperatorNeedsMeCandidateAuthorityContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("operator needs-me candidate authority contract failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "memory candidate authority") {
		t.Fatalf("finding should name memory candidate authority: %+v", findings)
	}
}

func TestOperatorNeedsMeCandidateAuthorityContractFailsOnRawMemoryCounts(t *testing.T) {
	repo := writeWorkbenchActionCardFixtureRepo(t, map[string]string{
		"cmd/compound/orient_operator.go":      "func candidateInboxSummary() { _ = \"raw inbox proposed\" }\n",
		"cmd/compound/orient_operator_test.go": "func TestOrientOperator(t *testing.T) { _ = \"candidate_actionable\" }\n",
		"docs/architecture/status.md":          "Operator orient lists candidate counts.\n",
	})
	findings := checkOperatorNeedsMeCandidateAuthorityContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{
		"memoryCandidateInboxSummary",
		"TestOrientOperatorUsesMemoryCandidateAuthorityForNeedsMe",
		"Operator needs-me candidate authority note",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should name missing %q: %+v", want, findings)
		}
	}
}

func TestOperatorOrientAdjacentProgressContractResolved(t *testing.T) {
	findings := checkOperatorOrientAdjacentProgressContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("operator orient adjacent progress contract failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "held-valve adjacent progress") {
		t.Fatalf("finding should name held-valve adjacent progress: %+v", findings)
	}
}

func TestOperatorOrientAdjacentProgressContractFailsOnShallowSurface(t *testing.T) {
	repo := writeWorkbenchActionCardFixtureRepo(t, map[string]string{
		"cmd/compound/orient_operator.go":      "type orientRunning struct { OpenCount int }\n",
		"cmd/compound/orient_operator_test.go": "func TestOrientOperator(t *testing.T) { _ = \"open\" }\n",
		"docs/architecture/status.md":          "Operator orientation lists open work.\n",
	})
	findings := checkOperatorOrientAdjacentProgressContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{"adjacent_progress", "session_context", "TestOrientOperatorSurfacesHeldValveAdjacentProgress", "Operator orientation adjacent-progress doctor note"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should name missing %q: %+v", want, findings)
		}
	}
}

func TestOperatorSourceControlProjectionContractResolved(t *testing.T) {
	findings := checkOperatorSourceControlProjectionContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("operator source-control projection contract failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "source-control cleanup reviews") {
		t.Fatalf("finding should name source-control cleanup reviews: %+v", findings)
	}
}

func TestOperatorSourceControlProjectionContractFailsOnShallowSurface(t *testing.T) {
	repo := writeWorkbenchActionCardFixtureRepo(t, map[string]string{
		"cmd/compound/orient_operator.go":      "type orientNeedsMe struct { ReviewRequired int }\n",
		"cmd/compound/orient_operator_test.go": "func TestOrientOperator(t *testing.T) { _ = \"git status\" }\n",
		"docs/architecture/status.md":          "Operator orientation says check git status manually.\n",
	})
	findings := checkOperatorSourceControlProjectionContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{"source_control", "TestOrientOperatorCarriesSourceControlReviewProjection", "Operator source-control projection note"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should name missing %q: %+v", want, findings)
		}
	}
}

func TestASCDailySignalsPublicBoundaryContractResolved(t *testing.T) {
	findings := checkASCDailySignalsPublicBoundaryContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("ASC daily signals public boundary contract failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "product-signal normalization") {
		t.Fatalf("finding should name product-signal normalization: %+v", findings)
	}
}

func TestASCDailySignalsPublicBoundaryContractFailsOnPrivateCollectorDependency(t *testing.T) {
	repo := writeWorkbenchActionCardFixtureRepo(t, map[string]string{
		"docs/operations/asc-daily-signals.md": "# ASC Daily Signals\n\nUse a named private collector and launchd for ASC. This requires launchd.\n",
		"docs/architecture/status.md":          "ASC daily source note: a private collector fetches app data.\n",
	})
	findings := checkASCDailySignalsPublicBoundaryContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{
		"any collector can produce ASC-shaped output",
		"ASC daily signals public-boundary note",
		"still contains stale phrase \"requires launchd\"",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should name missing %q: %+v", want, findings)
		}
	}
}

func TestPublicAlphaSupportedScopeContractResolved(t *testing.T) {
	findings := checkPublicAlphaSupportedScopeContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("public-alpha supported scope contract failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "support boundary") {
		t.Fatalf("finding should name the support boundary: %+v", findings)
	}
}

func TestPublicAlphaSupportedScopeContractFailsWhenBoundaryStatementsDrift(t *testing.T) {
	repo := writeWorkbenchActionCardFixtureRepo(t, map[string]string{
		"docs/security/supported-scope.md": "# Supported Scope\n\nFile storage is one option among many.\n",
		"README.md":                        "# compound\n",
		"docs/getting-started.md":          "# Getting Started\n",
		"docs/harness-wiring.md":           "# Harness Wiring\n",
		"docs/architecture/status.md":      "# status\n",
	})
	findings := checkPublicAlphaSupportedScopeContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{
		"the only supported storage backend",
		"Supported Scope (Public Alpha)",
		"file backend only",
		"never mutation-ready",
		"the only supported public-alpha backend",
		"Current blockers",
		"projection-only by default",
		"delegated launch, steer, progress",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should name missing %q: %+v", want, findings)
		}
	}
}

func TestReviewSurfaceCoverageContractResolved(t *testing.T) {
	findings := checkReviewSurfaceCoverageContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("review surface coverage contract failed against real repo: %+v", findings)
	}
	if !strings.Contains(findings[0].Message, "decision/action coverage") {
		t.Fatalf("finding should name review affordance coverage: %+v", findings)
	}
}

func TestReviewSurfaceCoverageContractFailsOnRawArtifactList(t *testing.T) {
	repo := writeWorkbenchActionCardFixtureRepo(t, map[string]string{
		"internal/reviewartifact/queue.go":               "type QueueCounts struct { Total int `json:\"total\"` }\nfunc Queue() {}\n",
		"internal/reviewartifact/reviewartifact_test.go": "func TestQueue(t *testing.T) { _ = \"total\" }\n",
		"cmd/compound/main.go":                           "func cmdReviews() { println(\"reviews total\") }\n",
		"cmd/compound/main_test.go":                      "func TestReviews(t *testing.T) { _ = \"reviews total\" }\n",
		"docs/architecture/status.md":                    "Review queue lists review artifacts.\n",
	})
	findings := checkReviewSurfaceCoverageContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	msg := findings[0].Message
	for _, want := range []string{"decision_card_count", "review_surface", "review_required_missing_action", "fallback_summary_count"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure should name missing %q: %+v", want, findings)
		}
	}
}

func writeWorkbenchActionCardFixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func writeAdaptationSteeringFixtureRepo(t *testing.T, prompt, rule, pointer string) string {
	t.Helper()
	repo := t.TempDir()
	planDir := filepath.Join(repo, "docs_private", "plan")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "codex-goal-prompt.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "OPERATING-SYSTEM-ADAPTATION-RULE.md"), []byte(rule), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planDir, "codex-goal-prompt-compounding-runtime.md"), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func goodAdaptationGoalPromptFixture() string {
	return strings.Join([]string{
		"# COMPOUND Goal Prompt",
		"Absorb operating-system by context-aware adaptation, not copy/paste.",
		"Pick a consequential bounded move.",
		"Public Compound stays generic; private dogfood adapters feed generic doors.",
		"Held operator valves are constraints, not mission failure.",
		"Oracle is evidence, not authority.",
		"Do not report Compound as globally blocked while adjacent proof work exists.",
		"## Context-Aware Adaptation Rules",
		"Every adaptation names a silence condition, proof command, old-owner, and public/private boundary.",
		"Instruction tuning preserves trigger, action, boundary, and proof.",
	}, "\n")
}

func goodAdaptationRuleFixture() string {
	return strings.Join([]string{
		"# Operating-System Adaptation Rule",
		"## Instruction Tuning Standard",
		"- Trigger: when the rule should load.",
		"- Action: what changes.",
		"- Boundary: what it must not touch.",
		"- Silence condition: when it should not load.",
		"- Proof: the command/test/projection that consumes it.",
		"- Retirement: the old surface becomes evidence-only.",
		"Only merge and proof count as compounding system deltas.",
	}, "\n")
}

func TestDistillerDuplicateContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "distiller-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("distiller contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected distiller-contract finding")
	}
}

func TestDistillerDuplicateContractFailsOnMemoryOnlyDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`distiller run` now records duplicate memory outputs as idempotent `NoPromoteDecision` rows.\n"
	if err := os.WriteFile(filepath.Join(docs, "status.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkDistillerDuplicateContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "non-memory duplicate candidate") {
		t.Fatalf("failure should name missing candidate coverage: %+v", findings)
	}
}

func TestDistillerDuplicateContractRequiresCandidateCoverage(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "status.md"), []byte("distiller duplicate handling exists\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkDistillerDuplicateContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "non-memory duplicate candidate") {
		t.Fatalf("failure should name missing candidate coverage: %+v", findings)
	}
}

func TestLLMExtractionGroundingContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "llm-grounding-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("llm grounding contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected llm-grounding-contract finding")
	}
}

func TestLLMExtractionGroundingContractFailsOnGenericExtractorDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "OpenAICompatibleExtraction validates returned LLMExtractionResponse through LLMExtractor.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkLLMExtractionGroundingContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "request-grounded LLM extraction") {
		t.Fatalf("failure should name missing request grounding: %+v", findings)
	}
}

func TestLLMExtractionGroundingContractRequiresEvidenceAndSourceRefs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "Eval/live extraction is request-grounded, but this stale doc omits the evidence gate.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkLLMExtractionGroundingContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "evidence_refs/source_refs") {
		t.Fatalf("failure should name missing refs: %+v", findings)
	}
}

func TestDistillerCandidateShapeContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "distiller-shape-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("distiller shape contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected distiller-shape-contract finding")
	}
}

func TestDistillerCandidateShapeContractFailsOnThinDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "LLMExtractionResponse carries candidates with kind and confidence.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkDistillerCandidateShapeContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "candidate shape guard") {
		t.Fatalf("failure should name missing shape guard: %+v", findings)
	}
}

func TestRouteOutcomeContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "route-outcome-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("route outcome contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected route-outcome-contract finding")
	}
}

func TestRouteOutcomeContractFailsOnMetadataOnlyDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound route import` records ExecutionRouteEvidence cost and selected target metadata.\n"
	if err := os.WriteFile(filepath.Join(docs, "status.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkRouteOutcomeContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "receipt-linked route outcome review") {
		t.Fatalf("failure should name missing outcome review: %+v", findings)
	}
}

func TestRouteOutcomeContractRequiresStateDeltaAndNoPromote(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound route outcome-review` joins route evidence to a Receipt and does not mutate gateway/provider/model-router config.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkRouteOutcomeContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "state_delta/no-promote") {
		t.Fatalf("failure should name missing state/no-promote guard: %+v", findings)
	}
}
func TestKnowledgePageContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "knowledge-page-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("knowledge-page contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected knowledge-page-contract finding")
	}
}

func TestKnowledgePageContractFailsOnVagueDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "knowledge-base pages become Evidence / Claim / Position candidates.\n"
	if err := os.WriteFile(filepath.Join(docs, "source-adapters.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkKnowledgePageContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "memory import-page") {
		t.Fatalf("failure should name missing import-page contract: %+v", findings)
	}
}

func TestWorkPacketContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "packet-work-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("packet work contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected packet-work-contract finding")
	}
}

func TestWorkPacketContractFailsOnPointerOnlyDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "Active packets point agents to the current goal packet.\n"
	if err := os.WriteFile(filepath.Join(docs, "runtime.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkWorkPacketContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "work import-packet") {
		t.Fatalf("failure should name missing import-packet contract: %+v", findings)
	}
}

func TestWorkPacketContractRequiresNoProofMutationOrPromotion(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`work import-packet` creates a packet hash proposed WorkEnvelope from markdown.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkWorkPacketContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "no packet mutation") {
		t.Fatalf("failure should name missing no-mutation/no-proof guard: %+v", findings)
	}
}

func TestMCPorterTopologyContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "mcporter-topology-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("mcporter topology contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected mcporter-topology-contract finding")
	}
}

func TestMCPorterTopologyContractFailsOnNormalizedOnlyDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound capability import-topology` maps normalized topology YAML or generated resolver JSON into CapabilityGraph nodes.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkMCPorterTopologyContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "mcporter list --json") {
		t.Fatalf("failure should name missing mcporter catalog contract: %+v", findings)
	}
}

func TestMCPorterTopologyContractRequiresNoPromoteAndNoExecution(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound capability import-topology` adapts `mcporter list --json` output and marks auth/error/offline servers non-surfacing.\n"
	if err := os.WriteFile(filepath.Join(docs, "source-adapters.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := checkMCPorterTopologyContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "SourceObservation/no-promote") {
		t.Fatalf("failure should name missing source/no-promote guard: %+v", findings)
	}
}

func TestMemoryEngineContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "memory-engine-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("memory engine contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected memory-engine-contract finding")
	}
}

func TestMemoryEngineContractFailsOnEngineDoctorOnlyDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`memory engine-doctor` and `memory engine-compare` prove a rented engine leaves ledger bytes unchanged.\n"
	if err := os.WriteFile(filepath.Join(docs, "memory.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkMemoryEngineContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "memory doctor") {
		t.Fatalf("failure should name missing memory doctor contract: %+v", findings)
	}
}

func TestMemoryEngineContractRequiresLedgerAndAuthorityGuards(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`memory doctor` observes configured sidecar via `COMPOUND_MEM_ENGINE_BIN` sync/embedding and `engine-compare` checks swap parity.\n"
	if err := os.WriteFile(filepath.Join(docs, "memory.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkMemoryEngineContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "ledger_unchanged") {
		t.Fatalf("failure should name missing ledger/no-authority guard: %+v", findings)
	}
}

func TestHarnessEntrypointContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "harness-entrypoint-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("harness entrypoint contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected harness-entrypoint-contract finding")
	}
}

func TestHarnessEntrypointContractFailsWithoutOSEntrypointDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound harness observe` records built-in and normalized harness registries as SourceObservation/no-promote evidence without installing hooks.\n"
	if err := os.WriteFile(filepath.Join(docs, "runtime.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkHarnessEntrypointContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "--os-entrypoints") {
		t.Fatalf("failure should name missing os-entrypoints contract: %+v", findings)
	}
}

func TestHarnessEntrypointContractRejectsCapabilityOverclaim(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound harness observe --os-entrypoints file` records SourceObservation/no-promote evidence and installs no hooks; OS entrypoints imply hook blocking, MCP, skills, and receipts.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkHarnessEntrypointContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "session-start-only") {
		t.Fatalf("failure should name missing session-start-only guard: %+v", findings)
	}
}

func TestHookSourceControlContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "hook-source-control-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("hook source-control contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected hook-source-control-contract finding")
	}
}

func TestHookSourceControlContractFailsOnRawTargetDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`hook pre-tool-use` consumes source-control status for target paths through read-only git and does not mutate repos.\n"
	if err := os.WriteFile(filepath.Join(docs, "runtime.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkHookSourceControlContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "relative target normalization") {
		t.Fatalf("failure should name missing target normalization: %+v", findings)
	}
}

func TestHookLiveParityContextSpineContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "hook-live-parity-context-spine-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("hook live parity context-spine contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected hook-live-parity-context-spine-contract finding")
	}
}

func TestHookLiveParityContextSpineContractFailsOnDocsOnlyClaim(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "hook-live-parity-context-spine-contract: `hook session-start --live --parity-log` records context request, effective contract, context pack, and injection refs.\n"
	if err := os.WriteFile(filepath.Join(docs, "runtime.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkHookLiveParityContextSpineContract(repo)
	if len(findings) != 1 || findings[0].Severity != Warn {
		t.Fatalf("findings = %+v, want one warning for missing live hook source", findings)
	}
	if !strings.Contains(findings[0].Message, "cmd/compound/hook_live.go") {
		t.Fatalf("failure should name missing live hook source: %+v", findings)
	}
}

func TestHookReadinessActionContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "hook-readiness-action-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("hook readiness action contract failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected hook-readiness-action-contract finding")
	}
}

func TestHookReadinessActionContractFailsOnProseOnlyActions(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs", "architecture")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "Hook readiness authority note: contract hooks-authority reports pre_tool_use_readiness actions before any registration flip.\n"
	if err := os.WriteFile(filepath.Join(docs, "status.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "internal", "contract"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "internal", "contract", "contract.go"), []byte("type InstalledHookRuntimeReadiness struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "internal", "envelope"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "internal", "envelope", "envelope.go"), []byte("type HookReadiness struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "internal", "contract", "contract_test.go"), []byte("func TestReadiness(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "cmd", "compound", "main_test.go"), []byte("func TestHooksAuthority(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "cmd", "compound", "hook_permission_prompt_test.go"), []byte("func TestHookPermissionPrompt(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "internal", "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "internal", "work", "workbench.go"), []byte("func Workbench() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "cmd", "compound", "proof_check.go"), []byte("func ProofCheck() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkHookReadinessActionContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "prove_fixture_hook") ||
		!strings.Contains(findings[0].Message, "non_blocking") ||
		!strings.Contains(findings[0].Message, "no native permission decision") ||
		!strings.Contains(findings[0].Message, "--enforce") ||
		!strings.Contains(findings[0].Message, "native_blocking") ||
		!strings.Contains(findings[0].Message, "codex-pretooluse-native-gate-proof") ||
		!strings.Contains(findings[0].Message, "expected_exit_code") ||
		!strings.Contains(findings[0].Message, "operator_live_adapter_required") {
		t.Fatalf("failure should name missing action cards: %+v", findings)
	}
}

func TestStandingGrantContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "standing-grant-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("standing grant contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected standing-grant-contract finding")
	}
}

func TestStandingGrantContractFailsOnUnscopedDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "StandingGrant lets policy allow approved actions without asking.\n"
	if err := os.WriteFile(filepath.Join(docs, "runtime.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkStandingGrantContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "target_path/budget/blast/reversibility") {
		t.Fatalf("failure should name missing scoped envelope fields: %+v", findings)
	}
}

func TestPlacementGuardContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "placement-guard-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("placement guard contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected placement-guard-contract finding")
	}
}

func TestPlacementGuardContractFailsOnHookOnlyDocs(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "PlacementGuard hook blocks writes by checking owner placement.\n"
	if err := os.WriteFile(filepath.Join(docs, "runtime.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkPlacementGuardContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "confidence:block") {
		t.Fatalf("failure should name missing confidence threshold: %+v", findings)
	}
}

func TestProductContractContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "product-contract-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("product contract check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected product-contract-contract finding")
	}
}

func TestProductLoopOpenHypothesisContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "product-loop-open-hypothesis-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("product loop open-hypothesis check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected product-loop-open-hypothesis-contract finding")
	}
}

func TestOperatorProductLoopProjectionContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "operator-product-loop-projection-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("operator product-loop projection check failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected operator-product-loop-projection-contract finding")
	}
}

func TestOperatorProductLoopProjectionContractFailsWithoutReadModelConsumer(t *testing.T) {
	repo := t.TempDir()
	mustWriteArchFixture(t, repo, "cmd/compound/orient_operator.go", strings.Join([]string{
		"ProductLoops *orientProductLoops",
		"json:\"product_loops,omitempty\"",
		"OpenHypothesesByMissingSignalSource",
		"ClosedReviewsByNextAction",
		"product_loops: products=%d open_hypothesis_debt=%d closed_review_actions=%d",
		"compound\", \"product\", \"loops\"",
	}, "\n"))
	mustWriteArchFixture(t, repo, "cmd/compound/orient_operator_test.go", strings.Join([]string{
		"TestOrientOperatorCarriesProductLoopEvidenceDebtProjection",
		"seedOrientProductLoopProjectionHome",
		"missing_signal_source=posthog",
		"closed_review_next=keep screenshot one more window",
		"compound product loops",
	}, "\n"))
	mustWriteArchFixture(t, repo, "docs/architecture/status.md", strings.Join([]string{
		"Operator product-loop projection note",
		"`compound orient --operator --json.product_loops`",
		"`product.BuildLoopModel` read model",
		"`compound product loops` as drill-down",
	}, "\n"))
	findings := checkOperatorProductLoopProjectionContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want missing product.BuildLoopModel failure", findings)
	}
	if !strings.Contains(findings[0].Message, "product.BuildLoopModel") ||
		!strings.Contains(findings[0].Message, "buildOrientProductLoops") {
		t.Fatalf("failure should name missing read-model consumer: %+v", findings)
	}
}

func TestProductContractContractFailsWithoutReleaseSource(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`product import --project-contract <project.yaml>` records deterministic identity surface channel privacy and never analytics signals or causality.\n"
	if err := os.WriteFile(filepath.Join(docs, "source-adapters.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkProductContractContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "--release") {
		t.Fatalf("failure should name missing release contract: %+v", findings)
	}
}

func TestProductContractContractRejectsAnalyticsCausalityOverclaim(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`product import --project-contract <project.yaml> --release <release.yaml>` records deterministic identity surface channel privacy and creates analytics signals with causal attribution.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkProductContractContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "no analytics signals or causality") {
		t.Fatalf("failure should name analytics/causality guard: %+v", findings)
	}
}

func TestCapabilityEvaluationContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "capability-evaluation-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("capability evaluation contract failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected capability-evaluation-contract finding")
	}
}

func TestSourceObservationVerdictContractResolved(t *testing.T) {
	findings := checkSourceObservationVerdictContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("source observation verdict contract failed against real repo: %+v", findings)
	}
}

func TestPanelCapabilityFeedbackContractResolved(t *testing.T) {
	findings := checkPanelCapabilityFeedbackContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("panel capability feedback contract failed against real repo: %+v", findings)
	}
}

func TestExternalRepoContextEvaluationContractResolved(t *testing.T) {
	findings := checkExternalRepoContextEvaluationContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("external repo context-evaluation contract failed against real repo: %+v", findings)
	}
}

func TestExternalRepoContextEvaluationContractFailsWithoutDecisionCard(t *testing.T) {
	repo := t.TempDir()
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_intake.go", strings.Join([]string{
		"ExternalRepoContextEvaluationStep",
		"EvaluationPlan",
		"externalRepoContextEvaluationPlan",
		"existing-owner-fit",
		"capability-shape-fit",
		"risk-cost-fit",
		"Context-Aware Evaluation Plan",
		"silence:",
		"proof:",
		"retirement:",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_review.go", strings.Join([]string{
		"evaluation lens: ",
		"evaluation_step_count",
		"no clone/install/serve/auth/graph mutation/memory promotion",
		"externalRepoIntakeDecisionChoices",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_intake_test.go", strings.Join([]string{
		"item.EvaluationPlan",
		"hasExternalRepoEvaluationStep",
		"harness-fit",
		"DecisionCard.Meta",
		"evaluation lens: harness-fit",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_evaluation_plan.go", strings.Join([]string{
		"contextPlan := externalRepoContextEvaluationPlan(report.Repo, match)",
		"externalRepoContextEvaluationSignals(contextPlan)",
		"context_evaluation_proofs",
		"context_evaluation_retirement",
		"context_evaluation_step_count",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/evaluation_plan.go", strings.Join([]string{
		"signalStringSlice(eval.Signals, \"context_evaluation_lenses\")",
		"context lens ",
		"silence condition reviewed",
		"context evaluation proof: ",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/evaluation_review_queue.go", strings.Join([]string{
		"signalStringSlice(plan.Signals, \"context_evaluation_lenses\")",
		"context_evaluation_silence",
		"context_evaluation_retirement",
		"indexedSignal",
		"evaluation lens: ",
	}, "\n"))
	mustWriteArchFixture(t, repo, "docs/architecture/status.md", strings.Join([]string{
		"External repo context-evaluation note",
		"`compound reviews --area external-repo-intake --json`",
		"context-aware evaluation plan",
		"trigger/action/boundary/silence/proof/retirement",
		"capability-evaluation plans and decision cards",
		"`context lens ...` review gates",
		"`evaluation lens: ...` reasons",
		"no clone/install/serve/auth/graph mutation/memory promotion",
	}, "\n"))
	findings := checkExternalRepoContextEvaluationContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "externalRepoIntakeDecisionCard") {
		t.Fatalf("failure should name missing decision-card builder: %+v", findings)
	}
}

func TestExternalRepoContextEvaluationContractFailsWithoutCapabilityEvaluationPropagation(t *testing.T) {
	repo := t.TempDir()
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_intake.go", strings.Join([]string{
		"ExternalRepoContextEvaluationStep",
		"EvaluationPlan",
		"externalRepoContextEvaluationPlan",
		"existing-owner-fit",
		"capability-shape-fit",
		"risk-cost-fit",
		"Context-Aware Evaluation Plan",
		"silence:",
		"proof:",
		"retirement:",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_review.go", strings.Join([]string{
		"externalRepoIntakeDecisionCard",
		"evaluation lens: ",
		"evaluation_step_count",
		"no clone/install/serve/auth/graph mutation/memory promotion",
		"externalRepoIntakeDecisionChoices",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_intake_test.go", strings.Join([]string{
		"item.EvaluationPlan",
		"hasExternalRepoEvaluationStep",
		"harness-fit",
		"DecisionCard.Meta",
		"evaluation lens: harness-fit",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_evaluation_plan.go", strings.Join([]string{
		"contextPlan := externalRepoContextEvaluationPlan(report.Repo, match)",
		"externalRepoContextEvaluationSignals(contextPlan)",
		"context_evaluation_proofs",
		"context_evaluation_retirement",
		"context_evaluation_step_count",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/evaluation_plan.go", strings.Join([]string{
		"signalStringSlice(eval.Signals, \"context_evaluation_lenses\")",
		"context lens ",
		"silence condition reviewed",
		"context evaluation proof: ",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/evaluation_review_queue.go", strings.Join([]string{
		"signalStringSlice(plan.Signals, \"context_evaluation_lenses\")",
		"context_evaluation_silence",
		"context_evaluation_retirement",
		"indexedSignal",
		"evaluation lens: ",
	}, "\n"))
	mustWriteArchFixture(t, repo, "docs/architecture/status.md", strings.Join([]string{
		"External repo context-evaluation note",
		"`compound reviews --area external-repo-intake --json`",
		"context-aware evaluation plan",
		"trigger/action/boundary/silence/proof/retirement",
		"capability-evaluation plans and decision cards",
		"`context lens ...` review gates",
		"`evaluation lens: ...` reasons",
		"no clone/install/serve/auth/graph mutation/memory promotion",
	}, "\n"))

	findings := checkExternalRepoContextEvaluationContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "external_repo_evaluation_plan.go") ||
		!strings.Contains(findings[0].Message, "context_evaluation_lenses") {
		t.Fatalf("failure should name missing capability-evaluation context propagation: %+v", findings)
	}
}

func TestExternalRepoIntakeStateProjectionContractResolved(t *testing.T) {
	findings := checkExternalRepoIntakeStateProjectionContract(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("external repo intake state projection contract failed against real repo: %+v", findings)
	}
}

func TestExternalRepoIntakeStateProjectionContractFailsWithoutOpenOwnerDebt(t *testing.T) {
	repo := t.TempDir()
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_intake.go", strings.Join([]string{
		`json:"open_review_count"`,
		"annotateExternalRepoDecisionState",
		"countPendingExternalRepoOwnerReviews",
		"ExternalRepoCapabilityEvaluationStatus",
		"ExternalRepoCapabilityGraphStatus",
	}, "\n"))
	mustWriteArchFixture(t, repo, "internal/capgraph/external_repo_intake_test.go", strings.Join([]string{
		"default report should keep raw decision count but clear owner-review debt",
		`CapabilityEvaluationStatus != "pending"`,
		`CapabilityGraphStatus != "blocked_on_capability_evaluation"`,
	}, "\n"))
	mustWriteArchFixture(t, repo, "cmd/compound/capability.go", strings.Join([]string{
		"keep_needs_review_decision",
		"high_value: %s decision=%s lane=%s owner_review=%s capability_evaluation=%s graph=%s owner=%s",
	}, "\n"))
	mustWriteArchFixture(t, repo, "cmd/compound/main_test.go", strings.Join([]string{
		"default external-intake text should distinguish raw decision count from open owner-review debt",
		"owner_review=recorded capability_evaluation=pending graph=blocked_on_capability_evaluation",
	}, "\n"))
	mustWriteArchFixture(t, repo, "docs/architecture/status.md", strings.Join([]string{
		"Default `capability external-intake` separates raw `keep_needs_review` classification from actual `open_owner_reviews`",
		"high-value rows show owner-review, capability-evaluation, and graph status",
		"instead of calling recorded/imported rows review-next work",
	}, "\n"))
	findings := checkExternalRepoIntakeStateProjectionContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "open_owner_reviews") {
		t.Fatalf("failure should name missing open owner-review debt label: %+v", findings)
	}
}

func TestCapabilityEvaluationContractFailsWhenEvalCanSurface(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound capability import-evaluations` creates eval nodes with evaluated_by edges and SourceObservation/no-promote evidence.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkCapabilityEvaluationContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "never surface") {
		t.Fatalf("failure should name non-surfacing guard: %+v", findings)
	}
}

func TestCapabilityEvaluationContractFailsWithoutReviewGate(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound capability import-evaluations` imports evidence-only eval records with evaluated_by edges, SourceObservation/no-promote evidence, and eval records never surface as agent offers.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkCapabilityEvaluationContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "evaluation-review") || !strings.Contains(findings[0].Message, "import-skills") {
		t.Fatalf("failure should name review gate and import boundary: %+v", findings)
	}
}

func TestCapabilityEvaluationContractFailsWhenReviewMutatesGraph(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := strings.Join([]string{
		"`compound capability import-evaluations` imports evidence-only eval records with evaluated_by edges, SourceObservation/no-promote evidence, and eval records never surface as agent offers.",
		"`compound capability evaluation-plan` writes a .review.yaml review YAML.",
		"`compound capability evaluation-review <review.yaml>` validates an edited decision with rationale and confidence, writes a reviewed manifest without mutating graph state, but evaluation-review mutates graph state directly.",
		"Only `import-skills` mutates the graph after a reviewed manifest exists.",
	}, "\n")
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkCapabilityEvaluationContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "evaluation-review mutates") {
		t.Fatalf("failure should name direct mutation contradiction: %+v", findings)
	}
}

func TestConversationMiningContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "conversation-mining-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("conversation mining contract failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected conversation-mining-contract finding")
	}
}

func TestConversationMiningContractFailsWithoutGates(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound distiller import-conversation-mining` imports mined conversations for Dream.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkConversationMiningContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "lossiness-aware") || !strings.Contains(findings[0].Message, "improvement-plan") || !strings.Contains(findings[0].Message, "memory promotion") {
		t.Fatalf("failure should name missing lossiness/no-promotion guards: %+v", findings)
	}
}

func TestConversationCoverageContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "conversation-coverage-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("conversation coverage contract failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected conversation-coverage-contract finding")
	}
}

func TestConversationCoverageContractFailsWithoutGates(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound distiller import-conversation-coverage` imports coverage rows for Dream.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkConversationCoverageContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "summary-only") || !strings.Contains(findings[0].Message, "no row import") || !strings.Contains(findings[0].Message, "memory promotion") {
		t.Fatalf("failure should name coverage gates: %+v", findings)
	}
}

func TestDecisionSourceAuthorityContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "decision-source-authority-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("decision source authority contract failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected decision-source-authority-contract finding")
	}
}

func TestDecisionSourceAuthorityContractFailsWithoutCoverageAndRuntimeDedupe(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`decisions import --policy-dir <dir>` imports DecisionLifecycle records.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkDecisionSourceAuthorityContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "source-authority coverage") || !strings.Contains(findings[0].Message, "runtime decision:// dedupe") {
		t.Fatalf("failure should name missing authority/runtime guards: %+v", findings)
	}
}

func TestDistillerImprovementReviewContractFailsWithoutGate(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound distiller improvement-plan` writes a review note but applies method changes.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkDistillerImprovementReviewContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "eval_proof_refs") || !strings.Contains(findings[0].Message, "gated review YAML") {
		t.Fatalf("failure should name missing review/proof guards: %+v", findings)
	}
}

func TestEpistemicsDoctrineResolved(t *testing.T) {
	findings := checkEpistemicsDoctrine(repoRoot)
	if len(findings) != 1 || findings[0].Severity == Fail {
		t.Fatalf("epistemics doctrine check failed against real repo: %+v", findings)
	}
}

func TestEpistemicsDoctrineFailsWhenDocMissing(t *testing.T) {
	repo := writeEpistemicsFixtureRepo(t, epistemicsFixtureRows())
	if err := os.Remove(filepath.Join(repo, "docs", "architecture", "epistemics.md")); err != nil {
		t.Fatal(err)
	}
	findings := checkEpistemicsDoctrine(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want missing-doc failure", findings)
	}
	if !strings.Contains(findings[0].Message, "epistemics.md") {
		t.Fatalf("failure should name epistemics.md: %+v", findings)
	}
}

func TestEpistemicsDoctrineFailsWithTooFewRows(t *testing.T) {
	repo := writeEpistemicsFixtureRepo(t, epistemicsFixtureRows()[:6])
	findings := checkEpistemicsDoctrine(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want row-count failure", findings)
	}
	if !strings.Contains(findings[0].Message, "6 doctrine row") {
		t.Fatalf("failure should name parsed row count: %+v", findings)
	}
}

func TestEpistemicsDoctrineFailsForPhantomPackage(t *testing.T) {
	rows := epistemicsFixtureRows()
	rows[2] = "| 3 | contradiction review | internal/ghost | compound synthesis sweep |"
	repo := writeEpistemicsFixtureRepo(t, rows)
	findings := checkEpistemicsDoctrine(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want phantom package failure", findings)
	}
	if !strings.Contains(findings[0].Message, "internal/ghost") || !strings.Contains(findings[0].Message, "not found") {
		t.Fatalf("failure should name missing internal package: %+v", findings)
	}
}

func TestEpistemicsDoctrineFailsForPhantomCommandVerb(t *testing.T) {
	rows := epistemicsFixtureRows()
	rows[4] = "| 5 | route evidence is not task success | internal/routeevidence | compound ghost-command run |"
	repo := writeEpistemicsFixtureRepo(t, rows)
	findings := checkEpistemicsDoctrine(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want phantom command failure", findings)
	}
	if !strings.Contains(findings[0].Message, "ghost-command") || !strings.Contains(findings[0].Message, "cmd/compound/command_registry.go") {
		t.Fatalf("failure should name missing command registry row: %+v", findings)
	}
}

func writeEpistemicsFixtureRepo(t *testing.T, rows []string) string {
	t.Helper()
	repo := t.TempDir()
	for _, dir := range []string{
		"cmd/compound",
		"docs/architecture",
		"internal/arch",
		"internal/candidates",
		"internal/capgraph",
		"internal/contract",
		"internal/distiller",
		"internal/envelope",
		"internal/harness",
		"internal/knowledge",
		"internal/mem",
		"internal/panel",
		"internal/product",
		"internal/routeevidence",
		"internal/work",
	} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	commandRegistryGo := `package main
func compoundCommandRegistry() map[string]any {
	return map[string]any{
		"architecture": nil,
		"capability":   nil,
		"contract":     nil,
		"memory":       nil,
		"route":        nil,
		"synthesis":    nil,
		"workbench":    nil,
	}
}
`
	if err := os.WriteFile(filepath.Join(repo, "cmd", "compound", "command_registry.go"), []byte(commandRegistryGo), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("| n | principle | implementing_object | proof_command |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, row := range rows {
		b.WriteString(row)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "architecture", "epistemics.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func epistemicsFixtureRows() []string {
	return []string{
		"| 1 | expectations become contracts | internal/contract, internal/harness | compound contract compile |",
		"| 2 | one root identity closes proof | internal/work, internal/envelope | compound workbench |",
		"| 3 | contradiction reopens review | internal/mem | compound synthesis sweep |",
		"| 4 | capability offers do not execute | internal/capgraph, internal/panel | compound capability surface |",
		"| 5 | route evidence is not task success | internal/routeevidence, internal/product | compound route outcome-review |",
		"| 6 | history becomes evidence | internal/knowledge, internal/distiller, internal/candidates | compound memory recall |",
		"| 7 | public core gated intake | internal/arch, internal/capgraph | compound architecture doctor |",
	}
}

func TestPluginServingContractResolved(t *testing.T) {
	r := DoctorSource(repoRoot, false)
	var saw bool
	for _, f := range r.Findings {
		if f.Check != "plugin-serving-contract" {
			continue
		}
		saw = true
		if f.Severity == Fail {
			t.Fatalf("plugin serving contract failed against real repo: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected plugin-serving-contract finding")
	}
}

func TestPluginServingContractFailsWhenDocsMakePluginsExecutable(t *testing.T) {
	repo := t.TempDir()
	docs := filepath.Join(repo, "docs")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "`compound capability import-skills --os-plugin-board-json <skills-selection-board.json>` imports plugins as active offers.\n"
	if err := os.WriteFile(filepath.Join(docs, "contracts.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := checkPluginServingContract(repo)
	if len(findings) != 1 || findings[0].Severity != Fail {
		t.Fatalf("findings = %+v, want one failure", findings)
	}
	if !strings.Contains(findings[0].Message, "retired adapter") || !strings.Contains(findings[0].Message, "non-surfacing") {
		t.Fatalf("failure should name retired/non-surfacing plugin boundary: %+v", findings)
	}
}

func mustWriteArchFixture(t *testing.T, repo, rel, text string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- cmd/compound budget gate tests ---
//
// The two ratchets (MaxNonTestCompoundFiles and MaxNonTestCompoundLOC) are independent, so
// each test fixes one metric at the in-budget state and varies the other. The fixtures use
// tiny, fixed-content files (each one newline so LOC counting is exact), and each test
// asserts the gate reads cmd/compound only — internal/ and cmd/compound-* siblings are
// written alongside as deliberate traps.

func cmdCompoundBudgetFindings(t *testing.T, repo string, strict bool) []Finding {
	t.Helper()
	return checkCmdCompoundBudget(repo, strict)
}

func assertCmdCompoundBudgetSeverity(t *testing.T, fs []Finding, field, want string) {
	t.Helper()
	for _, f := range fs {
		if !strings.Contains(f.Message, field) {
			continue
		}
		if string(f.Severity) != want {
			t.Fatalf("severity for %q = %s, want %s: %+v", field, f.Severity, want, f)
		}
		return
	}
	t.Fatalf("no cmd-compound-budget finding for %q in: %+v", field, fs)
}

func cmdCompoundBudgetCount(fs []Finding, contains string) (Severity, string, bool) {
	for _, f := range fs {
		if !strings.Contains(f.Message, contains) {
			continue
		}
		return f.Severity, f.Message, true
	}
	return "", "", false
}

// TestCheckCmdCompoundBudgetPassesOnRealRepo: the live cmd/compound tree is at-or-under
// both ratchets as of this WorkEnvelope. The file ceiling remains pinned while the finding
// reports the measured count and extraction-created headroom; LOC is under budget with explicit
// headroom reported in the Info message. A future commit that blows either budget will trip this
// test (see TestCheckCmdCompoundBudgetFailsOn...).
func TestCheckCmdCompoundBudgetPassesOnRealRepo(t *testing.T) {
	const pinnedFileBudget = 112
	if MaxNonTestCompoundFiles != pinnedFileBudget {
		t.Fatalf("cmd/compound file budget changed: got %d, want pinned ceiling %d", MaxNonTestCompoundFiles, pinnedFileBudget)
	}
	entries, err := os.ReadDir(filepath.Join(repoRoot, "cmd", "compound"))
	if err != nil {
		t.Fatal(err)
	}
	fileCount := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			fileCount++
		}
	}
	if fileCount > MaxNonTestCompoundFiles {
		t.Fatalf("cmd/compound file count %d exceeds pinned budget %d", fileCount, MaxNonTestCompoundFiles)
	}

	fs := cmdCompoundBudgetFindings(t, repoRoot, false)
	if len(fs) != 2 {
		t.Fatalf("want 2 findings (files + LOC), got %d: %+v", len(fs), fs)
	}
	filesSev, filesMsg, ok := cmdCompoundBudgetCount(fs, "non-test .go files")
	if !ok {
		t.Fatalf("missing files finding in: %+v", fs)
	}
	if filesSev != Info {
		t.Fatalf("files finding must be Info on a passing tree, got %s: %s", filesSev, filesMsg)
	}
	if !strings.Contains(filesMsg, itoa(fileCount)+"/"+itoa(MaxNonTestCompoundFiles)) {
		t.Fatalf("files finding should report measured count %d/%d, got: %s", fileCount, MaxNonTestCompoundFiles, filesMsg)
	}
	if !strings.Contains(filesMsg, "headroom "+itoa(MaxNonTestCompoundFiles-fileCount)) {
		t.Fatalf("files finding should report headroom %d, got: %s", MaxNonTestCompoundFiles-fileCount, filesMsg)
	}
	locSev, locMsg, ok := cmdCompoundBudgetCount(fs, "non-test .go LOC")
	if !ok {
		t.Fatalf("missing LOC finding in: %+v", fs)
	}
	if locSev != Info {
		t.Fatalf("LOC finding must be Info on a passing tree, got %s: %s", locSev, locMsg)
	}
	if !strings.Contains(locMsg, "/"+itoa(MaxNonTestCompoundLOC)) {
		t.Fatalf("LOC finding should report /%d budget, got: %s", MaxNonTestCompoundLOC, locMsg)
	}
	if !strings.Contains(locMsg, "headroom") {
		t.Fatalf("LOC finding must report headroom so the ratchet floor is visible: %s", locMsg)
	}
}

// TestCheckCmdCompoundBudgetFailsOnFileCountExceeded: a tmpdir with one MORE non-test .go
// file than the budget must FAIL the files metric even though LOC is far under. The LOC
// metric stays Info (independent ratchets — see comment in checkCmdCompoundBudget).
func TestCheckCmdCompoundBudgetFailsOnFileCountExceeded(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Write MaxNonTestCompoundFiles + 1 files, each one line so LOC stays tiny.
	for i := 0; i <= MaxNonTestCompoundFiles; i++ {
		name := filepath.Join(repo, "cmd", "compound", "f"+itoa(i)+".go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := cmdCompoundBudgetFindings(t, repo, true)
	filesSev, filesMsg, ok := cmdCompoundBudgetCount(fs, "non-test .go files")
	if !ok || filesSev != Fail {
		t.Fatalf("files metric must FAIL when over budget (strict), got %s (msg=%s, ok=%v): %+v", filesSev, filesMsg, ok, fs)
	}
	if !strings.Contains(filesMsg, "over budget") {
		t.Fatalf("failure should explain the over-budget direction so a maintainer can act: %s", filesMsg)
	}
	// LOC metric stays Info — independent ratchet.
	locSev, _, ok := cmdCompoundBudgetCount(fs, "non-test .go LOC")
	if !ok || locSev != Info {
		t.Fatalf("LOC metric must stay Info when only files is over budget, got %s (ok=%v): %+v", locSev, ok, fs)
	}
}

// TestCheckCmdCompoundBudgetFailsOnLOCExceeded: a tmpdir with files at-budget but each
// bloat enough to push total LOC over the ceiling must FAIL the LOC metric, while the files
// metric stays Info. Mirrors the files-over-budget test on the other ratchet.
func TestCheckCmdCompoundBudgetFailsOnLOCExceeded(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	// One file. Body is many newlines so LOC = body length, well over budget while file count = 1.
	body := strings.Repeat("\n", MaxNonTestCompoundLOC+1)
	if err := os.WriteFile(filepath.Join(repo, "cmd", "compound", "huge.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := cmdCompoundBudgetFindings(t, repo, true)
	locSev, locMsg, ok := cmdCompoundBudgetCount(fs, "non-test .go LOC")
	if !ok || locSev != Fail {
		t.Fatalf("LOC metric must FAIL when over budget (strict), got %s (msg=%s, ok=%v): %+v", locSev, locMsg, ok, fs)
	}
	if !strings.Contains(locMsg, "over budget") {
		t.Fatalf("failure should explain the over-budget direction so a maintainer can act: %s", locMsg)
	}
	filesSev, _, ok := cmdCompoundBudgetCount(fs, "non-test .go files")
	if !ok || filesSev != Info {
		t.Fatalf("files metric must stay Info when only LOC is over budget, got %s (ok=%v): %+v", filesSev, ok, fs)
	}
}

// TestCheckCmdCompoundBudgetIgnoresTestFiles: a fixture with the budget-allowed number of
// non-test files PLUS enough _test.go files to blow the budget IF counted must still PASS.
// This is the mutation target: if a maintainer removes the `*_test.go` exclusion from
// checkCmdCompoundBudget, the gate fires and this test fails — so the test guards the
// exclusion itself, not just the overall budget.
func TestCheckCmdCompoundBudgetIgnoresTestFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Under-budget non-test files.
	for i := 0; i < MaxNonTestCompoundFiles-1; i++ {
		name := filepath.Join(repo, "cmd", "compound", "f"+itoa(i)+".go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Far more _test.go files than the non-test budget. If exclusion breaks, file count
	// inflates to 2*MaxNonTestCompoundFiles-1 and the gate fires.
	for i := 0; i < MaxNonTestCompoundFiles*3; i++ {
		name := filepath.Join(repo, "cmd", "compound", "t"+itoa(i)+"_test.go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := cmdCompoundBudgetFindings(t, repo, false)
	for _, f := range fs {
		if f.Severity == Fail {
			t.Fatalf("test files must not count toward the budget, but a finding failed: %+v", f)
		}
	}
	_, filesMsg, _ := cmdCompoundBudgetCount(fs, "non-test .go files")
	// Files finding should reflect ONLY the non-test count, not the inflated total.
	if !strings.Contains(filesMsg, itoa(MaxNonTestCompoundFiles-1)+"/"+itoa(MaxNonTestCompoundFiles)) {
		t.Fatalf("files finding should report %d/%d (test files excluded), got: %s",
			MaxNonTestCompoundFiles-1, MaxNonTestCompoundFiles, filesMsg)
	}
}

// TestCheckCmdCompoundBudgetIgnoresCmdCompoundSiblings: cmd/compound-install-verify and
// friends are separate surfaces, each free to grow on its own budget. A fixture that
// explodes the sibling surface must NOT push the cmd/compound budget over.
func TestCheckCmdCompoundBudgetIgnoresCmdCompoundSiblings(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "compound-install-verify"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Under-budget cmd/compound.
	if err := os.WriteFile(filepath.Join(repo, "cmd", "compound", "one.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Far over-budget sibling. If the gate accidentally reads `cmd/compound-*`, file count
	// would inflate and the gate would fire on the metric below.
	for i := 0; i < MaxNonTestCompoundFiles*4; i++ {
		name := filepath.Join(repo, "cmd", "compound-install-verify", "s"+itoa(i)+".go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := cmdCompoundBudgetFindings(t, repo, false)
	for _, f := range fs {
		if f.Severity == Fail {
			t.Fatalf("cmd/compound-* siblings must not feed the cmd/compound budget, but a finding failed: %+v", f)
		}
	}
	_, filesMsg, _ := cmdCompoundBudgetCount(fs, "non-test .go files")
	if !strings.Contains(filesMsg, "1/"+itoa(MaxNonTestCompoundFiles)) {
		t.Fatalf("files finding should report only cmd/compound/, got: %s", filesMsg)
	}
}

// TestCheckCmdCompoundBudgetIgnoresInternalDir: internal/ is not the surface this gate
// bounds — it has its own classification totality check. A fixture with internal/ full of
// .go files must not feed the cmd/compound budget.
func TestCheckCmdCompoundBudgetIgnoresInternalDir(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "internal", "fakeorgan"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "cmd", "compound", "one.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Far over-budget internal/. Should be invisible to this gate.
	for i := 0; i < MaxNonTestCompoundFiles*4; i++ {
		name := filepath.Join(repo, "internal", "fakeorgan", "f"+itoa(i)+".go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := cmdCompoundBudgetFindings(t, repo, false)
	for _, f := range fs {
		if f.Severity == Fail {
			t.Fatalf("internal/ must not feed the cmd/compound budget, but a finding failed: %+v", f)
		}
	}
	_, filesMsg, _ := cmdCompoundBudgetCount(fs, "non-test .go files")
	if !strings.Contains(filesMsg, "1/"+itoa(MaxNonTestCompoundFiles)) {
		t.Fatalf("files finding should ignore internal/, got: %s", filesMsg)
	}
}

// TestCheckCmdCompoundBudgetStrictEscalatesOverBudgetToFail: the file-count and LOC
// findings stay Warn when over budget and `strict` is false, but flip to Fail under
// `--strict`. The "weaken-the-gate" direction is the only one that would lower strict
// further; the ratchet only tightens from here.
func TestCheckCmdCompoundBudgetStrictEscalatesOverBudgetToFail(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("\n", MaxNonTestCompoundLOC+1)
	if err := os.WriteFile(filepath.Join(repo, "cmd", "compound", "huge.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// non-strict: Warn, not Fail — preserves green-build semantics for routine surfaces.
	fsNon := cmdCompoundBudgetFindings(t, repo, false)
	locSevNon, _, _ := cmdCompoundBudgetCount(fsNon, "non-test .go LOC")
	if locSevNon != Warn {
		t.Fatalf("non-strict over-budget LOC must be Warn, got %s: %+v", locSevNon, fsNon)
	}
	// strict: Fail, the ratchet actually bites.
	fsStrict := cmdCompoundBudgetFindings(t, repo, true)
	locSevStrict, _, _ := cmdCompoundBudgetCount(fsStrict, "non-test .go LOC")
	if locSevStrict != Fail {
		t.Fatalf("strict over-budget LOC must be Fail, got %s: %+v", locSevStrict, fsStrict)
	}
}

// TestDoctorEmitsCmdCompoundBudgetFindings: integration smoke through DoctorSource(repoRoot,
// true). Direct tests already pin the check's behavior; this one pins the WIRE — that the
// check is actually registered in the shared doctor implementation. If a future refactor drops the
// `f = append(f, checkCmdCompoundBudget(repoRoot, strict)...)` call, the unit tests still
// pass against the unwired function, and the gate silently stops gating. This test catches
// that drift: a real run of DoctorSource(strict=true) against the live repo must surface exactly
// two cmd-compound-budget findings (one per independent metric).
func TestDoctorEmitsCmdCompoundBudgetFindings(t *testing.T) {
	report := DoctorSource(repoRoot, true)
	if !report.OK {
		t.Fatalf("DoctorSource(strict=true) against the live repo must be green, got failures: %+v", report.Findings)
	}
	var budget []Finding
	for _, f := range report.Findings {
		if f.Check == "cmd-compound-budget" {
			budget = append(budget, f)
		}
	}
	if len(budget) != 2 {
		t.Fatalf("Doctor must emit exactly 2 cmd-compound-budget findings (files + LOC), got %d: %+v", len(budget), budget)
	}
	hasFiles, hasLOC := false, false
	for _, f := range budget {
		if strings.Contains(f.Message, "non-test .go files") {
			hasFiles = true
		}
		if strings.Contains(f.Message, "non-test .go LOC") {
			hasLOC = true
		}
	}
	if !hasFiles || !hasLOC {
		t.Fatalf("Doctor's cmd-compound-budget findings must cover BOTH metrics (files + LOC), got: %+v", budget)
	}
}

// TestCheckCmdCompoundBudgetMutationExclusionBrokenFails: this is the mutation guard for
// the `*_test.go` exclusion. The fixture has exactly MaxNonTestCompoundFiles-2 non-test
// files (under budget) and MaxNonTestCompoundFiles+10 test files. If exclusion is intact,
// the gate reports under budget. If a future refactor accidentally counts test files, the
// inflated total blows the budget and the gate fires — this test catches that regression.
// Mechanically: the assertion is "no finding has severity Fail", and the file-count
// finding's message names the non-test count (not the inflated one). Either assertion
// detects a broken exclusion.
func TestCheckCmdCompoundBudgetMutationExclusionBrokenFails(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "cmd", "compound"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Under-budget non-test files. Two less than the budget so the test files can push
	// past the ceiling if exclusion breaks.
	nonTestCount := MaxNonTestCompoundFiles - 2
	for i := 0; i < nonTestCount; i++ {
		name := filepath.Join(repo, "cmd", "compound", "f"+itoa(i)+".go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Many test files. If exclusion breaks, total file count exceeds budget.
	testCount := MaxNonTestCompoundFiles + 10
	for i := 0; i < testCount; i++ {
		name := filepath.Join(repo, "cmd", "compound", "t"+itoa(i)+"_test.go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := cmdCompoundBudgetFindings(t, repo, true)
	for _, f := range fs {
		if f.Severity == Fail {
			t.Fatalf("with the *_test.go exclusion intact the gate must NOT fail — the "+
				"mutation guard caught a broken exclusion (the gate counted test files): "+
				"%+v", f)
		}
	}
	// Independently confirm the files finding's count message names ONLY the non-test count,
	// so a future regression that inflates it would also trip this assertion.
	_, filesMsg, _ := cmdCompoundBudgetCount(fs, "non-test .go files")
	if !strings.Contains(filesMsg, itoa(nonTestCount)+"/"+itoa(MaxNonTestCompoundFiles)) {
		t.Fatalf("files finding must report the non-test count only (%d/%d), got: %s",
			nonTestCount, MaxNonTestCompoundFiles, filesMsg)
	}
	if strings.Contains(filesMsg, itoa(nonTestCount+testCount)+"/") {
		t.Fatalf("files finding appears to count test files too (%d/%d would be the inflated total): %s",
			nonTestCount+testCount, MaxNonTestCompoundFiles, filesMsg)
	}
}
