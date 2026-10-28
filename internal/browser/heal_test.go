package browser

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- probe ---------------------------------------------------------------

func TestProbeLoggedInMarkerExitsZeroAndRecordsLastProbe(t *testing.T) {
	f := newHealFixture(t, false)
	f.state("logged_in", "")
	code, out, errOut := f.cli("probe", "--profile", fakeProfileID, "--json")
	if code != 0 {
		t.Fatalf("probe exit = %d, want 0 (out=%s err=%s)", code, out, errOut)
	}
	var res probeResult
	decodeJSONOut(t, out, &res)
	if !res.LoggedIn || res.Profile != fakeProfileID || res.EvidencePath == "" {
		t.Fatalf("probe result = %+v", res)
	}
	row := f.registryRow()
	last, ok := row["last_probe"].(map[string]any)
	if !ok || last["logged_in"] != true || last["at"] == "" || last["evidence_path"] != res.EvidencePath {
		t.Fatalf("registry last_probe = %#v", row["last_probe"])
	}
	if row["unknown_future"] != "kept" || row["login_recipe"] != "login" {
		t.Fatalf("registry update dropped fields it does not own: %#v", row)
	}
	evidenceDir := filepath.Join(f.home, "browser", "evidence", fakeProfileID)
	if !strings.HasPrefix(res.EvidencePath, evidenceDir+string(os.PathSeparator)) {
		t.Fatalf("evidence %q not under %q", res.EvidencePath, evidenceDir)
	}
	if _, err := os.Stat(res.EvidencePath); err != nil {
		t.Fatalf("evidence screenshot missing: %v", err)
	}
	if f.countArgv("[open] [https://app.example.test/account]") != 1 || f.countArgv("[close]") != 1 {
		t.Fatalf("probe did not open the check URL once and close: %v", f.argvLines())
	}
}

func TestProbeRetriesAgentBrowserReconnectRace(t *testing.T) {
	f := newHealFixture(t, false)
	f.state("logged_in", "")
	f.state("connect-fail-once", "")
	if code, out, errOut := f.cli("probe", "--profile", fakeProfileID); code != 0 {
		t.Fatalf("probe after a reconnect race exit=%d out=%s err=%s", code, out, errOut)
	}
	if n := f.countArgv("[open] [https://app.example.test/account]"); n != 2 {
		t.Fatalf("open attempts = %d, want 2 (one refused, one retried)", n)
	}
}

func TestProbeMarkerAbsentExitsThree(t *testing.T) {
	f := newHealFixture(t, false)
	code, out, _ := f.cli("probe", "--profile", fakeProfileID, "--json")
	if code != 3 {
		t.Fatalf("probe exit = %d, want 3 (out=%s)", code, out)
	}
	last, _ := f.registryRow()["last_probe"].(map[string]any)
	if last == nil || last["logged_in"] != false {
		t.Fatalf("registry last_probe = %#v", last)
	}
}

func TestProbeRoleAndURLMarkersMustAllMatch(t *testing.T) {
	f := newHealFixture(t, false)
	f.writeRegistry(map[string]any{"logged_in_marker": map[string]any{
		"role": "button", "name": "Sign out", "url_pattern": `^https://app\.example\.test/account$`,
	}})
	f.snapshot(0, map[string][2]string{"e1": {"heading", "Account"}, "e2": {"button", "Sign out"}})
	if code, out, _ := f.cli("probe", "--profile", fakeProfileID); code != 0 {
		t.Fatalf("role+url marker present: exit %d out=%s", code, out)
	}
	f.state("url-in", "https://app.example.test/login?next=account")
	f.state("logged_in", "")
	if code, _, _ := f.cli("probe", "--profile", fakeProfileID); code != 3 {
		t.Fatalf("url marker mismatch must read logged out, exit %d", code)
	}
}

