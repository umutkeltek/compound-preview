package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
)

// Exercise the public recovery doors, including format selection. A v1 archive
// that merely passes its own roundtrip must not stand in for the streamed path.
func TestHomeStreamedArchivePublicRoundTrip(t *testing.T) {
	t.Parallel()
	bin := compoundTestBinary(t)
	source := t.TempDir()
	if err := home.Init(source, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	want := []byte("synthetic recovery consumer\n")
	rel := filepath.Join("config", "streamed-fixture.txt")
	if err := os.WriteFile(filepath.Join(source, rel), want, 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "home.tar.gz")
	runStreamedHomeCLI(t, bin, source, true, "home", "backup", "--format", "streamed-v2", "--target", archive)
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "compound-home-archive-header.json" || header.Size > 4096 {
		t.Fatalf("streamed profile was not selected: first header = %+v", header)
	}
	data, err := io.ReadAll(io.LimitReader(reader, 4097))
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		SchemaVersion string `json:"schema_version"`
		Kind          string `json:"kind"`
	}
	if err := json.Unmarshal(data, &marker); err != nil || marker.SchemaVersion != "compound.home_archive.v0.2" || marker.Kind != "backup" {
		t.Fatalf("unexpected streamed descriptor: %s, error: %v", data, err)
	}
	runStreamedHomeCLI(t, bin, source, true, "home", "verify", archive)
	// The original body is unavailable; restore must consume archived bytes.
	if err := os.Rename(filepath.Join(source, rel), filepath.Join(t.TempDir(), "retained-source.txt")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	runStreamedHomeCLI(t, bin, source, true, "home", "restore", archive, "--target", target)
	got, err := os.ReadFile(filepath.Join(target, rel))
	if err != nil || string(got) != string(want) {
		t.Fatalf("restored body = %q, error: %v", got, err)
	}
	runStreamedHomeCLI(t, bin, target, true, "home", "doctor", "--json")
	runStreamedHomeCLI(t, bin, target, true, "workbench", "--json")
	runStreamedHomeCLI(t, bin, source, false, "home", "restore", archive, "--target", target)
	got, err = os.ReadFile(filepath.Join(target, rel))
	if err != nil || string(got) != string(want) {
		t.Fatalf("refused overwrite changed restored body = %q, error: %v", got, err)
	}
}

func TestHomeStreamedLinkedArchivePublicRoundTrip(t *testing.T) {
	t.Parallel()
	bin := compoundTestBinary(t)
	source := filepath.Join(t.TempDir(), "home")
	if err := home.Init(source, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	bodyPath := filepath.Join(source, "config", "retained.txt")
	if err := os.WriteFile(bodyPath, []byte("restored consumer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bodyPath, filepath.Join(source, "config", "latest")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "linked.tar.gz")
	runStreamedHomeCLI(t, bin, source, true, "home", "backup", "--format", "streamed-v3", "--target", archive)
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	header, err := reader.Next()
	if err != nil || header.Name != "compound-home-archive-header.json" || header.Size > 4096 {
		t.Fatalf("v3 header: %v, %v", header, err)
	}
	data, err := io.ReadAll(io.LimitReader(reader, 4097))
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &marker); err != nil || marker.SchemaVersion != "compound.home_archive.v0.3" {
		t.Fatalf("v3 not selected: %s, %v", data, err)
	}
	runStreamedHomeCLI(t, bin, source, true, "home", "verify", archive)
	if err := os.Rename(source, source+"-unavailable"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	runStreamedHomeCLI(t, bin, target, true, "home", "restore", archive, "--target", target)
	link := filepath.Join(target, "config", "latest")
	if got, err := os.Readlink(link); err != nil || got != "retained.txt" {
		t.Fatalf("portable link = %q, %v", got, err)
	}
	if got, err := os.ReadFile(link); err != nil || string(got) != "restored consumer\n" {
		t.Fatalf("restored consumer = %q, %v", got, err)
	}
	runStreamedHomeCLI(t, bin, target, true, "home", "doctor", "--json")
	runStreamedHomeCLI(t, bin, target, true, "workbench", "--json")
}

func TestHomeBackupUnknownFormatRefusesPublication(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := home.Init(source, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "must-not-exist.tar.gz")
	out := runStreamedHomeCLI(t, compoundTestBinary(t), source, false, "home", "backup", "--format", "future-unreviewed", "--target", archive)
	if !strings.Contains(strings.ToLower(out), "format") {
		t.Fatalf("refusal does not explain format: %s", out)
	}
	if _, err := os.Lstat(archive); !os.IsNotExist(err) {
		t.Fatalf("unknown format published an archive: %v", err)
	}
}

func TestHomeBackupMissingFormatRefusesPublication(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := home.Init(source, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "must-not-exist.tar.gz")
	runStreamedHomeCLI(t, compoundTestBinary(t), source, false, "home", "backup", "--target", archive, "--format")
	if _, err := os.Lstat(archive); !os.IsNotExist(err) {
		t.Fatalf("missing format published an archive: %v", err)
	}
}

func TestHomeBackupExplicitV1AndLeadingFormatRemainCompatible(t *testing.T) {
	t.Parallel()
	bin := compoundTestBinary(t)
	source := t.TempDir()
	if err := home.Init(source, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"v1", "streamed-v2"} {
		t.Run(format, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "home.tar.gz")
			runStreamedHomeCLI(t, bin, source, true, "home", "--format", format, "backup", "--target", archive)
			runStreamedHomeCLI(t, bin, source, true, "home", "verify", archive)
			target := filepath.Join(t.TempDir(), "restored")
			runStreamedHomeCLI(t, bin, source, true, "home", "restore", archive, "--target", target)
			runStreamedHomeCLI(t, bin, target, true, "home", "doctor", "--json")
		})
	}
}

func runStreamedHomeCLI(t *testing.T, bin, homePath string, success bool, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if success && err != nil {
		t.Fatalf("%v failed: %v\n%s", args, err, out)
	}
	if !success {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("%v did not refuse with process failure: %v\n%s", args, err, out)
		}
	}
	return string(out)
}

func TestHomeVerifyJSONReturnsNativeArchiveChecksum(t *testing.T) {
	t.Parallel()
	bin := compoundTestBinary(t)
	source := t.TempDir()
	if err := home.Init(source, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "native-checksum.tar.gz")
	runStreamedHomeCLI(t, bin, source, true, "home", "backup", "--format", "streamed-v3", "--target", archive)
	output := runStreamedHomeCLI(t, bin, source, true, "home", "verify", archive, "--json")
	var report home.VerifyReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("native verifier did not serve JSON: %s %v", output, err)
	}
	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if !report.OK || report.ArchiveSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("native verifier checksum mismatch: %+v", report)
	}
}
