package authority

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/reviewartifact"
	"compound/internal/work"
	"golang.org/x/sys/unix"
)

const launchdRepairArea = "launchd-repair"
const LaunchdRepairCapability = "system.launchd.repair_existing"

type LaunchdSourceProof struct {
	Path         string `json:"path"`
	ResolvedPath string `json:"resolved_path"`
	SHA256       string `json:"sha256"`
}
type LaunchdRepairPlan struct {
	ID                  string                  `json:"id"`
	Digest              string                  `json:"digest"`
	AuthorizationDigest string                  `json:"authorization_digest"`
	WorkID              string                  `json:"work_id"`
	Actor               string                  `json:"actor"`
	CreatedAt           time.Time               `json:"created_at"`
	UserHome            string                  `json:"user_home"`
	UID                 int                     `json:"uid"`
	Label               string                  `json:"label"`
	Domain              string                  `json:"domain"`
	PlistPath           string                  `json:"plist_path"`
	Original            []byte                  `json:"original"`
	Candidate           []byte                  `json:"candidate"`
	Mode                uint32                  `json:"mode"`
	OriginalArgs        []string                `json:"original_args"`
	CandidateArgs       []string                `json:"candidate_args"`
	Runtime             LaunchdRuntime          `json:"runtime"`
	SourceProofs        []LaunchdSourceProof    `json:"source_proofs"`
	Handoff             *LaunchdTerminalHandoff `json:"handoff,omitempty"`
}

