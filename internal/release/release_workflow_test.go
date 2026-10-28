package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInactiveGitHubReleaseWorkflowHasReadOnlyBuildsAndIsolatedPublishAuthority(t *testing.T) {
	body, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow releaseWorkflow
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
		t.Fatalf("top-level permissions = %v, want contents:read only", workflow.Permissions)
	}
	build, ok := workflow.Jobs["build-reproducible"]
	if !ok {
		t.Fatal("release workflow lacks build-reproducible job")
	}
	if build.RunsOn != "ubuntu-24.04" {
		t.Fatalf("build runner = %q, want pinned ubuntu-24.04", build.RunsOn)
	}
	if len(build.Permissions) != 0 && (len(build.Permissions) != 1 || build.Permissions["contents"] != "read") {
		t.Fatalf("build permissions = %v, want inherited read-only", build.Permissions)
	}
	install, ok := workflow.Jobs["clean-install"]
	if !ok {
		t.Fatal("release workflow lacks clean-install job")
	}
	wantInstallMatrix := []map[string]string{
		{"runner": "ubuntu-24.04", "asset": "linux_amd64"},
		{"runner": "macos-15", "asset": "darwin_arm64"},
	}
	if len(install.Strategy.Matrix.Include) != len(wantInstallMatrix) {
		t.Fatalf("clean-install matrix = %#v, want exactly %#v", install.Strategy.Matrix.Include, wantInstallMatrix)
	}
	for i, want := range wantInstallMatrix {
		got := install.Strategy.Matrix.Include[i]
		if len(got) != len(want) || got["runner"] != want["runner"] || got["asset"] != want["asset"] {
			t.Fatalf("clean-install matrix = %#v, want exactly %#v", install.Strategy.Matrix.Include, wantInstallMatrix)
		}
	}
	publish, ok := workflow.Jobs["publish"]
	if !ok {
		t.Fatal("release workflow lacks publish job")
	}
	for key, want := range map[string]string{"contents": "write", "id-token": "write", "attestations": "write"} {
		if publish.Permissions[key] != want {
			t.Fatalf("publish permission %s = %q, want %q", key, publish.Permissions[key], want)
		}
	}
	if len(publish.Permissions) != 3 {
		t.Fatalf("publish permissions = %v, want exactly contents/id-token/attestations write", publish.Permissions)
	}
	if !containsWorkflowStrings(publish.Needs, "build-reproducible", "clean-install") {
		t.Fatalf("publish needs = %v, want both build-reproducible and clean-install", publish.Needs)
	}
	for name, job := range workflow.Jobs {
		if name == "publish" {
			continue
		}
		for _, permission := range job.Permissions {
			if permission == "write" {
				t.Fatalf("non-publish job %s has write permission: %v", name, job.Permissions)
			}
		}
	}
}

