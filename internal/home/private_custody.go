package home

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const PrivateCustodyReportSchema = "compound.private_custody_report.v0.1"

var privateCustodyKnownDirs = []string{"private", "private/artifacts", "private/artifacts/sha256"}

type PrivateStageRecovery struct {
	Name        string
	ExpectedRef string
}

type PrivateCustodyOptions struct {
	DryRun, Repair bool
	RecoverStages  []PrivateStageRecovery
}

type privateCustodyHooks struct {
	beforeRootOpen      func()
	beforeRootChmodOpen func()
	beforeChmodOpen     func(string)
	beforeStageLink     func(string)
	beforeStageUnlink   func(string)
	syncRecoveryDir     func(*os.Root, string, string, string) error
	beforeArtifactLock  func()
}

type PrivateCustodyReport struct {
	SchemaVersion string                  `json:"schema_version"`
	Root          string                  `json:"root"`
	Status        string                  `json:"status"`
	OK            bool                    `json:"ok"`
	Changed       bool                    `json:"changed"`
	DryRun        bool                    `json:"dry_run"`
	Counts        PrivateCustodyCounts    `json:"counts"`
	Findings      []PrivateCustodyFinding `json:"findings,omitempty"`
	Actions       []PrivateCustodyAction  `json:"actions,omitempty"`
}

type PrivateCustodyCounts struct {
	Checked  int `json:"checked"`
	OK       int `json:"ok"`
	Drift    int `json:"drift"`
	Manual   int `json:"manual"`
	Repaired int `json:"repaired"`
}

type PrivateCustodyFinding struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Action   string `json:"action"`
	Detail   string `json:"detail"`
}

type PrivateCustodyAction struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func DiagnosePrivateCustody(homePath string) PrivateCustodyReport {
	return PrivateCustody(homePath, PrivateCustodyOptions{})
}
func RepairPrivateCustody(homePath string, opts PrivateCustodyOptions) PrivateCustodyReport {
	opts.Repair = true
	return PrivateCustody(homePath, opts)
}

func PrivateCustody(homePath string, opts PrivateCustodyOptions) PrivateCustodyReport {
	return privateCustodyWithHooks(homePath, opts, privateCustodyHooks{})
}

func privateCustodyWithHooks(homePath string, opts PrivateCustodyOptions, hooks privateCustodyHooks) PrivateCustodyReport {
	rawHome := strings.TrimSpace(homePath)
	absHome, absErr := filepath.Abs(filepath.Clean(rawHome))
	if absErr == nil {
		homePath = absHome
	}
	r := PrivateCustodyReport{SchemaVersion: PrivateCustodyReportSchema, Root: filepath.Join(homePath, "private", "artifacts", "sha256"), OK: true, Status: "ok", DryRun: opts.DryRun}
	if rawHome == "" || absErr != nil {
		if absErr == nil {
			absErr = fmt.Errorf("home path is required")
		}
		r.addFinding(homePath, "home_root", "manual", "stable current-user-owned directory", "invalid", "manual", absErr.Error())
		r.finish(opts)
		return r
	}
	if err := validatePrivateStageSelections(opts.RecoverStages); err != nil {
		r.addFinding(r.Root, "private_stage_recovery", "manual", "exact stage and digest selection", "invalid", "manual", err.Error())
		r.finish(opts)
		return r
	}
	root, err := openStableOwnedHomeRootPermissive(homePath, hooks)
	if err != nil {
		r.addFinding(absDisplay(homePath, "."), "home_root", "manual", "stable current-user-owned directory mode=0700", "unsafe", "manual", err.Error())
		r.finish(opts)
		return r
	}
	defer root.Close()
	if !checkPrivateHomeRoot(&r, root, homePath, opts, hooks) {
		r.finish(opts)
		return r
	}
	safe := true
	for _, rel := range privateCustodyKnownDirs {
		if !safe {
			break
		}
		safe = checkPrivateDir(&r, root, homePath, rel, opts, hooks)
	}
	if safe {
		if len(opts.RecoverStages) == 0 || !opts.Repair {
			checkPrivateObjects(&r, root, homePath, opts, hooks)
		} else {
			if hooks.beforeArtifactLock != nil {
				hooks.beforeArtifactLock()
			}
			acquired, err := withPrivateArtifactDirectoryLock(homePath, true, func(lockedRoot *os.Root) error {
				before, beforeErr := root.Stat(filepath.FromSlash(privateArtifactRelativeDir))
				locked, lockedErr := lockedRoot.Stat(".")
				if err := errors.Join(beforeErr, lockedErr); err != nil {
					return err
				}
				if !os.SameFile(before, locked) {
					return fmt.Errorf("private custody home generation changed before artifact lock")
				}
				checkPrivateObjectsAt(&r, lockedRoot, homePath, opts, hooks, ".")
				return nil
			})
			if err != nil || !acquired {
				detail := "private artifact writer is active"
				if err != nil {
					detail = err.Error()
				}
				r.addFinding(r.Root, "private_stage_recovery", "manual", "exclusive private artifact custody", "busy_or_unsafe", "retry", detail)
			}
		}
	}
	r.finish(opts)
	return r
}

