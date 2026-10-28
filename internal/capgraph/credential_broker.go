package capgraph

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	homepkg "compound/internal/home"
)

// CredentialUsageSchemaVersion tags the credential-usage audit records appended
// by `compound credential exec`. Each record names WHO used WHICH surface WHEN —
// surface id, work id, env name, kind — and NEVER the credential value, so
// "which lane used which credential when" stays answerable without a secret
// ever entering a transcript or a repo. The record is evidence (named consumer:
// operator audit of credential use), not authority; the capability graph and
// the custodian's store retain custody.
const CredentialUsageSchemaVersion = "compound.credential_usage.v0.1"

var ErrCredentialSurfaceNotRegistered = errors.New("credential surface not registered")

// ResolveCredentialForSurface returns one child-environment assignment from a
// registered, Compound-custodied secret. It never returns the value in an
// error or audit record. Browser tasks use this instead of caller shell state.
func ResolveCredentialForSurface(homePath, surfaceID, workID string, expected CredentialHandle) (string, error) {
	info, err := os.Lstat(filepath.Join(homePath, "registries", "capability-connection-surfaces.jsonl"))
	if os.IsNotExist(err) {
		return "", fmt.Errorf("%w: %s", ErrCredentialSurfaceNotRegistered, surfaceID)
	} else if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("credential surface registry is not a regular file")
	}
	store, err := OpenReadOnly(homePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrCredentialSurfaceNotRegistered, surfaceID)
		}
		return "", err
	}
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		return "", err
	}
	for _, surface := range surfaces {
		if surface.NodeID != surfaceID {
			continue
		}
		if surface.CredentialHandle != expected {
			return "", fmt.Errorf("credential surface %q has an unexpected handle", surfaceID)
		}
		if !strings.HasPrefix(surface.CredentialHandle.Ref, "home-secret://") {
			return "", fmt.Errorf("credential surface %q needs a Compound home secret", surfaceID)
		}
		env, broker, err := resolveCredentialEnv(homePath, surface.CredentialHandle)
		if err != nil {
			return "", err
		}
		if broker != "" || len(env) != 1 {
			return "", fmt.Errorf("credential surface %q did not resolve one environment value", surfaceID)
		}
		if err := recordCredentialUsage(homePath, surfaceID, workID, surface.CredentialHandle, "resolved"); err != nil {
			return "", err
		}
		return env[0], nil
	}
	return "", fmt.Errorf("credential surface %q absent from existing registry; refuse environment fallback", surfaceID)
}

// CredentialExecCommand implements `compound credential exec` — the one verb
// that turns a ConnectionSurface's CredentialHandle into a live child process.
//
//	compound credential exec --surface <node-id> [--work <work_id>] [--env K=V]... -- <cmd> [args...]
//
// It resolves the surface's credential handle, obtains the value from whichever
// custodian the handle's kind names, sets exactly the handle's CredentialEnvName
// (plus any --env overrides) in the child's environment, and execs. The value
// exists in the child process and nowhere else — not in a transcript, not in a
// file Compound reads back, not in a repo. Fail-closed: unresolvable surface,
// unknown kind, unreadable custody, empty value, or missing command → error, no
// exec. Every use is recorded (surface + work id + env name, never the value).
//
// The four kinds differ only in WHERE the value comes from; the consumer never
// knows which case it is in:
//
//   - home-secret: Compound custodies it (COMPOUND_HOME/secrets/<slug>/<file>).
//   - env-file:    an operator dotenv at <path>#<VAR> holds it.
//   - selector:    a non-secret selector variable (profile name); no value.
//   - brokered:    the custodian's own broker command injects it; Compound runs
//     the broker with the child argv appended and never reads the value.
func CredentialExecCommand(args []string, stdout, stderr io.Writer) int {
	if usageRequested(args) {
		fmt.Fprintln(stderr, "usage: compound credential exec --surface <node-id> [--work <work_id>] [--env K=V]... -- <cmd> [args...]")
		return 2
	}
	dash := indexOf(args, "--")
	// Only compound's own flags live before `--`; anything after it is the
	// child argv and must never be parsed as a credential flag (a child could
	// legitimately take --env / --surface itself).
	flagScope := args
	if dash >= 0 {
		flagScope = args[:dash]
	}
	surfaceID := flagValue(flagScope, "--surface")
	workID := flagValue(flagScope, "--work")
	var extraEnv []string
	for i := 0; i < len(flagScope); i++ {
		if flagScope[i] != "--env" {
			continue
		}
		if i+1 >= len(flagScope) {
			fmt.Fprintln(stderr, "error: credential exec: --env needs K=V")
			return 2
		}
		extraEnv = append(extraEnv, flagScope[i+1])
		i++
	}
	var child []string
	if dash >= 0 {
		child = args[dash+1:]
	}
	if strings.TrimSpace(surfaceID) == "" {
		fmt.Fprintln(stderr, "error: credential exec: --surface <node-id> is required")
		return 2
	}
	if len(child) == 0 || strings.TrimSpace(child[0]) == "" {
		fmt.Fprintln(stderr, "error: credential exec: a command after `--` is required")
		return 2
	}

	homePath, err := homepkg.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "error: credential exec:", err)
		return 1
	}
	store, err := Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error: credential exec:", err)
		return 1
	}
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		fmt.Fprintln(stderr, "error: credential exec:", err)
		return 1
	}
	var surface *ConnectionSurface
	for i := range surfaces {
		if surfaces[i].NodeID == surfaceID {
			surface = &surfaces[i]
			break
		}
	}
	if surface == nil {
		fmt.Fprintf(stderr, "error: credential exec: no connection surface for node %q\n", surfaceID)
		return 1
	}

	env, broker, err := resolveCredentialEnv(homePath, surface.CredentialHandle)
	if err != nil {
		fmt.Fprintln(stderr, "error: credential exec:", err)
		return 1
	}
	if err := recordCredentialUsage(homePath, surfaceID, workID, surface.CredentialHandle, "resolved"); err != nil {
		fmt.Fprintln(stderr, "error: credential exec: record usage:", err)
		return 1
	}

	var cmd *exec.Cmd
	if broker != "" {
		// The custodian's broker injects the value; Compound never reads it.
		// The broker receives the child argv so it can exec the child with the
		// credential already in the environment (e.g. `sb tenant exec -- <cmd>`).
		parts := strings.Fields(broker)
		if len(parts) == 0 {
			fmt.Fprintln(stderr, "error: credential exec: brokered handle has an empty broker command")
			return 1
		}
		cmd = exec.Command(parts[0], append(parts[1:], child...)...)
	} else {
		cmd = exec.Command(child[0], child[1:]...)
	}
	cmd.Env = append(os.Environ(), env...)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(stderr, "error: credential exec:", err)
		return 1
	}
	return 0
}

