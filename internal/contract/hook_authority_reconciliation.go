package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/harness"
	"compound/internal/hookplan"
	"gopkg.in/yaml.v3"
)

const (
	liveHookInstallationRef              = "harness-installation://live"
	managedHookInstallationRef           = "harness-installation://managed"
	installedHooksHookInstallImportedVia = "hook install"
)

// InstalledHooksReconciliation keeps desired authority, observed installation,
// and their adjudication separate on the wire.
type InstalledHooksReconciliation struct {
	Plan           hookplan.HookPlan             `json:"hook_plan"`
	Observation    hookplan.InstallationSnapshot `json:"installation_observation"`
	Reconciliation hookplan.Reconciliation       `json:"reconciliation"`
}

// ImportHookInstallReport verifies and records the exact managed posture
// materialized by one hook install command. Every reported config revision is
// checked before authority is touched, then all selected harnesses are
// published through one combined manifest and one source-authority record.
func ImportHookInstallReport(
	homePath string,
	harnessHome string,
	report hookplan.HookInstallReport,
	now time.Time,
) ([]InstalledHooksImportReport, error) {
	if report.Check {
		return nil, fmt.Errorf("check-only hook install cannot update installed-hook authority")
	}
	if len(report.Entries) == 0 {
		return nil, fmt.Errorf("hook install report has no entries")
	}
	manifest := InstalledHooksManifest{
		SchemaVersion: installedHooksSchemaVersion,
	}
	type configEvidence struct {
		Harness        string `json:"harness"`
		ConfigRef      string `json:"config_ref"`
		ConfigRevision string `json:"config_revision"`
	}
	evidence := make([]configEvidence, 0, len(report.Entries))
	for _, entry := range report.Entries {
		sourceData, err := os.ReadFile(entry.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("read installed %s hook config: %w", entry.Harness, err)
		}
		actualRevision := "sha256:" + hashBytesFull(sourceData)
		expectedRevision := strings.TrimSpace(entry.ConfigRevision)
		if expectedRevision == "" {
			return nil, fmt.Errorf("%s installed hook config revision required", entry.Harness)
		}
		if actualRevision != expectedRevision {
			return nil, fmt.Errorf(
				"%s installed hook config revision mismatch: got %s want %s",
				entry.Harness,
				actualRevision,
				expectedRevision,
			)
		}
		harnessName := normalizeInstalledHookHarness(entry.Harness)
		if harnessName == "" {
			return nil, fmt.Errorf("installed hook harness required")
		}
		evidence = append(evidence, configEvidence{
			Harness:        harnessName,
			ConfigRef:      entry.ConfigPath,
			ConfigRevision: expectedRevision,
		})
		for _, binding := range entry.Bindings {
			event := strings.TrimSpace(binding.Event)
			command := strings.TrimSpace(binding.Command)
			if event == "" || command == "" {
				return nil, fmt.Errorf("%s installed hook binding needs event and command", entry.Harness)
			}
			if event == "PreToolUse" && binding.Blocking != hookCommandHasExactField(command, "--enforce") {
				return nil, fmt.Errorf(
					"%s PreToolUse blocking metadata does not match explicit --enforce command",
					entry.Harness,
				)
			}
			status := "current"
			if event == "PreToolUse" && !binding.Blocking {
				status = "non_blocking"
			}
			manifest.Hooks = append(manifest.Hooks, InstalledHook{
				Harness:            harnessName,
				Name:               event,
				Event:              event,
				Matcher:            strings.TrimSpace(binding.Matcher),
				Ref:                canonicalHookHandlerRef(command, harnessHome),
				InvocationRevision: canonicalHookInvocationRevision(command, harnessHome),
				Status:             status,
			})
		}
	}
	sort.Slice(evidence, func(i, j int) bool {
		left := strings.Join([]string{evidence[i].Harness, evidence[i].ConfigRef, evidence[i].ConfigRevision}, "\x00")
		right := strings.Join([]string{evidence[j].Harness, evidence[j].ConfigRef, evidence[j].ConfigRevision}, "\x00")
		return left < right
	})
	sourceData, err := json.Marshal(struct {
		SchemaVersion string           `json:"schema_version"`
		Configs       []configEvidence `json:"configs"`
	}{
		SchemaVersion: "compound.hook_install_evidence.v0.1",
		Configs:       evidence,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal hook install evidence: %w", err)
	}
	attestInstalledHookHandlersAtHome(&manifest, harnessHome)
	authoritySnapshots, err := snapshotHookInstallAuthority(homePath)
	if err != nil {
		return nil, fmt.Errorf("snapshot installed hook authority: %w", err)
	}
	if err := prepareHookInstallAuthorityPostimages(
		authoritySnapshots,
		homePath,
		managedHookInstallationRef,
		sourceData,
		manifest,
		installedHooksHookInstallImportedVia,
		now,
	); err != nil {
		return nil, fmt.Errorf("prepare installed hook authority publication: %w", err)
	}
	result, err := importInstalledHooks(
		homePath,
		managedHookInstallationRef,
		sourceData,
		manifest,
		installedHooksHookInstallImportedVia,
		now,
	)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("import installed hook authority: %w", err),
			rollbackHookInstallAuthority(authoritySnapshots),
		)
	}
	return []InstalledHooksImportReport{result}, nil
}

