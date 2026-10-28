package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pathRunner answers with whatever path the test wants, ignoring the output root
// it was handed. It stands in for a compromised or buggy generator.
type pathRunner struct {
	path     string
	rootSeen string
}

func (r *pathRunner) Run(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
	var req map[string]any
	_ = json.Unmarshal(stdin, &req)
	if root, ok := req["output_dir"].(string); ok {
		r.rootSeen = root
	}
	return []byte(`{"path":` + strconv.Quote(r.path) + `,"mime_type":"image/png"}`), nil
}

// TestGeneratorCannotIngestFileOutsideOutputRoot is the red-first falsifier for a
// file-disclosure hole shipped in 8059782a and confirmed by live probe: a generator
// returned a path to a file it never created, and Compound ingested those bytes into
// the artifact store and attached them to a draft.
//
// An enrolled generator is operator-installed, but it is still a separate program
// that may be third-party, compromised, or simply wrong. It must not be able to name
// ~/.ssh/id_ed25519, a credential file, or any other readable host file and have
// Compound custody it.
//
// The boundary: Compound owns a private per-run output root, the generator writes
// inside it, and nothing outside it is accepted.
func TestGeneratorCannotIngestFileOutsideOutputRoot(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())

	outside := filepath.Join(t.TempDir(), "not-an-asset.txt")
	if err := os.WriteFile(outside, []byte("SYNTHETIC-NOT-A-REAL-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &pathRunner{path: outside}
	_, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{
		Adapter: "local-sdxl",
		Prompt:  "give me a host file",
	}, runner, mediaGenNow)
	if err == nil {
		t.Fatal("a generator must not be able to ingest a file outside the output root Compound gave it")
	}
	if !strings.Contains(err.Error(), "output") {
		t.Fatalf("refusal must explain the containment boundary, got: %v", err)
	}
	if runner.rootSeen == "" {
		t.Fatal("the generator must be TOLD its output root; a boundary it cannot see is one it cannot honour")
	}
}

// TestGeneratorOutputRootIsPrivateAndCleanedUp pins that the root Compound hands out
// is its own, is not world-readable, and does not accumulate on disk across runs.
func TestGeneratorOutputRootIsPrivateAndCleanedUp(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())

	var rootDuringRun string
	var modeDuringRun os.FileMode
	runner := runnerFunc(func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		root, _ := req["output_dir"].(string)
		rootDuringRun = root
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		modeDuringRun = info.Mode().Perm()
		if err := os.WriteFile(filepath.Join(root, "out.png"), []byte("\x89PNG\r\n\x1a\nok"), 0o644); err != nil {
			return nil, err
		}
		return []byte(`{"path":"out.png","mime_type":"image/png"}`), nil
	})

	media, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "local-sdxl", Prompt: "p"}, runner, mediaGenNow)
	if err != nil {
		t.Fatalf("a generator writing inside its output root must succeed: %v", err)
	}
	if len(media.Bytes) == 0 {
		t.Fatal("asset bytes not returned")
	}
	if modeDuringRun&0o077 != 0 {
		t.Fatalf("output root mode = %v, want owner-only: another local user must not read a render in progress", modeDuringRun)
	}
	if _, err := os.Stat(rootDuringRun); !os.IsNotExist(err) {
		t.Fatalf("output root %q survived the run; a per-run root must not accumulate", rootDuringRun)
	}
}

// TestGeneratorCannotEscapeOutputRootBySymlink keeps the FINAL-component case
// refused, unchanged from when O_NOFOLLOW handled it: the asset name itself is a
// symlink pointing OUT of the root. The rooted Lstat check refuses it before any
// open happens, so this must never regress while the traversal changes shape.
func TestGeneratorCannotEscapeOutputRootBySymlink(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SYNTHETIC-NOT-A-REAL-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := runnerFunc(func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		root, _ := req["output_dir"].(string)
		if err := os.Symlink(outside, filepath.Join(root, "out.png")); err != nil {
			return nil, err
		}
		return []byte(`{"path":"out.png","mime_type":"image/png"}`), nil
	})

	if _, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "local-sdxl", Prompt: "p"}, runner, mediaGenNow); err == nil {
		t.Fatal("a symlink inside the output root pointing outside it must be refused")
	}
}