func openStableOwnedHomeRoot(homePath string, hooks privateCustodyHooks) (*os.Root, error) {
	root, err := openStableOwnedHomeRootPermissive(homePath, hooks)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if info.Mode().Perm() != 0o700 {
		_ = root.Close()
		return nil, fmt.Errorf("compound home root must be mode 0700: mode=%04o", info.Mode().Perm())
	}
	return root, nil
}

func openStableOwnedHomeRootPermissive(homePath string, hooks privateCustodyHooks) (*os.Root, error) {
	before, err := os.Lstat(homePath)
	if err != nil {
		return nil, err
	}
	if err := validateHomeRootInfo(before); err != nil {
		return nil, err
	}
	if hooks.beforeRootOpen != nil {
		hooks.beforeRootOpen()
	}
	root, err := os.OpenRoot(homePath)
	if err != nil {
		return nil, err
	}
	opened, openErr := root.Stat(".")
	after, afterErr := os.Lstat(homePath)
	if openErr != nil || afterErr != nil {
		_ = root.Close()
		return nil, errors.Join(openErr, afterErr)
	}
	if err := validateHomeRootInfo(opened); err != nil {
		_ = root.Close()
		return nil, err
	}
	if err := validateHomeRootInfo(after); err != nil {
		_ = root.Close()
		return nil, err
	}
	if !os.SameFile(before, opened) || !os.SameFile(before, after) {
		_ = root.Close()
		return nil, fmt.Errorf("compound home root identity changed while opening: %s", homePath)
	}
	return root, nil
}

func validateHomeRootInfo(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("compound home root must be a non-symlink directory")
	}
	if !ownedByCurrent(info) {
		return fmt.Errorf("compound home root must be owned by the current uid: %s", ownerModeString(info))
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("compound home root must not be group/world writable: mode=%04o", info.Mode().Perm())
	}
	return nil
}

