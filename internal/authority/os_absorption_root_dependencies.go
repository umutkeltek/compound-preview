package authority

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compound/internal/decisions"
	"compound/internal/home"
)

const osRootDependencyReportSchema = "compound.os_root_dependency_report.v0.1"

var legacyOSPathAuthorityRefs = []string{
	"source:umut-os",
	"source:infra-os",
	"source:decisions",
}

type OSRootDependencyReport struct {
	SchemaVersion    string                    `json:"schema_version"`
	Status           string                    `json:"status"`
	Roots            []string                  `json:"roots,omitempty"`
	InstalledHome    string                    `json:"installed_home,omitempty"`
	DependencyCount  int                       `json:"dependency_count"`
	CountsByKind     map[string]int            `json:"counts_by_kind"`
	Dependencies     []OSRootDependency        `json:"dependencies,omitempty"`
	DecisionCoverage OSRootDecisionCoverage    `json:"decision_coverage"`
	Custody          []OSRootSourceCustody     `json:"custody,omitempty"`
	Checks           []OSAbsorptionDoctorCheck `json:"checks"`
}

type OSRootDependency struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
}

type OSRootDecisionCoverage struct {
	Status   string `json:"status"`
	Root     string `json:"root,omitempty"`
	Eligible int    `json:"eligible"`
	Covered  int    `json:"covered"`
	Missing  int    `json:"missing"`
	Error    string `json:"error,omitempty"`
}

type OSRootSourceCustody struct {
	Family            string `json:"family"`
	Root              string `json:"root"`
	Status            string `json:"status"`
	ManifestPath      string `json:"manifest_path"`
	CustodyComplete   bool   `json:"custody_complete"`
	LifecycleState    string `json:"lifecycle_state,omitempty"`
	LifecycleTerminal bool   `json:"lifecycle_terminal"`
	Error             string `json:"error,omitempty"`
}

type hermesJobRegistry struct {
	Jobs []json.RawMessage `json:"jobs"`
}

type hermesJobIdentity struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Script  string `json:"script"`
	Workdir string `json:"workdir"`
}

type osRootCustodyManifest struct {
	Lifecycle *osRootCustodyLifecycle     `json:"lifecycle,omitempty"`
	Items     []osRootCustodyManifestItem `json:"items"`
}

type osRootCustodyLifecycle struct {
	State string `json:"state"`
}

type osRootCustodyManifestItem struct {
	BodyPolicy string `json:"body_policy"`
	BodyRef    string `json:"body_ref"`
	BodyBytes  int64  `json:"body_bytes"`
	FileHash   string `json:"file_hash"`
}

