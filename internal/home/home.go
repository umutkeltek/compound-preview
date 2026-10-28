// Package home owns COMPOUND_HOME — the runtime body. The public repo is source;
// generated runtime state (ledger, work, context, receipts, transitions, indexes,
// projections, inbox candidates, artifacts) lives under the home, NEVER the repo
// source tree. The layout is the guardrail: durable (ledger) vs candidate (inbox)
// vs rebuildable view (projections) vs rented (indexes) vs content-addressed
// (artifacts). See docs/architecture/runtime.md.
package home

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// ManifestName is the home's identity file; its presence marks a real home.
const ManifestName = "manifest.yaml"

// SchemaVersion of the home manifest.
const SchemaVersion = "compound.home.v0.1"

// ManifestKind is the fixed discriminator for a Compound home identity. The
// home_kind field remains placement/profile metadata and is deliberately not
// an identity discriminator.
const ManifestKind = "compound_home"

const (
	HomeConfigSchemaVersion = "compound.home_config.v0.1"
	BackupFreshnessMaxAge   = 48 * time.Hour
)

var (
	initAfterRootSecured = func(string) error { return nil }
	initRootFlock        = syscall.Flock
)

// Manifest is the home's identity record.
type Manifest struct {
	SchemaVersion  string   `yaml:"schema_version"`
	Kind           string   `yaml:"kind,omitempty"`
	HomeKind       string   `yaml:"home_kind"`
	CreatedAt      string   `yaml:"created_at"`
	ActiveModules  []string `yaml:"active_modules"`
	LegacyIdentity bool     `yaml:"-"`
}

// LoadManifest reads the identity record without mutating the home.
func LoadManifest(homePath string) (Manifest, error) {
	data, err := readManifestNoFollowBounded(filepath.Join(homePath, ManifestName))
	if err != nil {
		return Manifest{}, err
	}
	return decodeHomeManifest(data)
}

// HomeConfig is the optional COMPOUND_HOME/config/home.yaml operator config.
// Absence is normal for fresh installs; doctor checks that depend on it fail open.
type HomeConfig struct {
	SchemaVersion string `yaml:"schema_version,omitempty"`
	BackupDir     string `yaml:"backup_dir,omitempty"`
}

func HomeConfigPath(homePath string) string {
	return filepath.Join(homePath, "config", "home.yaml")
}

func LoadHomeConfig(homePath string) (HomeConfig, string, error) {
	path := HomeConfigPath(homePath)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return HomeConfig{}, path, nil
	}
	if err != nil {
		return HomeConfig{}, path, err
	}
	var cfg HomeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return HomeConfig{}, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.SchemaVersion != "" && cfg.SchemaVersion != HomeConfigSchemaVersion {
		return HomeConfig{}, path, fmt.Errorf("unsupported home config schema %q", cfg.SchemaVersion)
	}
	cfg.SchemaVersion = HomeConfigSchemaVersion
	return cfg, path, nil
}

// ResolutionSource identifies which input selected COMPOUND_HOME.
type ResolutionSource string

const (
	ResolutionSourceFlag          ResolutionSource = "flag"
	ResolutionSourceEnvironment   ResolutionSource = "environment"
	ResolutionSourceEnrolledLocal ResolutionSource = "enrolled-local"
	ResolutionSourceGlobalDefault ResolutionSource = "global-default"
)

const (
	discoveryLegacyHomeKind = "local_monolith"
	discoveryLocalHomeKind  = "local"
	manifestMaxBytes        = 64 * 1024
)

// Resolution describes a COMPOUND_HOME resolution decision.
type Resolution struct {
	Path   string
	Source ResolutionSource
}