type hookInstallAuthoritySnapshot struct {
	path       string
	existed    bool
	data       []byte
	mode       os.FileMode
	postKnown  bool
	postExists bool
	postData   []byte
	postMode   os.FileMode
}

func snapshotHookInstallAuthority(homePath string) ([]hookInstallAuthoritySnapshot, error) {
	paths := []string{
		InstalledHooksPath(homePath),
		InstalledHooksSourceAuthorityPath(homePath),
	}
	snapshots := make([]hookInstallAuthoritySnapshot, 0, len(paths))
	for _, path := range paths {
		snapshot := hookInstallAuthoritySnapshot{path: path}
		info, err := os.Lstat(path)
		switch {
		case err == nil:
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("installed hook authority %s must be a regular file", path)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, readErr
			}
			afterReadInfo, statErr := os.Lstat(path)
			if statErr != nil || !afterReadInfo.Mode().IsRegular() || !os.SameFile(info, afterReadInfo) {
				return nil, fmt.Errorf("installed hook authority %s changed during snapshot", path)
			}
			snapshot.existed = true
			snapshot.data = append([]byte(nil), data...)
			snapshot.mode = afterReadInfo.Mode()
		case os.IsNotExist(err):
		default:
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func prepareHookInstallAuthorityPostimages(
	snapshots []hookInstallAuthoritySnapshot,
	homePath string,
	sourcePath string,
	sourceData []byte,
	incoming InstalledHooksManifest,
	importedVia string,
	now time.Time,
) error {
	authority := hookInstallAuthoritySnapshotForPath(snapshots, InstalledHooksPath(homePath))
	if authority == nil {
		return fmt.Errorf("installed hook authority snapshot missing")
	}
	incomingHookCount := len(incoming.Hooks)
	finalManifest := incoming
	priorAuthorityRevision := ""
	if authority.existed {
		existing, err := parseInstalledHooksManifest(authority.path, authority.data)
		if err != nil {
			return err
		}
		priorAuthorityRevision = hashBytes(authority.data)
		finalManifest = mergeInstalledHooksManifest(existing, incoming)
	}
	finalManifest.SchemaVersion = installedHooksSchemaVersion
	manifestData, err := yaml.Marshal(finalManifest)
	if err != nil {
		return err
	}
	authorityRevision := hashBytes(manifestData)
	authority.postKnown = true
	authority.postExists = true
	authority.postData = manifestData
	authority.postMode = authority.mode
	if !authority.existed {
		authority.postMode, err = hookInstallAuthorityNewFileMode(authority.path, 0o644)
		if err != nil {
			return fmt.Errorf("derive installed hook authority mode: %w", err)
		}
	}

	sourceRevision := hashBytes(sourceData)
	evidenceRefs := []string{sourceRevision, InstalledHooksAuthorityRef, "authority_revision:" + authorityRevision}
	if priorAuthorityRevision != "" {
		evidenceRefs = append(evidenceRefs, "prior_authority_revision:"+priorAuthorityRevision)
	}
	record := InstalledHooksSourceAuthority{
		SchemaVersion:          InstalledHooksSourceAuthoritySchema,
		ID:                     installedHooksSourceAuthorityID(sourcePath, sourceRevision, importedVia, authorityRevision),
		CreatedAt:              now.UTC().Format(time.RFC3339),
		SourcePath:             sourcePath,
		SourceRevision:         sourceRevision,
		ImportedVia:            importedVia,
		AuthorityRef:           InstalledHooksAuthorityRef,
		AuthorityRevision:      authorityRevision,
		PriorAuthorityRevision: priorAuthorityRevision,
		AuthorityStatus:        "compound_primary",
		SourceRole:             "evidence_only_input_after_import",
		Retires:                "source_view_authority:" + sourcePath,
		RetirementCondition:    "EffectiveContract reads compound://config/installed-hooks from COMPOUND_HOME; source view remains rollback/evidence input",
		Rollback:               "re-import source hooks JSON or remove COMPOUND_HOME config/installed-hooks.yaml",
		Harnesses:              installedHookHarnesses(finalManifest),
		IncomingHookCount:      incomingHookCount,
		HookCount:              len(finalManifest.Hooks),
		EvidenceRefs:           evidenceRefs,
	}
	provenance := hookInstallAuthoritySnapshotForPath(snapshots, InstalledHooksSourceAuthorityPath(homePath))
	if provenance == nil {
		return fmt.Errorf("installed hook provenance snapshot missing")
	}
	duplicate, parseErr := installedHookAuthorityRecordExists(provenance.data, record.ID)
	if parseErr != nil {
		// Preserve the importer's existing failure order: malformed provenance is
		// discovered after the manifest write. Its unchanged preimage is still
		// recognizable during rollback; any other state becomes a conflict.
		return nil
	}
	recordData, err := json.Marshal(record)
	if err != nil {
		return err
	}
	provenance.postKnown = true
	provenance.postExists = true
	provenance.postMode = provenance.mode
	if !provenance.existed {
		provenance.postMode, err = hookInstallAuthorityNewFileMode(provenance.path, 0o644)
		if err != nil {
			return fmt.Errorf("derive installed hook provenance mode: %w", err)
		}
	}
	if duplicate {
		provenance.postData = append([]byte(nil), provenance.data...)
		return nil
	}
	provenance.postData = append(append([]byte(nil), provenance.data...), append(recordData, '\n')...)
	return nil
}

func hookInstallAuthorityNewFileMode(path string, requested os.FileMode) (os.FileMode, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	placeholder, err := os.CreateTemp(dir, ".compound-hook-authority-mode-*")
	if err != nil {
		return 0, err
	}
	probePath := placeholder.Name()
	if err := errors.Join(placeholder.Close(), os.Remove(probePath)); err != nil {
		return 0, err
	}
	probe, err := os.OpenFile(probePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, requested)
	if err != nil {
		return 0, err
	}
	info, statErr := probe.Stat()
	cleanupErr := errors.Join(probe.Close(), os.Remove(probePath))
	if statErr != nil || cleanupErr != nil {
		return 0, errors.Join(statErr, cleanupErr)
	}
	return info.Mode(), nil
}

func hookInstallAuthoritySnapshotForPath(
	snapshots []hookInstallAuthoritySnapshot,
	path string,
) *hookInstallAuthoritySnapshot {
	for i := range snapshots {
		if snapshots[i].path == path {
			return &snapshots[i]
		}
	}
	return nil
}

func installedHookAuthorityRecordExists(data []byte, id string) (bool, error) {
	for lineIndex, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record InstalledHooksSourceAuthority
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return false, fmt.Errorf("parse installed hooks source authority line %d: %w", lineIndex+1, err)
		}
		if strings.TrimSpace(record.ID) == "" {
			return false, fmt.Errorf("installed hooks source authority line %d missing id", lineIndex+1)
		}
		if record.ID == id {
			return true, nil
		}
	}
	return false, nil
}