func TestInactiveGitHubReleaseWorkflowProvesRealReproducibilityAndExactPublication(t *testing.T) {
	body, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		`version_re='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'`,
		`cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0`,
		`DIST="$RUNNER_TEMP/release-a"`,
		`DIST="$RUNNER_TEMP/release-b"`,
		`cmp "$RUNNER_TEMP/release-a/$name" "$RUNNER_TEMP/release-b/$name"`,
		`sha256sum --check SHA256SUMS`,
		`Clean-install exact release candidate`,
		`COMPOUND_RELEASE_TARBALL=`,
		`actions/attest-build-provenance@`,
		`Revalidate exact candidate before attestation`,
		`(cd "$candidate" && sha256sum --check SHA256SUMS)`,
		`tag_object_type=$(gh api`,
		`test "$tag_object_type" = commit`,
		`test "$remote_tag_sha" = "$GITHUB_SHA"`,
		`gh release create "$GITHUB_REF_NAME"`,
		`--draft`,
		`release_ids=$(gh api --paginate`,
		`--jq .draft`,
		`--jq .target_commitish`,
		`--jq '.assets[] | "\(.digest)  \(.name)"'`,
		`gh release edit "$GITHUB_REF_NAME" --draft=false --latest`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("release workflow missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"dist/*.tar.gz",
		"dist/*.cdx.json",
		"release-a/*.tar.gz",
		"release-a/*.cdx.json",
		"path: dist/",
		"path: $RUNNER_TEMP/release-a/",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("release workflow contains broad artifact selector %q", forbidden)
		}
	}
	if strings.Count(text, "make release-artifacts") != 2 {
		t.Fatalf("real release build count = %d, want exactly two", strings.Count(text, "make release-artifacts"))
	}
	revalidateAt := strings.Index(text, "Revalidate exact candidate before attestation")
	attestAt := strings.Index(text, "Attest every published release object")
	if revalidateAt > attestAt {
		t.Fatal("release candidate is attested before its downloaded bytes are revalidated")
	}
	createAt := strings.Index(text, `gh release create "$GITHUB_REF_NAME"`)
	inventoryAt := strings.Index(text, `--jq '.assets[] | "\(.digest)  \(.name)"'`)
	publishAt := strings.Index(text, `gh release edit "$GITHUB_REF_NAME" --draft=false --latest`)
	if !(createAt < inventoryAt && inventoryAt < publishAt) {
		t.Fatal("release is published before the exact draft inventory is checked")
	}
	for _, name := range releaseWorkflowArtifactTemplates() {
		if strings.Count(text, name) < 2 {
			t.Errorf("release artifact %q is not explicitly carried through workflow", name)
		}
	}
}

func TestInactiveGitHubReleaseWorkflowRunsCanonicalPublicGatesBeforeBuilding(t *testing.T) {
	body, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow releaseWorkflow
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	build, ok := workflow.Jobs["build-reproducible"]
	if !ok {
		t.Fatal("release workflow lacks build-reproducible job")
	}
	var runScripts strings.Builder
	for _, step := range build.Steps {
		runScripts.WriteString(step.Run)
		runScripts.WriteByte('\n')
	}
	text := runScripts.String()
	buildAt := strings.Index(text, "make release-artifacts")
	if buildAt < 0 {
		t.Fatal("release workflow does not build release artifacts")
	}
	for _, command := range []string{
		`git fetch --no-tags origin "+refs/heads/main:refs/remotes/origin/main"`,
		// The tag must be bound to main's exact TIP. Ancestry is not enough:
		// see the negative assertion below.
		`main_sha=$(git rev-parse refs/remotes/origin/main)`,
		`if [ "$main_sha" != "$GITHUB_SHA" ]; then`,
		`proof-check --ci`,
		`release export-public`,
		`gitleaks/v8@v8.30.1`,
	} {
		at := strings.Index(text, command)
		if at < 0 {
			t.Errorf("release workflow does not directly run required gate %q", command)
		} else if at > buildAt {
			t.Errorf("release workflow runs gate %q after building release artifacts", command)
		}
	}
	// Ancestry must never come back as the binding. `merge-base --is-ancestor`
	// is satisfied by ANY commit already in main's history, so it green-lights
	// tagging a months-old commit and publishing that stale tree under a fresh
	// version -- the proof run then describes code the release does not carry.
	if strings.Contains(text, "git merge-base --is-ancestor") {
		t.Error("release workflow binds the tag with `git merge-base --is-ancestor`, " +
			"which accepts any ancestor of main; require equality with main's exact tip instead")
	}
}

func TestInactiveGitHubReleaseWorkflowResumesOnlyExactDraftWithoutNetwork(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      string
		wantOK     bool
		wantCreate bool
		wantEdit   bool
	}{
		{name: "new draft", state: "absent", wantOK: true, wantCreate: true, wantEdit: true},
		{name: "exact existing draft", state: "exact", wantOK: true, wantEdit: true},
		{name: "already published", state: "published"},
		{name: "wrong tag", state: "wrong_tag"},
		{name: "wrong target", state: "wrong_target"},
		{name: "partial inventory", state: "partial"},
		{name: "unexpected inventory", state: "unexpected"},
		{name: "wrong asset digest", state: "wrong_digest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, log, err := runReleasePublishStep(t, test.state)
			if (err == nil) != test.wantOK {
				t.Fatalf("publish step success=%v, want %v: %v\n%s\nlog:\n%s", err == nil, test.wantOK, err, output, log)
			}
			created := strings.Contains(log, "release create")
			edited := strings.Contains(log, "release edit")
			if created != test.wantCreate {
				t.Fatalf("create=%v, want %v; log:\n%s", created, test.wantCreate, log)
			}
			if edited != test.wantEdit {
				t.Fatalf("publish edit=%v, want %v; log:\n%s", edited, test.wantEdit, log)
			}
		})
	}
}

func TestInactiveGitHubReleaseWorkflowCountsEveryCandidateEntryAndRejectsSymlinks(t *testing.T) {
	body, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, `-mindepth 1 -maxdepth 1 -type f | wc -l`) {
		t.Fatal("release workflow counts only regular files, allowing a symlink plus an unexpected file to preserve the count")
	}
	if strings.Count(text, `-mindepth 1 -maxdepth 1 -print | wc -l`) < 4 {
		t.Fatal("release workflow does not count every directory entry at each candidate boundary")
	}
	if strings.Count(text, `test ! -L`) < 4 {
		t.Fatal("release workflow does not reject symlinks at each candidate boundary")
	}
}

func TestGettingStartedSourceInstallIsBoundToSignedTagAndCommit(t *testing.T) {
	body, err := os.ReadFile("../../docs/getting-started.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		`VERSION=0.1.0`,
		`VERSION must be strict stable SemVer`,
		`TAG="v${VERSION}"`,
		`REPOSITORY=${COMPOUND_RELEASE_REPOSITORY:?`,
		`/releases/download/${TAG}`,
		`for ASSET in RELEASE SHA256SUMS SHA256SUMS.sig`,
		`https://raw.githubusercontent.com/${REPOSITORY}/${TAG}/release/allowed_signers`,
		`ssh-keygen -Y verify`,
		`-I compound-release`,
		`-n compound-release`,
		`SHA256:iAb0Eaq9vv9CrfNRy/YbitZVbaSpwqE0wY8uf5jftRE`,
		`sha256sum --check RELEASE.sha256`,
		`shasum -a 256 --check RELEASE.sha256`,
		`COMMIT=`,
		`git init compound`,
		`refs/tags/$TAG:refs/tags/$TAG`,
		`git rev-parse "refs/tags/$TAG^{commit}"`,
		`git checkout --detach "$COMMIT"`,
		`make install`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("source-install instructions lack immutable binding %q", required)
		}
	}
	for _, forbidden := range []string{
		`releases/latest`,
		`gh attestation verify`,
		`/${REPOSITORY}/main/`,
		`git clone `,
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("source-install instructions retain mutable selector %q", forbidden)
		}
	}
	if strings.Count(text, `https://raw.githubusercontent.com/${REPOSITORY}/${TAG}/release/allowed_signers`) != 2 || strings.Count(text, `ssh-keygen -Y verify`) != 2 {
		t.Error("both installation paths must verify the signature against the release tag's trust anchor")
	}
	source := text[strings.Index(text, "### Option B"):]
	if !(strings.Index(source, "ssh-keygen -Y verify") < strings.Index(source, "sha256sum --check") && strings.Index(source, "sha256sum --check") < strings.Index(source, "COMMIT=")) {
		t.Error("source commit must be read only after SSH and checksum verification")
	}
	if regexp.MustCompile(`github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+`).MatchString(text) {
		t.Error("source-install instructions hardcode a repository owner instead of using the explicit identity seam")
	}
}

func TestReleaseVersionHasOneImmutableSourceAuthority(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^VERSION\s*[?:+]?=`).Match(makefile) {
		t.Fatal("Makefile duplicates the release version; cmd/compound's compiled version must be the sole source authority")
	}

	mainSource, err := os.ReadFile("../../cmd/compound/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(mainSource), `const version = "`) != 1 {
		t.Fatal("cmd/compound must expose exactly one compiled version authority")
	}

	builder, err := os.ReadFile("../../scripts/release-artifacts.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`VERSION=%q is not strict stable SemVer`,
		`buildGo, "run", "./cmd/compound", "version"`,
		`does not match source version output`,
	} {
		if !strings.Contains(string(builder), required) {
			t.Errorf("release builder does not bind the requested tag version to compiled source authority via %q", required)
		}
	}
}

func TestGettingStartedDoesNotClaimAnUnpublishedArtifactExists(t *testing.T) {
	body, err := os.ReadFile("../../docs/getting-started.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"release-consumer instructions, not evidence that a release exists",
		"unreleased development build",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("getting-started guide lacks publication-status boundary %q", required)
		}
	}
}

func TestInactiveGitHubReleaseWorkflowPinsEveryThirdPartyAction(t *testing.T) {
	body, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow releaseWorkflow
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`^[^@]+@[0-9a-f]{40}$`)
	uses := 0
	for jobName, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if step.Uses == "" {
				continue
			}
			uses++
			if !pinned.MatchString(step.Uses) {
				t.Errorf("job %s action is not commit-pinned: %s", jobName, step.Uses)
			}
		}
	}
	if uses < 6 {
		t.Fatalf("parsed only %d action uses; workflow structure may be incomplete", uses)
	}
}

func TestInactiveGitHubReleaseWorkflowPublicBoundaryNamesHostedReleaseException(t *testing.T) {
	body, err := os.ReadFile("../../docs/BOUNDARIES.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	for _, required := range []string{
		"Routine CI (`.woodpecker.yml`)",
		"`.github/workflows/release.yml`",
		"read-only Linux job builds the candidate twice",
		"`ubuntu-24.04` → `linux_amd64` and `macos-15` → `darwin_arm64` jobs clean-install the exact candidate",
		"draft-publication job receives write authority",
		"historical description applies only to the inactive GitHub reference workflow",
		"GitHub-hosted Actions are disabled in this repository.",
		"Routine CI and release builds run on the owned Woodpecker agent.",
		"creates a draft",
		"Publication requires the maintainer's explicit review and click.",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("public boundary does not describe release workflow fact %q", required)
		}
	}
}

func TestGettingStartedNamesExactPrebuiltFloorAndSequentialRecoveryLimit(t *testing.T) {
	body, err := os.ReadFile("../../docs/getting-started.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	for _, required := range []string{
		"`darwin/arm64` on macOS 15 or newer",
		"`lockf -s -t 0 9`",
		"BusyBox-compatible `flock -n 9`",
		"Target publication uses two sequential renames, not an atomic pair switch.",
		"until the next installer invocation acquires the lock and deterministically restores or reconciles",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("getting-started guide omits release-install boundary %q", required)
		}
	}
}

type releaseWorkflow struct {
	Permissions map[string]string             `yaml:"permissions"`
	Jobs        map[string]releaseWorkflowJob `yaml:"jobs"`
}

type releaseWorkflowJob struct {
	RunsOn      string                  `yaml:"runs-on"`
	Needs       []string                `yaml:"needs"`
	Permissions map[string]string       `yaml:"permissions"`
	Strategy    releaseWorkflowStrategy `yaml:"strategy"`
	Steps       []releaseWorkflowStep   `yaml:"steps"`
}

type releaseWorkflowStrategy struct {
	Matrix struct {
		Include []map[string]string `yaml:"include"`
	} `yaml:"matrix"`
}

type releaseWorkflowStep struct {
	Name string `yaml:"name"`
	Uses string `yaml:"uses"`
	Run  string `yaml:"run"`
}

func runReleasePublishStep(t *testing.T, initialState string) (string, string, error) {
	t.Helper()
	body, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow releaseWorkflow
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	publish := workflow.Jobs["publish"]
	var script string
	for _, step := range publish.Steps {
		if strings.Contains(step.Name, "draft") {
			script = step.Run
			break
		}
	}
	if script == "" {
		t.Fatal("publish job lacks a draft publication step")
	}
	script = strings.ReplaceAll(script, "${{ steps.meta.outputs.version }}", "0.1.0")

	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range releaseWorkflowArtifactNames("0.1.0") {
		if err := os.WriteFile(filepath.Join(candidate, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	statePath := filepath.Join(root, "state")
	logPath := filepath.Join(root, "gh.log")
	inventoryPath := filepath.Join(root, "inventory")
	if err := os.WriteFile(statePath, []byte(initialState+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inventoryPath, []byte(strings.Join(releaseWorkflowArtifactNames("0.1.0"), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeGH := filepath.Join(fakeBin, "gh")
	if err := os.WriteFile(fakeGH, []byte(fakeGHScript), 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"RUNNER_TEMP="+root,
		"GITHUB_REPOSITORY=example.invalid/compound",
		"GITHUB_REF_NAME=v0.1.0",
		"GITHUB_SHA="+releaseFixtureCommit,
		"FAKE_GH_STATE="+statePath,
		"FAKE_GH_LOG="+logPath,
		"FAKE_GH_INVENTORY="+inventoryPath,
	)
	output, runErr := cmd.CombinedOutput()
	logBody, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return string(output), string(logBody), runErr
}

func releaseWorkflowArtifactNames(version string) []string {
	return []string{
		"RELEASE",
		"SHA256SUMS",
		"install-release.sh",
		"compound_" + version + "_darwin_arm64.tar.gz",
		"compound_" + version + "_darwin_arm64.cdx.json",
		"compound_" + version + "_darwin_arm64_compound-breadth-engine.cdx.json",
		"compound_" + version + "_linux_amd64.tar.gz",
		"compound_" + version + "_linux_amd64.cdx.json",
		"compound_" + version + "_linux_amd64_compound-breadth-engine.cdx.json",
	}
}

const fakeGHScript = `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_GH_LOG"
state=$(cat "$FAKE_GH_STATE")
case "$1" in
  api)
    case "$*" in
      *'/releases?per_page=100'*'.id'*)
        [ "$state" = absent ] || printf '42\n'
        ;;
      *'/releases/tags/'*'.id'*) printf '42\n' ;;
      *'/git/ref/tags/'*'.object.type'*) printf 'commit\n' ;;
      *'/git/ref/tags/'*'.object.sha'*) printf '%s\n' "$GITHUB_SHA" ;;
      *'/releases/42'*'.draft'*)
        [ "$state" = published ] && printf 'false\n' || printf 'true\n'
        ;;
      *'/releases/42'*'.prerelease'*) printf 'false\n' ;;
      *'/releases/42'*'.tag_name'*)
        [ "$state" = wrong_tag ] && printf 'v9.9.9\n' || printf '%s\n' "$GITHUB_REF_NAME"
        ;;
      *'/releases/42'*'.target_commitish'*)
        [ "$state" = wrong_target ] && printf '%040d\n' 0 || printf '%s\n' "$GITHUB_SHA"
        ;;
      *'/releases/42'*'.assets[].name'*)
		case "$state" in
          partial) sed '$d' "$FAKE_GH_INVENTORY" ;;
          unexpected) cat "$FAKE_GH_INVENTORY"; printf 'unexpected.bin\n' ;;
          *) cat "$FAKE_GH_INVENTORY" ;;
		esac
		;;
      *'/releases/42'*'.assets[]'*'.digest'*)
		if [ "$state" = partial ]; then
		  asset_names=$(sed '$d' "$FAKE_GH_INVENTORY")
		else
		  asset_names=$(cat "$FAKE_GH_INVENTORY")
		fi
		printf '%s\n' "$asset_names" | while IFS= read -r name; do
		  if command -v sha256sum >/dev/null 2>&1; then
		    digest=$(sha256sum "$RUNNER_TEMP/candidate/$name" | awk '{print $1}')
		  else
		    digest=$(shasum -a 256 "$RUNNER_TEMP/candidate/$name" | awk '{print $1}')
		  fi
		  if [ "$state" = wrong_digest ] && [ "$name" = RELEASE ]; then
		    digest=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
		  fi
		  printf 'sha256:%s  %s\n' "$digest" "$name"
		done
		if [ "$state" = unexpected ]; then
		  printf 'sha256:%064d  unexpected.bin\n' 0
		fi
		;;
      *) printf 'unexpected fake gh api: %s\n' "$*" >&2; exit 64 ;;
    esac
    ;;
  release)
    case "$2" in
      create)
        [ "$state" = absent ] || exit 70
        printf 'exact\n' > "$FAKE_GH_STATE"
        ;;
      view)
        case "$*" in
          *'--json isDraft'*) [ "$state" = published ] && printf 'false\n' || printf 'true\n' ;;
          *'--json assets'*) cat "$FAKE_GH_INVENTORY" ;;
          *) printf 'unexpected fake gh release view: %s\n' "$*" >&2; exit 64 ;;
        esac
        ;;
      edit)
        [ "$state" = exact ] || exit 71
        printf 'published\n' > "$FAKE_GH_STATE"
        ;;
      *) printf 'unexpected fake gh release call: %s\n' "$*" >&2; exit 64 ;;
    esac
    ;;
  *) printf 'unexpected fake gh call: %s\n' "$*" >&2; exit 64 ;;
esac
`

func workflowIncludesRunner(job releaseWorkflowJob, runner string) bool {
	for _, row := range job.Strategy.Matrix.Include {
		if row["runner"] == runner {
			return true
		}
	}
	return false
}

func containsWorkflowStrings(values []string, wants ...string) bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	for _, want := range wants {
		if !set[want] {
			return false
		}
	}
	return true
}

func releaseWorkflowArtifactTemplates() []string {
	return []string{
		"RELEASE",
		"SHA256SUMS",
		"install-release.sh",
		"compound_${version}_darwin_arm64.tar.gz",
		"compound_${version}_darwin_arm64.cdx.json",
		"compound_${version}_darwin_arm64_compound-breadth-engine.cdx.json",
		"compound_${version}_linux_amd64.tar.gz",
		"compound_${version}_linux_amd64.cdx.json",
		"compound_${version}_linux_amd64_compound-breadth-engine.cdx.json",
	}
}
