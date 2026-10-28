package home

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func validatePrivateStageSelections(stages []PrivateStageRecovery) error {
	seen := make(map[string]struct{}, len(stages))
	for _, stage := range stages {
		if !validPrivateStageName(stage.Name) {
			return fmt.Errorf("invalid private artifact stage name %q", stage.Name)
		}
		if _, err := ArtifactHex(stage.ExpectedRef); err != nil {
			return fmt.Errorf("stage %q requires an exact artifact:sha256 reference: %w", stage.Name, err)
		}
		if _, exists := seen[stage.Name]; exists {
			return fmt.Errorf("duplicate private artifact stage %q", stage.Name)
		}
		seen[stage.Name] = struct{}{}
	}
	return nil
}

func validPrivateStageName(name string) bool {
	if filepath.Base(name) != name || !strings.HasPrefix(name, ".artifact-") || len(name) <= len(".artifact-") {
		return false
	}
	for _, char := range name[len(".artifact-"):] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

type stablePrivateRecoveryFile struct {
	info   os.FileInfo
	bytes  int64
	digest string
}

func readStablePrivateRecoveryFile(root *os.Root, rel, display string) (stablePrivateRecoveryFile, error) {
	before, err := lstatRootedRegularPath(root, rel, display, "private artifact recovery")
	if err != nil {
		return stablePrivateRecoveryFile{}, err
	}
	file, err := root.Open(rel)
	if err != nil {
		return stablePrivateRecoveryFile{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return stablePrivateRecoveryFile{}, err
	}
	after, err := lstatRootedRegularPath(root, rel, display, "private artifact recovery")
	if err != nil {
		return stablePrivateRecoveryFile{}, err
	}
	if !sameRootedPath(before, after) || !os.SameFile(before[len(before)-1], opened) {
		return stablePrivateRecoveryFile{}, fmt.Errorf("private artifact path changed before read: %s", display)
	}
	if !ownedByCurrent(opened) {
		return stablePrivateRecoveryFile{}, fmt.Errorf("private artifact must be current-user-owned: %s", display)
	}
	if opened.Mode().Perm() != 0o600 {
		return stablePrivateRecoveryFile{}, fmt.Errorf("private artifact object has non-private mode: got %04o want 0600: %s", opened.Mode().Perm(), display)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return stablePrivateRecoveryFile{}, err
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return stablePrivateRecoveryFile{}, err
	}
	final, err := lstatRootedRegularPath(root, rel, display, "private artifact recovery")
	if err != nil {
		return stablePrivateRecoveryFile{}, err
	}
	if !sameRootedPath(before, final) || !os.SameFile(opened, openedAfter) || opened.Size() != n || openedAfter.Size() != n || !opened.ModTime().Equal(openedAfter.ModTime()) {
		return stablePrivateRecoveryFile{}, fmt.Errorf("private artifact path or bytes changed while reading: %s", display)
	}
	return stablePrivateRecoveryFile{info: openedAfter, bytes: n, digest: hex.EncodeToString(hash.Sum(nil))}, nil
}

func recoverSelectedPrivateStage(r *PrivateCustodyReport, root *os.Root, dirRel, homePath string, stage PrivateStageRecovery, opts PrivateCustodyOptions, hooks privateCustodyHooks) {
	stageRel := filepath.Join(dirRel, stage.Name)
	stageDisplay := filepath.Join(homePath, filepath.FromSlash(privateArtifactRelativeDir), stage.Name)
	digest, _ := ArtifactHex(stage.ExpectedRef) // selections are validated before any home mutation
	digestRel := filepath.Join(dirRel, digest)
	digestDisplay := filepath.Join(homePath, filepath.FromSlash(privateArtifactRelativeDir), digest)
	manual := func(err error) {
		r.addFinding(stageDisplay, "private_stage_recovery", "manual", stage.ExpectedRef, "unrecovered", "retry", err.Error())
	}
	source, err := readStablePrivateRecoveryFile(root, stageRel, stageDisplay)
	if errors.Is(err, os.ErrNotExist) {
		if info, verifyErr := stableDigestInfo(root, digestRel, digestDisplay, digest); verifyErr == nil && ownedByCurrent(info) && info.Mode().Perm() == 0o600 {
			if opts.Repair && !opts.DryRun {
				syncDir := syncRootedDirectoryStrict
				if hooks.syncRecoveryDir != nil {
					syncDir = hooks.syncRecoveryDir
				}
				if syncErr := syncDir(root, dirRel, digestDisplay, "private artifact recovery"); syncErr != nil {
					manual(fmt.Errorf("verify absent-stage digest durability: %w", syncErr))
					return
				}
			}
			r.addFinding(stageDisplay, "private_stage_recovery", "ok", stage.ExpectedRef, "already_recovered", "none", "stage absent; exact private digest verified")
		} else {
			manual(fmt.Errorf("selected stage absent and exact private digest unavailable or invalid: %v", verifyErr))
		}
		return
	}
	if err != nil {
		manual(err)
		return
	}
	links, ok := privateObjectLinkCount(source.info)
	if !ok || links < 1 || links > 2 {
		manual(fmt.Errorf("selected stage must have one link, or two links to its digest: %s", stageDisplay))
		return
	}
	if source.digest != digest {
		manual(fmt.Errorf("selected stage content hash differs from expected reference: %s", stageDisplay))
		return
	}
	if links == 2 {
		digestFile, digestErr := readStablePrivateRecoveryFile(root, digestRel, digestDisplay)
		if digestErr != nil || !os.SameFile(source.info, digestFile.info) || digestFile.digest != digest {
			manual(fmt.Errorf("two-link stage is not linked to its exact private digest: %s: %v", stageDisplay, digestErr))
			return
		}
	}
	if !opts.Repair || opts.DryRun {
		r.addFinding(stageDisplay, "private_stage_recovery", "drift", stage.ExpectedRef, fmt.Sprintf("stage bytes=%d", source.bytes), "recover", "observed bytes are not proof the original write completed")
		if opts.Repair && opts.DryRun {
			r.addAction("recover", stageDisplay, "private_stage_recovery", "dry_run", stage.ExpectedRef)
		}
		return
	}
	changed, err := publishSelectedPrivateStage(root, dirRel, homePath, stage, source, hooks)
	if err != nil {
		manual(err)
		if changed {
			r.addAction("recover", stageDisplay, "private_stage_recovery", "partial", stage.ExpectedRef)
		}
		return
	}
	r.addFinding(stageDisplay, "private_stage_recovery", "repaired", stage.ExpectedRef, "exact bytes custodied", "none", fmt.Sprintf("recovered %d observed bytes; original write completion unknown", source.bytes))
	r.addAction("recover", stageDisplay, "private_stage_recovery", "repaired", stage.ExpectedRef)
}

func publishSelectedPrivateStage(root *os.Root, dirRel, homePath string, stage PrivateStageRecovery, source stablePrivateRecoveryFile, hooks privateCustodyHooks) (changed bool, resultErr error) {
	stageRel := filepath.Join(dirRel, stage.Name)
	stageDisplay := filepath.Join(homePath, filepath.FromSlash(privateArtifactRelativeDir), stage.Name)
	digest, _ := ArtifactHex(stage.ExpectedRef)
	digestRel := filepath.Join(dirRel, digest)
	digestDisplay := filepath.Join(homePath, filepath.FromSlash(privateArtifactRelativeDir), digest)
	if hooks.beforeStageLink != nil {
		hooks.beforeStageLink(stageRel)
	}
	current, err := readStablePrivateRecoveryFile(root, stageRel, stageDisplay)
	if err != nil || !os.SameFile(source.info, current.info) || current.digest != digest || current.bytes != source.bytes {
		return false, fmt.Errorf("selected stage changed before publication: %s: %v", stageDisplay, err)
	}
	if err := syncStablePrivateRecoverySource(root, stageRel, stageDisplay, source.info); err != nil {
		return false, err
	}
	linkErr := root.Link(stageRel, digestRel)
	if linkErr != nil && !errors.Is(linkErr, os.ErrExist) {
		return false, fmt.Errorf("publish private artifact digest: %w", linkErr)
	}
	if linkErr == nil {
		changed = true
	}
	published, err := readStablePrivateRecoveryFile(root, digestRel, digestDisplay)
	if err != nil || published.digest != digest || published.bytes != source.bytes {
		return changed, fmt.Errorf("published private artifact failed verification: %s: %v", digestDisplay, err)
	}
	stageLinks, stageLinksOK := privateObjectLinkCount(current.info)
	digestLinks, digestLinksOK := privateObjectLinkCount(published.info)
	if !stageLinksOK || !digestLinksOK || (os.SameFile(source.info, published.info) && digestLinks != 2) || (!os.SameFile(source.info, published.info) && (stageLinks != 1 || digestLinks != 1)) {
		return changed, fmt.Errorf("private artifact recovery found unexpected hard links: %s", stageDisplay)
	}
	syncDir := syncRootedDirectoryStrict
	if hooks.syncRecoveryDir != nil {
		syncDir = hooks.syncRecoveryDir
	}
	if err := syncDir(root, dirRel, digestDisplay, "private artifact recovery"); err != nil {
		return changed, fmt.Errorf("persist private artifact digest before stage removal: %w", err)
	}
	if hooks.beforeStageUnlink != nil {
		hooks.beforeStageUnlink(stageRel)
	}
	current, err = readStablePrivateRecoveryFile(root, stageRel, stageDisplay)
	if err != nil || !os.SameFile(source.info, current.info) || current.digest != digest || current.bytes != source.bytes {
		return changed, fmt.Errorf("selected stage changed before removal: %s: %v", stageDisplay, err)
	}
	if err := root.Remove(stageRel); err != nil {
		return changed, fmt.Errorf("remove recovered private stage: %w", err)
	}
	changed = true
	if err := syncDir(root, dirRel, stageDisplay, "private artifact recovery"); err != nil {
		return changed, fmt.Errorf("persist recovered stage removal: %w", err)
	}
	if info, err := stableDigestInfo(root, digestRel, digestDisplay, digest); err != nil || !ownedByCurrent(info) || info.Mode().Perm() != 0o600 {
		return changed, fmt.Errorf("verify final private artifact custody: %s: %v", digestDisplay, err)
	}
	return changed, nil
}

func syncStablePrivateRecoverySource(root *os.Root, rel, display string, expected os.FileInfo) error {
	file, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(expected, opened) {
		return fmt.Errorf("selected stage changed before sync: %s: %v", display, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync selected private stage: %w", err)
	}
	final, err := lstatRootedRegularPath(root, rel, display, "private artifact recovery")
	if err != nil {
		return err
	}
	openedAfter, err := file.Stat()
	if err != nil || !os.SameFile(expected, final[len(final)-1]) || !os.SameFile(expected, openedAfter) || openedAfter.Size() != expected.Size() || !openedAfter.ModTime().Equal(expected.ModTime()) {
		return fmt.Errorf("selected stage changed during sync: %s: %v", display, err)
	}
	return nil
}
