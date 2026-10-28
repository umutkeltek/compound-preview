package actuator

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/policy"
	"compound/internal/work"
)

// cliTestNow mirrors the fixed clock in cmd/compound/main_test.go so the
// moved CLI tests keep their original deterministic timestamps.
var cliTestNow = time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

func seedActuatorCLIWork(t *testing.T, homePath, workID string) {
	t.Helper()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID:           workID,
		Owner:            "actuator-cli-test",
		SuccessPredicate: "actuator CLI test work completes",
	}, cliTestNow); err != nil {
		t.Fatal(err)
	}
}

// T1 (packet 2026-07-21-zip27-actuator-cli.md): `actuator budget-set --max-actions ten`
// must error naming the flag and the bad value, and must NOT write the budget file.
// At base, actuatorInt silently returned the default (0 == unlimited) on parse failure.
func TestActuatorBudgetSetRejectsUnparseableMaxActions(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"budget-set", "--channel", "test-channel", "--window-hours", "24",
		"--max-actions", "ten", "--home", homePath,
	}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("want non-zero exit for unparseable --max-actions, got 0; stdout=%q", stdout.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "--max-actions") || !strings.Contains(errText, "ten") {
		t.Fatalf("error should mention --max-actions and the bad value %q; got %q", "ten", errText)
	}
	budgetPath := filepath.Join(homePath, "actuator", "budget", "default.budget.json")
	if _, err := os.Stat(budgetPath); !os.IsNotExist(err) {
		t.Fatalf("budget file must NOT be written on parse failure (stat err=%v)", err)
	}
}

// actuatorFloat gets the same falsifier as actuatorInt (T1's sibling): a
// non-empty, non-parseable --max-spend-usd must error, not silently default.
func TestActuatorBudgetSetRejectsUnparseableMaxSpendUSD(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"budget-set", "--channel", "test-channel", "--window-hours", "24",
		"--max-spend-usd", "lots", "--home", homePath,
	}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("want non-zero exit for unparseable --max-spend-usd, got 0; stdout=%q", stdout.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "--max-spend-usd") || !strings.Contains(errText, "lots") {
		t.Fatalf("error should mention --max-spend-usd and the bad value %q; got %q", "lots", errText)
	}
	budgetPath := filepath.Join(homePath, "actuator", "budget", "default.budget.json")
	if _, err := os.Stat(budgetPath); !os.IsNotExist(err) {
		t.Fatalf("budget file must NOT be written on parse failure (stat err=%v)", err)
	}
}

// T2: a typo'd flag on `actuator run` must be rejected naming the unknown flag,
// exactly as work_cli.go's validateKnownFlags pattern already does for work
// subcommands. At base, actuator subcommands called validateKnownFlags zero times.
func TestActuatorRunRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"run", "--work", "w1", "--harness", "noop", "--max-actons", "3",
	}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("want exit 2 for unknown flag, got %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--max-actons") {
		t.Fatalf("error should name the unknown flag --max-actons; got %q", stderr.String())
	}
}

// Decision 2 applies to every actuator subcommand, not just `run` — enumerate the
// full dispatch table and prove each one rejects an unrecognized flag by name.
func TestActuatorSubcommandsRejectUnknownFlag(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{"plan", []string{"plan", "--work", "w1", "--bogus-flag", "x", "--home", homePath}},
		{"budget", []string{"budget", "--bogus-flag", "x", "--home", homePath}},
		{"budget-set", []string{"budget-set", "--channel", "c", "--window-hours", "24", "--bogus-flag", "x", "--home", homePath}},
		{"switchback", []string{"switchback", "--profile", "p.yaml", "--work", "w1", "--bogus-flag", "x", "--home", homePath}},
		{"run", []string{"run", "--work", "w1", "--harness", "noop", "--bogus-flag", "x", "--home", homePath}},
		{"post", []string{"post", "--work", "w1", "--content", "hi", "--bogus-flag", "x", "--home", homePath}},
		{"observe", []string{"observe", "--bogus-flag", "x", "--home", homePath}},
		{"held", []string{"held", "--bogus-flag", "x", "--home", homePath}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runCLI(tc.args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("%s: want exit 2 for unknown flag, got %d; stderr=%q", tc.name, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "--bogus-flag") {
				t.Fatalf("%s: error should name --bogus-flag; got %q", tc.name, stderr.String())
			}
		})
	}
}