// Resolve returns the authoritative COMPOUND_HOME path using strict precedence:
//  1. flagHome (--home)
//  2. $COMPOUND_HOME
//  3. ~/.compound (the global default)
//
// It deliberately does not adopt an ancestor .compound marker. Repository-local
// homes carry mutable authority (policy, harness, storage, ledgers); selecting
// one must be explicit at the composition root (for example --local or --home),
// never ambiently controlled by the current working directory.
func Resolve(flagHome string) (string, error) {
	resolution, err := ResolveHome(flagHome)
	if err != nil {
		return "", err
	}
	return resolution.Path, nil
}

// ResolveHome returns the authoritative COMPOUND_HOME path and provenance.
// Explicit flag/env paths are resolved without manifest validation so existing
// callers can diagnose bad homes in their normal command flow. Ancestor
// .compound markers are never authority here; see DiscoverAncestorHome for the
// non-authoritative compatibility API.
func ResolveHome(flagHome string) (Resolution, error) {
	if s := strings.TrimSpace(flagHome); s != "" {
		p, err := filepath.Abs(s)
		return Resolution{Path: p, Source: ResolutionSourceFlag}, err
	}
	if s := strings.TrimSpace(os.Getenv("COMPOUND_HOME")); s != "" {
		p, err := filepath.Abs(s)
		return Resolution{Path: p, Source: ResolutionSourceEnvironment}, err
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{Path: filepath.Join(h, ".compound"), Source: ResolutionSourceGlobalDefault}, nil
}

// DiscoverAncestorHome returns the nearest ancestor .compound/manifest.yaml
// for non-authoritative compatibility and read-only discovery workflows.
// Callers must not use this to select a mutating/policy/harness/storage home;
// use Resolve (or an explicit --local/--home selection at the CLI composition
// root) for authority.
func DiscoverAncestorHome() string {
	return findAncestorHome()
}

// LocalPath is <cwd>/.compound — the target for `home init --local`.
func LocalPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, ".compound"), nil
}

func findAncestorHome() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		cand := filepath.Join(dir, ".compound")
		if err := validateDiscoverableLocalManifest(filepath.Join(cand, ManifestName)); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// validateDiscoverableLocalManifest recognizes only the historical local
// markers used by the explicitly non-authoritative DiscoverAncestorHome
// bridge. Runtime identity validation is broader: any nonblank home_kind is
// profile metadata once the fixed schema_version and kind are compatible.
func validateDiscoverableLocalManifest(path string) error {
	data, err := readManifestNoFollowBounded(path)
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported home schema %q", manifest.SchemaVersion)
	}
	if manifest.HomeKind != discoveryLegacyHomeKind && manifest.HomeKind != discoveryLocalHomeKind {
		return fmt.Errorf("unsupported discoverable local home kind %q", manifest.HomeKind)
	}
	return nil
}

