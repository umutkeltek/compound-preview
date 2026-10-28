package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type woodpeckerReleaseStep struct {
	Image     string   `yaml:"image"`
	DependsOn []string `yaml:"depends_on"`
	When      []struct {
		Event    string `yaml:"event"`
		Evaluate string `yaml:"evaluate"`
	} `yaml:"when"`
	BackendOptions struct {
		Docker struct {
			User string `yaml:"user"`
		} `yaml:"docker"`
	} `yaml:"backend_options"`
	Volumes  []string `yaml:"volumes"`
	Commands []string `yaml:"commands"`
}

func woodpeckerReleaseSteps(t *testing.T) map[string]woodpeckerReleaseStep {
	t.Helper()
	body, err := os.ReadFile("../../.woodpecker.yml")
	if err != nil {
		t.Fatal(err)
	}
	var pipeline struct {
		Steps map[string]woodpeckerReleaseStep `yaml:"steps"`
	}
	if err := yaml.Unmarshal(body, &pipeline); err != nil {
		t.Fatal(err)
	}
	return pipeline.Steps
}

func TestWoodpeckerReleaseGatesAndDependencyChain(t *testing.T) {
	steps := woodpeckerReleaseSteps(t)
	if len(steps) != 7 {
		t.Fatalf("got %d steps, want four proof steps plus three release steps", len(steps))
	}
	for _, name := range []string{"prepare-bun", "proof-gates", "console-gates", "secret-scan"} {
		if len(steps[name].When) != 0 {
			t.Errorf("routine gate %s became conditional", name)
		}
	}
	previous := "secret-scan"
	for _, name := range []string{"release-build", "release-linux-drill", "release-stage"} {
		step := steps[name]
		if !reflect.DeepEqual(step.DependsOn, []string{previous}) {
			t.Errorf("%s dependencies = %v, want %s", name, step.DependsOn, previous)
		}
		if len(step.When) != 1 || step.When[0].Event != "manual" || step.When[0].Evaluate != `RELEASE_VERSION != ""` {
			t.Errorf("%s must AND manual event and nonempty release variable in one condition: %+v", name, step.When)
		}
		script := strings.ReplaceAll(strings.Join(step.Commands, "\n"), "$$", "$")
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(script)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s shell syntax: %v\n%s", name, err, output)
		}
		previous = name
	}
	for _, name := range []string{"release-linux-drill", "release-stage"} {
		if steps[name].BackendOptions.Docker.User != "1000:1000" || steps[name].Image != "docker.io/library/node:22-bookworm" {
			t.Errorf("%s must use Node 22 and uid 1000", name)
		}
	}
	if !reflect.DeepEqual(steps["release-stage"].Volumes, []string{"/srv/appdata/woodpecker/releases:/releases"}) {
		t.Fatal("release stage must use the owned bind mount")
	}
}

