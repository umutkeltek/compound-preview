package harness

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"compound/internal/envelope"
	homepkg "compound/internal/home"
	"compound/internal/policy"

	"gopkg.in/yaml.v3"
)

const (
	PermissionAuthoritySchemaVersion = "compound.harness_permission_authority.v0.1"
	HarnessArtifactSchemaVersion     = "compound.harness_artifact.v0.1"
)

type PermissionRules struct {
	Allow       []string `json:"allow" yaml:"allow"`
	Deny        []string `json:"deny" yaml:"deny"`
	Ask         []string `json:"ask" yaml:"ask"`
	DefaultMode string   `json:"default_mode" yaml:"default_mode"`
	// Ownership is empty/"managed" for additive reconciliation or "exact"
	// when Compound owns the complete permissions arrays. Exact ownership is
	// opt-in so existing harness authorities retain their hand-owned entries.
	Ownership string `json:"ownership,omitempty" yaml:"ownership,omitempty"`
}

type AutoModePolicy struct {
	Allow []string `json:"allow" yaml:"allow"`
}

type PermissionAuthority struct {
	SchemaVersion string          `json:"schema_version" yaml:"schema_version"`
	Harness       string          `json:"harness" yaml:"harness"`
	AuthorityRef  string          `json:"authority_ref" yaml:"authority_ref"`
	TargetPath    string          `json:"target_path" yaml:"target_path"`
	Permissions   PermissionRules `json:"permissions" yaml:"permissions"`
	AutoMode      AutoModePolicy  `json:"auto_mode" yaml:"auto_mode"`
	// Settings declares an owned dotted-path key set for a TOML target
	// (surgical line-edit mode). A dotted prefix names a TOML table (e.g.
	// "features.guardian_approval" owns the guardian_approval key inside
	// [features]); no dot means a top-level key. Only present for harnesses
	// that opt into TOML posture ownership; nil/empty keeps the existing
	// JSON-settings rendering path.
	Settings       map[string]any  `json:"settings,omitempty" yaml:"settings,omitempty"`
	SettingSources []SettingSource `json:"setting_sources,omitempty" yaml:"setting_sources,omitempty"`
}

type HarnessArtifact struct {
	SchemaVersion      string              `json:"schema_version"`
	Path               string              `json:"path"`
	Harness            string              `json:"harness"`
	SourceAuthorityRef string              `json:"source_authority_ref"`
	SourceRevision     string              `json:"source_revision"`
	ContentSHA256      string              `json:"content_sha256"`
	GeneratedAt        string              `json:"generated_at,omitempty"`
	ManagedEntries     map[string][]string `json:"managed_entries,omitempty"`
}

type PermissionRender struct {
	TargetPath string          `json:"target_path"`
	Changed    bool            `json:"changed"`
	Content    []byte          `json:"-"`
	Preimage   []byte          `json:"-"`
	PreExisted bool            `json:"-"`
	Artifact   HarnessArtifact `json:"artifact"`
	Details    []string        `json:"details,omitempty"`
}

const permissionApplyPendingSchemaVersion = "compound.harness_permission_apply_pending.v0.3"

type permissionApplyPending struct {
	SchemaVersion      string                  `json:"schema_version"`
	Harness            string                  `json:"harness"`
	TargetPath         string                  `json:"target_path"`
	RecoveryName       string                  `json:"recovery_name,omitempty"`
	RollbackName       string                  `json:"rollback_name,omitempty"`
	BeforeExisted      bool                    `json:"before_existed"`
	BeforeContent      []byte                  `json:"before_content,omitempty"`
	BeforeSHA256       string                  `json:"before_sha256,omitempty"`
	IntendedContent    []byte                  `json:"intended_content"`
	IntendedSHA256     string                  `json:"intended_sha256"`
	SourceAuthorityRef string                  `json:"source_authority_ref"`
	SourceRevision     string                  `json:"source_revision"`
	Artifact           HarnessArtifact         `json:"artifact"`
	Details            []string                `json:"details,omitempty"`
	Action             envelope.ActionIntent   `json:"action_intent"`
	Decision           envelope.PolicyDecision `json:"policy_decision"`
	AuthorizedAt       string                  `json:"authorized_at"`
	TargetChanged      bool                    `json:"target_changed"`
	ManifestChanged    bool                    `json:"manifest_changed"`
	Changed            bool                    `json:"changed"`
	FilesChanged       []string                `json:"files_changed"`
}

type PermissionApplyOutcome struct {
	Render          PermissionRender
	Action          envelope.ActionIntent
	Decision        envelope.PolicyDecision
	AuthorizedAt    string
	TargetChanged   bool
	ManifestChanged bool
	Changed         bool
	FilesChanged    []string
}

type PermissionApplyFinalize func(PermissionApplyOutcome) error

// PermissionApplyReserve atomically performs the supplied authorization
// revalidation and creates the work-owned lifecycle blocker for this outcome.
type PermissionApplyReserve func(PermissionApplyOutcome, PermissionApplyAuthorization) error

// PermissionApplyAbort clears a reservation only after the adapter has safely
// restored the pre-publication state.
type PermissionApplyAbort func(PermissionApplyOutcome) error

type PermissionApplyFinalizationPendingError struct {
	Cause error
}

func (e *PermissionApplyFinalizationPendingError) Error() string {
	return fmt.Sprintf("permission outcome is published with exact identity pending finalization: %v", e.Cause)
}

func (e *PermissionApplyFinalizationPendingError) Unwrap() error { return e.Cause }

func IsPermissionApplyFinalizationPending(err error) bool {
	var pending *PermissionApplyFinalizationPendingError
	return errors.As(err, &pending)
}

type PermissionDriftReport struct {
	Harness        string                `json:"harness"`
	TargetPath     string                `json:"target_path"`
	TargetRevision string                `json:"target_revision,omitempty"`
	Drifted        bool                  `json:"drifted"`
	Details        []string              `json:"details,omitempty"`
	SettingSources []SettingSourceReport `json:"setting_sources,omitempty"`
}

func PermissionAuthorityRef(harness string) string {
	return "compound://config/harness-permissions/" + strings.TrimSpace(harness)
}

func PermissionAuthorityPath(homePath, harness string) string {
	return filepath.Join(homePath, "config", "harness-permissions", strings.TrimSpace(harness)+".yaml")
}

func HarnessArtifactManifestPath(homePath string) string {
	return filepath.Join(homePath, "config", "harness-artifacts.jsonl")
}