type hookInstallAuthorityState struct {
	exists  bool
	regular bool
	data    []byte
	mode    os.FileMode
}

func readHookInstallAuthorityState(path string) (hookInstallAuthorityState, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return hookInstallAuthorityState{}, nil
	}
	if err != nil {
		return hookInstallAuthorityState{}, err
	}
	state := hookInstallAuthorityState{
		exists:  true,
		regular: info.Mode().IsRegular(),
		mode:    info.Mode(),
	}
	if !state.regular {
		return state, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return hookInstallAuthorityState{}, err
	}
	afterReadInfo, err := os.Lstat(path)
	if err != nil {
		return hookInstallAuthorityState{}, err
	}
	if !afterReadInfo.Mode().IsRegular() || !os.SameFile(info, afterReadInfo) {
		return hookInstallAuthorityState{}, fmt.Errorf("authority changed while reading")
	}
	state.data = data
	state.mode = afterReadInfo.Mode()
	return state, nil
}

func hookInstallAuthorityStateMatches(
	current hookInstallAuthorityState,
	exists bool,
	data []byte,
	mode os.FileMode,
) bool {
	if current.exists != exists {
		return false
	}
	if !exists {
		return true
	}
	return current.regular && bytes.Equal(current.data, data) && current.mode == mode
}

