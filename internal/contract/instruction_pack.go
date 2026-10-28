package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
)

// runtimeContextKey separates requests while allowing changed authority for the
// same request to advance its projection. Include cwd, topic and full task
// contract: the historical request ID alone does not distinguish workspaces.
func runtimeContextKey(in Input) string {
	in = canonicalRuntimeInput(in)
	// Automatic classification is source authority, not request identity. A
	// changed marker/registry must advance this context rather than orphan it.
	requestedRepoKind := ""
	if in.RepoKindVia == "flag" {
		requestedRepoKind = in.RepoKind
	}
	identity := struct {
		Harness, Cwd, RequestedRepoKind, Topic string
		Task                                   envelope.TaskContract
	}{in.Harness, filepath.Clean(in.Cwd), requestedRepoKind,
		strings.TrimSpace(in.Topic), resolvedTaskContract(in)}
	body, _ := json.Marshal(identity)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Canonicalize at the shared materializer, not only CLI parsing: live hook and
// MCP requests also enter here. Pure compiler fixtures may use absent synthetic
// paths; retain those lexically while resolving every existing workspace.
func canonicalRuntimeInput(in Input) Input {
	if registered, ok := harness.DefaultRegistry().Get(strings.TrimSpace(in.Harness)); ok {
		in.Harness = registered.Name
	}
	if strings.TrimSpace(in.Cwd) != "" {
		if absolute, err := filepath.Abs(in.Cwd); err == nil {
			in.Cwd = filepath.Clean(absolute)
			if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
				in.Cwd = filepath.Clean(resolved)
			}
		}
	}
	return in
}

// ScopedEffectiveContractPath belongs to the materialization owner. The harness
// reader accepts only this exact home-relative shape; the integration test joins
// the two production seams rather than trusting parallel path fixtures.
func ScopedEffectiveContractPath(homePath, key string) (string, error) {
	raw, err := hex.DecodeString(key)
	if err != nil || len(raw) != sha256.Size || key != strings.ToLower(key) {
		return "", fmt.Errorf("invalid effective contract context key")
	}
	return filepath.Join(homePath, "contracts", "effective", "scopes", key, "latest.json"), nil
}

// RenderInstructionPackForContext explicitly selects an installed consumer's
// context from an immutable, validated pair. Ordinary rendering retains that
// selection; an unrelated orient never rebinds it.
func RenderInstructionPackForContext(homePath, harnessName, preset, contextPackID string, now time.Time) (harness.InstructionPack, error) {
	pair, err := ReadContextPackContract(homePath, contextPackID)
	if err != nil {
		return harness.InstructionPack{}, err
	}
	registered, ok := harness.DefaultRegistry().Get(harnessName)
	if !ok {
		return harness.InstructionPack{}, fmt.Errorf("unknown instruction-pack harness %q", harnessName)
	}
	if pair.EffectiveContract.Harness != registered.Name {
		return harness.InstructionPack{}, fmt.Errorf("instruction-pack context harness mismatch")
	}
	path, err := ScopedEffectiveContractPath(homePath, pair.EffectiveContract.ContextKey)
	if err != nil {
		return harness.InstructionPack{}, err
	}
	ref, err := filepath.Rel(homePath, path)
	if err != nil {
		return harness.InstructionPack{}, err
	}
	return harness.RenderInstructionPackWithContractRef(homePath, registered.Name, preset, filepath.ToSlash(ref), now)
}