// T3: valid numeric + known flags still work end-to-end — one happy path per
// changed subcommand, proving decisions 1+2 add zero behavior change for valid
// input (existing green paths stay green).
func TestActuatorSubcommandsAcceptKnownFlagsEndToEnd(t *testing.T) {
	t.Parallel()
	t.Run("plan", func(t *testing.T) {
		homePath := t.TempDir()
		var stdout, stderr bytes.Buffer
		code := runCLI([]string{
			"plan", "--work", "w1", "--class", "local_reversible", "--reversibility", "easy",
			"--channel", "test", "--product", "p1", "--home", homePath, "--json",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("plan: want exit 0, got %d; stderr=%q", code, stderr.String())
		}
	})

	t.Run("budget", func(t *testing.T) {
		homePath := t.TempDir()
		var stdout, stderr bytes.Buffer
		code := runCLI([]string{"budget", "--product", "p1", "--home", homePath, "--json"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("budget: want exit 0, got %d; stderr=%q", code, stderr.String())
		}
	})

	t.Run("budget-set", func(t *testing.T) {
		homePath := t.TempDir()
		var stdout, stderr bytes.Buffer
		code := runCLI([]string{
			"budget-set", "--channel", "test-channel", "--window-hours", "24",
			"--max-actions", "5", "--max-spend-usd", "1.5", "--max-tokens", "100",
			"--product", "p1", "--home", homePath,
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("budget-set: want exit 0, got %d; stderr=%q", code, stderr.String())
		}
		budgetPath := filepath.Join(homePath, "actuator", "budget", "p1.budget.json")
		data, err := os.ReadFile(budgetPath)
		if err != nil {
			t.Fatalf("budget-set: expected budget file written: %v", err)
		}
		if !strings.Contains(string(data), `"max_actions": 5`) {
			t.Fatalf("budget-set: expected max_actions=5 in %s", data)
		}
	})

	t.Run("observe", func(t *testing.T) {
		homePath := t.TempDir()
		var stdout, stderr bytes.Buffer
		code := runCLI([]string{"observe", "--product", "p1", "--home", homePath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("observe: want exit 0, got %d; stderr=%q", code, stderr.String())
		}
	})

	t.Run("held", func(t *testing.T) {
		homePath := t.TempDir()
		var stdout, stderr bytes.Buffer
		code := runCLI([]string{"held", "--product", "p1", "--home", homePath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("held: want exit 0, got %d; stderr=%q", code, stderr.String())
		}
	})

	// run/post/switchback reach into internal/actuator + internal/act domain logic
	// this packet does not touch, so these assert only what changed here: known
	// flags must never be rejected as unknown, and passthrough argv after `--`
	// must never be flag-validated at all.
	t.Run("run_passthrough_args_not_validated", func(t *testing.T) {
		homePath := t.TempDir()
		var stdout, stderr bytes.Buffer
		runCLI([]string{
			"run", "--work", "w1", "--harness", "noop-does-not-exist",
			"--home", homePath, "--", "echo", "--not-a-real-flag",
		}, &stdout, &stderr)
		if strings.Contains(stderr.String(), "unknown flag") {
			t.Fatalf("run: known flags + passthrough argv after -- must not be flag-rejected; stderr=%q", stderr.String())
		}
	})

	t.Run("post_known_flags_not_rejected", func(t *testing.T) {
		homePath := t.TempDir()
		var stdout, stderr bytes.Buffer
		runCLI([]string{
			"post", "--work", "w1", "--content", "hello", "--product", "p1",
			"--channel", "distribution", "--surface", "generic", "--metric", "m1",
			"--expected-effect", "up", "--window", "7d", "--home", homePath,
		}, &stdout, &stderr)
		if strings.Contains(stderr.String(), "unknown flag") {
			t.Fatalf("post: known flags must not be rejected; stderr=%q", stderr.String())
		}
	})

	t.Run("switchback_known_flags_not_rejected", func(t *testing.T) {
		homePath := t.TempDir()
		profilePath := filepath.Join(t.TempDir(), "profile.yaml")
		if err := os.WriteFile(profilePath, []byte("id: test_profile\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		runCLI([]string{
			"switchback", "--profile", profilePath, "--work", "w1", "--home", homePath,
		}, &stdout, &stderr)
		if strings.Contains(stderr.String(), "unknown flag") {
			t.Fatalf("switchback: known flags must not be rejected; stderr=%q", stderr.String())
		}
	})
}

// postRecordFailureHome fails settlement AFTER the reservation and governed
// write, retaining the real post-dispatch accounting-failure falsifier.
// Callers are serial because they replace the accounting seam.
func postRecordFailureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	seedActuatorCLIWork(t, home, "work_rec")
	original := recordPostConsumption
	recordPostConsumption = func(*Ledger, Consumption) error { return errors.New("consumption sink unavailable") }
	t.Cleanup(func() { recordPostConsumption = original })
	return home
}

// Since 7c66eec7, Post can return a POPULATED PostResult (Executed=true,
// receipt/transition refs) together with a non-nil error — "the post landed but
// budget accounting failed" (F9.1). runActuatorPost must mirror runActuatorRun and
// surface that result, not drop it. These three falsifiers pin the CLI contract.

// TestActuatorPostJSONSurfacesResultWithRecordError: --json mode must emit the
// populated result on stdout (executed:true + non-empty receipt_ref) AND name the
// consumption failure on stderr with exit 1. RED before the fix (result dropped:
// stdout empty of the result, only stderr).
func TestActuatorPostJSONSurfacesResultWithRecordError(t *testing.T) {
	home := postRecordFailureHome(t)
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"post", "--work", "work_rec", "--product", "product-demo",
		"--content", "Ship agent work that compounds.", "--home", home, "--json",
	}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("want exit 1 on a consumption-record failure, got %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var got struct {
		Executed   bool   `json:"executed"`
		ReceiptRef string `json:"receipt_ref"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout must be the JSON PostResult even on error; unmarshal failed: %v; stdout=%q", err, stdout.String())
	}
	if !got.Executed {
		t.Fatalf("the post staged + receipted; JSON result must report executed:true; stdout=%q", stdout.String())
	}
	if got.ReceiptRef == "" {
		t.Fatalf("the operator must recover the receipt ref from stdout; got empty; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "consumption") {
		t.Fatalf("stderr must name the consumption-record failure; got %q", stderr.String())
	}
}

// TestActuatorPostHumanSurfacesReceiptWithRecordError: human mode must print the
// EXECUTED block (the receipt: line) AND name the failure on stderr with exit 1.
// RED before the fix (block skipped: no receipt: line on stdout).
func TestActuatorPostHumanSurfacesReceiptWithRecordError(t *testing.T) {
	home := postRecordFailureHome(t)
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"post", "--work", "work_rec", "--product", "product-demo",
		"--content", "Ship agent work that compounds.", "--home", home,
	}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("want exit 1 on a consumption-record failure, got %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "receipt:") {
		t.Fatalf("the operator must recover the receipt ref from the EXECUTED block; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "consumption") {
		t.Fatalf("stderr must name the consumption-record failure; got %q", stderr.String())
	}
}

// TestActuatorPostPreStagingFailureStaysErrorOnly: when Post fails BEFORE anything
// lands (Executed=false — here act.NewArtifactStore fails because artifacts/ is a
// file), human mode must stay error-only: exit 1, NO EXECUTED block on stdout.
// This guards the mirror against over-printing an EXECUTED block for a zero result.
func TestActuatorPostPreStagingFailureStaysErrorOnly(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	seedActuatorCLIWork(t, home, "work_pre")
	// Make the artifact-store path a FILE so act.NewArtifactStore's MkdirAll
	// (home/artifacts/sha256) fails before any post is staged.
	if err := os.WriteFile(filepath.Join(home, "artifacts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"post", "--work", "work_pre", "--product", "product-demo",
		"--content", "Ship agent work that compounds.", "--home", home,
	}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("want exit 1 on a pre-staging failure, got %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "EXECUTED") {
		t.Fatalf("nothing landed; the EXECUTED block must NOT print for a zero result; stdout=%q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatalf("a pre-staging failure must name the error on stderr; got empty")
	}
}

func TestActuatorSwitchbackJSONPreservesAccountingFailureAndExitsNonzero(t *testing.T) {
	homePath := t.TempDir()
	profilePath := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(profilePath, []byte("id: accounting_test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := actuatorSwitchbackFn
	actuatorSwitchbackFn = func(string, ProjectProfile, string, policy.Config, time.Time) (SwitchbackResult, error) {
		return SwitchbackResult{
			ProfileID: "accounting_test",
			WorkID:    "work_switchback_accounting",
			Steps: []StepResult{{
				Kind:             StepInstructions,
				Applied:          true,
				ReceiptRef:       "rec_accounting_test",
				TransitionRef:    "tr_accounting_test",
				AccountingFailed: true,
				AccountingError:  "accounting sink unavailable",
			}},
		}, errors.New("switchback accounting failed after receipted mutation")
	}
	t.Cleanup(func() { actuatorSwitchbackFn = original })

	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"switchback",
		"--profile", profilePath,
		"--work", "work_switchback_accounting",
		"--home", homePath,
		"--json",
	}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("accounting failure must exit 1, got %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var result SwitchbackResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout must preserve the structured partial result: %v; stdout=%q", err, stdout.String())
	}
	if len(result.Steps) != 1 || !result.Steps[0].Applied || !result.Steps[0].AccountingFailed ||
		result.Steps[0].ReceiptRef != "rec_accounting_test" {
		t.Fatalf("partial result lost applied/accounting/receipt state: %+v", result)
	}
	if !strings.Contains(stderr.String(), "accounting") {
		t.Fatalf("stderr must name the accounting failure, got %q", stderr.String())
	}
}

func TestActuatorRunJSONNamesMissingWorkError(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := runCLI([]string{
		"run",
		"--work", "work_missing",
		"--harness", "claude",
		"--permission-mode", "plan",
		"--prompt", "inspect only",
		"--home", homePath,
		"--json",
	}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("missing work must exit 1, got %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "work_missing") || !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("JSON mode must name the missing work error on stderr, got %q", stderr.String())
	}
}

func TestActuatorRunHeldCardPersistenceFailureStaysErrorOnly(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	seedActuatorCLIWork(t, homePath, "work_hold")
	actuatorDir := filepath.Join(homePath, "actuator")
	if err := os.MkdirAll(actuatorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actuatorDir, "held-moves"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runCLI([]string{
		"run",
		"--work", "work_hold",
		"--harness", "os-harness-entrypoints",
		"--home", homePath,
		"--", "true",
	}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("held-card persistence failure must exit 1, got %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "persist held-move card") {
		t.Fatalf("stderr must name the held-card persistence failure, got %q", stderr.String())
	}
	for _, misleading := range []string{"HELD", "run_attempt", "exit_code"} {
		if strings.Contains(stdout.String(), misleading) {
			t.Fatalf("pre-dispatch failure must not print a %q run summary; stdout=%q", misleading, stdout.String())
		}
	}
}