func inspectOSRootDependencies(homePath string) (OSRootDependencyReport, error) {
	report := OSRootDependencyReport{
		SchemaVersion: osRootDependencyReportSchema,
		Status:        "fail",
		CountsByKind:  map[string]int{},
	}
	roots, err := resolveLegacyOSRoots(homePath)
	if err != nil {
		return report, err
	}
	report.Roots = roots
	if len(roots) == 0 {
		report.Status = "not_configured"
		report.Checks = append(report.Checks, doctorCheck(
			"legacy_os_roots_declared",
			true,
			"no legacy operating-system roots declared; public-clone/retired-root scan skipped",
		))
		return report, nil
	}

	installedHome, found, err := ResolveInstalledHarnessHome(homePath)
	if err != nil {
		return report, err
	}
	if !found {
		report.Checks = append(report.Checks, doctorCheck(
			"installed_harness_home",
			false,
			"legacy roots are declared but installed_harness_home is absent",
		))
		return report, nil
	}
	report.InstalledHome = installedHome
	needles := legacyOSDependencyNeedles(installedHome, roots)
	for _, dependency := range inspectLegacySourceBootstraps(roots) {
		report.addDependency(dependency)
	}

	for _, target := range []struct {
		kind string
		name string
		path string
	}{
		{kind: "installed_instruction", name: "claude_global_instructions", path: filepath.Join(installedHome, ".claude", "CLAUDE.md")},
		{kind: "installed_instruction", name: "codex_global_instructions", path: filepath.Join(installedHome, ".codex", "AGENTS.md")},
		{kind: "session_hook", name: "claude_hook_registration", path: filepath.Join(installedHome, ".claude", "settings.json")},
		{kind: "session_hook", name: "codex_hook_registration", path: filepath.Join(installedHome, ".codex", "hooks.json")},
		{kind: "session_hook", name: "claude_recall_context_body", path: filepath.Join(installedHome, ".claude", "hooks", "RecallContext.hook.ts")},
		{kind: "session_hook", name: "codex_recall_context_body", path: filepath.Join(installedHome, ".codex", "hooks", "RecallContext.hook.ts")},
		{kind: "installed_shim", name: "os", path: filepath.Join(installedHome, ".local", "bin", "os")},
		{kind: "hermes_config", name: "hermes", path: filepath.Join(installedHome, ".hermes", "config.yaml")},
	} {
		if rootDependent, detail := fileReferencesLegacyOS(target.path, needles); rootDependent {
			report.addDependency(OSRootDependency{Kind: target.kind, Name: target.name, Path: target.path, Detail: detail})
		}
	}

	launchDir := filepath.Join(installedHome, "Library", "LaunchAgents")
	launchDependencies, err := inspectLaunchAgentDependencies(launchDir, needles)
	if err != nil {
		return report, err
	}
	for _, dependency := range launchDependencies {
		report.addDependency(dependency)
	}

	hermesDependencies, err := inspectHermesJobDependencies(installedHome, needles)
	if err != nil {
		return report, err
	}
	for _, dependency := range hermesDependencies {
		report.addDependency(dependency)
	}

	report.DecisionCoverage = inspectLegacyDecisionCoverage(homePath, roots)
	report.Custody = inspectLegacySourceCustody(homePath, roots)
	report.DependencyCount = len(report.Dependencies)
	sort.Slice(report.Dependencies, func(i, j int) bool {
		if report.Dependencies[i].Kind == report.Dependencies[j].Kind {
			if report.Dependencies[i].Name == report.Dependencies[j].Name {
				return report.Dependencies[i].Path < report.Dependencies[j].Path
			}
			return report.Dependencies[i].Name < report.Dependencies[j].Name
		}
		return report.Dependencies[i].Kind < report.Dependencies[j].Kind
	})

	custodyComplete := len(report.Custody) > 0
	for _, custody := range report.Custody {
		if !custody.CustodyComplete || !custody.LifecycleTerminal {
			custodyComplete = false
			break
		}
	}
	decisionComplete := report.DecisionCoverage.Status == "ok" && report.DecisionCoverage.Missing == 0
	report.Checks = append(report.Checks,
		doctorCheck("legacy_os_roots_declared", true, fmt.Sprintf("roots=%d", len(roots))),
		doctorCheck("installed_harness_home", true, installedHome),
		doctorCheck("active_root_dependencies", report.DependencyCount == 0, rootDependencyDetail(report)),
		doctorCheck("decision_source_coverage", decisionComplete, decisionCoverageDetail(report.DecisionCoverage)),
		doctorCheck("source_custody", custodyComplete, custodyDetail(report.Custody)),
	)
	if report.DependencyCount == 0 && decisionComplete && custodyComplete {
		report.Status = "ok"
	}
	return report, nil
}

func resolveLegacyOSRoots(homePath string) ([]string, error) {
	roots := make([]string, 0, len(legacyOSPathAuthorityRefs))
	seen := map[string]bool{}
	for _, ref := range legacyOSPathAuthorityRefs {
		root, found, err := resolveDeclaredOSPath(homePath, ref)
		if err != nil {
			return nil, err
		}
		if !found || root == "" || seen[root] {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots, nil
}

func legacyOSDependencyNeedles(installedHome string, roots []string) []string {
	seen := map[string]bool{}
	var needles []string
	add := func(value string) {
		value = filepath.ToSlash(strings.TrimSpace(value))
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		needles = append(needles, value)
	}
	for _, root := range roots {
		add(root)
		if relative, err := filepath.Rel(installedHome, root); err == nil && relative != "." && !strings.HasPrefix(relative, "..") {
			relative = filepath.ToSlash(relative)
			add("$HOME/" + relative)
			add("${HOME}/" + relative)
			add("~/" + relative)
		}
	}
	if common := commonLegacyOSParent(roots); common != "" {
		add(common)
		if relative, err := filepath.Rel(installedHome, common); err == nil && relative != "." && !strings.HasPrefix(relative, "..") {
			relative = filepath.ToSlash(relative)
			add("$HOME/" + relative)
			add("${HOME}/" + relative)
			add("~/" + relative)
		}
	}
	sort.Slice(needles, func(i, j int) bool { return len(needles[i]) > len(needles[j]) })
	return needles
}

func commonLegacyOSParent(roots []string) string {
	if len(roots) < 2 {
		return ""
	}
	candidate := filepath.Dir(roots[0])
	for candidate != "." && candidate != string(filepath.Separator) {
		all := true
		for _, root := range roots[1:] {
			relative, err := filepath.Rel(candidate, root)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				all = false
				break
			}
		}
		if all {
			return candidate
		}
		candidate = filepath.Dir(candidate)
	}
	return ""
}

func fileReferencesLegacyOS(path string, needles []string) (bool, string) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, ""
	}
	if err != nil {
		return true, "cannot inspect dependency: " + err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return true, "cannot inspect symlink target: " + err.Error()
		}
		if needle := matchingLegacyOSNeedle(target, needles); needle != "" {
			return true, "symlink target references " + needle
		}
	}
	if !info.Mode().IsRegular() {
		return false, ""
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return true, "cannot read dependency: " + err.Error()
	}
	if needle := matchingLegacyOSNeedle(string(body), needles); needle != "" {
		return true, "content references " + needle
	}
	return false, ""
}