func TestProbeRefusesUnenrolledProfileWithoutLaunching(t *testing.T) {
	f := newHealFixture(t, false)
	f.writeRegistry(map[string]any{"user_data_dir": filepath.Join(f.home, "never-enrolled")})
	code, out, errOut := f.cli("probe", "--profile", fakeProfileID, "--json")
	if code != 2 || !strings.Contains(out+errOut, "profile_not_enrolled") {
		t.Fatalf("unenrolled probe exit=%d out=%s err=%s", code, out, errOut)
	}
	if len(f.argvLines()) != 0 {
		t.Fatalf("unenrolled profile launched a browser: %v", f.argvLines())
	}
	if _, err := os.Stat(filepath.Join(f.home, "never-enrolled")); !os.IsNotExist(err) {
		t.Fatalf("probe created the unenrolled user_data_dir")
	}
}

func TestProbeWithoutMarkerConfigIsAnError(t *testing.T) {
	f := newHealFixture(t, false)
	f.writeRegistry(map[string]any{"logged_in_marker": nil})
	code, out, errOut := f.cli("probe", "--profile", fakeProfileID)
	if code != 2 || !strings.Contains(out+errOut, "logged_in_marker") {
		t.Fatalf("missing marker exit=%d out=%s err=%s", code, out, errOut)
	}
}

// --- recipes + run ---------------------------------------------------------

func saveDraftRecipe(f *healFixture) {
	f.writeRecipe("save-draft", []map[string]any{
		{"action": "navigate", "descriptor": map[string]any{"url": "https://app.example.test/{{page}}"}},
		{"action": "click", "descriptor": map[string]any{"role": "button", "name": "Save draft"}, "cached_ref": "e12"},
	})
}

func TestRunHealsDriftedRefAndUpdatesCachedRef(t *testing.T) {
	f := newHealFixture(t, false)
	saveDraftRecipe(f)
	// First snapshot still lists e12, but the element went stale; the
	// re-snapshot shows the same button under e47.
	f.snapshot(1, map[string][2]string{"e12": {"button", "Save draft"}})
	f.snapshot(0, map[string][2]string{"e3": {"heading", "Drafts"}, "e47": {"button", "Save draft"}})
	f.state("stale", "e12\n")
	code, out, errOut := f.cli("run", "--profile", fakeProfileID, "--recipe", "save-draft", "--param", "page=drafts", "--json")
	if code != 0 {
		t.Fatalf("run exit = %d (out=%s err=%s)", code, out, errOut)
	}
	var receipt runReceipt
	decodeJSONOut(t, out, &receipt)
	if receipt.Status != "ok" || len(receipt.Steps) != 2 {
		t.Fatalf("receipt = %+v", receipt)
	}
	click := receipt.Steps[1]
	if click.Outcome != "healed" || click.RefBefore != "e12" || click.RefAfter != "e47" {
		t.Fatalf("click step = %+v", click)
	}
	if receipt.Steps[0].Outcome != "cached" || receipt.HealedSteps != 1 {
		t.Fatalf("navigate/heal counts = %+v", receipt)
	}
	if f.countArgv("[open] [https://app.example.test/drafts]") != 1 {
		t.Fatalf("param not substituted into navigate: %v", f.argvLines())
	}
	if got := f.readRecipe("save-draft"); got.Steps[1].CachedRef != "e47" || got.Revision != 2 {
		t.Fatalf("recipe not updated: ref=%q revision=%d", got.Steps[1].CachedRef, got.Revision)
	}
	onDisk, err := os.ReadFile(filepath.Join(f.home, "browser", "runs", receipt.ID+".json"))
	if err != nil {
		t.Fatalf("receipt not written: %v", err)
	}
	if strings.Contains(string(onDisk), "app.example.test/drafts") {
		t.Fatalf("receipt carries the param-bound URL: %s", onDisk)
	}
}

