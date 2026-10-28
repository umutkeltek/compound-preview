package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/act"
	"compound/internal/home"
	"compound/internal/policy"
)

type alignmentCLIFixtureOwner struct {
	operation, work, actor, ref string
	fail                        bool
}

func (o *alignmentCLIFixtureOwner) Plan(r policy.CredentialPasswordAlignmentRequest, w, a string) (act.CredentialPasswordAlignmentPlanResult, error) {
	o.operation = "plan"
	o.work = w
	o.actor = a
	return act.CredentialPasswordAlignmentPlanResult{PlanRef: "fixture-plan-ref", PolicyOutcome: "hold"}, nil
}
func (o *alignmentCLIFixtureOwner) Execute(_ context.Context, ref, a string) (act.CredentialPasswordAlignmentResult, error) {
	o.operation = "execute"
	o.ref = ref
	o.actor = a
	if o.fail {
		return act.CredentialPasswordAlignmentResult{State: "failed", ApprovalConsumed: true}, errors.New("FIXTURE_RAW_DIAGNOSTIC")
	}
	return act.CredentialPasswordAlignmentResult{State: "completed"}, nil
}

func TestCredentialPasswordAlignmentCLIDispatchesOnlyPlanOrExecuteToNativeOwner(t *testing.T) {
	h := t.TempDir()
	path := filepath.Join(h, "metadata.json")
	if e := os.WriteFile(path, []byte(`{"roles":[{},{}],"catalog":{"roles":[{},{}]}}`), 0600); e != nil {
		t.Fatal(e)
	}
	old := newCredentialPasswordAlignmentOwner
	defer func() { newCredentialPasswordAlignmentOwner = old }()
	for _, op := range []string{"plan", "execute"} {
		fixture := &alignmentCLIFixtureOwner{}
		newCredentialPasswordAlignmentOwner = func(home string) (credentialPasswordAlignmentOwner, error) {
			if home != h {
				t.Fatal("wrong explicit home")
			}
			return fixture, nil
		}
		args := []string{op, "--home", h, "--actor", "fixture", "--json"}
		if op == "plan" {
			args = append(args, "--work", "work_fixture", "--request-file", path)
		} else {
			args = append(args, "--plan-ref", "fixture-plan-ref")
		}
		var stdout, stderr bytes.Buffer
		if code := RunCredentialPasswordAlignment(args, &stdout, &stderr); code != 0 || stderr.Len() != 0 || fixture.operation != op || fixture.actor != "fixture" {
			t.Fatalf("dispatch %s code=%d stderr=%s", op, code, stderr.String())
		}
		if op == "plan" && !strings.Contains(stdout.String(), `"policy_outcome":"hold"`) {
			t.Fatal("hold plan not reviewable")
		}
	}
}