func secureHomeRootForInit(homePath string) (*os.Root, os.FileInfo, error) {
	if _, err := os.Lstat(homePath); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(homePath, 0o700); err != nil {
			return nil, nil, fmt.Errorf("mkdir home root: %w", err)
		}
		// Mkdir is umask-filtered. Tighten the freshly created leaf before it
		// is opened or populated so even a restrictive umask has one exact
		// private home-root contract.
		if err := os.Chmod(homePath, 0o700); err != nil {
			return nil, nil, fmt.Errorf("chmod fresh home root: %w", err)
		}
	} else if err != nil {
		return nil, nil, err
	}
	before, err := os.Lstat(homePath)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, nil, fmt.Errorf("compound home root must be a non-symlink directory")
	}
	if !ownedByCurrent(before) {
		return nil, nil, fmt.Errorf("compound home root must be owned by the current uid: %s", ownerModeString(before))
	}
	root, err := os.OpenRoot(homePath)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*os.Root, os.FileInfo, error) {
		_ = root.Close()
		return nil, nil, err
	}
	opened, openErr := root.Stat(".")
	after, afterErr := os.Lstat(homePath)
	if err := errors.Join(openErr, afterErr); err != nil {
		return fail(err)
	}
	for _, info := range []os.FileInfo{opened, after} {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedByCurrent(info) {
			return fail(fmt.Errorf("compound home root must remain a current-user-owned non-symlink directory"))
		}
	}
	if !os.SameFile(before, opened) || !os.SameFile(before, after) {
		return fail(fmt.Errorf("compound home root identity changed while opening: %s", homePath))
	}
	if opened.Mode().Perm() == 0o700 {
		return root, opened, nil
	}
	dir, err := root.Open(".")
	if err != nil {
		return fail(err)
	}
	openedInfo, statErr := dir.Stat()
	names, readErr := dir.Readdirnames(1)
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if err := errors.Join(statErr, readErr); err != nil {
		_ = dir.Close()
		return fail(err)
	}
	if len(names) != 0 {
		_ = dir.Close()
		if opened.Mode().Perm()&0o022 != 0 {
			return fail(fmt.Errorf("existing populated home root is group/world writable (mode %04o); inspect it manually before migration", opened.Mode().Perm()))
		}
		return fail(fmt.Errorf("existing populated home root must be mode 0700 (got %04o); run compound home private-custody repair", opened.Mode().Perm()))
	}
	chmodErr := dir.Chmod(0o700)
	syncErr := dir.Sync()
	closeErr := dir.Close()
	finalOpened, finalOpenErr := root.Stat(".")
	finalPath, finalPathErr := os.Lstat(homePath)
	if err := errors.Join(chmodErr, syncErr, closeErr, finalOpenErr, finalPathErr); err != nil {
		return fail(err)
	}
	if openedInfo == nil || finalOpened == nil || finalPath == nil || !os.SameFile(opened, openedInfo) || !os.SameFile(opened, finalOpened) || !os.SameFile(opened, finalPath) || !ownedByCurrent(finalPath) || finalOpened.Mode().Perm() != 0o700 || finalPath.Mode().Perm() != 0o700 {
		return fail(fmt.Errorf("compound home root identity or mode changed while securing empty root"))
	}
	return root, finalOpened, nil
}

func checkPrivateHomeRoot(r *PrivateCustodyReport, root *os.Root, homePath string, opts PrivateCustodyOptions, hooks privateCustodyHooks) bool {
	info, err := root.Stat(".")
	if err != nil {
		r.addFinding(homePath, "home_root", "manual", "current-user-owned dir mode=0700", "unsafe", "manual", err.Error())
		return false
	}
	if info.Mode().Perm() == 0o700 {
		r.addFinding(homePath, "home_root", "ok", "mode=0700", "mode=0700", "none", "")
		return true
	}
	r.addFinding(homePath, "home_root", "drift", "mode=0700", fmt.Sprintf("mode=%04o", info.Mode().Perm()), "chmod", "permission drift exposes operational home bodies")
	finding := len(r.Findings) - 1
	if !opts.Repair {
		return false
	}
	if opts.DryRun {
		r.addAction("chmod", homePath, "home_root", "dry_run", "would chmod 0700")
		return false
	}
	if hooks.beforeRootChmodOpen != nil {
		hooks.beforeRootChmodOpen()
	}
	opened, err := root.Open(".")
	if err != nil {
		r.Findings[finding].Status = "manual"
		r.Findings[finding].Action = "manual"
		r.Findings[finding].Detail = err.Error()
		return false
	}
	openedInfo, statErr := opened.Stat()
	chmodErr := opened.Chmod(0o700)
	syncErr := opened.Sync()
	closeErr := opened.Close()
	after, afterErr := os.Lstat(homePath)
	if err := errors.Join(statErr, chmodErr, syncErr, closeErr, afterErr); err != nil || openedInfo == nil || after == nil || !os.SameFile(info, openedInfo) || !os.SameFile(info, after) || !ownedByCurrent(after) || after.Mode().Perm() != 0o700 {
		if err == nil {
			err = fmt.Errorf("compound home root identity or mode changed during repair")
		}
		r.Findings[finding].Status = "manual"
		r.Findings[finding].Action = "manual"
		r.Findings[finding].Detail = err.Error()
		return false
	}
	r.Findings[finding].Status = "repaired"
	r.Findings[finding].Actual = "mode=0700"
	r.Findings[finding].Detail = "permission repaired"
	r.addAction("chmod", homePath, "home_root", "repaired", "chmod 0700")
	return true
}

