package product

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var mediaGenNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// stubRunner stands in for a real generator process. The runner seam exists so
// every rule below is provable without a GPU, a network, or a checkout of somebody
// else's model weights.
type stubRunner struct {
	gotStdin []byte
	gotCfg   MediaGeneratorConfig
	stdout   string
	err      error
	assetDir string
	// asset, when non-empty, is written to a file the stdout JSON points at.
	asset []byte
}

func (r *stubRunner) Run(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
	r.gotStdin = stdin
	r.gotCfg = cfg
	if r.err != nil {
		return nil, r.err
	}
	// Write inside the output root Compound created for this run and answer with
	// a path RELATIVE to it — the control-channel contract.
	var req map[string]any
	_ = json.Unmarshal(stdin, &req)
	root, _ := req["output_dir"].(string)
	r.assetDir = root
	if len(r.asset) > 0 {
		if err := os.WriteFile(filepath.Join(root, "asset.png"), r.asset, 0o644); err != nil {
			return nil, err
		}
		r.stdout = `{"path":"asset.png","mime_type":"image/png"}`
	}
	return []byte(r.stdout), nil
}

func enrollGenerator(t *testing.T, homePath, name string, cfg map[string]any) {
	t.Helper()
	dir := filepath.Join(homePath, "actuator", "generators")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func imageGeneratorConfig() map[string]any {
	return map[string]any{
		"argv":      []string{"/usr/local/bin/sdxl-render"},
		"timeout_s": 120,
		"kind":      "image",
		"model":     "sdxl-1.0+lora:product-style-v3",
	}
}

// TestGenerateMediaProducesAttachableProvenancedAsset is the falsifier for the
// whole seam: an enrolled generator turns a prompt into an asset that the draft
// door can accept, carrying the provenance needed to reproduce it.
func TestGenerateMediaProducesAttachableProvenancedAsset(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())
	runner := &stubRunner{assetDir: t.TempDir(), asset: []byte("\x89PNG\r\n\x1a\nsynthetic")}

	media, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{
		Adapter: "local-sdxl",
		Prompt:  "a calm product screenshot, studio lighting",
		Params:  map[string]string{"seed": "42", "steps": "30"},
	}, runner, mediaGenNow)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if media.Kind != "image" {
		t.Fatalf("kind = %q, want image (from the enrolled config)", media.Kind)
	}
	if len(media.Bytes) == 0 {
		t.Fatal("no asset bytes returned")
	}
	if media.Adapter != "adapter:local-sdxl" {
		t.Fatalf("adapter = %q, want adapter:local-sdxl", media.Adapter)
	}
	if media.Model != "sdxl-1.0+lora:product-style-v3" {
		t.Fatalf("model = %q, want the enrolled model identity", media.Model)
	}
	if media.Prompt != "a calm product screenshot, studio lighting" {
		t.Fatalf("prompt not carried: %q", media.Prompt)
	}

	// The generator must actually receive the request, including params, so a
	// seed is reproducible rather than decorative.
	var sent map[string]any
	if err := json.Unmarshal(runner.gotStdin, &sent); err != nil {
		t.Fatalf("request sent to generator is not JSON: %v", err)
	}
	if sent["prompt"] != "a calm product screenshot, studio lighting" {
		t.Fatalf("prompt not sent to generator: %+v", sent)
	}
	params, _ := sent["params"].(map[string]any)
	if params["seed"] != "42" {
		t.Fatalf("params not sent to generator: %+v", sent)
	}

	// The produced value must be accepted by the draft door unchanged — the seam
	// is only real if its output is attachable.
	draft, err := RecordContentDraft(homePath, ContentDraftInput{
		ProductID: "product:prod_synth_a",
		Platform:  "instagram",
		Body:      "generated post",
		ExpiresAt: mediaGenNow.Add(336 * time.Hour),
		Media:     []ContentDraftMediaInput{media},
	}, mediaGenNow)
	if err != nil {
		t.Fatalf("generated media must be attachable to a draft: %v", err)
	}
	if len(draft.Media) != 1 || draft.Media[0].Adapter != "adapter:local-sdxl" {
		t.Fatalf("provenance lost on attach: %+v", draft.Media)
	}
}

// TestGeneratorCannotForgeItsOwnIdentity is the integrity falsifier and the reason
// provenance is trustworthy at all.
//
// The adapter name and the model come from the ENROLLED CONFIG — the registry key
// the operator invoked — never from the generator's own stdout. A generator that
// could name itself could attribute its output to a different adapter or model,
// and every downstream comparison ("which model wins?") would be measuring a claim
// the generator made about itself.
func TestGeneratorCannotForgeItsOwnIdentity(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())
	runner := &stubRunner{
		asset: []byte("\x89PNG\r\n\x1a\nx"),
		stdout: `{"path":"asset.png","mime_type":"image/png","adapter":"adapter:midjourney","` +
			`model":"a-model-i-made-up","kind":"video"}`,
	}

	media, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{
		Adapter: "local-sdxl",
		Prompt:  "p",
	}, runner, mediaGenNow)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if media.Adapter != "adapter:local-sdxl" {
		t.Fatalf("generator forged its adapter identity: %q", media.Adapter)
	}
	if media.Model != "sdxl-1.0+lora:product-style-v3" {
		t.Fatalf("generator forged its model identity: %q", media.Model)
	}
	if media.Kind != "image" {
		t.Fatalf("generator forged its media kind: %q", media.Kind)
	}
}