func TestCredentialPasswordAlignmentCLIGlobalHomeSurvivesDispatch(t *testing.T) {
	t.Setenv("COMPOUND_HOME", t.TempDir())
	h := t.TempDir()
	path := filepath.Join(h, "metadata.json")
	if err := os.WriteFile(path, []byte(`{"roles":[{},{}],"catalog":{"roles":[{},{}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	old := newCredentialPasswordAlignmentOwner
	defer func() { newCredentialPasswordAlignmentOwner = old }()
	for _, op := range []string{"plan", "execute"} {
		fixture := &alignmentCLIFixtureOwner{}
		newCredentialPasswordAlignmentOwner = func(selected string) (credentialPasswordAlignmentOwner, error) {
			if selected != h {
				t.Fatalf("owner selected %q, want global home %q", selected, h)
			}
			return fixture, nil
		}
		args := []string{"act", "credential-password-align", op, "--home", h, "--actor", "fixture", "--json"}
		if op == "plan" {
			args = append(args, "--work", "work_fixture", "--request-file", path)
		} else {
			args = append(args, "--plan-ref", "fixture-plan-ref")
		}
		prepared, err := home.PrepareGlobalCLIArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		for _, arg := range prepared {
			if arg == "--home" {
				t.Fatal("global parser did not consume home option")
			}
		}
		var stdout, stderr bytes.Buffer
		if code := RunCredentialPasswordAlignment(prepared[2:], &stdout, &stderr); code != 0 || stderr.Len() != 0 || fixture.operation != op {
			t.Fatalf("global %s dispatch code=%d stderr=%s", op, code, stderr.String())
		}
	}
}

func TestCredentialPasswordAlignmentCLIUnknownDuplicateOrCrossPhaseOptionsNeverConstructOwner(t *testing.T) {
	old := newCredentialPasswordAlignmentOwner
	defer func() { newCredentialPasswordAlignmentOwner = old }()
	newCredentialPasswordAlignmentOwner = func(string) (credentialPasswordAlignmentOwner, error) {
		t.Fatal("invalid CLI reached owner")
		return nil, nil
	}
	for _, args := range [][]string{
		{}, {"--help"}, {"execute", "--plan-ref", "ref"},
		{"execute", "--home", "/fixture", "--home", "/fixture/other", "--actor", "fixture", "--plan-ref", "ref"},
		{"execute", "--home", "/fixture", "--actor", "fixture", "--plan-ref", "ref", "--program", "arbitrary"},
		{"execute", "--home", "/fixture", "--actor", "fixture", "--plan-ref", "ref", "--request-file", "/fixture/meta.json"},
		{"execute", "--home", "/fixture", "--actor", "fixture", "--plan-ref", "ref", "--actor", "other"},
		{"execute", "--home", "relative", "--actor", "fixture", "--plan-ref", "ref"},
		{"plan", "--home", "/fixture", "--actor", "fixture", "--work", "work_fixture", "--request-file", "/fixture/.env"},
		{"plan", "--home", "/fixture", "--actor", "fixture", "--work", "work_fixture", "--request-file", "/fixture/metadata.json", "--confirm"},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunCredentialPasswordAlignment(args, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
			t.Fatal("invalid CLI accepted")
		}
	}
}

func TestCredentialPasswordAlignmentCLIRefusesSymlinkUnknownCredentialAndDuplicateJSONWithoutEcho(t *testing.T) {
	h := t.TempDir()
	sentinel := "FIXTURE_RAW_CREDENTIAL"
	old := newCredentialPasswordAlignmentOwner
	defer func() { newCredentialPasswordAlignmentOwner = old }()
	newCredentialPasswordAlignmentOwner = func(string) (credentialPasswordAlignmentOwner, error) {
		t.Fatal("invalid manifest reached owner")
		return nil, nil
	}
	for index, raw := range []string{`{"password":"` + sentinel + `"}`, `{"profile_id":"one","profile_id":"two"}`, strings.Repeat("x", 32769)} {
		path := filepath.Join(h, "metadata.json")
		if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		if index == 2 {
			alias := filepath.Join(h, "alias.json")
			if e := os.Symlink(path, alias); e != nil {
				t.Fatal(e)
			}
			path = alias
		}
		var stdout, stderr bytes.Buffer
		if code := RunCredentialPasswordAlignment([]string{"plan", "--home", h, "--actor", "fixture", "--work", "work_fixture", "--request-file", path}, &stdout, &stderr); code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), sentinel) {
			t.Fatal("manifest refused unsafely")
		}
	}
}

func TestCredentialPasswordAlignmentCLIPostAdmissionFailureRendersOnlySanitizedTypedResult(t *testing.T) {
	old := newCredentialPasswordAlignmentOwner
	defer func() { newCredentialPasswordAlignmentOwner = old }()
	newCredentialPasswordAlignmentOwner = func(string) (credentialPasswordAlignmentOwner, error) {
		return &alignmentCLIFixtureOwner{fail: true}, nil
	}
	var stdout, stderr bytes.Buffer
	code := RunCredentialPasswordAlignment([]string{"execute", "--home", t.TempDir(), "--actor", "fixture", "--plan-ref", "fixture-ref"}, &stdout, &stderr)
	if code != 1 || strings.Contains(stdout.String()+stderr.String(), "FIXTURE_RAW_DIAGNOSTIC") || !strings.Contains(stdout.String(), `"approval_consumed":true`) || !strings.Contains(stdout.String(), `"migration_accepted":false`) {
		t.Fatal("failure rendering leaked or lost native state")
	}
}