func rollbackHookInstallAuthority(snapshots []hookInstallAuthoritySnapshot) error {
	var rollbackErr error
	for i := len(snapshots) - 1; i >= 0; i-- {
		snapshot := snapshots[i]
		current, err := readHookInstallAuthorityState(snapshot.path)
		if err != nil {
			rollbackErr = errors.Join(
				rollbackErr,
				fmt.Errorf("installed hook authority rollback conflict at %s: %w", snapshot.path, err),
			)
			continue
		}
		if hookInstallAuthorityStateMatches(
			current,
			snapshot.existed,
			snapshot.data,
			snapshot.mode,
		) {
			continue
		}
		if !snapshot.postKnown || !hookInstallAuthorityStateMatches(
			current,
			snapshot.postExists,
			snapshot.postData,
			snapshot.postMode,
		) {
			rollbackErr = errors.Join(
				rollbackErr,
				fmt.Errorf(
					"installed hook authority rollback conflict at %s: current state does not match transaction post-image",
					snapshot.path,
				),
			)
			continue
		}
		if !snapshot.existed {
			if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
				rollbackErr = errors.Join(
					rollbackErr,
					fmt.Errorf("rollback installed hook authority %s: %w", snapshot.path, err),
				)
			}
			continue
		}
		if err := writeHookInstallAuthorityAtomically(snapshot.path, snapshot.data, snapshot.mode); err != nil {
			rollbackErr = errors.Join(
				rollbackErr,
				fmt.Errorf("rollback installed hook authority %s: %w", snapshot.path, err),
			)
		}
	}
	return rollbackErr
}

func writeHookInstallAuthorityAtomically(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".compound-hook-authority-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return errors.Join(err, temp.Close())
	}
	if _, err := temp.Write(data); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	keepTemp = false
	return nil
}

// ReconcileInstalledHooks reads Compound's desired installed-hooks authority
// and read-only harness config evidence rooted at harnessHome. It never writes
// either source and never promotes observed registrations into HookPlan.
func ReconcileInstalledHooks(compoundHome, harnessHome string, registry harness.Registry) (InstalledHooksReconciliation, error) {
	manifest, err := LoadInstalledHooksManifest(compoundHome)
	if err != nil {
		if !os.IsNotExist(err) {
			return InstalledHooksReconciliation{}, err
		}
		manifest = InstalledHooksManifest{SchemaVersion: installedHooksSchemaVersion}
	}
	plan, err := hookPlanFromInstalledManifest(manifest, harnessHome)
	if err != nil {
		return InstalledHooksReconciliation{}, err
	}
	observation, err := observeHarnessHookInstallation(harnessHome)
	if err != nil {
		return InstalledHooksReconciliation{}, err
	}
	reconciliation, err := hookplan.Reconcile(plan, observation, registry)
	if err != nil {
		return InstalledHooksReconciliation{}, err
	}
	return InstalledHooksReconciliation{
		Plan:           plan,
		Observation:    observation,
		Reconciliation: reconciliation,
	}, nil
}

