package work

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRemoteProofManifestOwnershipAndShape(t *testing.T) {
	no := false
	roots := []string{"owned"}
	valid := []RemoteProofInput{
		{Path: "owned", Kind: "directory"},
		{Path: "owned/.git", Kind: "directory"},
		{Path: "owned/.git/config", Kind: "file", SHA256: strings.Repeat("1", 64), Executable: &no},
		{Path: "owned/bin", Kind: "directory"},
		{Path: "owned/bin/link", Kind: "symlink", Target: "../.git/config"},
	}
	if err := validateRemoteProofInputs(roots, valid); err != nil {
		t.Fatal("owned nested metadata and internal relative link:", err)
	}
	cases := map[string]func([]RemoteProofInput) []RemoteProofInput{
		"duplicate":            func(v []RemoteProofInput) []RemoteProofInput { return append(v, v[len(v)-1]) },
		"unsorted":             func(v []RemoteProofInput) []RemoteProofInput { v[1], v[2] = v[2], v[1]; return v },
		"unowned":              func(v []RemoteProofInput) []RemoteProofInput { v[0].Path = ".git"; return v },
		"traversal":            func(v []RemoteProofInput) []RemoteProofInput { v[2].Path = "owned/.git/../../outside"; return v },
		"absolute":             func(v []RemoteProofInput) []RemoteProofInput { v[0].Path = "/owned"; return v },
		"backslash":            func(v []RemoteProofInput) []RemoteProofInput { v[2].Path = "owned/.git\\config"; return v },
		"missing-parent":       func(v []RemoteProofInput) []RemoteProofInput { return append(v[:1], v[2:]...) },
		"absent-parent":        func(v []RemoteProofInput) []RemoteProofInput { v[0].Kind = "absent"; return v },
		"absent-child":         func(v []RemoteProofInput) []RemoteProofInput { v[1].Kind = "absent"; return v },
		"root-link":            func(v []RemoteProofInput) []RemoteProofInput { v[0].Kind = "symlink"; v[0].Target = "."; return v },
		"escaping-link":        func(v []RemoteProofInput) []RemoteProofInput { v[4].Target = "../../outside"; return v },
		"absolute-link":        func(v []RemoteProofInput) []RemoteProofInput { v[4].Target = "/outside"; return v },
		"empty-link":           func(v []RemoteProofInput) []RemoteProofInput { v[4].Target = ""; return v },
		"bad-hash":             func(v []RemoteProofInput) []RemoteProofInput { v[2].SHA256 = "short"; return v },
		"missing-mode":         func(v []RemoteProofInput) []RemoteProofInput { v[2].Executable = nil; return v },
		"extra-file-target":    func(v []RemoteProofInput) []RemoteProofInput { v[2].Target = "x"; return v },
		"extra-directory-hash": func(v []RemoteProofInput) []RemoteProofInput { v[1].SHA256 = strings.Repeat("1", 64); return v },
		"missing-root":         func(v []RemoteProofInput) []RemoteProofInput { return v[1:] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateRemoteProofInputs(roots, mutate(slices.Clone(valid))); err == nil {
				t.Fatal("invalid input manifest accepted")
			}
		})
	}
	for _, roots := range [][]string{{".git"}, {"owned/.Git"}, {"owned", "owned"}, {"owned", "owned/sub"}, {"z", "a"}} {
		if err := validateRemoteProofRoots(roots); err == nil {
			t.Fatal("invalid roots accepted", roots)
		}
	}
	if err := validateRemoteProofInputs(roots, make([]RemoteProofInput, remoteProofMaxInputs+1)); err == nil {
		t.Fatal("unbounded manifest accepted")
	}
	reservation, _ := remoteProofTestFixture(t)
	d := *reservation.Profile.RemoteGoTest
	d.Inputs = append(d.Inputs, RemoteProofInput{Path: "outside", Kind: "absent"})
	d.InputsDigest, _ = remoteInputsDigest(d.InputRoots, d.Inputs)
	d.Digest = ""
	d.Digest, _ = remoteDigest(d)
	if err := validateRemoteProofDescriptor(d); err == nil {
		t.Fatal("resealed but unowned descriptor accepted")
	}
}

