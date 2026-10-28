package work

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

const remoteProofMaxInputs = 200000

func remoteProofPathValid(name string) bool {
	return name != "" && name != "." && !strings.HasPrefix(name, "/") &&
		path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../") &&
		!strings.ContainsAny(name, "\\\x00\u2028\u2029")
}

func remoteProofRootFor(name string, roots []string) string {
	for _, root := range roots {
		if name == root || strings.HasPrefix(name, root+"/") {
			return root
		}
	}
	return ""
}

func validateRemoteProofRoots(roots []string) error {
	if len(roots) == 0 || !slices.IsSorted(roots) {
		return fmt.Errorf("remote roots must be nonempty and sorted")
	}
	for i, root := range roots {
		if !remoteProofPathValid(root) || strings.Contains(strings.ToLower("/"+root+"/"), "/.git/") {
			return fmt.Errorf("invalid remote input root %q", root)
		}
		for _, prior := range roots[:i] {
			if root == prior || strings.HasPrefix(root, prior+"/") {
				return fmt.Errorf("remote input roots overlap")
			}
		}
	}
	return nil
}

// The local ceremony owns these full trees, including archived .git fixtures
// strictly below a declared root. Input roots never own source Git metadata.
func validateRemoteProofInputs(roots []string, entries []RemoteProofInput) error {
	if err := validateRemoteProofRoots(roots); err != nil {
		return err
	}
	if len(entries) > remoteProofMaxInputs {
		return fmt.Errorf("remote input manifest exceeds %d entries", remoteProofMaxInputs)
	}
	seen := make(map[string]RemoteProofInput, len(entries))
	for i, entry := range entries {
		name := entry.Path
		if !remoteProofPathValid(name) || i > 0 && entries[i-1].Path >= name {
			return fmt.Errorf("remote input paths must be safe, sorted and unique: %q", name)
		}
		root := remoteProofRootFor(name, roots)
		if root == "" {
			return fmt.Errorf("remote input %q has no declared owner", name)
		}
		if name != root {
			parent, ok := seen[path.Dir(name)]
			if !ok || parent.Kind != "directory" {
				return fmt.Errorf("remote input %q lacks a directory parent", name)
			}
		}
		switch entry.Kind {
		case "absent", "directory":
			if entry.Kind == "absent" && name != root {
				return fmt.Errorf("only entire remote roots may be absent")
			}
			if entry.SHA256 != "" || entry.Executable != nil || entry.Target != "" {
				return fmt.Errorf("unexpected remote %s metadata", entry.Kind)
			}
		case "file":
			if !remoteHex(entry.SHA256, 64) || entry.Executable == nil || entry.Target != "" {
				return fmt.Errorf("invalid remote file metadata")
			}
		case "symlink":
			if name == root || entry.SHA256 != "" || entry.Executable != nil || entry.Target == "" ||
				strings.HasPrefix(entry.Target, "/") || strings.ContainsAny(entry.Target, "\\\x00\u2028\u2029") {
				return fmt.Errorf("invalid remote symlink metadata")
			}
			target := path.Join(path.Dir(name), entry.Target)
			if remoteProofRootFor(target, []string{root}) != root {
				return fmt.Errorf("remote symlink escapes its declared root")
			}
		default:
			return fmt.Errorf("unsupported remote input kind %q", entry.Kind)
		}
		seen[name] = entry
	}
	for _, root := range roots {
		if _, ok := seen[root]; !ok {
			return fmt.Errorf("remote manifest omits root %q", root)
		}
	}
	return nil
}