func matchingLegacyOSNeedle(value string, needles []string) string {
	value = filepath.ToSlash(value)
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return needle
		}
	}
	if strings.Contains(value, "operating-system") {
		for _, ref := range legacyOSPathAuthorityRefs {
			name := strings.TrimPrefix(ref, "source:")
			if strings.Contains(value, name) {
				return "structured path operating-system/" + name
			}
		}
	}
	return ""
}

func inspectLegacySourceBootstraps(roots []string) []OSRootDependency {
	paths := map[string]string{}
	if common := commonLegacyOSParent(roots); common != "" {
		paths[filepath.Join(common, "AGENTS.md")] = "operating-system_root_agents"
	}
	for _, root := range roots {
		paths[filepath.Join(root, "AGENTS.md")] = filepath.Base(root) + "_agents"
		if filepath.Base(root) == "umut-os" {
			paths[filepath.Join(root, "LIVE_START.md")] = "umut_os_live_start"
		}
	}
	var dependencies []OSRootDependency
	for path, name := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		detail := "active source bootstrap exists under legacy root"
		if err != nil {
			detail = "cannot inspect source bootstrap: " + err.Error()
		} else if !info.Mode().IsRegular() {
			detail = "source bootstrap is not a regular file"
		}
		dependencies = append(dependencies, OSRootDependency{
			Kind:   "source_bootstrap",
			Name:   name,
			Path:   path,
			Detail: detail,
		})
	}
	return dependencies
}

func inspectLaunchAgentDependencies(root string, needles []string) ([]OSRootDependency, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read launch agents: %w", err)
	}
	installedHome := filepath.Dir(filepath.Dir(root))
	var dependencies []OSRootDependency
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".plist") || strings.Contains(name, ".disabled") {
			continue
		}
		path := filepath.Join(root, name)
		dependent, detail := fileReferencesLegacyOS(path, needles)
		if !dependent {
			references, decodeErr := launchAgentStringValues(path)
			if decodeErr != nil {
				return nil, fmt.Errorf("parse launch agent %s: %w", path, decodeErr)
			}
			for _, reference := range references {
				if !osRootPathWithin(installedHome, reference) || !launchAgentExecutableReference(reference) {
					continue
				}
				if targetDependent, targetDetail := fileReferencesLegacyOS(reference, needles); targetDependent {
					dependent = true
					detail = "target " + reference + " " + targetDetail
					break
				}
			}
		}
		if dependent {
			dependencies = append(dependencies, OSRootDependency{
				Kind:   "launchd",
				Name:   strings.TrimSuffix(name, ".plist"),
				Path:   path,
				Detail: detail,
			})
		}
	}
	return dependencies, nil
}

func launchAgentStringValues(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	const (
		openTag  = "<string>"
		closeTag = "</string>"
	)
	remaining := string(body)
	var values []string
	for {
		openAt := strings.Index(remaining, openTag)
		if openAt < 0 {
			break
		}
		remaining = remaining[openAt+len(openTag):]
		closeAt := strings.Index(remaining, closeTag)
		if closeAt < 0 {
			break
		}
		values = append(values, strings.TrimSpace(remaining[:closeAt]))
		remaining = remaining[closeAt+len(closeTag):]
	}
	return values, nil
}