func (p PermissionAuthority) Validate() error {
	if p.SchemaVersion != PermissionAuthoritySchemaVersion {
		return fmt.Errorf("schema_version = %q, want %q", p.SchemaVersion, PermissionAuthoritySchemaVersion)
	}
	if strings.TrimSpace(p.Harness) == "" || strings.TrimSpace(p.TargetPath) == "" {
		return fmt.Errorf("harness and target_path are required")
	}
	if strings.TrimSpace(p.AuthorityRef) != PermissionAuthorityRef(p.Harness) {
		return fmt.Errorf("authority_ref = %q, want %q", p.AuthorityRef, PermissionAuthorityRef(p.Harness))
	}
	if ownership := strings.TrimSpace(p.Permissions.Ownership); ownership != "" && ownership != "managed" && ownership != "exact" {
		return fmt.Errorf("permissions.ownership = %q, want managed or exact", p.Permissions.Ownership)
	}
	for name, rules := range map[string][]string{
		"permissions.allow": p.Permissions.Allow,
		"permissions.deny":  p.Permissions.Deny,
		"permissions.ask":   p.Permissions.Ask,
		"auto_mode.allow":   p.AutoMode.Allow,
	} {
		seen := map[string]bool{}
		for _, rule := range rules {
			rule = strings.TrimSpace(rule)
			if rule == "" {
				return fmt.Errorf("%s contains an empty rule", name)
			}
			if seen[rule] {
				return fmt.Errorf("%s contains duplicate rule %q", name, rule)
			}
			seen[rule] = true
		}
	}
	for _, rule := range p.Permissions.Allow {
		if strings.TrimSpace(rule) == "mcp__*" {
			return fmt.Errorf("permissions.allow rule %q is unsupported; use a literal server rule such as mcp__gbrain__*", rule)
		}
		if p.Harness == "claude_code" {
			if canonical, legacy := canonicalClaudeFileEditRule(rule); legacy {
				return fmt.Errorf("permissions.allow rule %q is unsupported by current Claude Code file checks; use %s", rule, canonical)
			}
		}
	}
	if len(p.AutoMode.Allow) == 0 || strings.TrimSpace(p.AutoMode.Allow[0]) != "$defaults" {
		return fmt.Errorf("auto_mode.allow must place $defaults first")
	}
	for i := 1; i < len(p.AutoMode.Allow); i++ {
		if strings.TrimSpace(p.AutoMode.Allow[i]) == "$defaults" {
			return fmt.Errorf("auto_mode.allow must contain $defaults once, first")
		}
	}
	if len(p.Settings) > 0 {
		paths := make([]string, 0, len(p.Settings))
		for path := range p.Settings {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			if strings.TrimSpace(path) == "" {
				return fmt.Errorf("settings contains an empty key path")
			}
			if _, _, err := splitTOMLSettingPath(path); err != nil {
				return err
			}
			if _, err := formatTOMLScalar(path, p.Settings[path]); err != nil {
				return err
			}
		}
	}
	return nil
}

func LoadPermissionAuthority(homePath, harness string) (PermissionAuthority, string, error) {
	path := PermissionAuthorityPath(homePath, harness)
	// Approval input must remain in owned custody, never follow an escaped
	// leaf or parent into an executor-writable external referent.
	body, err := homepkg.ReadRootedRegularFile(homePath, path, "permission authority")
	if err != nil {
		return PermissionAuthority{}, "", fmt.Errorf("read permission authority %s: %w", path, err)
	}
	var policy PermissionAuthority
	if err := yaml.Unmarshal(body, &policy); err != nil {
		return PermissionAuthority{}, "", fmt.Errorf("parse permission authority %s: %w", path, err)
	}
	if err := policy.Validate(); err != nil {
		return PermissionAuthority{}, "", fmt.Errorf("validate permission authority %s: %w", path, err)
	}
	if policy.Harness != strings.TrimSpace(harness) {
		return PermissionAuthority{}, "", fmt.Errorf("permission authority %s declares harness %q, want %q", path, policy.Harness, strings.TrimSpace(harness))
	}
	policy.Permissions.Allow = sortedRules(policy.Permissions.Allow)
	policy.Permissions.Deny = sortedRules(policy.Permissions.Deny)
	policy.Permissions.Ask = sortedRules(policy.Permissions.Ask)
	policy.AutoMode.Allow = append([]string{"$defaults"}, sortedRules(policy.AutoMode.Allow[1:])...)
	return policy, "sha256:" + digest(body), nil
}

func RenderPermissions(homePath, harness string) (PermissionRender, error) {
	policy, sourceRevision, err := LoadPermissionAuthority(homePath, harness)
	if err != nil {
		return PermissionRender{}, err
	}
	if tomlSettingsMode(policy) {
		return renderTOMLPermissions(harness, policy, sourceRevision)
	}
	before, err := os.ReadFile(policy.TargetPath)
	if err != nil && !os.IsNotExist(err) {
		return PermissionRender{}, fmt.Errorf("read settings target %s: %w", policy.TargetPath, err)
	}
	beforeExisted := err == nil
	settings := map[string]any{}
	if len(bytes.TrimSpace(before)) > 0 {
		if err := json.Unmarshal(before, &settings); err != nil {
			return PermissionRender{}, fmt.Errorf("settings target %s is not valid JSON: %w", policy.TargetPath, err)
		}
	}
	previous, _, err := latestHarnessArtifact(homePath, harness, policy.TargetPath)
	if err != nil {
		return PermissionRender{}, err
	}
	permissions := objectAt(settings, "permissions")
	if policy.Permissions.Ownership == "exact" {
		permissions["allow"] = append([]string(nil), policy.Permissions.Allow...)
		permissions["deny"] = append([]string(nil), policy.Permissions.Deny...)
		permissions["ask"] = append([]string(nil), policy.Permissions.Ask...)
	} else {
		currentAllow := stringSlice(permissions["allow"])
		if policy.Harness == "claude_code" {
			currentAllow = canonicalizeUnmanagedClaudeFileEditRules(currentAllow, previous.ManagedEntries["permissions.allow"])
		}
		permissions["allow"] = reconcileRules(currentAllow, previous.ManagedEntries["permissions.allow"], policy.Permissions.Allow, false)
		permissions["deny"] = reconcileRules(stringSlice(permissions["deny"]), previous.ManagedEntries["permissions.deny"], policy.Permissions.Deny, false)
		permissions["ask"] = reconcileRules(stringSlice(permissions["ask"]), previous.ManagedEntries["permissions.ask"], policy.Permissions.Ask, false)
	}
	permissions["defaultMode"] = policy.Permissions.DefaultMode
	settings["permissions"] = permissions
	autoMode := objectAt(settings, "autoMode")
	autoMode["allow"] = reconcileRules(stringSlice(autoMode["allow"]), previous.ManagedEntries["autoMode.allow"], policy.AutoMode.Allow, true)
	settings["autoMode"] = autoMode
	// Owned dotted paths reconcile after the permission objects so a settings
	// path may deliberately override a rendered key rather than race it.
	managedSettings, err := applyJSONSettingsPaths(settings, policy.Settings, previous.ManagedEntries)
	if err != nil {
		return PermissionRender{}, err
	}
	content, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return PermissionRender{}, err
	}
	content = append(content, '\n')
	managedEntries := map[string][]string{
		"permissions.allow": append([]string(nil), policy.Permissions.Allow...),
		"permissions.deny":  append([]string(nil), policy.Permissions.Deny...),
		"permissions.ask":   append([]string(nil), policy.Permissions.Ask...),
		"autoMode.allow":    append([]string(nil), policy.AutoMode.Allow...),
	}
	for key, value := range managedSettings {
		managedEntries[key] = value
	}
	artifact := HarnessArtifact{
		SchemaVersion: HarnessArtifactSchemaVersion, Path: policy.TargetPath, Harness: harness,
		SourceAuthorityRef: policy.AuthorityRef, SourceRevision: sourceRevision,
		ContentSHA256:  "sha256:" + digest(content),
		ManagedEntries: managedEntries,
	}
	return PermissionRender{
		TargetPath: policy.TargetPath,
		Changed:    !bytes.Equal(before, content),
		Content:    content,
		Preimage:   append([]byte(nil), before...),
		PreExisted: beforeExisted,
		Artifact:   artifact,
		Details:    permissionDelta(previous, policy),
	}, nil
}

var writeHarnessArtifactManifestForApply = WriteHarnessArtifactManifest
var applyPermissionsBeforeTargetCommit = func() {}
var applyPermissionsAfterTargetWrite = func() error { return nil }
var permissionApplyRecoveryAfterQuarantine = func(_, _ string) {}
var permissionApplyAfterCanonicalQuarantine = func(_ string) {}
var permissionApplyNow = time.Now
var permissionApplySyncBoundary = func(_ string, path string) error {
	return syncPermissionParentDirectory(path)
}

type PermissionApplyAuthorization func(action envelope.ActionIntent, decision envelope.PolicyDecision, now time.Time) error

func PermissionApplyPendingPath(homePath, harness string) string {
	return filepath.Join(homePath, "work", "transactions", "harness-permissions", strings.TrimSpace(harness)+".json")
}