func TestRunAcrossRunsCachedThenHealed(t *testing.T) {
	f := newHealFixture(t, false)
	saveDraftRecipe(f)
	f.snapshot(1, map[string][2]string{"e12": {"button", "Save draft"}})
	f.snapshot(0, map[string][2]string{"e47": {"button", "Save draft"}})
	var first, second runReceipt
	code, out, _ := f.cli("run", "--profile", fakeProfileID, "--recipe", "save-draft", "--param", "page=a", "--json")
	decodeJSONOut(t, out, &first)
	if code != 0 || first.Steps[1].Outcome != "cached" {
		t.Fatalf("first run exit=%d step=%+v", code, first.Steps[1])
	}
	code, out, _ = f.cli("run", "--profile", fakeProfileID, "--recipe", "save-draft", "--param", "page=a", "--json")
	decodeJSONOut(t, out, &second)
	if code != 0 || second.Steps[1].Outcome != "healed" || second.Steps[1].RefAfter != "e47" {
		t.Fatalf("second run exit=%d step=%+v", code, second.Steps[1])
	}
}

func TestRunNeverActsOnCachedRefThatNowNamesAnotherElement(t *testing.T) {
	f := newHealFixture(t, false)
	saveDraftRecipe(f)
	f.snapshot(0, map[string][2]string{"e12": {"button", "Delete account"}, "e30": {"button", "Save draft"}})
	code, out, _ := f.cli("run", "--profile", fakeProfileID, "--recipe", "save-draft", "--param", "page=a", "--json")
	var receipt runReceipt
	decodeJSONOut(t, out, &receipt)
	if code != 0 || receipt.Steps[1].Outcome != "healed" || receipt.Steps[1].RefAfter != "e30" {
		t.Fatalf("exit=%d step=%+v", code, receipt.Steps[1])
	}
	if f.countArgv("[click] [@e12]") != 0 {
		t.Fatalf("clicked the drifted cached ref: %v", f.argvLines())
	}
}

func TestRunDescriptorAbsentFailsStepAndStops(t *testing.T) {
	f := newHealFixture(t, false)
	f.writeRecipe("save-draft", []map[string]any{
		{"action": "click", "descriptor": map[string]any{"role": "button", "name": "Save draft"}, "cached_ref": "e12"},
		{"action": "press", "value": "Enter"},
	})
	f.snapshot(0, map[string][2]string{"e1": {"heading", "Nothing here"}, "e2": {"button", "Cancel"}})
	code, out, _ := f.cli("run", "--profile", fakeProfileID, "--recipe", "save-draft", "--json")
	if code != 1 {
		t.Fatalf("run exit = %d, want 1 (out=%s)", code, out)
	}
	var receipt runReceipt
	decodeJSONOut(t, out, &receipt)
	if receipt.Status != "failed" || receipt.Steps[0].Outcome != "failed" || len(receipt.Steps) != 1 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if f.countArgv("[press]") != 0 {
		t.Fatalf("ran a step after a failed one: %v", f.argvLines())
	}
	if _, err := os.Stat(filepath.Join(f.home, "browser", "runs", receipt.ID+".json")); err != nil {
		t.Fatalf("failed run left no receipt: %v", err)
	}
}

func TestRunMissingParamRefusesBeforeLaunch(t *testing.T) {
	f := newHealFixture(t, false)
	saveDraftRecipe(f)
	code, out, errOut := f.cli("run", "--profile", fakeProfileID, "--recipe", "save-draft")
	if code != 2 || !strings.Contains(out+errOut, "page") {
		t.Fatalf("exit=%d out=%s err=%s", code, out, errOut)
	}
	if len(f.argvLines()) != 0 {
		t.Fatalf("launched with an unbound param: %v", f.argvLines())
	}
}

func TestRecipesListAndShow(t *testing.T) {
	f := newHealFixture(t, false)
	saveDraftRecipe(f)
	f.writeRecipe("login", []map[string]any{{"action": "navigate", "descriptor": map[string]any{"url": "https://app.example.test/login"}}})
	code, out, _ := f.cli("recipes", "list", "--json")
	var list []recipeSummary
	decodeJSONOut(t, out, &list)
	if code != 0 || len(list) != 2 || list[0].Name != "login" || list[1].Name != "save-draft" || list[1].Steps != 2 {
		t.Fatalf("list exit=%d %+v", code, list)
	}
	code, out, _ = f.cli("recipes", "show", "--profile", fakeProfileID, "--recipe", "save-draft", "--json")
	var shown browserRecipe
	decodeJSONOut(t, out, &shown)
	if code != 0 || shown.Name != "save-draft" || shown.Steps[1].CachedRef != "e12" {
		t.Fatalf("show exit=%d %+v", code, shown)
	}
	if code, _, _ := f.cli("recipes", "show", "--profile", fakeProfileID, "--recipe", "../escape"); code != 2 {
		t.Fatalf("path-escaping recipe name accepted")
	}
}