// TestGeneratedMediaCarriesItsParameters pins the reproducibility claim. The seed
// and other params were sent to the generator but never persisted, so an asset could
// not actually be reproduced from its own record — the claim outran the data.
func TestGeneratedMediaCarriesItsParameters(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())
	runner := runnerFunc(func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		root, _ := req["output_dir"].(string)
		if err := os.WriteFile(filepath.Join(root, "out.png"), []byte("\x89PNG\r\n\x1a\nok"), 0o644); err != nil {
			return nil, err
		}
		return []byte(`{"path":"out.png"}`), nil
	})

	media, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{
		Adapter: "local-sdxl",
		Prompt:  "p",
		Params:  map[string]string{"seed": "42", "steps": "30"},
	}, runner, mediaGenNow)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if media.Params["seed"] != "42" || media.Params["steps"] != "30" {
		t.Fatalf("generation params not persisted, so the asset cannot be reproduced from its record: %+v", media.Params)
	}

	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID: "product:prod_synth_a",
		Platform:  "instagram",
		Body:      "b",
		ExpiresAt: mediaGenNow.Add(time.Hour),
		Media:     []ContentDraftMediaInput{media},
	}, mediaGenNow)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if draft.Media[0].Params["seed"] != "42" {
		t.Fatalf("params lost on attach: %+v", draft.Media[0])
	}
}

// TestGeneratorCannotEscapeOutputRootByIntermediateSymlink is the MEDIA-P0-001
// falsifier. Containment was checked lexically (filepath.Rel) plus O_NOFOLLOW on
// the FINAL component only — and O_NOFOLLOW does not travel. A generator that
// creates an INTERMEDIATE directory entry under the root pointing at an outside
// directory, then returns a path through it, hands Compound a path that is
// lexically inside the root and ends in a real regular file. Both checks pass,
// the kernel follows the intermediate link, and Compound custodies the outside
// file. Confirmed independently by the external release auditor.
func TestGeneratorCannotEscapeOutputRootByIntermediateSymlink(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())

	// A real directory OUTSIDE the output root with a real regular file in it.
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("SYNTHETIC-NOT-A-REAL-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := runnerFunc(func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		root, _ := req["output_dir"].(string)
		if err := os.Symlink(outsideDir, filepath.Join(root, "escape")); err != nil {
			return nil, err
		}
		return []byte(`{"path":"escape/secret.txt","mime_type":"image/png"}`), nil
	})

	media, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{
		Adapter: "local-sdxl",
		Prompt:  "p",
	}, runner, mediaGenNow)
	if err == nil {
		t.Fatalf("custody must refuse an asset reached through an intermediate symlink out of the output root; instead it ingested %q", media.Bytes)
	}
}

// TestGeneratorCannotIngestHardlinkedOutsideFile closes the alias channel: a
// hard link inside the root IS the outside file's inode — a regular file by
// every stat, with a second name somewhere Compound does not control. Custody
// must require the asset to be exclusively named inside the root.
//
// (This cannot and does not stop a generator from COPYING a readable host
// file's bytes into a fresh file it writes — that is the trusted-executable
// line, not a path rule. This test pins the link-count refusal only.)
func TestGeneratorCannotIngestHardlinkedOutsideFile(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SYNTHETIC-NOT-A-REAL-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := runnerFunc(func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		root, _ := req["output_dir"].(string)
		if err := os.Link(outside, filepath.Join(root, "out.png")); err != nil {
			return nil, err
		}
		return []byte(`{"path":"out.png","mime_type":"image/png"}`), nil
	})

	if _, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{
		Adapter: "local-sdxl",
		Prompt:  "p",
	}, runner, mediaGenNow); err == nil {
		t.Fatal("a hard link to a file outside the output root must not be custodied: the same inode has a name Compound does not control")
	}
}