func permissionApplyLockPath(homePath string) string {
	return filepath.Join(homePath, "lock", "harness-permissions.lock")
}

func permissionApplyLegacyPendingPath(homePath, harness string) string {
	return filepath.Join(homePath, "transactions", "harness-permissions", strings.TrimSpace(harness)+".json")
}

// ProbePermissionApplyRecovery returns the one pending journal a recovery may
// consume. Legacy-only state remains recoverable; coexistence is ambiguous and
// fails closed without choosing or removing either journal.
func ProbePermissionApplyRecovery(homePath, harness string) (string, bool, error) {
	canonicalPath := PermissionApplyPendingPath(homePath, harness)
	legacyPath := permissionApplyLegacyPendingPath(homePath, harness)
	canonicalExists, err := permissionApplyPendingExists(canonicalPath)
	if err != nil {
		return "", false, fmt.Errorf("inspect canonical pending permission apply transaction: %w", err)
	}
	legacyExists, err := permissionApplyPendingExists(legacyPath)
	if err != nil {
		return "", false, fmt.Errorf("inspect legacy pending permission apply transaction: %w", err)
	}
	if canonicalExists && legacyExists {
		return "", false, fmt.Errorf("canonical and legacy pending permission apply transactions coexist at %s and %s; refusing ambiguous recovery", canonicalPath, legacyPath)
	}
	if canonicalExists {
		return canonicalPath, true, nil
	}
	if legacyExists {
		return legacyPath, true, nil
	}
	return canonicalPath, false, nil
}

func permissionApplyPendingExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// ApplyPermissions reconciles only Compound-managed permission entries, writes
// the target through a recoverable transaction, and records the exact authority
// revision/content hash. The lock serializes Compound writers; the preimage CAS
// refuses a concurrent manual edit. An interrupted transaction stays visible to
// drift and is recovered before the next apply.
func ApplyPermissions(homePath string, approved PermissionRender, action envelope.ActionIntent, decision envelope.PolicyDecision, authorize PermissionApplyAuthorization, reserve PermissionApplyReserve, abort PermissionApplyAbort, finalize PermissionApplyFinalize) (PermissionRender, error) {
	if authorize == nil {
		return PermissionRender{}, fmt.Errorf("permission apply authorization revalidation is required")
	}
	if reserve == nil {
		return PermissionRender{}, fmt.Errorf("permission apply work reservation is required")
	}
	if abort == nil {
		return PermissionRender{}, fmt.Errorf("permission apply reservation abort is required")
	}
	if finalize == nil {
		return PermissionRender{}, fmt.Errorf("permission apply outcome finalization is required")
	}
	harnessName := strings.TrimSpace(approved.Artifact.Harness)
	if !validPermissionLeaf(harnessName) {
		return PermissionRender{}, fmt.Errorf("permission apply harness must be a local leaf")
	}
	var render PermissionRender
	err := withPermissionApplyLock(homePath, func() error {
		recovered, ok, err := recoverPermissionApplyPending(homePath, harnessName, abort, finalize)
		if err != nil {
			return err
		}
		if ok {
			render = recovered.Render
			return nil
		}
		if err := validatePermissionApplyApproval(approved, action, decision); err != nil {
			return err
		}
		current, err := RenderPermissions(homePath, harnessName)
		if err != nil {
			return err
		}
		if !samePermissionRender(current, approved) {
			return fmt.Errorf("approved render no longer matches current authority, target path, preimage, or content")
		}
		render = approved
		previous, manifestPresent, err := latestHarnessArtifact(homePath, harnessName, render.TargetPath)
		if err != nil {
			return err
		}
		manifestChanged := !manifestPresent || !sameHarnessArtifact(previous, render.Artifact)
		applyPermissionsBeforeTargetCommit()
		authorizedAt := permissionApplyNow().UTC()
		outcome := newPermissionApplyOutcome(homePath, render, action, decision, authorizedAt, manifestChanged)
		pending := newPermissionApplyPending(outcome)
		body, err := json.MarshalIndent(pending, "", "  ")
		if err != nil {
			return err
		}
		pendingPath := PermissionApplyPendingPath(homePath, harnessName)
		if err := writePermissionPrivateFileAtomic(pendingPath, append(body, '\n')); err != nil {
			return fmt.Errorf("write permission apply pending transaction: %w", err)
		}
		if err := permissionApplySyncBoundary("pending_write", pendingPath); err != nil {
			if removeErr := removePermissionPending(pendingPath); removeErr != nil {
				return errors.Join(fmt.Errorf("sync permission apply pending transaction: %w", err), removeErr)
			}
			return fmt.Errorf("sync permission apply pending transaction: %w", err)
		}
		if err := reserve(outcome, authorize); err != nil {
			return abortPermissionApplyBeforePublication(outcome, abort, pendingPath, fmt.Errorf("permission apply reservation authorization: %w", err))
		}
		var publication *permissionTargetPublication
		if outcome.TargetChanged {
			publication, err = beginPermissionTargetPublication(render, pending.RecoveryName)
			if err != nil {
				if publication != nil {
					defer publication.Close()
					return &PermissionApplyFinalizationPendingError{Cause: fmt.Errorf("permission target publication is %s and requires recovery: %w", publication.State(), err)}
				}
				return abortPermissionApplyBeforePublication(outcome, abort, pendingPath, err)
			}
			defer publication.Close()
			if err := permissionApplySyncBoundary("target_publish", render.TargetPath); err != nil {
				return rollbackPermissionPublication(publication, outcome, abort, pendingPath, fmt.Errorf("sync permission target publication: %w", err))
			}
			if err := applyPermissionsAfterTargetWrite(); err != nil {
				return fmt.Errorf("permission apply interrupted after target write: %w", err)
			}
		}
		if outcome.ManifestChanged {
			if err := writeHarnessArtifactManifestForApply(homePath, []HarnessArtifact{render.Artifact}); err != nil {
				if publication != nil {
					return rollbackPermissionPublication(publication, outcome, abort, pendingPath, fmt.Errorf("write harness artifact manifest: %w", err))
				}
				return abortPermissionApplyBeforePublication(outcome, abort, pendingPath, fmt.Errorf("write harness artifact manifest: %w", err))
			}
			if err := permissionApplySyncBoundary("manifest_write", HarnessArtifactManifestPath(homePath)); err != nil {
				return &PermissionApplyFinalizationPendingError{Cause: fmt.Errorf("sync harness artifact manifest: %w", err)}
			}
		}
		if err := validatePublishedPermissionOutcome(homePath, pending); err != nil {
			return &PermissionApplyFinalizationPendingError{Cause: err}
		}
		if err := finalize(outcome); err != nil {
			return &PermissionApplyFinalizationPendingError{Cause: err}
		}
		if outcome.TargetChanged {
			if err := cleanupPublishedPermissionRecovery(pending); err != nil {
				return &PermissionApplyFinalizationPendingError{Cause: fmt.Errorf("clean finalized permission recovery: %w", err)}
			}
		}
		if err := removePermissionPending(pendingPath); err != nil {
			return &PermissionApplyFinalizationPendingError{Cause: err}
		}
		return nil
	})
	if err != nil {
		return PermissionRender{}, err
	}
	return render, nil
}