// TestGenerateMediaRefusesUnenrolledGenerator keeps the registry closed: a
// generator is a deliberately enrolled program, not any name a caller invents.
func TestGenerateMediaRefusesUnenrolledGenerator(t *testing.T) {
	homePath := t.TempDir()
	_, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "whatever", Prompt: "p"}, &stubRunner{}, mediaGenNow)
	if err == nil {
		t.Fatal("an unenrolled generator must be refused")
	}
	if !strings.Contains(err.Error(), "whatever") {
		t.Fatalf("refusal must name the generator, got: %v", err)
	}
}

// TestGenerateMediaRefusesShellInterpreterGenerator proves the enrolled-executable
// rule is SHARED with the act transport seam rather than reimplemented. A config
// that could name bash or python would turn an enrolled generator into arbitrary
// command execution.
func TestGenerateMediaRefusesShellInterpreterGenerator(t *testing.T) {
	for _, exe := range []string{"/bin/bash", "/usr/bin/python3", "/bin/sh"} {
		t.Run(exe, func(t *testing.T) {
			homePath := t.TempDir()
			cfg := imageGeneratorConfig()
			cfg["argv"] = []string{exe, "render.py"}
			enrollGenerator(t, homePath, "shellish", cfg)
			_, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "shellish", Prompt: "p"}, &stubRunner{}, mediaGenNow)
			if err == nil {
				t.Fatalf("a %s generator must be refused", exe)
			}
			if !strings.Contains(err.Error(), "shell/interpreter") {
				t.Fatalf("refusal must come from the shared enrolled-executable rule, got: %v", err)
			}
		})
	}
}

// TestGenerateMediaRefusesUnusableGeneratorOutput pins that a generator which
// produces nothing usable fails loudly rather than minting an empty asset. An
// empty asset would attach to a post and publish a blank image.
func TestGenerateMediaRefusesUnusableGeneratorOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
	}{
		{"no output", ""},
		{"not json", "done!"},
		{"no path", `{"mime_type":"image/png"}`},
		{"missing file", `{"path":"asset.png"}`},
		{"absolute path", `{"path":"/etc/hosts"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())
			_, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "local-sdxl", Prompt: "p"}, &stubRunner{stdout: tc.stdout}, mediaGenNow)
			if err == nil {
				t.Fatal("unusable generator output must be refused, not turned into an empty asset")
			}
		})
	}
}

// TestGenerateMediaRefusesOversizedAsset bounds what a generator can hand back.
// An unbounded read is how one bad render exhausts memory on the machine that runs
// every other lane.
func TestGenerateMediaRefusesOversizedAsset(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())
	runner := runnerFunc(func(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
		var req map[string]any
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		root, _ := req["output_dir"].(string)
		if err := os.WriteFile(filepath.Join(root, "huge.png"), make([]byte, MaxGeneratedAssetBytes+1), 0o644); err != nil {
			return nil, err
		}
		return []byte(`{"path":"huge.png"}`), nil
	})
	_, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "local-sdxl", Prompt: "p"}, runner, mediaGenNow)
	if err == nil {
		t.Fatal("an oversized asset must be refused")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("refusal must explain the bound, got: %v", err)
	}
}

// TestGenerateMediaRefusesUnknownKind keeps the generator registry aligned with the
// draft media allowlist: a config declaring a kind the draft door would reject must
// fail at enrollment-read time, not after a GPU has spent minutes rendering.
func TestGenerateMediaRefusesUnknownKind(t *testing.T) {
	homePath := t.TempDir()
	cfg := imageGeneratorConfig()
	cfg["kind"] = "hologram"
	enrollGenerator(t, homePath, "weird", cfg)
	_, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "weird", Prompt: "p"}, &stubRunner{}, mediaGenNow)
	if err == nil {
		t.Fatal("a generator declaring an unattachable kind must be refused before it runs")
	}
	if !strings.Contains(err.Error(), "hologram") {
		t.Fatalf("refusal must name the kind, got: %v", err)
	}
}

// TestGenerateMediaRequiresPrompt pins that provenance cannot be empty at the
// source: an asset with no prompt cannot be reproduced or compared later.
func TestGenerateMediaRequiresPrompt(t *testing.T) {
	homePath := t.TempDir()
	enrollGenerator(t, homePath, "local-sdxl", imageGeneratorConfig())
	if _, err := GenerateContentDraftMedia(homePath, MediaGenerateRequest{Adapter: "local-sdxl", Prompt: "   "}, &stubRunner{}, mediaGenNow); err == nil {
		t.Fatal("an empty prompt must be refused")
	}
}