func readManifestNoFollowBounded(path string) ([]byte, error) {
	f, err := openManifestNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("manifest %s is not a regular file", path)
	}
	if fi.Size() > manifestMaxBytes {
		return nil, fmt.Errorf("manifest %s exceeds %d byte limit", path, manifestMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, manifestMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > manifestMaxBytes {
		return nil, fmt.Errorf("manifest %s exceeds %d byte limit", path, manifestMaxBytes)
	}
	return data, nil
}

// Init creates the habitat dirs and writes the manifest if absent. Idempotent:
// re-running never destroys existing state, only fills missing dirs.
func Init(homePath, kind string, now time.Time) (resultErr error) {
	defer func() {
		resultErr = withManifestRecoveryGuidance(resultErr)
	}()
	if kind == "" {
		kind = HomeKindGlobal
	}
	if err := validateHomeKind(kind); err != nil {
		return err
	}
	preflightManifest, hadPreflightManifest, err := preflightInitManifest(homePath)
	if err != nil {
		return fmt.Errorf("validate existing home manifest before initialization: %w", err)
	}
	root, rootInfo, err := secureHomeRootForInit(homePath)
	if err != nil {
		return fmt.Errorf("secure home root: %w", err)
	}
	defer root.Close()
	if err := initAfterRootSecured(homePath); err != nil {
		return err
	}
	initLock, err := acquireRootedInitLock(root, rootInfo)
	if err != nil {
		return fmt.Errorf("lock compound home root for initialization: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, releaseRootedInitLock(initLock))
	}()
	var newManifestBytes []byte
	if hadPreflightManifest {
		if _, err := validateRootedInitManifest(root, homePath, kind, preflightManifest); err != nil {
			return fmt.Errorf("validate rooted home manifest before scaffolding: %w", err)
		}
	} else if _, err := root.Lstat(ManifestName); errors.Is(err, os.ErrNotExist) {
		if err := requireEmptyRootForFreshManifest(root, homePath); err != nil {
			return err
		}
		m := Manifest{
			SchemaVersion: SchemaVersion,
			Kind:          ManifestKind,
			HomeKind:      kind,
			CreatedAt:     now.UTC().Format(time.RFC3339),
			ActiveModules: []string{},
		}
		b, err := yaml.Marshal(m)
		if err != nil {
			return err
		}
		newManifestBytes = b
		if err := writeRootedInitManifest(root, b, homePath); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		return fmt.Errorf("home manifest appeared after initialization preflight; refusing to adopt or rewrite it: %s", filepath.Join(homePath, ManifestName))
	}
	if !hadPreflightManifest {
		if _, err := validateRootedInitManifest(root, homePath, kind, newManifestBytes); err != nil {
			return fmt.Errorf("validate rooted home manifest before scaffolding: %w", err)
		}
	}
	if err := recoverOwnedInitManifestStaging(root, homePath); err != nil {
		return err
	}
	for _, d := range HabitatDirs() {
		if err := ensureRootedInitDirectory(root, filepath.FromSlash(d), 0o755, filepath.Join(homePath, d)); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	if err := initializePrivateCustodyDirsRooted(root, homePath); err != nil {
		return fmt.Errorf("initialize private custody: %w", err)
	}
	opened, openErr := root.Stat(".")
	pathInfo, pathErr := os.Lstat(homePath)
	if err := errors.Join(openErr, pathErr); err != nil {
		return err
	}
	if !os.SameFile(rootInfo, opened) || !os.SameFile(rootInfo, pathInfo) || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() || !ownedByCurrent(pathInfo) || pathInfo.Mode().Perm() != 0o700 {
		return fmt.Errorf("compound home root identity changed during initialization: %s", homePath)
	}
	return nil
}

func acquireRootedInitLock(root *os.Root, expectedRoot os.FileInfo) (*os.File, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	if expectedRoot == nil || opened.Mode()&os.ModeSymlink != 0 || !opened.IsDir() || !os.SameFile(expectedRoot, opened) {
		_ = dir.Close()
		return nil, fmt.Errorf("compound home init lock descriptor does not identify the pinned home root")
	}
	for {
		err = initRootFlock(int(dir.Fd()), syscall.LOCK_EX)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		break
	}
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}

func releaseRootedInitLock(dir *os.File) error {
	if dir == nil {
		return nil
	}
	unlockErr := initRootFlock(int(dir.Fd()), syscall.LOCK_UN)
	closeErr := dir.Close()
	return errors.Join(unlockErr, closeErr)
}

func ensureRootedInitDirectory(root *os.Root, rel string, mode os.FileMode, display string) error {
	rel = filepath.Clean(rel)
	if !filepath.IsLocal(rel) || rel == "." {
		return fmt.Errorf("invalid habitat directory path %q", rel)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	current := ""
	for _, part := range parts {
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}
		info, err := root.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("habitat path must be a non-symlink directory: %s", display)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := root.Mkdir(current, mode); err != nil {
			return err
		}
		// The path was just created beneath the pinned private home root.
		// Correct the process umask before opening or descending into it.
		if err := root.Chmod(current, mode); err != nil {
			return err
		}
		dir, err := root.Open(current)
		if err != nil {
			return err
		}
		opened, statErr := dir.Stat()
		chmodErr := dir.Chmod(mode)
		syncErr := dir.Sync()
		closeErr := dir.Close()
		final, finalErr := root.Lstat(current)
		if err := errors.Join(statErr, chmodErr, syncErr, closeErr, finalErr); err != nil {
			return err
		}
		if opened == nil || final == nil || !os.SameFile(opened, final) || final.Mode()&os.ModeSymlink != 0 || !final.IsDir() || final.Mode().Perm() != mode.Perm() {
			return fmt.Errorf("habitat directory identity or mode changed during init: %s", display)
		}
		if err := syncRootedDirectory(root, filepath.Dir(current), display, "home init"); err != nil {
			return err
		}
	}
	return nil
}

// CheckItem is one doctor finding.
type CheckItem struct {
	Name string
	OK   bool
}

// SetupCheck is one fresh-machine setup finding.
type SetupCheck struct {
	Name     string
	OK       bool
	Required bool
	Detail   string
	Fix      string
}

// SetupReport is the home-owned setup preflight. It belongs to home doctor so
// setup does not become a parallel doctor surface.
type SetupReport struct {
	InstallDir        string
	RequiredGoVersion string
	Checks            []SetupCheck
	OK                bool
}

// Report is the doctor result.
type Report struct {
	Home             string
	ManifestOK       bool
	Manifest         Manifest
	ManifestError    string
	ManifestRecovery string
	Dirs             []CheckItem
	DirsOK           int
	Habitat          HabitatInventory
	HabitatOK        bool
	InsideGitRepo    bool        // a global home inside a git repo is a "generated-state-in-source" smell
	Hygiene          []CheckItem // operational hygiene: terminal work swept, leases live-only
	HygieneOK        bool
	BackupWarnings   []CheckItem
	Setup            SetupReport
	SetupOK          bool
	PrivateCustody   PrivateCustodyReport
	OK               bool
}

// Doctor inspects a home: manifest present + parseable, every habitat dir present,
// whether the home sits inside a git repo (generated state must not be tracked),
// and operational hygiene (no terminal work hot, no stale leases — the sweep in
// `home archive-history` is the remedy). OK is gated on manifest + dirs only; the
// git and hygiene checks are reported, not fatal (a workspace-local home is
// legitimately inside a repo but must be gitignored, and the archive sweep must
// still be allowed to run on a hygiene-dirty home to clean it).
type DoctorOptions struct {
	Now time.Time
}

func Doctor(homePath string) Report {
	return DoctorWithOptions(homePath, DoctorOptions{})
}

// inspectHomeHealth computes the complete structural predicate shared by
// Doctor and mutation preflights. Diagnostics that do not contribute to OK
// stay in DoctorWithOptions so lock-held admission does not scan unrelated
// bodies.
func inspectHomeHealth(homePath string) Report {
	r := Report{Home: homePath}
	if b, err := readManifestNoFollowBounded(filepath.Join(homePath, ManifestName)); err != nil {
		r.ManifestError = fmt.Sprintf("read home manifest: %v", err)
	} else if manifest, err := decodeHomeManifest(b); err != nil {
		r.Manifest = manifest
		r.ManifestError = err.Error()
		if isManifestCompatibilityError(err) {
			r.ManifestRecovery = manifestRecoveryGuidance
		}
	} else {
		r.Manifest = manifest
		r.ManifestOK = true
	}
	dirs := HabitatDirs()
	for _, d := range dirs {
		fi, err := os.Stat(filepath.Join(homePath, d))
		ok := err == nil && fi.IsDir()
		if ok {
			r.DirsOK++
		}
		r.Dirs = append(r.Dirs, CheckItem{Name: d, OK: ok})
	}
	if habitat, err := InspectHabitat(homePath); err == nil {
		r.Habitat = habitat
		r.HabitatOK = habitat.OK
	}
	// r.OK is the HOME-health verdict: manifest, scaffold dirs, and a clean habitat inventory.
	// It deliberately does NOT fold in SetupOK — SetupOK checks the machine's install environment
	// (setup.compound_on_path, setup.install_dir_on_path), which is orthogonal to whether THIS home
	// is healthy. Folding it in made every home-health consumer (archive.requireHealthyHome, backup
	// restore verify, and any test asserting Doctor().OK) fail on a machine without an installed
	// compound — a CI runner or a fresh clone — even for a perfectly healthy home. SetupOK/Setup stay
	// reported fields so callers that care about install readiness still see it.
	r.OK = r.ManifestOK && r.DirsOK == len(dirs) && r.HabitatOK
	return r
}

func DoctorWithOptions(homePath string, opts DoctorOptions) Report {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	r := inspectHomeHealth(homePath)
	r.InsideGitRepo = underGitRepo(homePath)
	r.Hygiene, r.HygieneOK = hygieneChecks(homePath)
	r.BackupWarnings = backupFreshnessWarnings(homePath, now)
	r.Setup = DoctorSetup(SetupOptions{})
	r.SetupOK = r.Setup.OK
	r.PrivateCustody = DiagnosePrivateCustody(homePath)
	return r
}

func backupFreshnessWarnings(homePath string, now time.Time) []CheckItem {
	cfg, _, err := LoadHomeConfig(homePath)
	if err != nil {
		return []CheckItem{{Name: "backup.config", OK: false}}
	}
	if strings.TrimSpace(cfg.BackupDir) == "" {
		return nil
	}
	dir, err := resolveConfiguredBackupDir(homePath, cfg.BackupDir)
	if err != nil {
		return []CheckItem{{Name: "backup.config", OK: false}}
	}
	newest, ok, err := newestBackupArchive(dir)
	if err != nil || !ok {
		return []CheckItem{{Name: "backup.missing", OK: false}}
	}
	if now.Sub(newest) > BackupFreshnessMaxAge {
		return []CheckItem{{Name: "backup.stale", OK: false}}
	}
	return nil
}

func resolveConfiguredBackupDir(homePath, dir string) (string, error) {
	dir = strings.TrimSpace(expandUser(dir))
	if dir == "" {
		return "", fmt.Errorf("backup dir required")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(homePath, dir)
	}
	return filepath.Abs(dir)
}

func newestBackupArchive(dir string) (time.Time, bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false, err
	}
	var newest time.Time
	for _, entry := range entries {
		if entry.IsDir() || !isHomeBackupArchiveName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return time.Time{}, false, err
		}
		if newest.IsZero() || info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, !newest.IsZero(), nil
}

func isHomeBackupArchiveName(name string) bool {
	return strings.HasSuffix(name, ".tar.gz") &&
		(strings.HasPrefix(name, "home-") || strings.HasPrefix(name, "compound-home-"))
}

// SetupOptions lets tests inject host/tool state without mutating the process.
type SetupOptions struct {
	RepoRoot        string
	HomeDir         string
	InstallDir      string
	Path            string
	GoVersionOutput string
	LookPath        func(string) (string, error)
	GoVersion       func(env []string) (string, error)
}

func DoctorSetup(opts SetupOptions) SetupReport {
	installDir := strings.TrimSpace(opts.InstallDir)
	if installDir == "" {
		installDir = defaultInstallDir(opts.HomeDir)
	}
	installDir, _ = filepath.Abs(installDir)
	pathValue := opts.Path
	if pathValue == "" {
		pathValue = os.Getenv("PATH")
	}
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	repoRoot := strings.TrimSpace(opts.RepoRoot)
	if repoRoot == "" {
		repoRoot = findAncestorFile("go.mod")
	}
	requiredGo := requiredGoVersion(repoRoot)
	report := SetupReport{InstallDir: installDir, RequiredGoVersion: requiredGo, OK: true}
	add := func(check SetupCheck) {
		report.Checks = append(report.Checks, check)
		if check.Required && !check.OK {
			report.OK = false
		}
	}

	goPath, goErr := lookPath("go")
	goOK := goErr == nil
	goDetail := "go not found"
	if goOK {
		goDetail = goPath
		if requiredGo != "" {
			versionOut := strings.TrimSpace(opts.GoVersionOutput)
			if versionOut == "" {
				versionOut, _ = runGoVersion(opts.GoVersion)
			}
			if versionOut == "" {
				goOK = false
				goDetail = "could not read go version"
			} else if !goVersionAtLeast(versionOut, requiredGo) {
				goOK = false
				goDetail = versionOut + " < required go" + requiredGo
			} else {
				goDetail = versionOut
			}
		}
	}
	add(SetupCheck{
		Name:     "setup.go_toolchain",
		OK:       goOK,
		Required: true,
		Detail:   goDetail,
		Fix:      "install Go " + firstNonEmpty(requiredGo, "from go.dev/dl") + " or newer, then rerun `make install`",
	})

	add(SetupCheck{
		Name:     "setup.install_dir_on_path",
		OK:       pathContainsDir(pathValue, installDir),
		Required: true,
		Detail:   "install_dir=" + installDir,
		Fix:      `export PATH="` + installDir + `:$PATH"`,
	})

	compoundPath, compoundErr := lookPath("compound")
	add(SetupCheck{
		Name:     "setup.compound_on_path",
		OK:       compoundErr == nil,
		Required: true,
		Detail:   firstNonEmpty(compoundPath, "compound not found"),
		Fix:      "make install",
	})

	bunPath, bunErr := lookPath("bun")
	add(SetupCheck{
		Name:     "setup.bun_optional_console",
		OK:       bunErr == nil,
		Required: false,
		Detail:   firstNonEmpty(bunPath, "bun not found; console is optional"),
		Fix:      "install Bun from https://bun.sh when using the optional console",
	})
	return report
}

func runGoVersion(fn func(env []string) (string, error)) (string, error) {
	env := append(os.Environ(), "GOTOOLCHAIN=local")
	if fn != nil {
		out, err := fn(env)
		return strings.TrimSpace(out), err
	}
	cmd := exec.Command("go", "version")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func defaultInstallDir(homeDir string) string {
	if gobin := strings.TrimSpace(os.Getenv("GOBIN")); gobin != "" {
		return gobin
	}
	if homeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			homeDir = h
		}
	}
	if homeDir == "" {
		return filepath.Join(".", "bin")
	}
	return filepath.Join(homeDir, ".local", "bin")
}

func pathContainsDir(pathValue, dir string) bool {
	want, err := filepath.Abs(dir)
	if err != nil {
		want = filepath.Clean(dir)
	}
	for _, part := range filepath.SplitList(pathValue) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		got, err := filepath.Abs(part)
		if err != nil {
			got = filepath.Clean(part)
		}
		if got == want {
			return true
		}
	}
	return false
}

func findAncestorFile(name string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func requiredGoVersion(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" {
			return fields[1]
		}
	}
	return ""
}

func goVersionAtLeast(output, required string) bool {
	current := parseGoVersion(output)
	req := parseGoVersion(required)
	if len(current) == 0 || len(req) == 0 {
		return false
	}
	for i := 0; i < 3; i++ {
		if current[i] > req[i] {
			return true
		}
		if current[i] < req[i] {
			return false
		}
	}
	return true
}

func parseGoVersion(s string) []int {
	for _, field := range strings.Fields(s) {
		if strings.HasPrefix(field, "go") && len(field) > 2 && field[2] >= '0' && field[2] <= '9' {
			s = field
			break
		}
	}
	s = strings.TrimPrefix(s, "go")
	parts := strings.Split(s, ".")
	out := []int{0, 0, 0}
	found := false
	for i := 0; i < len(parts) && i < 3; i++ {
		part := strings.TrimFunc(parts[i], func(r rune) bool { return r < '0' || r > '9' })
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		out[i] = n
		found = true
	}
	if !found {
		return nil
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func underGitRepo(p string) bool {
	dir := p
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