func remoteProofGitFixture(t *testing.T) (string, RemoteProofRequest) {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.name", "Synthetic Proof")
	git("config", "user.email", "proof@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("docs_private/\nconsole/node_modules/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore")
	git("commit", "-qm", "synthetic source")
	owned := filepath.Join(repo, "docs_private", "plan", "archived", ".git")
	if err := os.MkdirAll(owned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "config"), []byte("opaque fixture bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git/config", filepath.Join(filepath.Dir(owned), "pointer")); err != nil {
		t.Fatal(err)
	}
	request := RemoteProofRequest{HeadSHA: git("rev-parse", "HEAD"), SourceTree: git("rev-parse", "HEAD^{tree}"), InputRoots: slices.Clone(ProofCeremonyOwnedIgnoredInputs)}
	slices.Sort(request.InputRoots)
	var err error
	request.Inputs, _, err = remoteProofManifest(context.Background(), repo, request.InputRoots, exec.CommandContext)
	if err != nil {
		t.Fatal(err)
	}
	return repo, request
}

func TestRemoteProofRealBundleArchivePreservesOwnedInputs(t *testing.T) {
	repo, request := remoteProofGitFixture(t)
	before, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	archive, cleanup, _, counts, err := remoteProofArchive(context.Background(), repo, request, exec.CommandContext)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if counts["archive"] <= 0 || counts["file_payload"] <= 0 {
		t.Fatal("missing payload accounting", counts)
	}
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	seen := map[string]bool{}
	bundle := filepath.Join(t.TempDir(), "source.bundle")
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[h.Name] = true
		switch h.Name {
		case "source.bundle":
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bundle, body, 0600); err != nil {
				t.Fatal(err)
			}
		case "request.json":
			var got RemoteProofRequest
			if err := json.NewDecoder(tr).Decode(&got); err != nil || !reflect.DeepEqual(got, request) {
				t.Fatalf("request changed: %v", err)
			}
		case "inputs/docs_private/plan/archived/.git/config":
			body, err := io.ReadAll(tr)
			if err != nil || string(body) != "opaque fixture bytes\n" {
				t.Fatal("nested metadata changed")
			}
		case "inputs/docs_private/plan/archived/pointer":
			if h.Typeflag != tar.TypeSymlink || h.Linkname != ".git/config" {
				t.Fatal("link changed")
			}
		}
		if h.Name != "request.json" && h.Name != "source.bundle" && !strings.HasPrefix(h.Name, "inputs/") {
			t.Fatal("unexpected archive namespace", h.Name)
		}
	}
	for _, name := range []string{"request.json", "source.bundle", "inputs/docs_private/plan/archived/.git/config", "inputs/docs_private/plan/archived/pointer"} {
		if !seen[name] {
			t.Fatal("missing", name)
		}
	}
	out, err := exec.Command("git", "bundle", "list-heads", bundle).CombinedOutput()
	if err != nil || !strings.Contains(string(out), request.HeadSHA+" HEAD") {
		t.Fatalf("named HEAD absent: %s %v", out, err)
	}
	after, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("source Git metadata changed")
	}
	if err := os.WriteFile(filepath.Join(repo, "docs_private/plan/archived/.git/config"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := remoteProofArchive(context.Background(), repo, request, exec.CommandContext); err == nil {
		t.Fatal("changed input accepted")
	}
}

func TestRemoteProofManifestRejectsRootAndEscapingSymlinks(t *testing.T) {
	for _, kind := range []string{"root", "escape", "chain"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			root := filepath.Join(repo, "owned")
			if kind == "root" {
				if err := os.Symlink(t.TempDir(), root); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../outside", filepath.Join(root, "out")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, "outside"), []byte("outside"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "chain" {
					if err := os.Symlink("out", filepath.Join(root, "first")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, _, err := remoteProofManifest(context.Background(), repo, []string{"owned"}, exec.CommandContext); err == nil {
				t.Fatal("escaping/root symlink accepted")
			}
		})
	}
}