func withPermissionApplyLock(homePath string, fn func() error) error {
	legacyPath := filepath.Join(homePath, "locks", "harness-permissions.lock")
	legacyFile, err := os.OpenFile(legacyPath, os.O_RDWR, 0)
	if err == nil {
		defer legacyFile.Close()
		if err := syscall.Flock(int(legacyFile.Fd()), syscall.LOCK_EX); err != nil {
			return err
		}
		defer syscall.Flock(int(legacyFile.Fd()), syscall.LOCK_UN)
	} else if !os.IsNotExist(err) {
		return err
	}

	path := permissionApplyLockPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

// WithPermissionPostureLock serializes a governed launch's final posture
// revalidation with Compound's permission publisher. Callers should hold it
// only across the live read and process start boundary; long-running child
// execution is deliberately outside the lock.
func WithPermissionPostureLock(homePath string, fn func() error) error {
	if fn == nil {
		return fmt.Errorf("permission posture lock callback required")
	}
	return withPermissionApplyLock(homePath, fn)
}

func recoverPermissionApplyPending(homePath, harness string, abort PermissionApplyAbort, finalize PermissionApplyFinalize) (PermissionApplyOutcome, bool, error) {
	var zero PermissionApplyOutcome
	path, exists, err := ProbePermissionApplyRecovery(homePath, harness)
	if err != nil {
		return zero, false, err
	}
	if !exists {
		return zero, false, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return zero, false, fmt.Errorf("read pending permission apply transaction: %w", err)
	}
	var pending permissionApplyPending
	if err := json.Unmarshal(body, &pending); err != nil {
		return zero, false, fmt.Errorf("parse pending permission apply transaction: %w", err)
	}
	outcome, err := validatePermissionApplyPending(homePath, harness, pending)
	if err != nil {
		return zero, false, err
	}
	current, _, readErr := readPermissionTargetSnapshot(pending.TargetPath)
	currentExisted := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return zero, false, fmt.Errorf("read pending permission target: %w", readErr)
	}
	currentIsBefore := currentExisted == pending.BeforeExisted && (!currentExisted || bytes.Equal(current, pending.BeforeContent))
	currentIsIntended := currentExisted && bytes.Equal(current, pending.IntendedContent)
	manifestIsIntended, err := permissionManifestMatchesPending(homePath, pending)
	if err != nil {
		return zero, false, err
	}
	if currentIsIntended && manifestIsIntended {
		if err := finalize(outcome); err != nil {
			return zero, false, &PermissionApplyFinalizationPendingError{Cause: err}
		}
		if pending.TargetChanged {
			if err := cleanupPublishedPermissionRecovery(pending); err != nil {
				return zero, false, &PermissionApplyFinalizationPendingError{Cause: fmt.Errorf("clean recovered permission publication: %w", err)}
			}
		}
		if err := removePermissionPending(path); err != nil {
			return zero, false, &PermissionApplyFinalizationPendingError{Cause: err}
		}
		return outcome, true, nil
	}
	if !pending.TargetChanged {
		if err := abort(outcome); err != nil {
			return zero, false, fmt.Errorf("abort pre-publication permission reservation: %w", err)
		}
		if err := removePermissionPending(path); err != nil {
			return zero, false, err
		}
		return zero, false, nil
	}
	if !currentIsBefore && !currentIsIntended {
		return zero, false, fmt.Errorf("pending permission apply target changed after interruption; refusing to overwrite manual state")
	}
	if currentIsBefore {
		if !pending.BeforeExisted && pending.RollbackName != "" {
			if err := cleanupInterruptedPermissionCreation(pending, false); err != nil {
				return zero, false, err
			}
		} else if err := cleanupUnusedPermissionRecovery(pending); err != nil {
			return zero, false, err
		}
		if err := abort(outcome); err != nil {
			return zero, false, fmt.Errorf("abort recovered permission reservation: %w", err)
		}
		if err := removePermissionPending(path); err != nil {
			return zero, false, err
		}
		return zero, false, nil
	}
	policyAuthority, sourceRevision, err := LoadPermissionAuthority(homePath, harness)
	if err != nil {
		return zero, false, fmt.Errorf("validate pending permission apply authority: %w", err)
	}
	if pending.SourceRevision != sourceRevision {
		return zero, false, fmt.Errorf("pending permission apply source revision %q no longer matches %q", pending.SourceRevision, sourceRevision)
	}
	if pending.SourceAuthorityRef != policyAuthority.AuthorityRef || pending.TargetPath != policyAuthority.TargetPath {
		return zero, false, fmt.Errorf("pending permission apply authority or target path no longer matches current authority")
	}
	if pending.BeforeExisted {
		publication, err := beginPermissionRecoveryPublication(pending, current)
		if err != nil {
			return zero, false, fmt.Errorf("recover pending permission apply pre-image: %w", err)
		}
		if err := publication.Close(); err != nil {
			return zero, false, fmt.Errorf("close recovered permission target root: %w", err)
		}
		if err := cleanupUnusedPermissionRecovery(pending); err != nil {
			return zero, false, fmt.Errorf("complete recovered permission pre-image: %w", err)
		}
	} else if err := cleanupInterruptedPermissionCreation(pending, true); err != nil {
		return zero, false, fmt.Errorf("recover interrupted permission target creation: %w", err)
	}
	if err := abort(outcome); err != nil {
		return zero, false, fmt.Errorf("abort recovered permission reservation: %w", err)
	}
	if err := removePermissionPending(path); err != nil {
		return zero, false, err
	}
	return zero, false, nil
}

func validPermissionLeaf(name string) bool {
	return name != "" && name != "." && filepath.Base(name) == name && filepath.IsLocal(name)
}

func newPermissionApplyOutcome(homePath string, render PermissionRender, action envelope.ActionIntent, decision envelope.PolicyDecision, authorizedAt time.Time, manifestChanged bool) PermissionApplyOutcome {
	filesChanged := []string{}
	if render.Changed {
		filesChanged = append(filesChanged, render.TargetPath)
	}
	if manifestChanged {
		filesChanged = append(filesChanged, HarnessArtifactManifestPath(homePath))
	}
	return PermissionApplyOutcome{
		Render:          render,
		Action:          action,
		Decision:        decision,
		AuthorizedAt:    authorizedAt.Format(time.RFC3339Nano),
		TargetChanged:   render.Changed,
		ManifestChanged: manifestChanged,
		Changed:         render.Changed || manifestChanged,
		FilesChanged:    filesChanged,
	}
}

func newPermissionApplyPending(outcome PermissionApplyOutcome) permissionApplyPending {
	render := outcome.Render
	pending := permissionApplyPending{
		SchemaVersion:      permissionApplyPendingSchemaVersion,
		Harness:            render.Artifact.Harness,
		TargetPath:         render.TargetPath,
		BeforeExisted:      render.PreExisted,
		BeforeContent:      append([]byte(nil), render.Preimage...),
		IntendedContent:    append([]byte(nil), render.Content...),
		IntendedSHA256:     render.Artifact.ContentSHA256,
		SourceAuthorityRef: render.Artifact.SourceAuthorityRef,
		SourceRevision:     render.Artifact.SourceRevision,
		Artifact:           render.Artifact,
		Details:            append([]string(nil), render.Details...),
		Action:             outcome.Action,
		Decision:           outcome.Decision,
		AuthorizedAt:       outcome.AuthorizedAt,
		TargetChanged:      outcome.TargetChanged,
		ManifestChanged:    outcome.ManifestChanged,
		Changed:            outcome.Changed,
		FilesChanged:       append([]string(nil), outcome.FilesChanged...),
	}
	if render.PreExisted {
		pending.BeforeSHA256 = "sha256:" + digest(render.Preimage)
	}
	if outcome.TargetChanged {
		pending.RecoveryName = permissionRecoveryName(render)
		pending.RollbackName = permissionCreationRollbackName(render)
	}
	return pending
}