func validateExistingHomeForPrivateInit(homePath string) error {
	info, err := os.Lstat(homePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return validateHomeRootInfo(info)
}

func initializePrivateCustodyDirsRooted(root *os.Root, homePath string) error {
	for _, rel := range privateCustodyKnownDirs {
		relPath := filepath.FromSlash(rel)
		display := absDisplay(homePath, rel)
		infos, err := lstatRootedDirectoryPath(root, relPath, display, "private custody", true)
		if err != nil {
			return err
		}
		if err := requireCurrentOwnerPath(infos, display); err != nil {
			return err
		}
		if len(infos) == len(strings.Split(rel, "/")) {
			// Init is additive. Existing permission drift is diagnosed by
			// doctor and changed only by the explicit repair command.
			continue
		}
		if err := requireSafePrivateParent(infos, display); err != nil {
			return err
		}
		if err := createRootedPrivateDirectory(root, relPath, display, privateCustodyHooks{}); err != nil {
			return err
		}
	}
	return nil
}

func (r *PrivateCustodyReport) finish(opts PrivateCustodyOptions) {
	for _, f := range r.Findings {
		switch f.Status {
		case "manual":
			r.OK = false
			r.Counts.Manual++
		case "drift":
			r.OK = false
			r.Counts.Drift++
		case "ok":
			r.Counts.OK++
		case "repaired":
			r.Counts.OK++
		}
	}
	for _, a := range r.Actions {
		if a.Status == "repaired" {
			r.Changed = true
			r.Counts.Repaired++
		} else if a.Status == "partial" {
			r.Changed = true
		}
	}
	switch {
	case r.Counts.Manual > 0:
		r.Status = "manual"
	case opts.DryRun && len(r.Actions) > 0:
		r.Status = "dry_run"
	case r.Counts.Drift > 0:
		r.Status = "drift"
	case r.Changed:
		r.Status = "repaired"
	default:
		r.Status = "ok"
	}
}

func checkPrivateDir(r *PrivateCustodyReport, root *os.Root, homePath, rel string, opts PrivateCustodyOptions, hooks privateCustodyHooks) bool {
	display, relPath := absDisplay(homePath, rel), filepath.FromSlash(rel)
	r.Counts.Checked++
	infos, err := lstatRootedDirectoryPath(root, relPath, display, "private custody", true)
	if err != nil {
		r.addFinding(display, "known_dir", "manual", "dir mode=0700", "unsafe_path", "manual", err.Error())
		return false
	}
	if err := requireCurrentOwnerPath(infos, display); err != nil {
		r.addFinding(display, "known_dir", "manual", "current-user-owned dir mode=0700", "foreign_owner", "manual", err.Error())
		return false
	}
	if len(infos) < len(strings.Split(rel, "/")) {
		if !opts.Repair {
			r.addFinding(display, "known_dir", "drift", "dir mode=0700", "missing", "mkdir", "known private directory missing")
			return true
		}
		if opts.DryRun {
			r.addFinding(display, "known_dir", "drift", "dir mode=0700", "missing", "mkdir", "known private directory missing")
			r.addAction("mkdir", display, "mkdir", "dry_run", "would create 0700 directory")
			return true
		}
		if err := requireSafePrivateParent(infos, display); err != nil {
			r.addFinding(display, "known_dir", "manual", "dir mode=0700", "unsafe_parent", "manual", err.Error())
			return false
		}
		if err := createRootedPrivateDirectory(root, relPath, display, hooks); err != nil {
			r.addFinding(display, "known_dir", "manual", "dir mode=0700", "missing", "manual", err.Error())
			return false
		}
		r.addFinding(display, "known_dir", "repaired", "dir mode=0700", "dir mode=0700", "mkdir", "created private directory")
		r.addAction("mkdir", display, "mkdir", "repaired", "created 0700 directory")
		return true
	}
	info := infos[len(infos)-1]
	if info.Mode().Perm() != 0o700 {
		return repairRootedChmod(r, root, relPath, display, "known_dir", 0o700, info.Mode().Perm(), true, opts, hooks)
	}
	r.addFinding(display, "known_dir", "ok", "dir mode=0700", "dir mode=0700", "none", "")
	return true
}

func requireCurrentOwnerPath(infos []os.FileInfo, display string) error {
	for _, info := range infos {
		if !ownedByCurrent(info) {
			return fmt.Errorf("private path component is not owned by the current uid: %s (%s)", display, ownerModeString(info))
		}
	}
	return nil
}

func requireSafePrivateParent(infos []os.FileInfo, display string) error {
	for _, info := range infos {
		if !ownedByCurrent(info) {
			return fmt.Errorf("private parent is not owned by the current uid: %s", display)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("private parent is group/world writable: %s (%04o)", display, info.Mode().Perm())
		}
	}
	return nil
}

func createRootedPrivateDirectory(root *os.Root, rel, display string, hooks privateCustodyHooks) error {
	if err := root.Mkdir(rel, 0o700); err != nil {
		return err
	}
	cleanup := func(cause error) error {
		removeErr := root.Remove(rel)
		syncErr := syncRootedDirectory(root, filepath.Dir(rel), display, "private custody")
		return errors.Join(cause, removeErr, syncErr)
	}
	// Mkdir is umask-filtered. This leaf was just created beneath a pinned
	// private root, so make it traversable at the exact private mode before
	// descriptor-based validation.
	if err := root.Chmod(rel, 0o700); err != nil {
		return cleanup(err)
	}
	if err := syncRootedDirectory(root, filepath.Dir(rel), display, "private custody"); err != nil {
		return cleanup(err)
	}
	if err := rootedChmodStable(root, rel, display, "private custody", 0o700, true, hooks); err != nil {
		return cleanup(err)
	}
	return nil
}

func checkPrivateObjects(r *PrivateCustodyReport, root *os.Root, homePath string, opts PrivateCustodyOptions, hooks privateCustodyHooks) {
	checkPrivateObjectsAt(r, root, homePath, opts, hooks, filepath.FromSlash(privateArtifactRelativeDir))
}

func checkPrivateObjectsAt(r *PrivateCustodyReport, root *os.Root, homePath string, opts PrivateCustodyOptions, hooks privateCustodyHooks, rootRel string) {
	displayRoot := absDisplay(homePath, privateArtifactRelativeDir)
	if _, err := lstatRootedDirectoryPath(root, rootRel, displayRoot, "private custody", false); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.addFinding(displayRoot, "private_digest_root", "manual", "readable dir", "unreadable", "manual", err.Error())
		}
		return
	}
	dir, err := root.Open(rootRel)
	if err != nil {
		r.addFinding(displayRoot, "private_digest_root", "manual", "readable dir", "unopenable", "manual", err.Error())
		return
	}
	names, readErr := dir.Readdirnames(-1)
	closeErr := dir.Close()
	if readErr != nil || closeErr != nil {
		r.addFinding(displayRoot, "private_digest_root", "manual", "readable dir", "read_error", "manual", errors.Join(readErr, closeErr).Error())
		return
	}
	sort.Strings(names)
	selected := make(map[string]PrivateStageRecovery, len(opts.RecoverStages))
	for _, stage := range opts.RecoverStages {
		selected[stage.Name] = stage
	}
	for _, name := range names {
		rel := filepath.Join(rootRel, name)
		display := filepath.Join(displayRoot, name)
		r.Counts.Checked++
		if stage, ok := selected[name]; ok {
			delete(selected, name)
			recoverSelectedPrivateStage(r, root, rootRel, homePath, stage, opts, hooks)
			continue
		}
		if !validPrivateDigestName(name) {
			r.addFinding(display, "private_digest_object", "manual", "64 lowercase hex regular mode=0600", name, "manual", "invalid digest object name")
			continue
		}
		info, err := stableDigestInfo(root, rel, display, name)
		if err != nil {
			r.addFinding(display, "private_digest_object", "manual", "regular sha256 matches filename mode=0600", "invalid", "manual", err.Error())
			continue
		}
		if !ownedByCurrent(info) {
			r.addFinding(display, "private_digest_object", "manual", "regular mode=0600", ownerModeString(info), "manual", "owner is not the current uid or cannot be verified")
			continue
		}
		if info.Mode().Perm() != 0o600 {
			repairRootedChmod(r, root, rel, display, "private_digest_object", 0o600, info.Mode().Perm(), false, opts, hooks)
			continue
		}
		r.addFinding(display, "private_digest_object", "ok", "regular mode=0600", "regular mode=0600", "none", "")
	}
	for _, stage := range opts.RecoverStages {
		if _, ok := selected[stage.Name]; ok {
			r.Counts.Checked++
			recoverSelectedPrivateStage(r, root, rootRel, homePath, stage, opts, hooks)
		}
	}
}

