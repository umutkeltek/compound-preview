package work

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"compound/internal/authority/gitrepository"
)

var installedPackageToken = regexp.MustCompile(`^[a-z0-9][a-z0-9+@._-]*$`)

func readInstalledPackageSnapshot(r InstalledPackageDeliveryCaptureRequest, target string) (installedPackageSnapshot, []byte, error) {
	x := installedPackageSnapshot{Manager: r.Manager, Prefix: r.Prefix, Package: r.Package, Version: r.Version, TargetPath: target}
	if r.Manager != "homebrew-cask" || !installedPackageToken.MatchString(r.Package) || r.Version == "" || strings.TrimSpace(r.Version) != r.Version {
		return x, nil, fmt.Errorf("unsupported installed package identity")
	}
	if !filepath.IsAbs(r.Prefix) || filepath.Clean(r.Prefix) != r.Prefix || !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return x, nil, fmt.Errorf("package prefix and target must be canonical absolute paths")
	}
	canonical, err := filepath.EvalSymlinks(r.Prefix)
	if err != nil || canonical != r.Prefix {
		return x, nil, fmt.Errorf("package prefix must not contain symlinks")
	}
	repo, found, err := canonicalGitRootForPath(r.Prefix)
	if err != nil || !found || repo != r.Prefix {
		return x, nil, fmt.Errorf("package prefix must be its exact owner Git root")
	}
	// Recognize the owner layout, never arbitrary ignored directories in a source checkout.
	for _, path := range []string{"bin/brew", "Library/Homebrew/brew.sh"} {
		b, e := privateDeliveryGit(repo, "ls-tree", "--name-only", "HEAD", "--", path)
		if e != nil || strings.TrimSpace(string(b)) != path {
			return x, nil, fmt.Errorf("package prefix lacks tracked Homebrew owner")
		}
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return x, nil, err
	}
	defer root.Close()
	rel, err := filepath.Rel(repo, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return x, nil, fmt.Errorf("package target outside owner prefix")
	}
	infos, err := privateDeliveryPathInfos(root, rel)
	if err != nil || !infos[len(infos)-1].IsDir() {
		return x, nil, fmt.Errorf("package target must be a real directory without symlink parents")
	}
	receiptPath := filepath.Join("Caskroom", r.Package, ".metadata", "INSTALL_RECEIPT.json")
	for _, path := range []string{rel, receiptPath} {
		if _, e := privateDeliveryPathInfos(root, path); e != nil {
			return x, nil, e
		}
		for _, args := range [][]string{{"ls-files", "--cached", "-z", "--", path}, {"ls-tree", "-r", "--name-only", "-z", "HEAD", "--", path}} {
			b, e := privateDeliveryGit(repo, args...)
			if e != nil || len(b) != 0 {
				return x, nil, fmt.Errorf("installed package footprint is tracked or unreadable")
			}
		}
		if _, e := privateDeliveryGit(repo, "check-ignore", "-q", "--", path); e != nil {
			return x, nil, fmt.Errorf("installed package footprint must be ignored by its owner")
		}
	}
	var receiptBody strings.Builder
	digest, err := privateDeliveryDigestTo(root, receiptPath, &receiptBody)
	if err != nil || receiptBody.Len() > privateDeliveryMetadataLimit {
		return x, nil, fmt.Errorf("invalid package receipt: %v", err)
	}
	body := []byte(receiptBody.String())
	var receipt homebrewInstalledReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		return x, nil, err
	}
	if receipt.HomebrewVersion == "" || receipt.Source.Tap != "homebrew/cask" || receipt.Source.Version != r.Version || !homebrewReceiptOwnsDirectory(receipt, filepath.ToSlash(rel)) {
		return x, nil, fmt.Errorf("installed receipt does not own this exact copied directory and version")
	}
	ref, err := privateDeliveryGit(repo, "rev-parse", "HEAD")
	if err != nil {
		return x, nil, err
	}
	x.RepositoryRef = strings.TrimSpace(string(ref))
	identity, err := gitrepository.ReadIdentity(repo, x.RepositoryRef)
	if err != nil {
		return x, nil, err
	}
	x.RepositoryFingerprint = identity.Fingerprint
	x.ReceiptRef = "artifact:sha256:" + digest
	x.TreeSHA256, x.FileCount, x.Bytes, err = installedPackageTree(target)
	return x, body, err
}

func homebrewReceiptOwnsDirectory(receipt homebrewInstalledReceipt, relative string) bool {
	type location struct {
		Base string `json:"base"`
		Path string `json:"path"`
	}
	type step struct {
		Type      string   `json:"type"`
		Recursive bool     `json:"recursive"`
		Source    location `json:"source"`
		Target    location `json:"target"`
	}
	for _, artifact := range receipt.Artifacts {
		var groups []struct {
			Steps []step `json:"steps"`
		}
		if json.Unmarshal(artifact["preflight_steps"], &groups) != nil {
			continue
		}
		for _, group := range groups {
			for _, s := range group.Steps {
				if s.Type == "copy" && s.Recursive && s.Source.Base == "staged_path" && s.Source.Path != "" && s.Target.Base == "homebrew_prefix" && s.Target.Path == relative {
					return true
				}
			}
		}
	}
	return false
}