func TestWoodpeckerReleaseBuildAndDrillContract(t *testing.T) {
	steps := woodpeckerReleaseSteps(t)
	build := strings.Join(steps["release-build"].Commands, "\n")
	for _, want := range []string{
		`setpriv --reuid=1000 --regid=1000 --clear-groups`,
		`test "$CI_COMMIT_SHA" = "$main_sha"`,
		`test "$(git rev-parse HEAD)" = "$CI_COMMIT_SHA"`,
		`git fetch --no-tags origin "+refs/heads/main:refs/remotes/origin/main"`,
		`go mod verify`, `release export-public --repo "$CI_WORKSPACE"`,
		`tar -tzf`, `cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0`,
		`for build in a b; do`, `VERSION="$RELEASE_VERSION" SOURCE_ROOT="$CI_WORKSPACE" DIST="$root/$build"`,
		`cmp "$root/a/SHA256SUMS" "$root/b/SHA256SUMS"`,
		`cmp "$root/a/$name" "$root/b/$name"`, `sha256sum --check SHA256SUMS`,
		`-mindepth 1 -maxdepth 1 -print | wc -l)" -eq 9`, `test ! -L`,
		`git rev-parse HEAD~1`, `git archive "$prior_commit"`,
		`git -C "$root/prior-source" reset --mixed "$prior_commit"`,
		`prior_version=$(cd "$root/prior-source" && go run ./cmd/compound version`,
		`VERSION="$prior_version" SOURCE_ROOT="$root/prior-source"`,
	} {
		if !strings.Contains(build, want) {
			t.Errorf("build lacks %q", want)
		}
	}
	if strings.Contains(build, "--is-ancestor") || strings.Contains(build, "0.0.0-prior") {
		t.Fatal("build permits stale main or invents an unsupported prior version")
	}
	drill := strings.Join(steps["release-linux-drill"].Commands, "\n")
	for _, want := range []string{`COMPOUND_RELEASE_TARBALL=`, `COMPOUND_HOME=`, `GOBIN=`, `compound $RELEASE_VERSION (build $CI_COMMIT_SHA)`, `compound-breadth-engine`, `node scripts/installed-release-acceptance.mjs`, `"$root/prior" "$(cat "$root/PRIOR_COMMIT")"`} {
		if !strings.Contains(drill, want) {
			t.Errorf("Linux drill lacks %q", want)
		}
	}
}

func TestWoodpeckerReleaseStageRefusesExistingCandidate(t *testing.T) {
	script := strings.Join(woodpeckerReleaseSteps(t)["release-stage"].Commands, "\n")
	for _, existing := range []string{"absent", "directory", "symlink"} {
		t.Run(existing, func(t *testing.T) {
			root := t.TempDir()
			stage := filepath.Join(root, "releases")
			candidate := filepath.Join(root, ".ci", "release", "a")
			tools := filepath.Join(root, "tools")
			for _, dir := range []string{stage, candidate, tools} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range releaseWorkflowArtifactNames("0.1.0") {
				if name != "SHA256SUMS" {
					if err := os.WriteFile(filepath.Join(candidate, name), []byte(name+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Use real checksums on both supported test hosts; only uid is synthetic.
			for name, body := range map[string]string{"id": "#!/bin/sh\nprintf '1000\\n'\n", "sha256sum": "#!/bin/sh\nexec shasum -a 256 \"$@\"\n"} {
				if err := os.WriteFile(filepath.Join(tools, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			checksum := exec.Command("sh", "-c", "shasum -a 256 * > SHA256SUMS")
			checksum.Dir = candidate
			if output, err := checksum.CombinedOutput(); err != nil {
				t.Fatalf("checksum: %v %s", err, output)
			}
			destination := filepath.Join(stage, "0.1.0")
			if existing == "directory" {
				if err := os.Mkdir(destination, 0700); err != nil {
					t.Fatal(err)
				}
			} else if existing == "symlink" {
				if err := os.Symlink(candidate, destination); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "-c", strings.ReplaceAll(script, `destination="/releases/`, `destination="`+stage+`/`))
			cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"), "CI_WORKSPACE="+root, "CI_PIPELINE_EVENT=manual", "RELEASE_VERSION=0.1.0", "CI_COMMIT_SHA="+releaseFixtureCommit, "CI_PIPELINE_NUMBER=42")
			output, err := cmd.CombinedOutput()
			if existing != "absent" {
				if err == nil || !strings.Contains(string(output), "release candidate already exists") {
					t.Fatalf("overwrote existing %s: %v %s", existing, err, output)
				}
				if _, err := os.Stat(filepath.Join(destination, "CANDIDATE")); !os.IsNotExist(err) {
					t.Fatal("refused candidate was modified")
				}
				return
			}
			if err != nil {
				t.Fatalf("stage: %v %s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(destination, "CANDIDATE"))
			if err != nil || !strings.HasPrefix(string(body), fmt.Sprintf("version 0.1.0\ncommit %s\npipeline 42\nsha256sums_sha256 ", releaseFixtureCommit)) {
				t.Fatalf("candidate identity: %s, %v", body, err)
			}
		})
	}
}