func stableDigestInfo(root *os.Root, rel, display, digest string) (os.FileInfo, error) {
	before, err := lstatRootedRegularPath(root, rel, display, "private custody")
	if err != nil {
		return nil, err
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	after, err := lstatRootedRegularPath(root, rel, display, "private custody")
	if err != nil {
		return nil, err
	}
	if !sameRootedPath(before, after) || !opened.Mode().IsRegular() || !os.SameFile(before[len(before)-1], opened) {
		return nil, fmt.Errorf("digest object path must be stable regular file")
	}
	if !singleLinkPrivateObject(opened) {
		return nil, fmt.Errorf("digest object must have exactly one filesystem link")
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return nil, err
	}
	final, err := lstatRootedRegularPath(root, rel, display, "private custody")
	if err != nil {
		return nil, err
	}
	if !sameRootedPath(before, final) || !os.SameFile(opened, final[len(final)-1]) {
		return nil, fmt.Errorf("digest object path changed while hashing")
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return nil, fmt.Errorf("content hash does not match digest filename")
	}
	return opened, nil
}

func repairRootedChmod(r *PrivateCustodyReport, root *os.Root, rel, display, kind string, want, got os.FileMode, isDir bool, opts PrivateCustodyOptions, hooks privateCustodyHooks) bool {
	r.addFinding(display, kind, "drift", fmt.Sprintf("mode=%04o", want), fmt.Sprintf("mode=%04o", got), "chmod", "permission drift")
	finding := len(r.Findings) - 1
	if !opts.Repair {
		return got&0o022 == 0
	}
	if opts.DryRun {
		r.addAction("chmod", display, "chmod", "dry_run", fmt.Sprintf("would chmod %04o", want))
		return got&0o022 == 0
	}
	if err := rootedChmodStable(root, rel, display, kind, want, isDir, hooks); err != nil {
		r.Findings[finding].Status = "manual"
		r.Findings[finding].Action = "manual"
		r.Findings[finding].Detail = err.Error()
		return false
	}
	if !isDir {
		info, err := stableDigestInfo(root, rel, display, filepath.Base(rel))
		if err != nil || !ownedByCurrent(info) || info.Mode().Perm() != want.Perm() {
			if err == nil {
				err = fmt.Errorf("private digest object state changed after chmod")
			}
			r.Findings[finding].Status = "manual"
			r.Findings[finding].Action = "manual"
			r.Findings[finding].Detail = err.Error()
			return false
		}
	}
	r.Findings[finding].Status = "repaired"
	r.Findings[finding].Actual = fmt.Sprintf("mode=%04o", want)
	r.Findings[finding].Detail = "permission repaired"
	r.addAction("chmod", display, "chmod", "repaired", fmt.Sprintf("chmod %04o", want))
	return true
}

func rootedChmodStable(root *os.Root, rel, display, kind string, want os.FileMode, isDir bool, hooks privateCustodyHooks) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("descriptor chmod unsupported on %s", runtime.GOOS)
	}
	var before []os.FileInfo
	var err error
	if isDir {
		before, err = lstatRootedDirectoryPath(root, rel, display, kind, false)
	} else {
		before, err = lstatRootedRegularPath(root, rel, display, kind)
	}
	if err != nil {
		return err
	}
	if hooks.beforeChmodOpen != nil {
		hooks.beforeChmodOpen(rel)
	}
	file, err := root.Open(rel)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !os.SameFile(before[len(before)-1], opened) {
		_ = file.Close()
		return fmt.Errorf("%s path identity changed before chmod: %s", kind, display)
	}
	if !ownedByCurrent(opened) {
		_ = file.Close()
		return fmt.Errorf("%s target is not owned by the current uid: %s", kind, display)
	}
	if !isDir && !singleLinkPrivateObject(opened) {
		_ = file.Close()
		return fmt.Errorf("%s target must have exactly one filesystem link: %s", kind, display)
	}
	if err := file.Chmod(want); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	var after []os.FileInfo
	if isDir {
		after, err = lstatRootedDirectoryPath(root, rel, display, kind, false)
	} else {
		after, err = lstatRootedRegularPath(root, rel, display, kind)
	}
	if err != nil {
		return err
	}
	if !sameRootedPath(before, after) {
		return fmt.Errorf("%s path identity changed after chmod: %s", kind, display)
	}
	if !ownedByCurrent(after[len(after)-1]) || after[len(after)-1].Mode().Perm() != want.Perm() {
		return fmt.Errorf("%s target state changed after chmod: %s", kind, display)
	}
	return nil
}