func validatePermissionApplyPending(homePath, harness string, pending permissionApplyPending) (PermissionApplyOutcome, error) {
	var zero PermissionApplyOutcome
	if pending.SchemaVersion != permissionApplyPendingSchemaVersion || pending.Harness != harness || strings.TrimSpace(pending.TargetPath) == "" {
		return zero, fmt.Errorf("pending permission apply transaction identity is invalid")
	}
	if err := validatePermissionRecoveryNames(pending); err != nil {
		return zero, err
	}
	targetChanged := !pending.BeforeExisted || !bytes.Equal(pending.BeforeContent, pending.IntendedContent)
	if pending.TargetChanged != targetChanged {
		return zero, fmt.Errorf("pending permission apply target outcome does not match derived content change")
	}
	if pending.TargetChanged != (pending.RecoveryName != "") || pending.TargetChanged != (pending.RollbackName != "") {
		return zero, fmt.Errorf("pending permission apply recovery identity does not match target outcome")
	}
	if pending.BeforeExisted {
		if pending.BeforeSHA256 != "sha256:"+digest(pending.BeforeContent) {
			return zero, fmt.Errorf("pending permission apply transaction pre-image digest is invalid")
		}
	} else if len(pending.BeforeContent) != 0 || pending.BeforeSHA256 != "" {
		return zero, fmt.Errorf("pending permission apply absent pre-image identity is invalid")
	}
	if pending.IntendedSHA256 != "sha256:"+digest(pending.IntendedContent) || pending.Artifact.ContentSHA256 != pending.IntendedSHA256 {
		return zero, fmt.Errorf("pending permission apply intended content digest is invalid")
	}
	if pending.Artifact.Harness != pending.Harness || pending.Artifact.Path != pending.TargetPath ||
		pending.SourceAuthorityRef != pending.Artifact.SourceAuthorityRef || pending.SourceRevision != pending.Artifact.SourceRevision {
		return zero, fmt.Errorf("pending permission apply artifact identity is invalid")
	}
	render := PermissionRender{
		TargetPath: pending.TargetPath,
		Changed:    pending.TargetChanged,
		Content:    append([]byte(nil), pending.IntendedContent...),
		Preimage:   append([]byte(nil), pending.BeforeContent...),
		PreExisted: pending.BeforeExisted,
		Artifact:   pending.Artifact,
		Details:    append([]string(nil), pending.Details...),
	}
	if err := validatePermissionApplyApproval(render, pending.Action, pending.Decision); err != nil {
		return zero, fmt.Errorf("pending permission apply governed identity is invalid: %w", err)
	}
	authorizedAt, err := time.Parse(time.RFC3339Nano, pending.AuthorizedAt)
	if err != nil {
		return zero, fmt.Errorf("pending permission apply authorized_at is invalid: %w", err)
	}
	if expiresAt := strings.TrimSpace(pending.Decision.ExpiresAt); expiresAt != "" {
		expires, err := time.Parse(time.RFC3339, expiresAt)
		if err != nil || !authorizedAt.Before(expires) {
			return zero, fmt.Errorf("pending permission apply decision was not current at authorization")
		}
	}
	wantFiles := []string{}
	if pending.TargetChanged {
		wantFiles = append(wantFiles, pending.TargetPath)
	}
	if pending.ManifestChanged {
		wantFiles = append(wantFiles, HarnessArtifactManifestPath(homePath))
	}
	if pending.Changed != (pending.TargetChanged || pending.ManifestChanged) || !reflect.DeepEqual(pending.FilesChanged, wantFiles) {
		return zero, fmt.Errorf("pending permission apply outcome delta is invalid")
	}
	return PermissionApplyOutcome{
		Render:          render,
		Action:          pending.Action,
		Decision:        pending.Decision,
		AuthorizedAt:    pending.AuthorizedAt,
		TargetChanged:   pending.TargetChanged,
		ManifestChanged: pending.ManifestChanged,
		Changed:         pending.Changed,
		FilesChanged:    append([]string(nil), pending.FilesChanged...),
	}, nil
}

func validatePermissionRecoveryNames(pending permissionApplyPending) error {
	for _, item := range []struct {
		label string
		name  string
	}{
		{label: "recovery", name: pending.RecoveryName},
		{label: "rollback", name: pending.RollbackName},
	} {
		if item.name != "" && !validPermissionLeaf(item.name) {
			return fmt.Errorf("pending permission apply %s name must be a local leaf", item.label)
		}
		if item.name != "" && item.name == filepath.Base(pending.TargetPath) {
			return fmt.Errorf("pending permission apply %s name aliases the canonical target", item.label)
		}
	}
	if pending.RecoveryName != "" && pending.RecoveryName == pending.RollbackName {
		return fmt.Errorf("pending permission apply recovery and rollback names alias each other")
	}
	return nil
}

func permissionManifestMatchesPending(homePath string, pending permissionApplyPending) (bool, error) {
	latest, ok, err := latestHarnessArtifact(homePath, pending.Harness, pending.TargetPath)
	if err != nil {
		return false, err
	}
	return ok && sameHarnessArtifact(latest, pending.Artifact), nil
}

func validatePublishedPermissionOutcome(homePath string, pending permissionApplyPending) error {
	current, _, err := readPermissionTargetSnapshot(pending.TargetPath)
	if err != nil {
		return fmt.Errorf("validate published permission target: %w", err)
	}
	if !bytes.Equal(current, pending.IntendedContent) {
		return fmt.Errorf("published permission target changed before outcome finalization")
	}
	manifestMatches, err := permissionManifestMatchesPending(homePath, pending)
	if err != nil {
		return err
	}
	if !manifestMatches {
		return fmt.Errorf("published permission manifest changed before outcome finalization")
	}
	return nil
}

func readPermissionTargetSnapshot(path string) ([]byte, os.FileInfo, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	return readPermissionRootedFile(root, filepath.Base(path))
}

func cleanupPublishedPermissionRecovery(pending permissionApplyPending) error {
	if err := validatePermissionRecoveryNames(pending); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(pending.TargetPath))
	if err != nil {
		return err
	}
	defer root.Close()
	canonical, canonicalInfo, err := readPermissionRootedFile(root, filepath.Base(pending.TargetPath))
	if err != nil || !bytes.Equal(canonical, pending.IntendedContent) {
		return errors.Join(fmt.Errorf("published permission target identity changed before recovery cleanup"), err)
	}
	recovery, recoveryInfo, err := readPermissionRootedFile(root, pending.RecoveryName)
	if os.IsNotExist(err) {
		return nil
	}
	expectedRecovery := pending.BeforeContent
	if !pending.BeforeExisted {
		expectedRecovery = pending.IntendedContent
	}
	if err != nil || !bytes.Equal(recovery, expectedRecovery) {
		return errors.Join(fmt.Errorf("published permission recovery identity or content changed"), err)
	}
	if !pending.BeforeExisted && !os.SameFile(canonicalInfo, recoveryInfo) {
		return fmt.Errorf("published permission creation recovery no longer matches canonical identity")
	}
	return quarantineAndRemovePermissionLeaf(root, filepath.Dir(pending.TargetPath), pending.RecoveryName, recoveryInfo, expectedRecovery, func() {
		permissionApplyRecoveryAfterQuarantine(pending.TargetPath, pending.RecoveryName)
	})
}

