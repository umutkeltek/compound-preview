package home

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRepairPrivateCustodyCreatesMissingDirs(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "global", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(homePath, "private")); err != nil {
		t.Fatal(err)
	}

	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{})
	if !report.OK || !report.Changed || report.Counts.Manual != 0 {
		t.Fatalf("repair report = %+v", report)
	}
	for _, rel := range privateCustodyKnownDirs {
		info, err := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode/type = %v %04o", rel, info.Mode(), info.Mode().Perm())
		}
	}
}

func TestRunPrivateCustodyCLISelectsLocalHome(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	ambientHome := filepath.Join(t.TempDir(), ".compound")
	if err := Init(ambientHome, "ambient-profile", time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", ambientHome)
	localHome := filepath.Join(workspace, ".compound")
	if err := Init(localHome, "local-profile", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(localHome, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := RunPrivateCustodyCLI([]string{"private-custody", "repair", "--local"}, &stdout, &stderr); code != 0 {
		t.Fatalf("private-custody --local exit=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if info, err := os.Lstat(localHome); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("local home mode=%v err=%v, want 0700", info, err)
	}
}

func TestRepairPrivateCustodyFixesDriftThenNoop(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	body := []byte("private artifact")
	digest := writePrivateObject(t, homePath, body, 0o644)
	for _, rel := range privateCustodyKnownDirs {
		if err := os.Chmod(filepath.Join(homePath, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{})
	if !report.OK || !report.Changed || report.Counts.Repaired == 0 || report.Counts.Manual != 0 {
		t.Fatalf("repair report = %+v", report)
	}
	if report.Counts.Drift != 0 {
		t.Fatalf("successful repair retained drift count: %+v", report)
	}
	for _, finding := range report.Findings {
		if finding.Status == "drift" || finding.Status == "manual" {
			t.Fatalf("successful repair retained unresolved finding: %+v", finding)
		}
	}
	for _, rel := range privateCustodyKnownDirs {
		info, err := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel)))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("dir %s mode after repair = %v %04o err=%v", rel, info.Mode(), info.Mode().Perm(), err)
		}
	}
	info, err := os.Lstat(filepath.Join(homePath, "private", "artifacts", "sha256", digest))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("object mode after repair = %v %04o err=%v", info.Mode(), info.Mode().Perm(), err)
	}

	again := RepairPrivateCustody(homePath, PrivateCustodyOptions{})
	if !again.OK || again.Changed || again.Counts.Repaired != 0 || again.Status != "ok" {
		t.Fatalf("second repair = %+v", again)
	}
}

func TestInitCreatesPrivateHomeRootIndependentOfUmask(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "global", time.Now()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("fresh home root mode = %04o, want 0700", info.Mode().Perm())
	}
}

func TestInitSecuresExistingEmptyHomeRoot(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o777} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			homePath := t.TempDir()
			if err := os.Chmod(homePath, mode); err != nil {
				t.Fatal(err)
			}
			if err := Init(homePath, "global", time.Now()); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(homePath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o700 {
				t.Fatalf("secured existing empty root mode = %04o, want 0700", info.Mode().Perm())
			}
		})
	}
}

func TestInitRefusesPopulatedPermissiveHomeRoot(t *testing.T) {
	homePath := t.TempDir()
	marker := filepath.Join(homePath, "existing")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(homePath, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Init(homePath, "global", time.Now())
	if err == nil || !strings.Contains(err.Error(), "existing populated home root must be mode 0700") {
		t.Fatalf("populated permissive home init err = %v", err)
	}
	info, statErr := os.Lstat(homePath)
	if statErr != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("populated root mode changed: mode=%v err=%v", info.Mode(), statErr)
	}
	got, readErr := os.ReadFile(marker)
	if readErr != nil || string(got) != "keep" {
		t.Fatalf("populated root marker changed: body=%q err=%v", got, readErr)
	}
}