// TestGeneratorCustodySurvivesOutputRootSwap pins identity retention: custody
// must resolve the asset beneath the root descriptor Compound opened, so a
// generator that renames the real root away and plants a decoy root (same path,
// same asset name, different bytes) mid-run cannot redirect what gets ingested.
func TestGeneratorCustodySurvivesOutputRootSwap(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())

	const original = "\x89PNG\r\n\x1a\nORIGINAL-RENDER"
	var moved string
	t.Cleanup(func() {
		if moved != "" {
			_ = os.RemoveAll(moved)
		}
	})

	runner := runnerFunc(func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		root, _ := req["output_dir"].(string)
		if err := os.WriteFile(filepath.Join(root, "out.png"), []byte(original), 0o644); err != nil {
			return nil, err
		}
		// Swap the root: the real render moves away, a decoy root with the
		// same asset name appears at the original path.
		moved = root + "-swapped-out"
		if err := os.Rename(root, moved); err != nil {
			return nil, err
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			return nil, err
		}
		decoy := filepath.Join(root, "out.png")
		if err := os.WriteFile(decoy, []byte("DECOY-RENDER"), 0o644); err != nil {
			return nil, err
		}
		return []byte(`{"path":"out.png","mime_type":"image/png"}`), nil
	})

	media, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{
		Adapter: "local-sdxl",
		Prompt:  "p",
	}, runner, mediaGenNow)
	if err != nil {
		t.Fatalf("a legitimate asset whose root path was swapped after the render must still be custodied through the original root descriptor: %v", err)
	}
	if string(media.Bytes) != original {
		t.Fatalf("custody read the swapped decoy %q instead of the rendered asset: root identity was not retained", media.Bytes)
	}
}

// TestGeneratorAbsolutePathAnswerIsRefused pins the control-channel contract on
// its own: the answer must be RELATIVE. Even a perfectly legitimate asset the
// generator really wrote inside the root is refused when named by absolute path —
// an absolute answer is a claim about the host filesystem, and custody never
// re-opens the lexical-containment question the old contract depended on.
// (Pre-fix, the absolute form was the ONLY accepted form — that contract is what
// let the MEDIA-P0-001 intermediate-symlink path through.)
func TestGeneratorAbsolutePathAnswerIsRefused(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())

	runner := runnerFunc(func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		root, _ := req["output_dir"].(string)
		if err := os.WriteFile(filepath.Join(root, "out.png"), []byte("\x89PNG\r\n\x1a\nlegit"), 0o644); err != nil {
			return nil, err
		}
		return []byte(`{"path":` + strconv.Quote(filepath.Join(root, "out.png")) + `,"mime_type":"image/png"}`), nil
	})

	_, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "local-sdxl", Prompt: "p"}, runner, mediaGenNow)
	if err == nil {
		t.Fatal("an absolute asset path must be refused outright, even when the file is real and inside the root")
	}
	if !strings.Contains(err.Error(), "relative") {
		t.Fatalf("refusal must name the relative-path contract, got: %v", err)
	}
}

// TestGeneratorNestedSubdirectoryAssetWorks guards against over-tightening: a
// real renderer often writes into subdirectories it creates under the root
// (render-chunks, intermediates, versioned output folders). A legitimately
// nested asset with a clean relative answer must still be ingested.
func TestGeneratorNestedSubdirectoryAssetWorks(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())

	const bytes = "\x89PNG\r\n\x1a\nnested-but-legitimate"
	runner := runnerFunc(func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		root, _ := req["output_dir"].(string)
		dir := filepath.Join(root, "renders", "v3")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "out.png"), []byte(bytes), 0o644); err != nil {
			return nil, err
		}
		return []byte(`{"path":"renders/v3/out.png","mime_type":"image/png"}`), nil
	})

	media, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "local-sdxl", Prompt: "p"}, runner, mediaGenNow)
	if err != nil {
		t.Fatalf("a nested asset inside the output root must be accepted: %v", err)
	}
	if string(media.Bytes) != bytes {
		t.Fatalf("nested asset bytes = %q, want %q", media.Bytes, bytes)
	}
}

type runnerFunc func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error)

func (f runnerFunc) Run(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) { return f(cfg, stdin) }