func launchdDigest(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func launchdBytesDigest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func launchdPlanDestination(p LaunchdRepairPlan) string {
	return "launchd:" + p.Domain + "/" + p.Label + ":" + p.WorkID + ":" + p.AuthorizationDigest
}
func launchdPlist(ctx context.Context, env LaunchdEnvironment, path string) (map[string]any, error) {
	r, err := launchdRun(ctx, env, "/usr/bin/plutil", "-convert", "json", "-o", "-", path)
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 || r.Stderr != "" {
		return nil, fmt.Errorf("plist conversion failed")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("plist is not a dictionary")
	}
	return obj, nil
}
func launchdArgs(obj map[string]any) ([]string, error) {
	raw, ok := obj["ProgramArguments"].([]any)
	if !ok || len(raw) < 3 {
		return nil, fmt.Errorf("explicit executable, script and subcommand arguments required")
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		s, ok := v.(string)
		if !ok || s == "" || strings.ContainsAny(s, "\x00\r\n") {
			return nil, fmt.Errorf("invalid program argument")
		}
		out[i] = s
	}
	if !filepath.IsAbs(out[0]) || !filepath.IsAbs(out[1]) {
		return nil, fmt.Errorf("absolute executable and script required")
	}
	return out, nil
}
func validateLaunchdCalendar(obj map[string]any) error {
	for _, key := range []string{"RunAtLoad", "KeepAlive", "Disabled"} {
		if v, present := obj[key]; present {
			b, ok := v.(bool)
			if !ok || b {
				return fmt.Errorf("%s must be absent or boolean false", key)
			}
		}
	}
	for _, key := range []string{"Sockets", "MachServices", "LaunchEvents", "WatchPaths", "QueueDirectories", "StartInterval", "StartOnMount", "Program", "EnableGlobbing", "ProcessType", "LimitLoadToSessionType", "LaunchOnlyOnce"} {
		if _, present := obj[key]; present {
			return fmt.Errorf("unsupported bootstrap affordance %s", key)
		}
	}
	var calendars []any
	switch v := obj["StartCalendarInterval"].(type) {
	case map[string]any:
		calendars = []any{v}
	case []any:
		calendars = v
	default:
		return fmt.Errorf("calendar-only existing schedule required")
	}
	if len(calendars) == 0 {
		return fmt.Errorf("empty calendar")
	}
	limits := map[string][2]int{"Minute": {0, 59}, "Hour": {0, 23}, "Day": {1, 31}, "Weekday": {0, 7}, "Month": {1, 12}}
	for _, v := range calendars {
		m, ok := v.(map[string]any)
		if !ok || len(m) == 0 {
			return fmt.Errorf("invalid calendar")
		}
		for k, n := range m {
			limit, ok := limits[k]
			f, number := n.(float64)
			if !ok || !number || f != float64(int(f)) || int(f) < limit[0] || int(f) > limit[1] {
				return fmt.Errorf("invalid calendar field %s", k)
			}
		}
	}
	return nil
}
func launchdOwnedFile(path string, uid int) ([]byte, os.FileMode, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != uint32(uid) || info.Mode().Perm()&0022 != 0 {
		return nil, 0, fmt.Errorf("regular, current-user-owned, non-writable-by-others plist required")
	}
	if info.Size() > 1024*1024 {
		return nil, 0, fmt.Errorf("plist exceeds byte bound")
	}
	b, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return nil, 0, err
	}
	if len(b) > 1024*1024 {
		return nil, 0, fmt.Errorf("plist exceeds byte bound")
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return nil, 0, fmt.Errorf("plist identity changed during read")
	}
	return b, info.Mode().Perm(), nil
}
func validateLaunchdPath(env LaunchdEnvironment, path, label string) error {
	if env.UID != os.Getuid() || env.UID < 1 || !filepath.IsAbs(env.UserHome) || !launchdLabelPattern.MatchString(label) {
		return fmt.Errorf("exact current-user GUI identity required")
	}
	home, err := filepath.EvalSymlinks(env.UserHome)
	if err != nil || home != filepath.Clean(env.UserHome) {
		return fmt.Errorf("canonical user home required")
	}
	for _, dir := range []string{home, filepath.Join(home, "Library"), filepath.Join(home, "Library", "LaunchAgents")} {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || st.Uid != uint32(env.UID) || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("owned non-symlink user LaunchAgent directories required")
		}
	}
	expected := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if path != expected {
		return fmt.Errorf("only exact existing user LaunchAgent path is eligible")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return fmt.Errorf("symlinked or escaping LaunchAgent refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", filepath.Dir(path), "rev-parse", "--show-toplevel")
	cmd.Env = gitrepository.ReadEnvironment()
	output, gitErr := cmd.CombinedOutput()
	if err := gitErr; err == nil {
		return fmt.Errorf("Git-owned target requires source delivery, not OS repair")
	} else if ctx.Err() != nil {
		return ctx.Err()
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 128 || !strings.Contains(string(output), "not a git repository") {
		return fmt.Errorf("cannot establish target outside Git")
	}
	return nil
}
func inspectLaunchdSource(proof LaunchdSourceProof) (LaunchdSourceProof, error) {
	if !filepath.IsAbs(proof.Path) {
		return proof, fmt.Errorf("absolute source proof path required")
	}
	resolved, err := filepath.EvalSymlinks(proof.Path)
	if err != nil {
		return proof, err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128*1024*1024 {
		return proof, fmt.Errorf("bounded regular source proof required")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return proof, err
	}
	defer f.Close()
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(f, 128*1024*1024+1))
	if err != nil {
		return proof, err
	}
	if n > 128*1024*1024 {
		return proof, fmt.Errorf("source exceeds proof bound")
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	if proof.ResolvedPath != "" && proof.ResolvedPath != resolved {
		return proof, fmt.Errorf("source symlink target drift")
	}
	if proof.SHA256 != "" && proof.SHA256 != hash {
		return proof, fmt.Errorf("source content drift: %s", proof.Path)
	}
	proof.ResolvedPath = resolved
	proof.SHA256 = hash
	return proof, nil
}
func BuildLaunchdRepairPlan(ctx context.Context, env LaunchdEnvironment, workID, actor, plistPath, candidatePath string, proofs []LaunchdSourceProof) (LaunchdRepairPlan, error) {
	var p LaunchdRepairPlan
	if !strings.HasPrefix(workID, "work_") || actor == "" {
		return p, fmt.Errorf("work and actor required")
	}
	label := strings.TrimSuffix(filepath.Base(plistPath), ".plist")
	if err := validateLaunchdPath(env, plistPath, label); err != nil {
		return p, err
	}
	original, mode, err := launchdOwnedFile(plistPath, env.UID)
	if err != nil {
		return p, err
	}
	candidate, _, err := launchdOwnedFile(candidatePath, env.UID)
	if err != nil {
		return p, err
	}
	before, err := launchdPlist(ctx, env, plistPath)
	if err != nil {
		return p, err
	}
	after, err := launchdPlist(ctx, env, candidatePath)
	if err != nil {
		return p, err
	}
	if before["Label"] != label || after["Label"] != label {
		return p, fmt.Errorf("plist label mismatch")
	}
	if err := validateLaunchdCalendar(before); err != nil {
		return p, err
	}
	if err := validateLaunchdCalendar(after); err != nil {
		return p, err
	}
	oldArgs, err := launchdArgs(before)
	if err != nil {
		return p, err
	}
	newArgs, err := launchdArgs(after)
	if err != nil {
		return p, err
	}
	if !reflect.DeepEqual(oldArgs[:3], newArgs[:3]) {
		return p, fmt.Errorf("existing executable, script and subcommand cannot change")
	}
	delete(before, "ProgramArguments")
	delete(after, "ProgramArguments")
	if !reflect.DeepEqual(before, after) {
		return p, fmt.Errorf("only ProgramArguments may change")
	}
	if len(proofs) < 2 || len(proofs) > 32 {
		return p, fmt.Errorf("explicit executable and script source proofs required")
	}
	seen := map[string]bool{}
	sealed := make([]LaunchdSourceProof, 0, len(proofs))
	for _, proof := range proofs {
		if seen[proof.Path] || len(proof.SHA256) != 64 {
			return p, fmt.Errorf("unique explicit source proof digest required")
		}
		v, err := inspectLaunchdSource(proof)
		if err != nil {
			return p, err
		}
		seen[proof.Path] = true
		sealed = append(sealed, v)
	}
	if !seen[newArgs[0]] || !seen[newArgs[1]] {
		return p, fmt.Errorf("both executable and script must have explicit source proofs")
	}
	runtime := ProbeLaunchdRuntime(ctx, env, label)
	if runtime.OverrideState != "disabled" || runtime.LoadedState != "absent" || runtime.Error != "" {
		return p, fmt.Errorf("repair requires exact disabled override and unloaded service: %s/%s %s", runtime.OverrideState, runtime.LoadedState, runtime.Error)
	}
	p = LaunchdRepairPlan{WorkID: workID, Actor: actor, CreatedAt: launchdNow(env), UserHome: env.UserHome, UID: env.UID, Label: label, Domain: runtime.Domain, PlistPath: plistPath, Original: original, Candidate: candidate, Mode: uint32(mode), OriginalArgs: oldArgs, CandidateArgs: newArgs, Runtime: runtime, SourceProofs: sealed}
	p.AuthorizationDigest = launchdDigest(p)
	p.Digest = launchdDigest(p)
	p.ID = "ldrp_" + p.Digest[:24]
	return p, nil
}
func validateLaunchdPlan(p LaunchdRepairPlan) error {
	copy := p
	copy.ID = ""
	copy.Digest = ""
	if launchdDigest(copy) != p.Digest || p.ID != "ldrp_"+p.Digest[:24] {
		return fmt.Errorf("launchd plan digest mismatch")
	}
	copy.AuthorizationDigest = ""
	if launchdDigest(copy) != p.AuthorizationDigest {
		return fmt.Errorf("launchd authorization digest mismatch")
	}
	return nil
}
func launchdArtifactPath(home, name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid launchd artifact name")
	}
	dir := filepath.Join(home, "reviews", launchdRepairArea)
	for _, p := range []string{home, filepath.Join(home, "reviews"), dir} {
		info, err := os.Lstat(p)
		if os.IsNotExist(err) && p != home {
			continue
		}
		if err != nil {
			return "", err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || st.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0022 != 0 {
			return "", fmt.Errorf("owned non-symlink launchd artifact directory required")
		}
	}
	owned, err := reviewartifact.Dir(home, launchdRepairArea)
	if err != nil {
		return "", err
	}
	return filepath.Join(owned, name), nil
}
func writeLaunchdArtifact(home, name string, v any) error {
	path, err := launchdArtifactPath(home, name)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("launchd artifact is not regular")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if prior, err := os.ReadFile(path); err == nil {
		if bytesEqual(prior, body) {
			return nil
		}
		return fmt.Errorf("immutable launchd artifact conflict: %s", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func bytesEqual(a, b []byte) bool { return string(a) == string(b) }
func readLaunchdArtifact(home, name string, v any) error {
	path, err := launchdArtifactPath(home, name)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 8*1024*1024 {
		return fmt.Errorf("invalid launchd artifact file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func loadLaunchdPlan(home, id string) (LaunchdRepairPlan, error) {
	var p LaunchdRepairPlan
	if err := readLaunchdArtifact(home, id+".plan.json", &p); err != nil {
		return p, err
	}
	if p.ID != id {
		return p, fmt.Errorf("plan filename mismatch")
	}
	return p, validateLaunchdPlan(p)
}
func launchdAction(p LaunchdRepairPlan, id, phase string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_" + launchdDigest(id)[:24], CreatedAt: now.Format(time.RFC3339Nano), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: p.WorkID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier2", EvidenceRefs: []string{"launchd-plan:" + p.ID, "sha256:" + p.Digest}, Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: p.Actor, Capability: LaunchdRepairCapability, Action: phase, Target: envelope.ActionTarget{Kind: "launchd-user-agent", ID: p.Domain + "/" + p.Label, Path: p.PlistPath}, Destination: launchdPlanDestination(p), ArgumentsHash: p.AuthorizationDigest, MutationClass: "external_side_effect", Reversibility: "easy", BlastRadius: "narrow", Preview: "exact existing calendar-only LaunchAgent repair; no kickstart", RollbackPlan: "explicit reviewed rollback only; preserve foreign edits and unrelated overrides"}
}
func authorizeLaunchdArtifact(home string, p LaunchdRepairPlan, suffix string, now time.Time) error {
	store, err := work.Open(home)
	if err != nil {
		return err
	}
	a := launchdAction(p, p.ID+suffix, "record_launchd_repair_"+suffix, now)
	a.Capability = "shell.operator_scope"
	a.MutationClass = "local_reversible"
	a.BlastRadius = "repo"
	a.Target.Path = home
	result, err := store.EvaluateAndRecordAction(p.WorkID, a, now)
	if err != nil {
		return err
	}
	if !result.Recorded {
		return fmt.Errorf("launchd artifact policy %s", result.Decision.Outcome)
	}
	return nil
}
func RecordLaunchdRepairPlan(home string, p LaunchdRepairPlan, now time.Time) error {
	if err := validateLaunchdPlan(p); err != nil {
		return err
	}
	if err := authorizeLaunchdArtifact(home, p, "plan", now); err != nil {
		return err
	}
	return writeLaunchdArtifact(home, p.ID+".plan.json", p)
}
func lockLaunchdLabel(home, label string) (func(), error) {
	path, err := launchdArtifactPath(home, "label-"+launchdDigest(label)[:24]+".lock")
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("same-label launchd operation busy")
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
}
func launchdWorkDigest(w envelope.WorkEnvelope) string { return "sha256:" + launchdDigest(w) }
func launchdExpectedWork(before envelope.WorkEnvelope, a envelope.ActionIntent, pd envelope.PolicyDecision) envelope.WorkEnvelope {
	w := before
	w.ActionIntentIDs = append([]string{}, before.ActionIntentIDs...)
	found := false
	for _, id := range w.ActionIntentIDs {
		found = found || id == a.ID
	}
	if !found {
		w.ActionIntentIDs = append(w.ActionIntentIDs, a.ID)
	}
	w.PolicyDecisionID = pd.ID
	return w
}
