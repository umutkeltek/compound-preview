package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalPublisherKeepsSigningAndPublicationGates(t *testing.T) {
	path := "../../scripts/release-publish-local.sh"
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		`set -euo pipefail`, `${COMPOUND_RELEASE_STAGE:?`, `Darwin/arm64`,
		`git fetch origin main --tags`, `git cat-file -t "refs/tags/$tag"`,
		`git rev-parse "refs/tags/$tag^{commit}"`, `git rev-parse refs/remotes/origin/main`,
		`git ls-remote --tags origin`, `git worktree add --detach`,
		`cmp "$root/expected-candidate" "$staged/CANDIDATE"`,
		`cmp "$root/local/SHA256SUMS" "$staged/SHA256SUMS"`,
		`installed-release-acceptance.mjs`, `"$root/prior" "$prior_commit"`,
		`allowed_signers="$source/release/allowed_signers"`,
		`ssh-keygen -Y sign`, `ssh-keygen -Y verify -f "$allowed_signers" -I compound-release -n compound-release -s SHA256SUMS.sig < SHA256SUMS`,
		`--draft --verify-tag`, `--notes-file`, `--jq .draft`, `== true`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("publisher lacks %q", want)
		}
	}
	for _, forbidden := range []string{"--draft=false", "gh release edit", "git push", "--clobber"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("publisher contains forbidden mutation %q", forbidden)
		}
	}
	if !(strings.Index(script, "cmp \"$root/local/SHA256SUMS\"") < strings.Index(script, "installed-release-acceptance.mjs") && strings.Index(script, "installed-release-acceptance.mjs") < strings.Index(script, "ssh-keygen -Y sign") && strings.Index(script, "ssh-keygen -Y verify") < strings.Index(script, "gh release create")) {
		t.Fatal("publisher must compare, drill, sign, verify, then create draft")
	}
	if output, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("publisher syntax: %v\n%s", err, output)
	}
	for _, version := range []string{"0.0.0-prior", "01.2.3", "../bad", "0.1.0"} {
		cmd := exec.Command("bash", path, version)
		cmd.Env = append(os.Environ(), "COMPOUND_RELEASE_STAGE=")
		output, err := cmd.CombinedOutput()
		if err == nil || (version == "0.1.0" && !strings.Contains(string(output), "COMPOUND_RELEASE_STAGE")) || (version != "0.1.0" && !strings.Contains(string(output), "strict stable SemVer")) {
			t.Errorf("version/stage preflight %q: %v %s", version, err, output)
		}
	}
}

func TestLocalPublisherRefusesUnboundTagsBeforeTransfer(t *testing.T) {
	script, err := filepath.Abs("../../scripts/release-publish-local.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"lightweight", "old_main", "remote_mismatch"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			fakeGit := `#!/bin/sh
case "$*" in
  'fetch '*) exit 0 ;;
  'cat-file -t '*) if [ "$TAG_STATE" = lightweight ]; then echo commit; else echo tag; fi ;;
  'rev-parse refs/tags/v0.1.0^{commit}') printf '%040d\n' 1 ;;
  'rev-parse refs/remotes/origin/main') if [ "$TAG_STATE" = old_main ]; then printf '%040d\n' 2; else printf '%040d\n' 1; fi ;;
  'rev-parse refs/tags/v0.1.0') printf '%040d\n' 3 ;;
  'ls-remote '*) printf '%040d\trefs/tags/v0.1.0\n%040d\trefs/tags/v0.1.0^{}\n' 4 1 ;;
  *) echo "unexpected git: $*" >&2; exit 99 ;;
esac
`
			for name, body := range map[string]string{
				"git":   fakeGit,
				"uname": "#!/bin/sh\ncase $1 in -s) echo Darwin;; -m) echo arm64;; esac\n",
				"scp":   "#!/bin/sh\ntouch \"$TRANSFER_MARKER\"\nexit 99\n",
				"go":    "#!/bin/sh\nexit 0\n", "make": "#!/bin/sh\nexit 0\n",
				"node": "#!/bin/sh\nexit 0\n", "ssh-keygen": "#!/bin/sh\nexit 0\n",
				"gh": "#!/bin/sh\nexit 99\n", "shasum": "#!/bin/sh\nexit 0\n",
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(root, "transferred")
			cmd := exec.Command("bash", script, "0.1.0")
			cmd.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "COMPOUND_RELEASE_STAGE=publisher@example.invalid:/releases", "TAG_STATE="+state, "TRANSFER_MARKER="+marker)
			output, err := cmd.CombinedOutput()
			want := map[string]string{"lightweight": "tag must exist and be annotated", "old_main": "tag must equal the exact origin/main tip", "remote_mismatch": "remote annotated tag differs"}[state]
			if err == nil || !strings.Contains(string(output), want) {
				t.Fatalf("tag refusal %s: %v %s", state, err, output)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("transferred candidate before tag checks passed")
			}
		})
	}
}
