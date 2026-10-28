package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/hook"
	"compound/internal/hookplan"
	"compound/internal/policy"
)

// doctor_hook_enforcement.go answers the question that made a real fail-open last 48 minutes with
// no signal: the hooks are installed and their configured command exists — but does that binary
// honor the configured observation-only or explicitly enforced posture?
//
// Every other hook signal doctor reports is a comparison of CONFIG bytes: hook install/drift/absent
// compares the command string in the harness config against the one Compound would write. All of it
// stays green when the path is right and the file at that path is a stub, because a stub that exits
// 0 is indistinguishable from an allow. Silence and success are the same observation.
//
// So this check probes BEHAVIOUR, and picks a probe whose correct answer cannot exist in advance:
// it synthesizes a mutation containing a nonce minted for this run, sends it to the exact installed
// live command on stdin, and checks the isolated ActionIntent, non-allowing PolicyDecision, and
// HookEvent whose input hash binds that action. It then checks the configured posture:
// observation-only must exit zero, while explicit --enforce must return the exact hold/deny exit.
// A canned response fails the first check; an exit-0 no-op fails both.
//
// Read-only like the rest of doctor: the probe runs with COMPOUND_HOME pointed at a throwaway
// directory and its cwd inside it, so the hook records its events there and nothing touches
// operator state.

// hookEnforcementProbeTimeout bounds each probe. Doctor is a routine command; a hook binary that
// cannot answer a single fixture within this budget is reported, not waited for.
const hookEnforcementProbeTimeout = 5 * time.Second

const (
	hookPostureObservationOnly = hookplan.HookPostureObservationOnly
	hookPostureEnforced        = hookplan.HookPostureEnforced
)

// doctorHookEnforcement is one binary+posture the installed hooks would invoke. A shared binary
// can therefore have one observation row and one enforcement row without conflating them.
type doctorHookEnforcement struct {
	// Harnesses names every configured harness whose hook command resolves to this binary and
	// posture. Equivalent rows share one subprocess probe.
	Harnesses []string `json:"harnesses"`
	Binary    string   `json:"binary"`
	Posture   string   `json:"posture"` // observation_only | enforced
	Status    string   `json:"status"`  // observing | enforcing | fail_open
	Detail    string   `json:"detail,omitempty"`
}

type installedHookPosture = hookplan.InstalledHookPosture

type hookProbeTarget struct {
	Binary          string
	Args            []string
	Posture         string
	ExpectedHarness string
	Harnesses       map[string]bool
}

// buildHookEnforcementRows probes each distinct binary+posture the installed hook configs invoke.
// It reads the harness configs doctor already located rather than re-deriving paths, so the rows
// describe the same install the hook section describes.
func buildHookEnforcementRows(hooks []doctorHook) ([]doctorHookEnforcement, int) {
	targets := map[string]*hookProbeTarget{}
	for _, entry := range hooks {
		for _, installed := range installedHookPostures(entry.ConfigPath) {
			expectedHarness := hook.DoctorExpectedHarness(entry.Harness)
			key := installed.Binary + "\x00" + strings.Join(installed.Args, "\x00") + "\x00" + expectedHarness
			target := targets[key]
			if target == nil {
				target = &hookProbeTarget{
					Binary:          installed.Binary,
					Args:            append([]string(nil), installed.Args...),
					Posture:         installed.Posture,
					ExpectedHarness: expectedHarness,
					Harnesses:       map[string]bool{},
				}
				targets[key] = target
			}
			target.Harnesses[entry.Harness] = true
		}
	}
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	rows := make([]doctorHookEnforcement, 0, len(keys))
	failing := 0
	for _, key := range keys {
		target := targets[key]
		harnesses := make([]string, 0, len(target.Harnesses))
		for harness := range target.Harnesses {
			harnesses = append(harnesses, harness)
		}
		sort.Strings(harnesses)
		detail := probeHookPosture(target.Binary, target.Args, target.Posture, target.ExpectedHarness)
		status := "observing"
		if target.Posture == hookPostureEnforced {
			status = "enforcing"
		}
		if detail != "" {
			status = "fail_open"
			failing++
		}
		rows = append(rows, doctorHookEnforcement{
			Harnesses: harnesses,
			Binary:    target.Binary,
			Posture:   target.Posture,
			Status:    status,
			Detail:    detail,
		})
	}
	return rows, failing
}