// resolveCredentialEnv returns the env pairs to set in the child (compound-side
// kinds) plus a broker argv when the handle delegates injection to a custodian
// command. Exactly one of the two is non-empty. The value is never logged.
func resolveCredentialEnv(homePath string, h CredentialHandle) (env []string, broker string, err error) {
	if strings.TrimSpace(h.Ref) == "" {
		return nil, "", nil // no credential; caller just execs with --env
	}
	kindName, locator, _ := strings.Cut(strings.TrimSpace(h.Ref), "://")
	envName := strings.TrimSpace(h.EnvName)
	switch kindName {
	case "home-secret":
		// locator is <slug>/<file> under COMPOUND_HOME/secrets.
		rel := filepath.FromSlash(locator)
		if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			return nil, "", fmt.Errorf("home-secret locator %q is not a relative secrets path", locator)
		}
		path := filepath.Join(homePath, "secrets", rel)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, "", fmt.Errorf("home-secret %s: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, "", fmt.Errorf("home-secret %s must be a regular mode-0600 file", path)
		}
		realHome, err := filepath.EvalSymlinks(homePath)
		if err != nil {
			return nil, "", err
		}
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, "", err
		}
		if !strings.HasPrefix(realPath, filepath.Join(realHome, "secrets")+string(os.PathSeparator)) {
			return nil, "", fmt.Errorf("home-secret %s leaves Compound custody", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("home-secret %s: %w", path, err)
		}
		value := strings.TrimRight(string(data), "\r\n")
		if value == "" {
			return nil, "", fmt.Errorf("home-secret %s is empty", path)
		}
		return []string{envName + "=" + value}, "", nil
	case "env-file":
		pathPart, varPart, _ := strings.Cut(locator, "#")
		if varPart == "" {
			return nil, "", fmt.Errorf("env-file locator %q must name the variable as <path>#<VAR>", locator)
		}
		value, err := readDotenvVar(pathPart, varPart)
		if err != nil {
			return nil, "", err
		}
		if value == "" {
			return nil, "", fmt.Errorf("env-file %s has empty %s", pathPart, varPart)
		}
		return []string{envName + "=" + value}, "", nil
	case "selector":
		// A selector is not a secret: the locator (e.g. a profile name) IS the value.
		return []string{envName + "=" + locator}, "", nil
	case "brokered":
		return nil, strings.TrimSpace(h.Broker), nil
	default:
		return nil, "", fmt.Errorf("unknown credential kind %q", kindName)
	}
}

// readDotenvVar reads one variable from an operator-managed dotenv. Minimal,
// deliberately: comments and blank lines skipped, KEY=VALUE split on the first
// '=', surrounding whitespace and one pair of quotes trimmed. A missing file or
// a missing variable is an error (fail closed — a handle pointing at nothing is
// not a design).
func readDotenvVar(path, key string) (string, error) {
	// Expand a leading ~/ to the operator home so locators read naturally.
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(h, strings.TrimPrefix(path, "~/"))
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("env-file %s: %w", path, err)
	}
	defer f.Close()
	want := key + "="
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, want) {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, want))
		val = strings.Trim(val, `"'`)
		return val, nil
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("env-file %s: %w", path, err)
	}
	return "", fmt.Errorf("env-file %s has no variable %s", path, key)
}

