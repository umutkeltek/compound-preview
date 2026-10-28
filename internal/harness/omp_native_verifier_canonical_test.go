package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestOMPCanonicalValidatorExactBodiesAndObserverWindow(t *testing.T) {
	binding, err := resolveOMPVerifierInterpreter("")
	if err != nil {
		t.Fatal(err)
	}
	const prompt, cwd = "literal <&> prompt", "/fixture/work"
	start := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC).UnixMilli()
	midnight := time.Date(2026, 9, 10, 23, 59, 59, 0, time.UTC).UnixMilli()
	reminder := func(date, path string) string {
		return fmt.Sprintf("<system-reminder>\nToday: %s; current working directory: '%s'. Do not repeat this information in your reply.\n</system-reminder>", date, path)
	}
	canonical := reminder("2026-09-10", cwd)
	block := func(text string) map[string]any { return map[string]any{"type": "text", "text": text} }
	user := func(content any) map[string]any { return map[string]any{"role": "user", "content": content} }
	array := func(r, p string) []any { return []any{block(r), block(p)} }
	one := func(content any) []any { return []any{user(content)} }
	system := map[string]any{"role": "system", "content": "fixture system"}
	type testCase struct {
		Name     string `json:"name"`
		Messages any    `json:"messages"`
		Start    int64  `json:"start"`
		Request  int64  `json:"request"`
		Want     bool   `json:"want"`
	}
	var cases []testCase
	add := func(name string, messages any, want bool) {
		cases = append(cases, testCase{name, messages, start, start + 1000, want})
	}
	add("exact_string", one(canonical+"\n\n"+prompt), true)
	add("exact_blocks", one(array(canonical, prompt)), true)
	add("leading_system", []any{system, user(array(canonical, prompt))}, true)
	add("raw_string", one(prompt), false)
	add("raw_array", one([]any{block(prompt)}), false)
	add("yesterday_same_day", one(array(reminder("2026-09-09", cwd), prompt)), false)
	add("tomorrow_same_day", one(array(reminder("2026-09-11", cwd), prompt)), false)
	add("wrong_cwd", one(array(reminder("2026-09-10", "/wrong"), prompt)), false)
	add("prompt_suffix", one(array(canonical, prompt+" extra")), false)
	add("prompt_prefix", one(array(canonical, "extra "+prompt)), false)
	add("wrong_separator", one(canonical+"\n"+prompt), false)
	add("extra_user", []any{user(array(canonical, prompt)), user("extra")}, false)
	add("extra_block", one([]any{block(canonical), block(prompt), block("extra")}), false)
	add("extra_block_key", one([]any{block(canonical), map[string]any{"type": "text", "text": prompt, "extra": true}}), false)
	add("reversed_blocks", one(array(prompt, canonical)), false)
	add("extra_system", []any{system, system, user(array(canonical, prompt))}, false)
	add("trailing_system", []any{user(array(canonical, prompt)), system}, false)
	add("invalid_system_content", []any{map[string]any{"role": "system", "content": 42}, user(array(canonical, prompt))}, false)
	for _, role := range []string{"assistant", "developer", "tool"} {
		add("extra_"+role, []any{map[string]any{"role": role, "content": "extra"}, user(array(canonical, prompt))}, false)
	}
	add("null_messages", nil, false)
	add("null_message", []any{nil}, false)
	add("empty_messages", []any{}, false)
	cases = append(cases,
		testCase{"midnight_before", one(array(canonical, prompt)), midnight, midnight + 2000, true},
		testCase{"midnight_after", one(array(reminder("2026-09-11", cwd), prompt)), midnight, midnight + 2000, true},
		testCase{"midnight_unobserved_day", one(array(reminder("2026-09-12", cwd), prompt)), midnight, midnight + 2000, false},
		testCase{"backward_clock", one(array(canonical, prompt)), start, start - 1, false},
		testCase{"over_deadline", one(array(canonical, prompt)), start, start + 30001, false},
	)
	payload, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	encodedPrompt, _ := json.Marshal(prompt)
	encodedCWD, _ := json.Marshal(cwd)
	code := ompCanonicalValidatorJS + "\nconst cases=" + string(payload) + ";const p=" + string(encodedPrompt) + ";const cwd=" + string(encodedCWD) + ";process.stdout.write(JSON.stringify(cases.map(c=>({name:c.name,ok:ompCanonicalValidateUserMessages(c.messages,p,cwd,c.start,c.request).ok}))));"
	cmd := exec.Command(binding.Path, "-e", code)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("production JS validator: %v; stderr=%s", err, stderr.String())
	}
	var got []struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("validator JSON: %v; output=%s", err, out)
	}
	if len(got) != len(cases) {
		t.Fatalf("results=%d, cases=%d", len(got), len(cases))
	}
	for i, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got[i].Name != tc.Name || got[i].OK != tc.Want {
				t.Fatalf("got %+v, want ok=%v", got[i], tc.Want)
			}
		})
	}
	// The production date formatter must use the child's local date, not UTC.
	at := time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC).UnixMilli()
	localMessages, _ := json.Marshal(one(array(canonical, prompt)))
	localCode := ompCanonicalValidatorJS + fmt.Sprintf("\nprocess.stdout.write(String(ompCanonicalValidateUserMessages(%s,%s,%s,%d,%d).ok));", localMessages, encodedPrompt, encodedCWD, at, at+1000)
	localCmd := exec.Command(binding.Path, "-e", localCode)
	localCmd.Env = append(os.Environ(), "TZ=America/Los_Angeles")
	if out, err := localCmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("local calendar control: %v; %s", err, out)
	}
}

func TestOMPCanonicalNativeCWDNormalization(t *testing.T) {
	binding, err := resolveOMPVerifierInterpreter("")
	if err != nil {
		t.Fatal(err)
	}
	code := ompCanonicalValidatorJS + `
const a=require("node:assert/strict");
const same = p => "/real/work";
const different = p => p;
const missing = p => { throw Error("missing"); };
a.equal(ompCanonicalNativeCwd("/private/var/work","darwin",same),"/var/work");
a.equal(ompCanonicalNativeCwd("/private/var/work","darwin",different),"/private/var/work");
a.equal(ompCanonicalNativeCwd("/private/var/work","darwin",missing),"/private/var/work");
a.equal(ompCanonicalNativeCwd("/private/var/work","linux",same),"/private/var/work");
a.equal(ompCanonicalNativeCwd("/privateer/work","darwin",same),"/privateer/work");
a.equal(ompCanonicalNativeCwd("/fixture/work","darwin",same),"/fixture/work");
a.equal(ompCanonicalNativeCwd("C:\\\\fixture\\\\work","win32",same),"C://fixture//work");
process.stdout.write("cwd-normalization: ok");
`
	if out, err := exec.Command(binding.Path, "-e", code).CombinedOutput(); err != nil {
		t.Fatalf("production CWD normalization: %v: %s", err, out)
	}
}
