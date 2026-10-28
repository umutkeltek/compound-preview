package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"compound/internal/authority"
	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/policy"
	"compound/internal/work"
)

// guardActualPermissionTarget is independent of Git and proposal declarations.
// This is a native raw-file boundary, not containment of arbitrary same-account
// processes. Dedicated typed permission apply retains its own authority checks.
func guardActualPermissionTarget(fx Fixture, deps Deps, ai envelope.ActionIntent) (envelope.ActionIntent, error) {
	if ai.MutationClass == "read" || ai.Target.Kind != "file" ||
		strings.TrimSpace(deps.HomePath) == "" || !filepath.IsAbs(ai.Target.Path) {
		return ai, nil
	}
	target, err := authority.CanonicalSourceControlMutationTarget(fx.Cwd, ai.Target.Path)
	if err != nil {
		return ai, fmt.Errorf("resolve actual mutation target: %w", err)
	}
	permissionDir := filepath.Dir(harness.PermissionAuthorityPath(deps.HomePath, "codex"))
	permissionDir, err = authority.CanonicalSourceControlMutationTarget(deps.HomePath, permissionDir)
	if err != nil {
		return ai, fmt.Errorf("resolve permission authority root: %w", err)
	}
	rel, err := filepath.Rel(permissionDir, target)
	if err != nil {
		return ai, fmt.Errorf("compare permission authority target: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		// A leaf symlink may resolve outward, but the logical authority entry
		// is still protected. Its owner rejects the escaped body independently.
		parent, err := authority.CanonicalSourceControlMutationTarget(fx.Cwd, filepath.Dir(ai.Target.Path))
		if err != nil {
			return ai, fmt.Errorf("resolve logical mutation parent: %w", err)
		}
		logical := filepath.Join(parent, filepath.Base(ai.Target.Path))
		rel, err = filepath.Rel(permissionDir, logical)
		if err != nil {
			return ai, fmt.Errorf("compare logical permission authority target: %w", err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return ai, nil
		}
		target = logical
	}
	ai.Target.Path = target
	denyBinding := func(reason string) (envelope.ActionIntent, error) {
		guarded := policy.GuardActualEditableSurface(ai, "permissions", false, false)
		guarded.SecurityReasons = append(guarded.SecurityReasons, "protected-target work binding: "+reason)
		return guarded, nil
	}
	workID := strings.TrimSpace(ai.IDs.WorkID)
	if workID == "" {
		return denyBinding("absent")
	}
	store, err := work.OpenReadOnly(deps.HomePath)
	if err != nil {
		return denyBinding("unreadable store")
	}
	item, err := store.Get(workID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return denyBinding("missing record")
		}
		return denyBinding("unreadable record")
	}
	if item.ID != workID {
		return denyBinding("identity mismatch")
	}
	if err := item.Validate(); err != nil {
		return denyBinding("invalid envelope")
	}
	return policy.GuardActualEditableSurface(ai, "permissions", work.IsDirectorProposed(item), true), nil
}