func hookPlanFromInstalledManifest(manifest InstalledHooksManifest, harnessHome string) (hookplan.HookPlan, error) {
	plan := hookplan.HookPlan{
		SchemaVersion: hookplan.HookPlanSchemaVersion,
		ID:            "hook-plan",
		AuthorityRef:  InstalledHooksAuthorityRef,
	}
	for _, hook := range manifest.Hooks {
		status := strings.TrimSpace(hook.Status)
		if !desiredHookStatus(status) && status != "preserved_external" {
			continue
		}
		event := strings.TrimSpace(hook.Event)
		if event == "" {
			event = strings.TrimSpace(hook.Name)
		}
		binding := hookplan.Binding{
			Harness:            strings.TrimSpace(hook.Harness),
			Event:              event,
			Matcher:            strings.TrimSpace(hook.Matcher),
			HandlerRef:         canonicalHookHandlerRef(hook.Ref, harnessHome),
			InvocationRevision: strings.TrimSpace(hook.InvocationRevision),
			HandlerRevision:    strings.TrimSpace(hook.Revision),
			Activation:         hookActivation(status),
			Blocking:           event == "PreToolUse" && status != "non_blocking",
		}
		if status == "preserved_external" {
			binding.ID = preservedHookBindingID(binding)
			binding.Activation = ""
			binding.Blocking = false
			plan.PreservedBindings = append(plan.PreservedBindings, binding)
			continue
		}
		binding.ID = desiredHookBindingID(binding)
		plan.Bindings = append(plan.Bindings, binding)
	}
	sort.SliceStable(plan.Bindings, func(i, j int) bool {
		return desiredHookBindingSortKey(plan.Bindings[i]) < desiredHookBindingSortKey(plan.Bindings[j])
	})
	sort.SliceStable(plan.PreservedBindings, func(i, j int) bool {
		return desiredHookBindingSortKey(plan.PreservedBindings[i]) < desiredHookBindingSortKey(plan.PreservedBindings[j])
	})
	encoded, err := json.Marshal(struct {
		Bindings          []hookplan.Binding `json:"bindings"`
		PreservedBindings []hookplan.Binding `json:"preserved_bindings"`
	}{Bindings: plan.Bindings, PreservedBindings: plan.PreservedBindings})
	if err != nil {
		return hookplan.HookPlan{}, fmt.Errorf("marshal desired hook plan: %w", err)
	}
	plan.Revision = "sha256:" + hashBytes(encoded)
	return plan, nil
}

func desiredHookStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "", "current", "active", "non_blocking", "pending", "pending_activation", "activation_required":
		return true
	default:
		return false
	}
}

func hookActivation(status string) string {
	switch strings.TrimSpace(status) {
	case "pending", "pending_activation", "activation_required":
		return hookplan.ActivationOperator
	default:
		return hookplan.ActivationManaged
	}
}

func desiredHookBindingID(binding hookplan.Binding) string {
	id := "hook://" + binding.Harness + "/" + binding.Event
	if binding.Matcher != "" {
		id += "?matcher=" + hashBytes([]byte(binding.Matcher))
	}
	return id
}

func preservedHookBindingID(binding hookplan.Binding) string {
	id := "external-hook://" + binding.Harness + "/" + binding.Event
	identity := strings.Join([]string{binding.Matcher, binding.HandlerRef, binding.InvocationRevision}, "\x00")
	return id + "?identity=" + hashBytes([]byte(identity))
}

func desiredHookBindingSortKey(binding hookplan.Binding) string {
	return strings.Join([]string{binding.Harness, binding.Event, binding.Matcher, binding.ID, binding.HandlerRef}, "\x00")
}

type observedHarnessHooksFile struct {
	Hooks map[string][]observedHarnessHookGroup `json:"hooks"`
}

type observedHarnessHookGroup struct {
	Matcher  string                       `json:"matcher"`
	Disabled bool                         `json:"disabled"`
	Hooks    []observedHarnessHookCommand `json:"hooks"`
}

type observedHarnessHookCommand struct {
	Type     string `json:"type"`
	Command  string `json:"command"`
	Disabled bool   `json:"disabled"`
}

type hookInstallationSource struct {
	Harness string
	Ref     string
	Path    string
}