// installedHookPostures is the one installed-binding reader (hookplan): doctor probes what it
// returns and the launch stamp records the posture it names, so both describe the same install.
var installedHookPostures = hookplan.InstalledHookPostures

// hookCommandBinary extracts the executable from a hook command line. It stays as the compatibility
// surface for the earlier binary-only check while sharing the posture parser's tokenization.
func hookCommandBinary(command string) string {
	words, ok := hookCommandWords(command)
	if !ok || len(words) == 0 {
		return ""
	}
	return words[0]
}

var hookCommandWords = hookplan.HookCommandWords

// probeHookPosture returns "" when the exact installed live command evaluates the mutation and
// honors the configured process-exit posture, or a one-line reason why it does not.
func probeHookPosture(binaryPath string, installedArgs []string, posture, expectedHarness string) string {
	if detail := hook.ValidateDoctorProbeArgs(installedArgs, posture == hookPostureEnforced, expectedHarness); detail != "" {
		return detail
	}
	nonce, err := mintHookProbeNonce()
	if err != nil {
		return "could not mint a probe nonce: " + err.Error()
	}
	sandbox, err := os.MkdirTemp("", "compound-hook-probe-")
	if err != nil {
		return "could not create a probe sandbox: " + err.Error()
	}
	defer os.RemoveAll(sandbox)

	homePath := filepath.Join(sandbox, "home")
	if err := home.Init(homePath, "doctor-hook-probe", time.Now().UTC()); err != nil {
		return "could not initialize the probe home: " + err.Error()
	}
	targetPath := filepath.Join(sandbox, "probe-"+nonce)
	input, err := hook.DoctorLiveProbeInput(nonce, targetPath)
	if err != nil {
		return "could not build the live probe input: " + err.Error()
	}

	ctx, cancel := context.WithTimeout(context.Background(), hookEnforcementProbeTimeout)
	defer cancel()
	if posture != hookPostureEnforced && posture != hookPostureObservationOnly {
		return fmt.Sprintf("unknown configured hook posture %q", posture)
	}
	args := append([]string(nil), installedArgs...)
	cmd := exec.CommandContext(ctx, binaryPath, args...)
	cmd.Dir = sandbox
	cmd.Env = hook.DoctorProbeEnvironment(homePath)
	cmd.Stdin = bytes.NewReader(input)
	out, runErr := cmd.Output()
	if ctx.Err() != nil {
		return fmt.Sprintf("%s probe timed out after %s", hookPostureLabel(posture), hookEnforcementProbeTimeout)
	}
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return fmt.Sprintf("running `%s %s` for %s failed: %v",
				binaryPath, strings.Join(args, " "), hookPostureLabel(posture), runErr)
		}
		exitCode = exitErr.ExitCode()
	}
	if detail := hook.InspectDoctorLiveResponse(out); detail != "" {
		return fmt.Sprintf("%s probe: %s", hookPostureLabel(posture), detail)
	}
	outcome, detail := hook.InspectDoctorLiveEvidence(homePath, targetPath)
	if detail != "" {
		return fmt.Sprintf("%s probe: %s", hookPostureLabel(posture), detail)
	}
	wantExit, ok := hookPostureExpectedExit(posture, outcome)
	if !ok {
		return fmt.Sprintf("%s probe produced unsupported PolicyDecision outcome %q", hookPostureLabel(posture), outcome)
	}
	if exitCode != wantExit {
		return fmt.Sprintf(
			"%s probe evaluated PolicyDecision outcome %q but exited %d, want %d",
			hookPostureLabel(posture),
			outcome,
			exitCode,
			wantExit,
		)
	}
	return ""
}

func hookPostureExpectedExit(posture, outcome string) (int, bool) {
	return hook.DoctorExpectedExit(posture, outcome)
}

func hookPostureLabel(posture string) string {
	if posture == hookPostureEnforced {
		return "explicit enforcement"
	}
	return "observation-only"
}

// probeHookEnforcement retains the original helper's meaning for callers that explicitly want to
// prove enforcement.
func probeHookEnforcement(binaryPath string) string {
	return probeHookPosture(binaryPath, []string{
		"hook", "pre-tool-use", "--live", "--harness", "claude_code", "--enforce",
	}, hookPostureEnforced, "claude_code")
}

// checkHookEnforcementResponse is the pure half: given what the binary printed and the nonce it was
// asked about, decide whether only a real policy gate could have answered.
func checkHookEnforcementResponse(out []byte, nonce string) string {
	_, detail := inspectHookEnforcementResponse(out, nonce)
	return detail
}