func TestInitKeepsOnePinnedRootThroughScaffolding(t *testing.T) {
	parent := t.TempDir()
	homePath := filepath.Join(parent, "home")
	if err := os.Mkdir(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-home")
	replacementMarker := filepath.Join(homePath, "replacement")
	oldHook := initAfterRootSecured
	t.Cleanup(func() { initAfterRootSecured = oldHook })
	initAfterRootSecured = func(string) error {
		if err := os.Rename(homePath, moved); err != nil {
			return err
		}
		if err := os.Mkdir(homePath, 0o700); err != nil {
			return err
		}
		return os.WriteFile(replacementMarker, []byte("keep"), 0o600)
	}
	err := Init(homePath, "global", time.Now())
	if err == nil || !strings.Contains(err.Error(), "identity changed during initialization") {
		t.Fatalf("root replacement init err = %v", err)
	}
	got, readErr := os.ReadFile(replacementMarker)
	if readErr != nil || string(got) != "keep" {
		t.Fatalf("replacement marker changed: body=%q err=%v", got, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(homePath, ManifestName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("replacement received manifest: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(moved, ManifestName)); statErr != nil {
		t.Fatalf("pinned original did not receive scaffold before drift refusal: %v", statErr)
	}
}

func TestRepairPrivateCustodyDiagnosesAndRepairsHomeRootMode(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "global", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(homePath, 0o755); err != nil {
		t.Fatal(err)
	}
	diagnosis := DiagnosePrivateCustody(homePath)
	if diagnosis.OK || diagnosis.Status != "drift" {
		t.Fatalf("0755 home root diagnosis = %+v", diagnosis)
	}
	if got := findPrivateCustodyFinding(t, diagnosis.Findings, "home_root"); got.Expected != "mode=0700" || got.Actual != "mode=0755" || got.Status != "drift" {
		t.Fatalf("home root finding = %+v", got)
	}
	repair := RepairPrivateCustody(homePath, PrivateCustodyOptions{})
	if !repair.OK || !repair.Changed || repair.Counts.Repaired == 0 {
		t.Fatalf("home root repair = %+v", repair)
	}
	info, err := os.Lstat(homePath)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("home root after repair mode=%v err=%v", info.Mode(), err)
	}
}

func findPrivateCustodyFinding(t *testing.T, findings []PrivateCustodyFinding, kind string) PrivateCustodyFinding {
	t.Helper()
	for _, finding := range findings {
		if finding.Kind == kind {
			return finding
		}
	}
	t.Fatalf("missing private custody finding kind %s: %+v", kind, findings)
	return PrivateCustodyFinding{}
}

func TestInitCreatesMissingPrivateCustodyWithoutRepairingExistingDrift(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	privatePath := filepath.Join(homePath, "private")
	if err := Init(homePath, HomeKindGlobal, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(privatePath, "artifacts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(privatePath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Init(homePath, "global", time.Now()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("home init silently repaired existing private mode: got %04o want 0755", got)
	}
	for _, rel := range []string{"private/artifacts", "private/artifacts/sha256"} {
		info, err := os.Lstat(filepath.Join(homePath, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("new %s mode = %04o, want 0700", rel, got)
		}
	}
}

func TestInitRejectsSymlinkedHomeRootWithoutCreatingPrivateCustody(t *testing.T) {
	parent := t.TempDir()
	realHome := filepath.Join(parent, "real-home")
	if err := os.Mkdir(realHome, 0o755); err != nil {
		t.Fatal(err)
	}
	linkHome := filepath.Join(parent, "link-home")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Fatal(err)
	}
	if err := Init(linkHome, "global", time.Now()); err == nil {
		t.Fatal("home init accepted symlinked root")
	}
	if _, err := os.Lstat(filepath.Join(realHome, "private")); !os.IsNotExist(err) {
		t.Fatalf("home init mutated symlink target private custody: %v", err)
	}
}

func TestInitRefusesUnsafePrivateAncestorWithoutRepairingOrDescending(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	privatePath := filepath.Join(homePath, "private")
	if err := os.MkdirAll(privatePath, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(privatePath, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := Init(homePath, "global", time.Now()); err == nil {
		t.Fatal("home init descended through group/world-writable private ancestor")
	}
	info, err := os.Lstat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o777 {
		t.Fatalf("home init silently repaired unsafe private ancestor: %04o", got)
	}
	if _, err := os.Lstat(filepath.Join(privatePath, "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("home init created beneath unsafe private ancestor: %v", err)
	}
}

func TestRepairPrivateCustodyRejectsSymlinkedHomeRoot(t *testing.T) {
	parent := t.TempDir()
	realHome := filepath.Join(parent, "real-home")
	writePrivateObject(t, realHome, []byte("root symlink"), 0o600)
	privatePath := filepath.Join(realHome, "private")
	if err := os.Chmod(privatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	linkHome := filepath.Join(parent, "link-home")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Fatal(err)
	}

	report := RepairPrivateCustody(linkHome, PrivateCustodyOptions{})
	if report.OK || report.Counts.Manual == 0 {
		t.Fatalf("symlinked home root was accepted: %+v", report)
	}
	info, err := os.Lstat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("symlinked home repair mutated target mode to %04o", got)
	}
}

func TestRepairPrivateCustodyRejectsRootReplacement(t *testing.T) {
	parent := t.TempDir()
	homePath := filepath.Join(parent, "home")
	writePrivateObject(t, homePath, []byte("root replacement"), 0o600)
	if err := os.Chmod(filepath.Join(homePath, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	movedHome := filepath.Join(parent, "moved-home")

	report := privateCustodyWithHooks(homePath, PrivateCustodyOptions{Repair: true}, privateCustodyHooks{
		beforeRootOpen: func() {
			if err := os.Rename(homePath, movedHome); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(movedHome, homePath); err != nil {
				t.Fatal(err)
			}
		},
	})
	if report.OK || report.Counts.Manual == 0 {
		t.Fatalf("replaced home root was accepted: %+v", report)
	}
	info, err := os.Lstat(filepath.Join(movedHome, "private"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("root replacement mutated original tree mode to %04o", got)
	}
}

func TestRepairPrivateCustodyRejectsDigestReplacementBeforeChmod(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	body := []byte("digest replacement")
	digest := writePrivateObject(t, homePath, body, 0o644)
	rel := filepath.Join("private", "artifacts", "sha256", digest)
	path := filepath.Join(homePath, rel)
	original := path + ".original"

	report := privateCustodyWithHooks(homePath, PrivateCustodyOptions{Repair: true}, privateCustodyHooks{
		beforeChmodOpen: func(gotRel string) {
			if gotRel != rel {
				return
			}
			if err := os.Rename(path, original); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	})
	if report.OK || report.Counts.Manual == 0 {
		t.Fatalf("digest replacement was accepted: %+v", report)
	}
	for _, checkPath := range []string{path, original} {
		info, err := os.Lstat(checkPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Fatalf("replacement race chmodded %s to %04o", checkPath, got)
		}
	}
}

func TestPrivateCustodyJSONUsesStableSnakeCaseKeys(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	writePrivateObject(t, homePath, []byte("json keys"), 0o644)
	report := DiagnosePrivateCustody(homePath)
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, forbidden := range []string{`"Checked"`, `"ID"`, `"Path"`, `"Expected"`, `"Detail"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("private custody JSON contains unstable key %s: %s", forbidden, text)
		}
	}
	for _, required := range []string{`"checked"`, `"id"`, `"path"`, `"expected"`, `"detail"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("private custody JSON missing %s: %s", required, text)
		}
	}
}

func TestPrivateCustodyOwnershipIgnoresUIDEnvironment(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	writePrivateObject(t, homePath, []byte("real uid"), 0o600)
	t.Setenv("UID", strconv.Itoa(os.Getuid()+10000))
	report := DiagnosePrivateCustody(homePath)
	if !report.OK || report.Counts.Manual != 0 {
		t.Fatalf("spoofed UID environment changed ownership result: %+v", report)
	}
}

func TestPrivateCustodyTextReportIsConsistentAfterRepair(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	writePrivateObject(t, homePath, []byte("text report"), 0o644)
	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{})
	var output bytes.Buffer
	if err := WritePrivateCustodyReport(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, required := range []string{"private_custody: ok", "private_custody.status: repaired", "drift=0", "manual=0"} {
		if !strings.Contains(text, required) {
			t.Fatalf("successful text report missing %q:\n%s", required, text)
		}
	}
	if strings.Contains(text, " drift ") || strings.Contains(text, " manual ") {
		t.Fatalf("successful text report retained unresolved finding:\n%s", text)
	}
}

func TestRepairPrivateCustodyManualRefusals(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	root := filepath.Join(homePath, "private", "artifacts", "sha256")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, validDigestFor([]byte("symlink")))); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, validDigestFor([]byte("dir"))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "BADNAME"), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{})
	if report.OK || report.Counts.Manual < 3 || report.Changed {
		t.Fatalf("manual refusal report = %+v", report)
	}
	for _, f := range report.Findings {
		if f.Status == "manual" && f.Action != "manual" {
			t.Fatalf("manual finding must stay manual: %+v", f)
		}
	}
}

func TestRepairPrivateCustodyLeavesPublicObjectsUntouched(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("legacy public")
	ref, err := store.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ArtifactHex(ref)
	if err != nil {
		t.Fatal(err)
	}
	publicRoot := filepath.Join(homePath, "artifacts", "sha256")
	publicPath := filepath.Join(publicRoot, digest)

	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{})
	info, err := os.Lstat(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("public object mode changed to %04o", info.Mode().Perm())
	}
	if report.Counts.Manual != 0 {
		t.Fatalf("public artifacts must stay outside private-custody findings: %+v", report)
	}
	for _, finding := range report.Findings {
		if finding.Path == publicPath || finding.Kind == "legacy_public_artifact" {
			t.Fatalf("public artifact was misclassified as private-custody drift: %+v", finding)
		}
	}
}

func TestRepairPrivateCustodyRejectsHardLinkedDigestObject(t *testing.T) {
	parent := t.TempDir()
	homePath := filepath.Join(parent, ".compound")
	root := filepath.Join(homePath, "private", "artifacts", "sha256")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("hard-linked private object")
	digest := validDigestFor(body)
	external := filepath.Join(parent, "external")
	if err := os.WriteFile(external, body, 0o644); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(root, digest)
	if err := os.Link(external, objectPath); err != nil {
		t.Skipf("hard links unavailable on this filesystem: %v", err)
	}

	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{})
	if report.OK || report.Changed || report.Counts.Manual == 0 {
		t.Fatalf("hard-linked private object was accepted or mutated: %+v", report)
	}
	for _, path := range []string{external, objectPath} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Fatalf("repair chmodded hard-linked path %s to %04o", path, got)
		}
	}
}

func TestRepairPrivateCustodyDryRun(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	writePrivateObject(t, homePath, []byte("dry run"), 0o644)
	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{DryRun: true})
	if report.Changed || len(report.Actions) == 0 || report.Actions[0].Status != "dry_run" {
		t.Fatalf("dry-run report = %+v", report)
	}
}

func writePrivateObject(t *testing.T, homePath string, body []byte, mode os.FileMode) string {
	t.Helper()
	root := filepath.Join(homePath, "private", "artifacts", "sha256")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := validDigestFor(body)
	if err := os.WriteFile(filepath.Join(root, digest), body, mode); err != nil {
		t.Fatal(err)
	}
	return digest
}

func validDigestFor(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