func launchAgentExecutableReference(path string) bool {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, os.PathListSeparator) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 != 0 {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sh", ".zsh", ".bash", ".py", ".ts", ".js", ".mjs":
		return true
	default:
		return false
	}
}
func osRootPathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func inspectHermesJobDependencies(installedHome string, needles []string) ([]OSRootDependency, error) {
	scriptRoot := filepath.Join(installedHome, ".hermes", "scripts")
	rootDependentScripts := map[string]string{}
	entries, err := os.ReadDir(scriptRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read Hermes scripts: %w", err)
	}
	for _, entry := range entries {
		path := filepath.Join(scriptRoot, entry.Name())
		if dependent, _ := fileReferencesLegacyOS(path, needles); dependent {
			rootDependentScripts[entry.Name()] = path
		}
	}

	registryPath := filepath.Join(installedHome, ".hermes", "cron", "jobs.json")
	body, err := os.ReadFile(registryPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Hermes job registry: %w", err)
	}
	var registry hermesJobRegistry
	if err := json.Unmarshal(body, &registry); err != nil {
		return nil, fmt.Errorf("parse Hermes job registry: %w", err)
	}
	var dependencies []OSRootDependency
	for _, raw := range registry.Jobs {
		var identity hermesJobIdentity
		if err := json.Unmarshal(raw, &identity); err != nil || !identity.Enabled {
			continue
		}
		needle := matchingLegacyOSNeedle(string(raw), needles)
		scriptPath := rootDependentScripts[filepath.Base(strings.TrimSpace(identity.Script))]
		if needle == "" && scriptPath == "" {
			continue
		}
		detail := "enabled job"
		if needle != "" {
			detail += " references " + needle
		}
		if scriptPath != "" {
			detail += "; shim " + scriptPath + " resolves into legacy source"
		}
		dependencies = append(dependencies, OSRootDependency{
			Kind:   "hermes_job",
			Name:   firstNonEmpty(strings.TrimSpace(identity.Name), filepath.Base(identity.Script)),
			Path:   registryPath,
			Detail: detail,
		})
	}
	return dependencies, nil
}

func inspectLegacyDecisionCoverage(homePath string, roots []string) OSRootDecisionCoverage {
	root := legacyRootByBase(roots, "decisions")
	if root == "" {
		return OSRootDecisionCoverage{Status: "not_configured"}
	}
	coverage := OSRootDecisionCoverage{Status: "fail", Root: root}
	eligible, err := decisions.EligiblePolicyDecisionFiles(root)
	if errors.Is(err, os.ErrNotExist) {
		return inspectUnavailableLegacyDecisionCoverage(homePath, root)
	}
	if err != nil {
		coverage.Error = err.Error()
		return coverage
	}
	report, err := decisions.SourceAuthorityCoverage(homePath, eligible)
	if err != nil {
		coverage.Error = err.Error()
		return coverage
	}
	coverage.Eligible = len(report.Eligible)
	coverage.Missing = len(report.Missing)
	coverage.Covered = coverage.Eligible - coverage.Missing
	if coverage.Missing == 0 {
		coverage.Status = "ok"
	}
	return coverage
}

func inspectUnavailableLegacyDecisionCoverage(homePath, root string) OSRootDecisionCoverage {
	coverage := OSRootDecisionCoverage{Status: "fail", Root: root}
	custody := inspectLegacySourceFamilyCustody(homePath, "decisions", root)
	if !custody.CustodyComplete || !legacySourceLifecycleFrozen(custody.LifecycleState) {
		coverage.Error = fmt.Sprintf(
			"legacy decision root unavailable without frozen complete custody: status=%s lifecycle=%s error=%s",
			custody.Status,
			custody.LifecycleState,
			custody.Error,
		)
		return coverage
	}
	records, err := decisions.LoadSourceAuthority(homePath)
	if err != nil {
		coverage.Error = err.Error()
		return coverage
	}
	eligible := map[string]bool{}
	covered := map[string]bool{}
	for _, record := range records {
		sourcePath := filepath.Clean(strings.TrimSpace(record.SourcePath))
		if record.SourceKind != "policy" || !osRootPathWithin(root, sourcePath) {
			continue
		}
		eligible[sourcePath] = true
		if record.AuthorityStatus == "compound_primary" {
			covered[sourcePath] = true
		}
	}
	coverage.Eligible = len(eligible)
	coverage.Covered = len(covered)
	coverage.Missing = coverage.Eligible - coverage.Covered
	if coverage.Eligible == 0 {
		coverage.Error = "frozen custody has no Compound decision-source authority records for the retired root"
		return coverage
	}
	if coverage.Missing == 0 {
		coverage.Status = "ok"
	}
	return coverage
}