func observeHarnessHookInstallation(harnessHome string) (hookplan.InstallationSnapshot, error) {
	snapshot := hookplan.InstallationSnapshot{
		SchemaVersion: hookplan.InstallationSnapshotSchemaVersion,
		Ref:           liveHookInstallationRef,
	}
	sources := []hookInstallationSource{
		{Harness: "claude_code", Ref: "harness://claude_code/settings", Path: filepath.Join(harnessHome, ".claude", "settings.json")},
		{Harness: "claude_zai", Ref: "harness://claude_zai/settings", Path: filepath.Join(harnessHome, ".config", "switchback", "claude", "zai-direct", "settings.json")},
		{Harness: "claude_minimax", Ref: "harness://claude_minimax/settings", Path: filepath.Join(harnessHome, ".config", "switchback", "claude", "_providers", "minimax-lane", "settings.json")},
		{Harness: "claude_qwen", Ref: "harness://claude_qwen/settings", Path: filepath.Join(harnessHome, ".config", "switchback", "claude", "_providers", "qwen", "settings.json")},
		{Harness: "codex", Ref: "harness://codex/hooks", Path: filepath.Join(harnessHome, ".codex", "hooks.json")},
	}
	var sourceRevisions []string
	seen := map[string]bool{}
	for _, source := range sources {
		data, err := os.ReadFile(source.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return hookplan.InstallationSnapshot{}, fmt.Errorf("read %s hook installation: %w", source.Harness, err)
		}
		sourceRevisions = append(sourceRevisions, source.Ref+"@sha256:"+hashBytes(data))
		bindings, err := parseObservedHarnessHooks(data, source, harnessHome)
		if err != nil {
			return hookplan.InstallationSnapshot{}, err
		}
		for _, binding := range bindings {
			key := strings.Join([]string{binding.Harness, binding.Event, binding.Matcher, binding.HandlerRef, binding.InvocationRevision, binding.SourceRef}, "\x00")
			if seen[key] {
				continue
			}
			seen[key] = true
			snapshot.Bindings = append(snapshot.Bindings, binding)
		}
	}
	sort.SliceStable(snapshot.Bindings, func(i, j int) bool {
		return observedHookBindingSortKey(snapshot.Bindings[i]) < observedHookBindingSortKey(snapshot.Bindings[j])
	})
	revisionMaterial, err := json.Marshal(struct {
		Sources  []string                   `json:"sources"`
		Bindings []hookplan.ObservedBinding `json:"bindings"`
	}{Sources: sourceRevisions, Bindings: snapshot.Bindings})
	if err != nil {
		return hookplan.InstallationSnapshot{}, fmt.Errorf("marshal observed hook installation: %w", err)
	}
	snapshot.Revision = "sha256:" + hashBytes(revisionMaterial)
	return snapshot, nil
}

func parseObservedHarnessHooks(data []byte, source hookInstallationSource, harnessHome string) ([]hookplan.ObservedBinding, error) {
	var file observedHarnessHooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s hook installation: %w", source.Harness, err)
	}
	events := make([]string, 0, len(file.Hooks))
	for event := range file.Hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	var bindings []hookplan.ObservedBinding
	for _, event := range events {
		for _, group := range file.Hooks[event] {
			for _, command := range group.Hooks {
				if command.Type != "" && command.Type != "command" {
					continue
				}
				handlerRef := canonicalHookHandlerRef(command.Command, harnessHome)
				if handlerRef == "" {
					continue
				}
				bindings = append(bindings, hookplan.ObservedBinding{
					Harness:            source.Harness,
					Event:              event,
					Matcher:            strings.TrimSpace(group.Matcher),
					HandlerRef:         handlerRef,
					InvocationRevision: canonicalHookInvocationRevision(command.Command, harnessHome),
					HandlerRevision:    installedHookHandlerRevision(handlerRef, harnessHome),
					Active:             !group.Disabled && !command.Disabled,
					SourceRef:          source.Ref,
				})
			}
		}
	}
	return bindings, nil
}

func observedHookBindingSortKey(binding hookplan.ObservedBinding) string {
	return strings.Join([]string{binding.Harness, binding.Event, binding.Matcher, binding.BindingID, binding.HandlerRef, binding.InvocationRevision, binding.SourceRef}, "\x00")
}