// --- heal --------------------------------------------------------------------

func loginRecipe(f *healFixture) {
	f.writeRecipe("login", []map[string]any{
		{"action": "navigate", "descriptor": map[string]any{"url": "https://app.example.test/login"}},
		{"action": "fill", "descriptor": map[string]any{"role": "textbox", "name": "Email"}, "value": "{{secret:username}}"},
		{"action": "fill", "descriptor": map[string]any{"role": "textbox", "name": "Password"}, "value": "{{secret:password}}"},
		{"action": "click", "descriptor": map[string]any{"role": "button", "name": "Sign in"}},
	})
	f.snapshot(0, map[string][2]string{"e3": {"textbox", "Email"}, "e4": {"textbox", "Password"}, "e5": {"button", "Sign in"}})
	f.state("login-ref", "e5")
}

func readQueue(t *testing.T, home string) []returnQueueEntry {
	t.Helper()
	file, err := os.Open(filepath.Join(home, "browser", "return-queue.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var out []returnQueueEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var e returnQueueEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("queue line %q: %v", scanner.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

func TestHealLogsInThroughProviderAndReprobes(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	code, out, errOut := f.cli("heal", "--profile", fakeProfileID, "--json")
	if code != 0 {
		t.Fatalf("heal exit = %d (out=%s err=%s)", code, out, errOut)
	}
	var res healResult
	decodeJSONOut(t, out, &res)
	if res.Status != "healed" || !res.LoggedIn {
		t.Fatalf("heal result = %+v", res)
	}
	attempts, _ := f.registryRow()["heal_attempts"].([]any)
	if len(attempts) != 1 || attempts[0].(map[string]any)["result"] != "logged_in" {
		t.Fatalf("heal_attempts = %#v", attempts)
	}
	if last, _ := f.registryRow()["last_probe"].(map[string]any); last["logged_in"] != true {
		t.Fatalf("re-probe not recorded: %#v", last)
	}
	stdin := f.readState("batch-1.stdin") + f.readState("batch-2.stdin")
	if !strings.Contains(stdin, fakeSecret) {
		t.Fatalf("secret fill did not travel on agent-browser stdin")
	}
	if strings.Contains(out, fakeSecret) || strings.Contains(errOut, fakeSecret) {
		t.Fatalf("secret reached heal output")
	}
	receipts, _ := filepath.Glob(filepath.Join(f.home, "browser", "runs", "*.json"))
	for _, path := range receipts {
		body, _ := os.ReadFile(path)
		if bytes.Contains(body, []byte(fakeSecret)) || bytes.Contains(body, []byte("operator@example.test")) {
			t.Fatalf("run receipt %s carries a credential value", path)
		}
	}
	requests := strings.Split(strings.TrimSpace(f.readState("provider-requests.log")), "\n")
	if len(requests) != 1 || !strings.Contains(requests[0], `"profileName":"`+fakeProfileID+`","itemRef":"`+fakeCredentialRef+`"`) {
		t.Fatalf("want one credential.resolve for the profile and its credential_ref, got: %q", requests)
	}
}

func TestHealRecordsAttemptBeforeDrivingTheLoginPage(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	if code, out, _ := f.cli("heal", "--profile", fakeProfileID); code != 0 {
		t.Fatalf("heal exit %d: %s", code, out)
	}
	var mid struct {
		Profiles []selfHealProfile `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(f.readState("registry-at-login-click.json")), &mid); err != nil || len(mid.Profiles) != 1 {
		t.Fatalf("registry copy at login click: %v", err)
	}
	if got := mid.Profiles[0].HealAttempts; len(got) != 1 || got[0].Result != "started" {
		t.Fatalf("attempt not persisted before the login click: %+v", got)
	}
}

func TestHealRedactsCredentialEchoedInAgentBrowserError(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	f.state("echo-fill-error", "")
	code, out, errOut := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 3 || res.Status != "login_failed" || res.RunID == "" {
		t.Fatalf("exit=%d %+v", code, res)
	}
	body, err := os.ReadFile(filepath.Join(f.home, "browser", "runs", res.RunID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{string(body), out, errOut} {
		if strings.Contains(rendered, "operator@example.test") || strings.Contains(rendered, fakeSecret) {
			t.Fatalf("credential echoed by agent-browser leaked:\n%s", rendered)
		}
	}
	if !strings.Contains(string(body), "[redacted]") {
		t.Fatalf("echoed value not redacted in receipt:\n%s", body)
	}
}

func TestHealAlreadyLoggedInMakesNoAttempt(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	f.state("logged_in", "")
	code, out, _ := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 0 || res.Status != "already_logged_in" {
		t.Fatalf("exit=%d %+v", code, res)
	}
	if _, ok := f.registryRow()["heal_attempts"]; ok {
		t.Fatalf("logged-in heal recorded an attempt")
	}
	if f.readState("provider.log") != "" {
		t.Fatalf("logged-in heal read credentials")
	}
}

func TestHealThirdAttemptWithinWindowRefusedAndQueued(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	f.state("login-never", "")
	for i := 1; i <= 2; i++ {
		code, out, _ := f.cli("heal", "--profile", fakeProfileID, "--json")
		var res healResult
		decodeJSONOut(t, out, &res)
		if code != 3 || res.Status != "login_failed" {
			t.Fatalf("heal %d exit=%d %+v", i, code, res)
		}
	}
	clicksBefore := f.countArgv("[click] [@e5]")
	code, out, _ := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 3 || res.Status != "refused" || res.Reason != "heal_attempts_exhausted" {
		t.Fatalf("third heal exit=%d %+v", code, res)
	}
	if f.countArgv("[click] [@e5]") != clicksBefore {
		t.Fatalf("third heal ran the login recipe")
	}
	if attempts, _ := f.registryRow()["heal_attempts"].([]any); len(attempts) != 2 {
		t.Fatalf("heal_attempts = %d, want 2", len(attempts))
	}
	var exhausted []returnQueueEntry
	for _, e := range readQueue(t, f.home) {
		if e.Reason == "heal_attempts_exhausted" {
			exhausted = append(exhausted, e)
		}
	}
	if len(exhausted) != 1 || exhausted[0].Profile != fakeProfileID || exhausted[0].NextAction == "" || exhausted[0].At == "" {
		t.Fatalf("return queue = %+v", readQueue(t, f.home))
	}
	// A fourth refusal inside the window does not spam the queue.
	before := len(readQueue(t, f.home))
	if code, _, _ := f.cli("heal", "--profile", fakeProfileID); code != 3 {
		t.Fatalf("fourth heal exit = %d", code)
	}
	if after := len(readQueue(t, f.home)); after != before {
		t.Fatalf("queue grew on a repeated refusal: %d -> %d", before, after)
	}
}

func TestHealAttemptsOutsideWindowDoNotCount(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	old := time.Now().UTC().Add(-7 * time.Hour).Format(time.RFC3339)
	f.writeRegistry(map[string]any{"heal_attempts": []any{
		map[string]any{"at": old, "result": "login_failed"},
		map[string]any{"at": old, "result": "login_failed"},
	}})
	code, out, _ := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 0 || res.Status != "healed" {
		t.Fatalf("exit=%d %+v", code, res)
	}
}

func TestHealProviderMissingRefusesCleanly(t *testing.T) {
	f := newHealFixture(t, false)
	loginRecipe(f)
	code, out, errOut := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 3 || res.Status != "refused" || res.Reason != "credential_provider_missing" {
		t.Fatalf("exit=%d %+v err=%s", code, res, errOut)
	}
	if _, ok := f.registryRow()["heal_attempts"]; ok {
		t.Fatalf("a refusal counted as a heal attempt")
	}
	if f.countArgv("[batch]") != 0 || f.countArgv("[click]") != 0 {
		t.Fatalf("refused heal still drove the login page: %v", f.argvLines())
	}
	queue := readQueue(t, f.home)
	if len(queue) != 1 || queue[0].Reason != "credential_provider_missing" {
		t.Fatalf("queue = %+v", queue)
	}
}

func TestHealProviderFieldUnavailableBurnsNoAttempt(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	if err := os.Remove(filepath.Join(f.stateDir, "secret")); err != nil {
		t.Fatal(err)
	}
	code, out, _ := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 3 || res.Status != "refused" || res.Reason != "credential_unavailable" {
		t.Fatalf("exit=%d %+v", code, res)
	}
	if _, ok := f.registryRow()["heal_attempts"]; ok {
		t.Fatalf("a provider gap burned a heal attempt")
	}
	if f.countArgv("[batch]") != 0 || f.countArgv("[click]") != 0 {
		t.Fatalf("login page driven without every credential: %v", f.argvLines())
	}
}

func TestHealInstalledCompoundWithoutProviderVerbReadsAsMissing(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	f.state("provider-verb-absent", "")
	code, out, _ := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 3 || res.Reason != "credential_provider_missing" {
		t.Fatalf("exit=%d %+v", code, res)
	}
	if _, ok := f.registryRow()["heal_attempts"]; ok {
		t.Fatalf("a missing provider burned a heal attempt")
	}
}

func TestHealTwoFactorPromptQueuesForOperator(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	f.state("login-never", "")
	f.state("text-out", "Enter the verification code we sent")
	f.writeRegistry(map[string]any{"two_factor_marker": map[string]any{"text": "verification code"}})
	code, out, _ := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 3 || res.Status != "two_factor_required" {
		t.Fatalf("exit=%d %+v", code, res)
	}
	queue := readQueue(t, f.home)
	if len(queue) != 1 || queue[0].Reason != "two_factor_required" {
		t.Fatalf("queue = %+v", queue)
	}
}

// --- doctor ------------------------------------------------------------------

func TestSelfHealDoctorRendersWithoutValues(t *testing.T) {
	f := newHealFixture(t, true)
	loginRecipe(f)
	if code, out, _ := f.cli("heal", "--profile", fakeProfileID); code != 0 {
		t.Fatalf("heal exit %d: %s", code, out)
	}
	saveDraftRecipe(f)
	f.snapshot(0, map[string][2]string{"e47": {"button", "Save draft"}})
	if code, out, _ := f.cli("run", "--profile", fakeProfileID, "--recipe", "save-draft", "--param", "page=private-param-value"); code != 0 {
		t.Fatalf("run exit %d: %s", code, out)
	}
	report, err := buildSelfHealDoctor(f.home, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Profiles) != 1 {
		t.Fatalf("profiles = %+v", report.Profiles)
	}
	row := report.Profiles[0]
	if row.Profile != fakeProfileID || row.Enrollment != "enrolled" || row.LastProbe == nil || !row.LastProbe.LoggedIn ||
		row.HealAttemptsInWindow != 1 || row.Recipes != 2 || row.Runs7d != 2 || row.HealedSteps7d != 4 || row.QueueEntries != 0 {
		t.Fatalf("doctor row = %+v", row)
	}
	var text bytes.Buffer
	renderSelfHealDoctorText(&text, report)
	body, _ := json.Marshal(report)
	for _, rendered := range []string{text.String(), string(body)} {
		for _, value := range []string{fakeSecret, "operator@example.test", "private-param-value"} {
			if strings.Contains(rendered, value) {
				t.Fatalf("doctor rendered a value %q:\n%s", value, rendered)
			}
		}
	}
	if !strings.Contains(text.String(), fakeProfileID) || !strings.Contains(text.String(), "healed_7d=4") {
		t.Fatalf("doctor text:\n%s", text.String())
	}
}