func legacySourceLifecycleFrozen(state string) bool {
	switch strings.TrimSpace(state) {
	case "legacy_frozen", "quiet_window_proven", "archived", "retired":
		return true
	default:
		return false
	}
}

func inspectLegacySourceCustody(homePath string, roots []string) []OSRootSourceCustody {
	families := []struct {
		family string
		base   string
	}{
		{family: "os", base: "umut-os"},
		{family: "infra", base: "infra-os"},
		{family: "decisions", base: "decisions"},
	}
	var custody []OSRootSourceCustody
	for _, item := range families {
		root := legacyRootByBase(roots, item.base)
		if root == "" {
			continue
		}
		record := inspectLegacySourceFamilyCustody(homePath, item.family, root)
		custody = append(custody, record)
	}
	return custody
}

func inspectLegacySourceFamilyCustody(homePath, family, root string) OSRootSourceCustody {
	canonicalPath := filepath.Join(homePath, "sources", "manifests", family+".json")
	legacyPath := filepath.Join(homePath, "registries", "source-manifests", family+".json")
	record := OSRootSourceCustody{
		Family:       family,
		Root:         root,
		Status:       "missing",
		ManifestPath: canonicalPath,
	}
	path := canonicalPath
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		path = legacyPath
		body, err = os.ReadFile(path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return record
	}
	if err != nil {
		record.Status = "fail"
		record.Error = err.Error()
		return record
	}
	record.ManifestPath = path
	var manifest osRootCustodyManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		record.Status = "fail"
		record.Error = err.Error()
		return record
	}
	if len(manifest.Items) == 0 {
		record.Status = "fail"
		record.Error = "manifest has no items"
		return record
	}
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		record.Status = "fail"
		record.Error = err.Error()
		return record
	}
	for _, item := range manifest.Items {
		if item.BodyPolicy != "owned_snapshot" || item.BodyRef == "" || item.FileHash == "" {
			record.Status = "pending"
			record.Error = "manifest contains inventory-only or incomplete items"
			return record
		}
		ownedBody, err := store.Get(item.BodyRef)
		if err != nil {
			record.Status = "fail"
			record.Error = err.Error()
			return record
		}
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(ownedBody))
		if digest != item.FileHash || int64(len(ownedBody)) != item.BodyBytes {
			record.Status = "fail"
			record.Error = "owned body hash or size mismatch"
			return record
		}
	}
	record.CustodyComplete = true
	if manifest.Lifecycle != nil {
		record.LifecycleState = strings.TrimSpace(manifest.Lifecycle.State)
	}
	record.LifecycleTerminal = record.LifecycleState == "archived" || record.LifecycleState == "retired"
	if !record.LifecycleTerminal {
		record.Status = "pending"
		record.Error = "manifest lifecycle is not archived or retired"
		return record
	}
	record.Status = "ok"
	return record
}

func legacyRootByBase(roots []string, base string) string {
	for _, root := range roots {
		if filepath.Base(root) == base {
			return root
		}
	}
	return ""
}

func (report *OSRootDependencyReport) addDependency(dependency OSRootDependency) {
	report.Dependencies = append(report.Dependencies, dependency)
	report.CountsByKind[dependency.Kind]++
}

func rootDependencyDetail(report OSRootDependencyReport) string {
	if report.DependencyCount == 0 {
		return "active dependencies=0"
	}
	var kinds []string
	for kind, count := range report.CountsByKind {
		kinds = append(kinds, fmt.Sprintf("%s=%d", kind, count))
	}
	sort.Strings(kinds)
	return fmt.Sprintf("active dependencies=%d (%s)", report.DependencyCount, strings.Join(kinds, ", "))
}

func decisionCoverageDetail(coverage OSRootDecisionCoverage) string {
	if coverage.Error != "" {
		return coverage.Error
	}
	if coverage.Status == "not_configured" {
		return "decisions root not declared"
	}
	return fmt.Sprintf("eligible=%d covered=%d missing=%d", coverage.Eligible, coverage.Covered, coverage.Missing)
}

func custodyDetail(custody []OSRootSourceCustody) string {
	if len(custody) == 0 {
		return "no legacy source roots declared"
	}
	var details []string
	for _, item := range custody {
		state := item.LifecycleState
		if state == "" {
			state = "missing"
		}
		details = append(details, fmt.Sprintf(
			"%s=custody:%t lifecycle:%s terminal:%t",
			item.Family,
			item.CustodyComplete,
			state,
			item.LifecycleTerminal,
		))
	}
	return strings.Join(details, ", ")
}