// recordCredentialUsage audits credential resolution, not provider contact or
// child launch. The phase field prevents a policy-held task from appearing as
// a completed credential use. It records only surface/work metadata, no value.
// Failure is hard: even secret resolution must leave an audit trail.
func recordCredentialUsage(homePath, surfaceID, workID string, h CredentialHandle, phase string) error {
	path := filepath.Join(homePath, "registries", "credential-usage.jsonl")
	rec := struct {
		Schema    string    `json:"schema_version"`
		SurfaceID string    `json:"surface_id"`
		WorkID    string    `json:"work_id,omitempty"`
		EnvName   string    `json:"env_name,omitempty"`
		Kind      string    `json:"kind,omitempty"`
		Phase     string    `json:"phase"`
		At        time.Time `json:"at"`
	}{
		Schema:    CredentialUsageSchemaVersion,
		SurfaceID: surfaceID,
		WorkID:    strings.TrimSpace(workID),
		EnvName:   strings.TrimSpace(h.EnvName),
		Phase:     phase,
		At:        time.Now().UTC(),
	}
	if kind, _, ok := strings.Cut(strings.TrimSpace(h.Ref), "://"); ok {
		rec.Kind = kind
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func usageRequested(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// CredentialBindCommand attaches a CredentialHandle to an existing connection
// surface in place — the operator enrollment door. Values never appear: the
// handle is an address into whichever store already custodies the credential.
//
//	compound credential bind --surface <node-id> --ref <kind>://<locator> --custodian <owner> --env <NAME> [--broker <cmd>]
//
// The handle is validated at the write door (pointer-not-value), so a pasted key
// is refused before it can reach the sidecar or orient.
func CredentialBindCommand(args []string, stdout, stderr io.Writer) int {
	if usageRequested(args) {
		fmt.Fprintln(stderr, "usage: compound credential bind --surface <node-id> --ref <kind>://<locator> --custodian <owner> --env <NAME> [--broker <cmd>]")
		return 2
	}
	nodeID := flagValue(args, "--surface")
	h := CredentialHandle{
		Ref:       flagValue(args, "--ref"),
		Custodian: flagValue(args, "--custodian"),
		EnvName:   flagValue(args, "--env"),
		Broker:    flagValue(args, "--broker"),
	}
	if strings.TrimSpace(nodeID) == "" {
		fmt.Fprintln(stderr, "error: credential bind: --surface <node-id> is required")
		return 2
	}
	if err := ValidateCredentialHandle(h); err != nil {
		fmt.Fprintln(stderr, "error: credential bind:", err)
		return 2
	}
	homePath, err := homepkg.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "error: credential bind:", err)
		return 1
	}
	changed, err := SetConnectionSurfaceCredentialGoverned(homePath, nodeID, h, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error: credential bind:", err)
		return 1
	}
	fmt.Fprintf(stdout, "bound %s -> %s (custodian %s)\n", nodeID, h.Ref, h.Custodian)
	if !changed {
		fmt.Fprintln(stdout, "unchanged: the surface already carries this exact handle")
	}
	return 0
}

// CredentialListCommand prints every connection surface that carries a
// credential handle — addresses and env names only, never values — so "which
// lane uses which credential" is answerable at a glance.
func CredentialListCommand(args []string, stdout, stderr io.Writer) int {
	if usageRequested(args) {
		fmt.Fprintln(stderr, "usage: compound credential list [--json]")
		return 2
	}
	homePath, err := homepkg.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "error: credential list:", err)
		return 1
	}
	store, err := Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error: credential list:", err)
		return 1
	}
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		fmt.Fprintln(stderr, "error: credential list:", err)
		return 1
	}
	type row struct {
		NodeID    string `json:"node_id"`
		Ref       string `json:"credential_ref"`
		Custodian string `json:"credential_custodian"`
		EnvName   string `json:"credential_env_name"`
	}
	var rows []row
	for _, cs := range surfaces {
		if strings.TrimSpace(cs.CredentialHandle.Ref) == "" {
			continue
		}
		rows = append(rows, row{NodeID: cs.NodeID, Ref: cs.CredentialHandle.Ref, Custodian: cs.CredentialHandle.Custodian, EnvName: cs.CredentialHandle.EnvName})
	}
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		}
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"schema_version": CredentialUsageSchemaVersion, "surfaces": rows})
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no connection surfaces carry a credential handle")
		return 0
	}
	for _, r := range rows {
		fmt.Fprintf(stdout, "%s -> %s (custodian=%s, env=%s)\n", r.NodeID, r.Ref, r.Custodian, r.EnvName)
	}
	return 0
}

func flagValue(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func indexOf(args []string, token string) int {
	for i, a := range args {
		if a == token {
			return i
		}
	}
	return -1
}