func cleanupUnusedPermissionRecovery(pending permissionApplyPending) error {
	if err := validatePermissionRecoveryNames(pending); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(pending.TargetPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer root.Close()
	recovery, recoveryInfo, err := readPermissionRootedFile(root, pending.RecoveryName)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !bytes.Equal(recovery, pending.IntendedContent) {
		return errors.Join(fmt.Errorf("unused permission recovery identity or content changed"), err)
	}
	return quarantineAndRemovePermissionLeaf(root, filepath.Dir(pending.TargetPath), pending.RecoveryName, recoveryInfo, pending.IntendedContent, func() {
		permissionApplyRecoveryAfterQuarantine(pending.TargetPath, pending.RecoveryName)
	})
}

func cleanupInterruptedPermissionCreation(pending permissionApplyPending, canonicalPresent bool) error {
	if err := validatePermissionRecoveryNames(pending); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(pending.TargetPath))
	if err != nil {
		if os.IsNotExist(err) && !canonicalPresent {
			return nil
		}
		return err
	}
	defer root.Close()
	recovery, recoveryInfo, err := readPermissionRootedFile(root, pending.RecoveryName)
	rollback, rollbackInfo, rollbackErr := readPermissionRootedFile(root, pending.RollbackName)
	if os.IsNotExist(err) && os.IsNotExist(rollbackErr) && !canonicalPresent {
		return nil
	}
	if err != nil || !bytes.Equal(recovery, pending.IntendedContent) {
		return errors.Join(fmt.Errorf("interrupted creation recovery identity or content changed"), err)
	}
	if rollbackErr == nil {
		if !bytes.Equal(rollback, pending.IntendedContent) || !os.SameFile(rollbackInfo, recoveryInfo) {
			return fmt.Errorf("interrupted creation rollback evidence no longer matches recovery identity")
		}
		if err := quarantineAndRemovePermissionLeaf(root, filepath.Dir(pending.TargetPath), pending.RollbackName, rollbackInfo, pending.IntendedContent, nil); err != nil {
			return err
		}
		return quarantineAndRemovePermissionLeaf(root, filepath.Dir(pending.TargetPath), pending.RecoveryName, recoveryInfo, pending.IntendedContent, func() {
			permissionApplyRecoveryAfterQuarantine(pending.TargetPath, pending.RecoveryName)
		})
	}
	if !os.IsNotExist(rollbackErr) {
		return rollbackErr
	}
	if !canonicalPresent {
		return quarantineAndRemovePermissionLeaf(root, filepath.Dir(pending.TargetPath), pending.RecoveryName, recoveryInfo, pending.IntendedContent, func() {
			permissionApplyRecoveryAfterQuarantine(pending.TargetPath, pending.RecoveryName)
		})
	}
	canonicalName := filepath.Base(pending.TargetPath)
	canonical, canonicalInfo, err := readPermissionRootedFile(root, canonicalName)
	if err != nil || !bytes.Equal(canonical, pending.IntendedContent) || !os.SameFile(canonicalInfo, recoveryInfo) {
		return errors.Join(fmt.Errorf("interrupted creation canonical and recovery identities no longer match"), err)
	}
	if err := root.Rename(canonicalName, pending.RollbackName); err != nil {
		return fmt.Errorf("quarantine interrupted permission creation: %w", err)
	}
	if err := syncPermissionParentDirectory(pending.TargetPath); err != nil {
		return fmt.Errorf("sync interrupted permission creation quarantine: %w", err)
	}
	permissionApplyAfterCanonicalQuarantine(pending.TargetPath)
	rollback, rollbackInfo, err = readPermissionRootedFile(root, pending.RollbackName)
	if err != nil || !bytes.Equal(rollback, pending.IntendedContent) || !os.SameFile(rollbackInfo, canonicalInfo) {
		return errors.Join(fmt.Errorf("quarantined permission creation changed before cleanup"), err)
	}
	recovery, recoveryInfo, err = readPermissionRootedFile(root, pending.RecoveryName)
	if err != nil || !bytes.Equal(recovery, pending.IntendedContent) || !os.SameFile(recoveryInfo, canonicalInfo) {
		return errors.Join(fmt.Errorf("permission creation recovery changed after canonical quarantine"), err)
	}
	if err := quarantineAndRemovePermissionLeaf(root, filepath.Dir(pending.TargetPath), pending.RollbackName, rollbackInfo, pending.IntendedContent, nil); err != nil {
		return err
	}
	return quarantineAndRemovePermissionLeaf(root, filepath.Dir(pending.TargetPath), pending.RecoveryName, recoveryInfo, pending.IntendedContent, func() {
		permissionApplyRecoveryAfterQuarantine(pending.TargetPath, pending.RecoveryName)
	})
}

func quarantineAndRemovePermissionLeaf(root *os.Root, parent, name string, expectedInfo os.FileInfo, expectedContent []byte, afterQuarantine func()) error {
	quarantineName := fmt.Sprintf(".%s.%d.%d.permission-cleanup", name, os.Getpid(), time.Now().UnixNano())
	if !validPermissionLeaf(quarantineName) {
		return fmt.Errorf("permission cleanup quarantine name is invalid")
	}
	if err := root.Rename(name, quarantineName); err != nil {
		return fmt.Errorf("quarantine permission recovery leaf: %w", err)
	}
	if err := syncPermissionParentDirectory(filepath.Join(parent, name)); err != nil {
		return fmt.Errorf("sync permission recovery quarantine: %w", err)
	}
	if afterQuarantine != nil {
		afterQuarantine()
	}
	quarantined, quarantinedInfo, err := readPermissionRootedFile(root, quarantineName)
	if err != nil || !os.SameFile(expectedInfo, quarantinedInfo) || !bytes.Equal(quarantined, expectedContent) {
		return errors.Join(fmt.Errorf("quarantined permission recovery identity or content changed; evidence preserved"), err)
	}
	if err := root.Remove(quarantineName); err != nil {
		return fmt.Errorf("remove validated permission recovery quarantine: %w", err)
	}
	if err := syncPermissionParentDirectory(filepath.Join(parent, name)); err != nil {
		return fmt.Errorf("sync validated permission recovery removal: %w", err)
	}
	return nil
}

func validatePermissionApplyApproval(approved PermissionRender, action envelope.ActionIntent, decision envelope.PolicyDecision) error {
	if err := action.Validate(); err != nil {
		return fmt.Errorf("permission apply action intent invalid: %w", err)
	}
	if err := decision.Validate(); err != nil {
		return fmt.Errorf("permission apply policy decision invalid: %w", err)
	}
	if strings.TrimSpace(approved.Artifact.Harness) == "" || approved.TargetPath == "" || approved.TargetPath != approved.Artifact.Path {
		return fmt.Errorf("permission apply approved render target identity is invalid")
	}
	if approved.Artifact.ContentSHA256 != "sha256:"+digest(approved.Content) {
		return fmt.Errorf("permission apply approved render content digest is invalid")
	}
	if action.Capability != "harness.permissions.apply" {
		return fmt.Errorf("permission apply capability must be harness.permissions.apply")
	}
	if action.Action != "reconcile_harness_permissions" {
		return fmt.Errorf("permission apply action must be reconcile_harness_permissions")
	}
	if action.Target.Kind != "harness_settings" || action.Target.ID != approved.Artifact.Harness || action.Target.Path != approved.TargetPath {
		return fmt.Errorf("permission apply action target does not bind approved render")
	}
	if action.MutationClass != "security" || action.Reversibility != "easy" || action.BlastRadius != "local" {
		return fmt.Errorf("permission apply requires security mutation_class with easy reversibility and local blast radius")
	}
	if !policy.HasOperatorProvenance(action.Actor) {
		return fmt.Errorf("permission apply requires explicit operator provenance")
	}
	if decision.ActionIntentID != action.ID || decision.IDs.WorkID != action.IDs.WorkID {
		return fmt.Errorf("permission apply policy decision does not bind action intent")
	}
	if !policy.Allowed(decision) || strings.TrimSpace(decision.StandingGrantID) == "" {
		return fmt.Errorf("permission apply requires an allowed standing-grant policy decision")
	}
	return nil
}

func samePermissionRender(a, b PermissionRender) bool {
	return a.TargetPath == b.TargetPath && a.Changed == b.Changed && a.PreExisted == b.PreExisted &&
		bytes.Equal(a.Content, b.Content) && bytes.Equal(a.Preimage, b.Preimage) &&
		reflect.DeepEqual(a.Artifact, b.Artifact) && reflect.DeepEqual(a.Details, b.Details)
}

type permissionRootedCAS interface {
	State() homepkg.RootedFileCASState
	Commit() error
	Rollback() error
}