func canonicalHookHandlerRef(value, harnessHome string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	normalized := strings.ReplaceAll(value, "${HOME}", "$HOME")
	normalizationHome := strings.TrimSpace(harnessHome)
	if normalizationHome == "" {
		normalizationHome, _ = os.UserHomeDir()
	}
	if normalizationHome != "" {
		normalized = strings.ReplaceAll(normalized, filepath.Clean(normalizationHome), "$HOME")
	}
	if strings.HasPrefix(normalized, "~/") {
		normalized = "$HOME/" + strings.TrimPrefix(normalized, "~/")
	}
	fields := canonicalHookCommandFields(normalized)
	for _, field := range fields {
		if strings.HasPrefix(field, "handler:") {
			return field
		}
	}
	for _, field := range fields {
		if field != "compound" && !strings.HasSuffix(field, "/.local/bin/compound") {
			continue
		}
		return field
	}
	for i := len(fields) - 1; i >= 0; i-- {
		field := fields[i]
		for _, marker := range []string{"$HOME/.claude/hooks/", "$HOME/.codex/hooks/"} {
			if index := strings.Index(field, marker); index >= 0 {
				return strings.TrimRight(field[index:], `"'`)
			}
		}
		if strings.HasPrefix(field, "hook://") || strings.HasPrefix(field, "handler:") {
			return field
		}
	}
	return "handler:" + canonicalHookInvocationRevision(normalized, "")
}

func canonicalHookInvocationRevision(value, harnessHome string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	normalized := strings.ReplaceAll(value, "${HOME}", "$HOME")
	normalizationHome := strings.TrimSpace(harnessHome)
	if normalizationHome == "" {
		normalizationHome, _ = os.UserHomeDir()
	}
	if normalizationHome != "" {
		normalized = strings.ReplaceAll(normalized, filepath.Clean(normalizationHome), "$HOME")
	}
	if strings.HasPrefix(normalized, "~/") {
		normalized = "$HOME/" + strings.TrimPrefix(normalized, "~/")
	}
	return "sha256:" + hashBytes([]byte(strings.Join(canonicalHookCommandFields(normalized), " ")))
}

// installedHookHandlerRevision independently attests the bytes behind a concrete handler ref.
// Opaque handler identities remain unverifiable (empty); a desired concrete revision paired with
// an empty observation reconciles as drift, so missing/unreadable installed handlers fail loudly.
func installedHookHandlerRevision(handlerRef, harnessHome string) string {
	path := strings.TrimSpace(handlerRef)
	if path == "" || strings.HasPrefix(path, "handler:") || strings.HasPrefix(path, "hook://") {
		return ""
	}
	home := strings.TrimSpace(harnessHome)
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" {
		path = strings.ReplaceAll(path, "${HOME}", home)
		path = strings.ReplaceAll(path, "$HOME", home)
		if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return "sha256:" + hashBytesFull(data)
}

// canonicalHookCommandFields tokenizes a hook command the way a POSIX shell would: whitespace
// separates fields, but text wrapped in single or double quotes is kept as ONE field even if it
// contains whitespace. This matters because hookplan.HookCommand shell-quotes the binary path
// (internal/hookplan/install.go) so a spaced install path (e.g. an app-bundle path like
// "/tmp/Compound Apps/compound") execs correctly — the naive strings.Fields split used to tear a
// quoted-spaced path into multiple bogus fields, which corrupted canonicalHookHandlerRef /
// canonicalHookInvocationRevision and made a correctly-quoted install look like handler drift. A
// command with no quotes at all (the historical case) tokenizes identically to before: this walk
// degenerates to plain whitespace splitting whenever no quote character is present.
func canonicalHookCommandFields(command string) []string {
	var fields []string
	var current strings.Builder
	hasField := false
	var quote rune
	for _, r := range command {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			hasField = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if hasField {
				fields = append(fields, current.String())
				current.Reset()
				hasField = false
			}
		default:
			current.WriteRune(r)
			hasField = true
		}
	}
	if hasField {
		fields = append(fields, current.String())
	}
	// Harmless fallback: trim any stray quote characters that survived tokenizing (e.g. an
	// unmatched quote), matching the pre-tokenizer behavior for fields that never had a matched
	// quote pair around them.
	for i := range fields {
		fields[i] = strings.Trim(fields[i], "\"'`")
	}
	return fields
}