func validPrivateDigestName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func (r *PrivateCustodyReport) addFinding(path, kind, status, expected, actual, action, detail string) {
	r.Findings = append(r.Findings, PrivateCustodyFinding{ID: findingID(kind, path, actual), Path: path, Kind: kind, Status: status, Expected: expected, Actual: actual, Action: action, Detail: detail})
}
func (r *PrivateCustodyReport) addAction(actionKind, path, kind, status, detail string) {
	r.Actions = append(r.Actions, PrivateCustodyAction{ID: actionID(actionKind, path), Path: path, Kind: kind, Status: status, Detail: detail})
}
func findingID(kind, path, actual string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + filepath.ToSlash(path) + "\x00" + actual))
	return "private-custody-" + hex.EncodeToString(sum[:])[:12]
}
func actionID(kind, path string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + filepath.ToSlash(path)))
	return "private-custody-action-" + hex.EncodeToString(sum[:])[:12]
}
func absDisplay(homePath, rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(homePath, filepath.FromSlash(rel))
}
func modeString(info fs.FileInfo) string {
	m := info.Mode()
	parts := []string{fmt.Sprintf("mode=%04o", m.Perm())}
	if m&os.ModeSymlink != 0 {
		parts = append(parts, "symlink")
	} else if info.IsDir() {
		parts = append(parts, "dir")
	} else if m.IsRegular() {
		parts = append(parts, "regular")
	} else {
		parts = append(parts, "nonregular")
	}
	sort.Strings(parts[1:])
	return strings.Join(parts, " ")
}
func ownerModeString(info os.FileInfo) string {
	uid, ok := fileOwnerUID(info)
	if !ok {
		return modeString(info) + " owner=unknown"
	}
	return fmt.Sprintf("%s owner_uid=%d", modeString(info), uid)
}