var beginPermissionRootedFileCAS = func(
	root *os.Root,
	canonicalName string,
	recoveryName string,
	expectedData []byte,
	expectedInfo os.FileInfo,
	replacementData []byte,
	replacementInfo os.FileInfo,
) (permissionRootedCAS, error) {
	return homepkg.BeginRootedFileCAS(root, canonicalName, recoveryName, expectedData, expectedInfo, replacementData, replacementInfo)
}

type permissionTargetPublication struct {
	root        *os.Root
	publication permissionRootedCAS
}

func (p *permissionTargetPublication) Commit() error   { return p.publication.Commit() }
func (p *permissionTargetPublication) Rollback() error { return p.publication.Rollback() }
func (p *permissionTargetPublication) Close() error    { return p.root.Close() }
func (p *permissionTargetPublication) State() homepkg.RootedFileCASState {
	return p.publication.State()
}

func beginPermissionTargetPublication(render PermissionRender, recoveryName string) (*permissionTargetPublication, error) {
	parent := filepath.Dir(render.TargetPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(render.TargetPath)
	current, currentInfo, err := readPermissionRootedFile(root, name)
	if os.IsNotExist(err) && !render.PreExisted {
		current, currentInfo, err = nil, nil, nil
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if (currentInfo != nil) != render.PreExisted || !bytes.Equal(current, render.Preimage) {
		_ = root.Close()
		return nil, fmt.Errorf("settings target %s preimage changed before permission publication", render.TargetPath)
	}
	replacementInfo, err := writePermissionRootedReplacement(root, recoveryName, render.Content, currentInfo)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	publication, err := beginPermissionRootedFileCAS(root, name, recoveryName, current, currentInfo, render.Content, replacementInfo)
	if err != nil {
		if publication == nil || publication.State() == homepkg.RootedFileCASNotPublished {
			_ = root.Remove(recoveryName)
			_ = root.Close()
			return nil, err
		}
		return &permissionTargetPublication{root: root, publication: publication}, err
	}
	return &permissionTargetPublication{root: root, publication: publication}, nil
}

func beginPermissionRecoveryPublication(pending permissionApplyPending, current []byte) (*permissionTargetPublication, error) {
	root, err := os.OpenRoot(filepath.Dir(pending.TargetPath))
	if err != nil {
		return nil, err
	}
	name := filepath.Base(pending.TargetPath)
	actual, currentInfo, err := readPermissionRootedFile(root, name)
	if err != nil || !bytes.Equal(actual, current) {
		_ = root.Close()
		return nil, errors.Join(fmt.Errorf("pending target changed during recovery"), err)
	}
	recoveryData, recoveryInfo, err := readPermissionRootedFile(root, pending.RecoveryName)
	if err != nil || !bytes.Equal(recoveryData, pending.BeforeContent) {
		_ = root.Close()
		return nil, errors.Join(fmt.Errorf("pending recovery pre-image changed"), err)
	}
	publication, err := homepkg.BeginRootedFileCAS(root, name, pending.RecoveryName, actual, currentInfo, recoveryData, recoveryInfo)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return &permissionTargetPublication{root: root, publication: publication}, nil
}

func writePermissionRootedReplacement(root *os.Root, name string, body []byte, currentInfo os.FileInfo) (os.FileInfo, error) {
	mode := os.FileMode(0o644)
	if currentInfo != nil {
		mode = currentInfo.Mode().Perm()
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return nil, err
	}
	cleanup := func(cause error) (os.FileInfo, error) {
		_ = file.Close()
		_ = root.Remove(name)
		return nil, cause
	}
	if n, err := file.Write(body); err != nil {
		return cleanup(err)
	} else if n != len(body) {
		return cleanup(io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return cleanup(err)
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(name)
		return nil, err
	}
	data, info, err := readPermissionRootedFile(root, name)
	if err != nil || !bytes.Equal(data, body) {
		return cleanup(errors.Join(fmt.Errorf("permission replacement changed before publication"), err))
	}
	return info, nil
}

func readPermissionRootedFile(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("permission target must be a regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	after, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) || int64(len(data)) != after.Size() {
		return nil, nil, fmt.Errorf("permission target changed while read")
	}
	return data, after, nil
}

func permissionRecoveryName(render PermissionRender) string {
	return fmt.Sprintf(".%s.%d.%s.permission-swap", filepath.Base(render.TargetPath), os.Getpid(), strings.TrimPrefix(render.Artifact.ContentSHA256, "sha256:")[:12])
}

func permissionCreationRollbackName(render PermissionRender) string {
	return fmt.Sprintf(".%s.%d.%s.permission-rollback", filepath.Base(render.TargetPath), os.Getpid(), strings.TrimPrefix(render.Artifact.ContentSHA256, "sha256:")[:12])
}

func rollbackPermissionPublication(publication *permissionTargetPublication, outcome PermissionApplyOutcome, abort PermissionApplyAbort, pendingPath string, cause error) error {
	if err := publication.Rollback(); err != nil {
		return fmt.Errorf("%v; manual/concurrent state preserved and pending recovery retained: %w", cause, err)
	}
	if err := abort(outcome); err != nil {
		return fmt.Errorf("%v; settings pre-image restored; reservation retained for recovery: %w", cause, err)
	}
	if err := removePermissionPending(pendingPath); err != nil {
		return fmt.Errorf("%v; settings pre-image restored; %w", cause, err)
	}
	return fmt.Errorf("%w; settings pre-image restored", cause)
}

func abortPermissionApplyBeforePublication(outcome PermissionApplyOutcome, abort PermissionApplyAbort, pendingPath string, cause error) error {
	if err := abort(outcome); err != nil {
		return fmt.Errorf("%v; permission reservation retained for recovery: %w", cause, err)
	}
	if err := removePermissionPending(pendingPath); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func removePermissionPending(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove permission apply pending transaction: %w", err)
	}
	if err := permissionApplySyncBoundary("pending_remove", path); err != nil {
		return fmt.Errorf("sync permission apply pending removal: %w", err)
	}
	return nil
}

func syncPermissionParentDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) {
		return syncErr
	}
	return closeErr
}

func sameHarnessArtifact(a, b HarnessArtifact) bool {
	return a.Path == b.Path &&
		a.Harness == b.Harness &&
		a.SourceAuthorityRef == b.SourceAuthorityRef &&
		a.SourceRevision == b.SourceRevision &&
		a.ContentSHA256 == b.ContentSHA256 &&
		reflect.DeepEqual(a.ManagedEntries, b.ManagedEntries)
}

func permissionNativeDrift(homePath, harness string) (PermissionDriftReport, error) {
	policy, _, err := LoadPermissionAuthority(homePath, harness)
	if err != nil {
		return PermissionDriftReport{}, err
	}
	render, err := RenderPermissions(homePath, harness)
	if err != nil {
		return PermissionDriftReport{}, err
	}
	report := PermissionDriftReport{Harness: harness, TargetPath: render.TargetPath}
	_, pending, err := ProbePermissionApplyRecovery(homePath, harness)
	if err != nil {
		return report, err
	}
	if pending {
		report.Details = append(report.Details, "incomplete permission apply transaction requires recovery")
	}
	if tomlSettingsMode(policy) {
		// RenderPermissions parsed and retained this exact preimage; the TOML
		// drift checker below validates owned keys from these same bytes.
		report.TargetRevision = "sha256:" + digest(render.Preimage)
		return tomlPermissionDrift(policy, render, report)
	}
	current, err := os.ReadFile(render.TargetPath)
	if err != nil {
		return report, err
	}
	// TargetRevision identifies the exact bytes validated below. Consumers
	// must attest this revision rather than re-reading the mutable target and
	// accidentally attributing unvalidated replacement bytes.
	report.TargetRevision = "sha256:" + digest(current)
	var settings map[string]any
	if err := json.Unmarshal(current, &settings); err != nil {
		return report, err
	}
	for _, rule := range stringSlice(objectAt(settings, "permissions")["allow"]) {
		if rule == "mcp__*" {
			report.Details = append(report.Details, "unsupported permissions.allow rule mcp__*")
		}
	}
	// Named before the whole-content comparison: a drifted owned key must say
	// which key and what value, not only that the file differs somewhere.
	report.Details = append(report.Details, jsonSettingsDrift(settings, policy.Settings)...)
	if !jsonEquivalent(current, render.Content) {
		report.Details = append(report.Details, managedRuleDifferences(current, render.Content)...)
	}
	report.Details = uniqueSorted(report.Details)
	report.Drifted = len(report.Details) > 0
	return report, nil
}

func WriteHarnessArtifactManifest(homePath string, artifacts []HarnessArtifact) error {
	existing, err := loadHarnessArtifacts(homePath)
	if err != nil {
		return err
	}
	byKey := map[string]HarnessArtifact{}
	for _, artifact := range existing {
		byKey[artifact.Harness+"\x00"+artifact.Path] = artifact
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, artifact := range artifacts {
		artifact.SchemaVersion = HarnessArtifactSchemaVersion
		if artifact.GeneratedAt == "" {
			artifact.GeneratedAt = now
		}
		byKey[artifact.Harness+"\x00"+artifact.Path] = artifact
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, key := range keys {
		if err := encoder.Encode(byKey[key]); err != nil {
			return err
		}
	}
	path := HarnessArtifactManifestPath(homePath)
	return writePermissionFileAtomic(path, body.Bytes())
}

func writePermissionFileAtomic(path string, body []byte) error {
	return writePermissionFileAtomicMode(path, body, 0o644)
}

func writePermissionPrivateFileAtomic(path string, body []byte) error {
	return writePermissionFileAtomicMode(path, body, 0o600)
}

func writePermissionFileAtomicMode(path string, body []byte, defaultMode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := defaultMode
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func loadHarnessArtifacts(homePath string) ([]HarnessArtifact, error) {
	path := HarnessArtifactManifestPath(homePath)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var out []HarnessArtifact
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var artifact HarnessArtifact
		if err := json.Unmarshal(scanner.Bytes(), &artifact); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		out = append(out, artifact)
	}
	return out, scanner.Err()
}

func latestHarnessArtifact(homePath, harness, targetPath string) (HarnessArtifact, bool, error) {
	artifacts, err := loadHarnessArtifacts(homePath)
	if err != nil {
		return HarnessArtifact{}, false, err
	}
	for i := len(artifacts) - 1; i >= 0; i-- {
		if artifacts[i].Harness == harness && artifacts[i].Path == targetPath {
			return artifacts[i], true, nil
		}
	}
	return HarnessArtifact{ManagedEntries: map[string][]string{}}, false, nil
}

func reconcileRules(current, previousManaged, desired []string, defaultsFirst bool) []string {
	managed := map[string]bool{}
	for _, rule := range previousManaged {
		managed[rule] = true
	}
	// The artifact published with this render claims every desired rule as
	// managed. Adopt already-present exact matches into the same canonical
	// block now, rather than preserving their old positions for one render and
	// reporting order drift only after the manifest starts claiming them.
	for _, rule := range desired {
		managed[rule] = true
	}
	out, seen := []string{}, map[string]bool{}
	for _, rule := range current {
		if managed[rule] || seen[rule] {
			continue
		}
		seen[rule], out = true, append(out, rule)
	}
	if defaultsFirst {
		filtered := out[:0]
		for _, rule := range out {
			if rule != "$defaults" {
				filtered = append(filtered, rule)
			}
		}
		out = filtered
		if len(desired) > 0 && desired[0] == "$defaults" {
			out = append([]string{"$defaults"}, out...)
			seen["$defaults"] = true
			desired = desired[1:]
		}
	}
	for _, rule := range desired {
		if !seen[rule] {
			seen[rule], out = true, append(out, rule)
		}
	}
	return out
}

func canonicalizeUnmanagedClaudeFileEditRules(current, previousManaged []string) []string {
	managed := map[string]bool{}
	for _, rule := range previousManaged {
		managed[rule] = true
	}
	out, seen := make([]string, 0, len(current)), map[string]bool{}
	for _, rule := range current {
		canonical := rule
		if !managed[rule] {
			if replacement, legacy := canonicalClaudeFileEditRule(rule); legacy {
				canonical = replacement
			}
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out
}

func canonicalClaudeFileEditRule(rule string) (string, bool) {
	rule = strings.TrimSpace(rule)
	for _, prefix := range []string{"Write(", "MultiEdit("} {
		if strings.HasPrefix(rule, prefix) && strings.HasSuffix(rule, ")") {
			return "Edit(" + strings.TrimPrefix(rule, prefix), true
		}
	}
	return rule, false
}

func objectAt(root map[string]any, key string) map[string]any {
	if value, ok := root[key].(map[string]any); ok {
		return value
	}
	return map[string]any{}
}

func stringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if rule, ok := value.(string); ok {
				out = append(out, rule)
			}
		}
		return out
	default:
		return nil
	}
}

func sortedRules(rules []string) []string {
	out := make([]string, len(rules))
	for i := range rules {
		out[i] = strings.TrimSpace(rules[i])
	}
	sort.Strings(out)
	return out
}

func jsonEquivalent(a, b []byte) bool {
	if len(bytes.TrimSpace(a)) == 0 {
		a = []byte("{}")
	}
	var av, bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && reflect.DeepEqual(av, bv)
}

func managedRuleDifferences(current, intended []byte) []string {
	var got, want map[string]any
	if json.Unmarshal(current, &got) != nil || json.Unmarshal(intended, &want) != nil {
		return []string{"settings JSON differs"}
	}
	var out []string
	for _, path := range []string{"permissions.allow", "permissions.deny", "permissions.ask", "autoMode.allow"} {
		parts := strings.Split(path, ".")
		gotRules := stringSlice(objectAt(got, parts[0])[parts[1]])
		wantRules := stringSlice(objectAt(want, parts[0])[parts[1]])
		gotSet, wantSet := map[string]bool{}, map[string]bool{}
		for _, rule := range gotRules {
			gotSet[rule] = true
		}
		for _, rule := range wantRules {
			wantSet[rule] = true
		}
		for _, rule := range wantRules {
			if !gotSet[rule] {
				out = append(out, fmt.Sprintf("%s missing %q", path, rule))
			}
		}
		for _, rule := range gotRules {
			if !wantSet[rule] {
				out = append(out, fmt.Sprintf("%s extra %q", path, rule))
			}
		}
		if reflect.DeepEqual(gotSet, wantSet) && !reflect.DeepEqual(gotRules, wantRules) {
			if path == "autoMode.allow" {
				out = append(out, "autoMode.allow order differs; $defaults must be first")
			} else {
				out = append(out, path+" managed rule order differs")
			}
		}
	}
	gotMode, wantMode := objectAt(got, "permissions")["defaultMode"], objectAt(want, "permissions")["defaultMode"]
	if gotMode != wantMode {
		out = append(out, fmt.Sprintf("permissions.defaultMode is %q, want %q", gotMode, wantMode))
	}
	return out
}

func permissionDelta(previous HarnessArtifact, policy PermissionAuthority) []string {
	var out []string
	desired := map[string][]string{"permissions.allow": policy.Permissions.Allow, "permissions.deny": policy.Permissions.Deny, "permissions.ask": policy.Permissions.Ask, "autoMode.allow": policy.AutoMode.Allow}
	for path, oldRules := range previous.ManagedEntries {
		keep := map[string]bool{}
		for _, rule := range desired[path] {
			keep[rule] = true
		}
		for _, rule := range oldRules {
			if !keep[rule] {
				out = append(out, fmt.Sprintf("%s remove managed %q", path, rule))
			}
		}
	}
	return uniqueSorted(out)
}

func uniqueSorted(values []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, value := range values {
		if !seen[value] {
			seen[value], out = true, append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