func inspectHookEnforcementResponse(out []byte, nonce string) (string, string) {
	var res struct {
		Event struct {
			HookName  string `json:"hook_name"`
			Status    string `json:"status"`
			InputHash string `json:"input_hash"`
		} `json:"event"`
		Produced []json.RawMessage `json:"produced"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", "the hook produced no parseable HookEvent (an exit code alone is not a decision)"
	}
	if res.Event.HookName != "PreToolUse" {
		return "", fmt.Sprintf("the hook reported hook_name %q, want PreToolUse", res.Event.HookName)
	}
	wantHash := hookProbeDigest(nonce)
	if res.Event.InputHash != wantHash {
		return "", fmt.Sprintf("the hook's input_hash is %q, but this run's probe hashes to %q — the answer was not derived from the input",
			res.Event.InputHash, wantHash)
	}
	for _, raw := range res.Produced {
		var decision envelope.PolicyDecision
		if json.Unmarshal(raw, &decision) != nil || decision.SchemaVersion != envelope.SVPolicyDecision {
			continue
		}
		if policy.Allowed(decision) {
			return "", fmt.Sprintf("the gate ALLOWED a destructive probe mutation (outcome %q)", decision.Outcome)
		}
		return decision.Outcome, ""
	}
	return "", "the hook emitted no PolicyDecision for a destructive mutation, so nothing was gated"
}

// hookEnforcementProbeFixture builds the mutation the gate must refuse. The ActionIntent id is the
// run's nonce — that is what makes the expected answer impossible to precompute, because the hook
// event's input hash is derived from it. The harness is fixed rather than taken from the config
// entry: the probe is about the BINARY, and pinning one known harness keeps a lane-specific harness
// name from turning a real fail-open into an unknown.
func hookEnforcementProbeFixture(nonce string) []byte {
	fixture := map[string]any{
		"hook":    "pre-tool-use",
		"harness": "claude_code",
		"work_id": "work_doctor_probe",
		"action": map[string]any{
			"schema_version": envelope.SVActionIntent,
			"id":             nonce,
			"created_at":     "2026-01-01T00:00:00Z",
			"producer":       map[string]any{"kind": "hook", "name": "compound"},
			"ids":            map[string]any{"work_id": "work_doctor_probe"},
			"scope":          map[string]any{},
			"authority":      map[string]any{"source_class": "runtime"},
			"privacy":        "private",
			"risk":           "tier2",
			"evidence_refs":  []string{},
			"uncertainty":    map[string]any{"level": "known"},
			"actor":          "claude_code",
			"capability":     "filesystem.delete",
			"action":         "delete_tree",
			"target":         map[string]any{"kind": "path", "path": "build/cache"},
			"mutation_class": "destructive",
			"reversibility":  "hard",
			"blast_radius":   "repo",
		},
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func mintHookProbeNonce() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "act_doctorprobe" + hex.EncodeToString(raw), nil
}

func hookProbeDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// printHookEnforcement renders the rows. A passing row stays to one line — a health surface that
// prints a wall of green trains the operator to skip it, and this is the section that must be read
// on the day it is not green.
func printHookEnforcement(stdout io.Writer, rep doctorReport) {
	fmt.Fprintln(stdout, "\n## hook posture (does the hook binary honor its configured mode?)")
	if len(rep.HookEnforcement) == 0 {
		fmt.Fprintln(stdout, "- (no installed hook command to probe)")
		return
	}
	for _, row := range rep.HookEnforcement {
		harnesses := strings.Join(row.Harnesses, ",")
		if row.Status == "observing" {
			fmt.Fprintf(stdout, "- ok   %-24s %s (observation-only; evaluated policy and exited 0)\n", harnesses, row.Binary)
			continue
		}
		if row.Status == "enforcing" {
			fmt.Fprintf(stdout, "- ok   %-24s %s (explicit enforcement; evaluated policy and returned the exact gate exit)\n", harnesses, row.Binary)
			continue
		}
		fmt.Fprintf(stdout, "- FAIL %-24s %s (%s)\n", harnesses, row.Binary, hookPostureLabel(row.Posture))
		fmt.Fprintf(stdout, "    %s\n", row.Detail)
	}
	if rep.HookEnforcementFailing > 0 {
		fmt.Fprintln(stdout, "  -> an installed binary does not honor its configured hook posture; reinstall it (`make install`) and re-run this check")
	}
}
