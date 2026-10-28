package product

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"compound/internal/act"
)

// media_generate.go is the pluggable generator seam: the door that turns a prompt
// into an asset a ContentDraft can carry.
//
// It lives in internal/product rather than a package of its own because what it
// produces is a ContentDraftMedia, which this package already owns. localmodel is
// LLM routing, adapter is read-only foreign normalization, and actuator routes work
// to executions — none of them owns asset generation. If a second consumer appears,
// extract it then.
//
// The seam has one integrity rule that everything downstream depends on: an
// asset's PROVENANCE comes from the enrolled config, never from the generator's own
// output. A generator that could name its own adapter or model could attribute its
// work to another, and every later comparison ("which model actually wins?") would
// be measuring a claim the generator made about itself.
//
// The seam's second rule is PATH CUSTODY. Compound owns a per-run output root,
// opens a descriptor for it BEFORE the generator runs, and only ever ingests a
// file resolved beneath that descriptor. The generator answers with a path
// RELATIVE to the root; an absolute answer, a path whose final component is a
// symlink, a path that traverses a symlink out of the root at ANY component, a
// hard link, and a root whose path was swapped mid-run are each refused.
//
// Honesty about what custody buys, because a reader must not over-trust it: an
// enrolled generator is a trusted local executable running as the user. It can
// read any host file it has permission to read and COPY the bytes into its own
// permitted output directory, and custody will correctly ingest that copy.
// Nothing here is a sandbox, and no path rule can make it one. What custody
// prevents is confused-path INGESTION: the generator naming, linking, or
// swapping its way to a file outside the root Compound handed it, so Compound
// stores and republishes bytes the operator never asked it to render.

// MaxGeneratedAssetBytes bounds what one generator may hand back. An unbounded read
// is how a single bad render exhausts memory on the machine that runs every lane.
const MaxGeneratedAssetBytes = 64 * 1024 * 1024

// maxGeneratorStdout bounds the generator's control-channel output. The asset
// itself travels as a file path, so this only needs to hold a small JSON object.
const maxGeneratorStdout = 64 * 1024

// generatorAllowedBareNames is empty on purpose: a media generator must be enrolled
// by clean absolute path. Unlike a transport (where "claude" or "codex" is a
// meaningful bare name resolved on PATH), a generator is operator-specific tooling,
// and resolving it through PATH would make which program ran depend on environment.
var generatorAllowedBareNames = map[string]bool{}

// MediaGeneratorConfig is one enrolled generator, read from
// $COMPOUND_HOME/actuator/generators/<name>.json. It mirrors the exec transport
// config deliberately: same shape, same safety rule, one more declared field pair
// (kind + model) because an asset carries provenance a text post does not.
type MediaGeneratorConfig struct {
	Argv     []string `json:"argv"`
	TimeoutS int      `json:"timeout_s"`
	Kind     string   `json:"kind"`
	Model    string   `json:"model"`
}

// MediaGenerateRequest is what the operator asks for.
type MediaGenerateRequest struct {
	Adapter string
	Prompt  string
	AltText string
	Params  map[string]string
}

// generatorOutputRootPerm keeps a render-in-progress unreadable by other local
// users. The root is Compound's, not the generator's, and it is removed per run.
const generatorOutputRootPerm = 0o700

// MediaGeneratorRunner executes an enrolled generator. It is an interface so every
// rule in this file is provable without a GPU, a network, or model weights.
type MediaGeneratorRunner interface {
	Run(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error)
}

// generatorAnswer is the generator's control-channel reply. Note what is NOT read
// from it: adapter, model, and kind. Those are the enrolled config's to declare.
type generatorAnswer struct {
	Path     string `json:"path"`
	MIMEType string `json:"mime_type"`
}

// GenerateContentDraftMedia runs an enrolled generator and returns an asset ready
// to attach to a draft. The returned value is a ContentDraftMediaInput precisely so
// the draft door validates it on the way in — this function has no privileged path
// into a draft.
func GenerateContentDraftMedia(homePath string, req MediaGenerateRequest, runner MediaGeneratorRunner, now time.Time) (ContentDraftMediaInput, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return ContentDraftMediaInput{}, fmt.Errorf("prompt required: an asset with no prompt cannot be reproduced or compared later")
	}
	name := strings.TrimSpace(req.Adapter)
	cfg, err := LoadMediaGeneratorConfig(homePath, name)
	if err != nil {
		return ContentDraftMediaInput{}, err
	}
	if runner == nil {
		runner = ExecMediaGeneratorRunner{}
	}
	// Compound owns the output root. A generator that could name any readable path
	// could have Compound custody a private key or credential file — this is not
	// hypothetical, it was live in 8059782a and confirmed by probe.
	outputRoot, err := os.MkdirTemp("", "compound-generator-")
	if err != nil {
		return ContentDraftMediaInput{}, err
	}
	defer os.RemoveAll(outputRoot)
	if err := os.Chmod(outputRoot, generatorOutputRootPerm); err != nil {
		return ContentDraftMediaInput{}, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(outputRoot)
	if err != nil {
		return ContentDraftMediaInput{}, err
	}
	// Hold a descriptor for the root from BEFORE the generator runs until
	// custody completes. Every asset open below resolves beneath THIS
	// descriptor, so the generator cannot redirect custody by renaming the
	// root away and planting a decoy at its path, or by symlinking the path
	// elsewhere: the traversal never re-resolves the root's path.
	genRoot, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return ContentDraftMediaInput{}, err
	}
	defer genRoot.Close()
	payload, err := json.Marshal(map[string]any{
		"kind":       cfg.Kind,
		"model":      cfg.Model,
		"prompt":     prompt,
		"params":     req.Params,
		"output_dir": resolvedRoot,
		"issued_at":  now.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return ContentDraftMediaInput{}, err
	}
	stdout, err := runner.Run(cfg, payload)
	if err != nil {
		return ContentDraftMediaInput{}, fmt.Errorf("generator %q failed: %w", name, err)
	}
	bytesOut, mime, err := readGeneratorAsset(name, genRoot, stdout)
	if err != nil {
		return ContentDraftMediaInput{}, err
	}
	return ContentDraftMediaInput{
		Kind:     cfg.Kind,
		Bytes:    bytesOut,
		MIMEType: mime,
		AltText:  strings.TrimSpace(req.AltText),
		// Provenance from the ENROLLED CONFIG, never from the generator's answer.
		Adapter: "adapter:" + name,
		Model:   cfg.Model,
		Prompt:  prompt,
		Params:  req.Params,
	}, nil
}

// LoadMediaGeneratorConfig reads and validates one enrolled generator. Everything
// that can be checked before a GPU spends minutes rendering is checked here.
func LoadMediaGeneratorConfig(homePath, name string) (MediaGeneratorConfig, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return MediaGeneratorConfig{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if name == "" || filepath.Base(name) != name || strings.Contains(name, ".") {
		return MediaGeneratorConfig{}, fmt.Errorf("generator name %q must be a plain name", name)
	}
	path := filepath.Join(homePath, "actuator", "generators", name+".json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return MediaGeneratorConfig{}, fmt.Errorf("generator %q is not enrolled: no config at %s", name, path)
	}
	if err != nil {
		return MediaGeneratorConfig{}, err
	}
	var cfg MediaGeneratorConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return MediaGeneratorConfig{}, fmt.Errorf("generator %q config: %w", name, err)
	}
	cfg.Kind = strings.ToLower(strings.TrimSpace(cfg.Kind))
	if !contentDraftMediaKinds[cfg.Kind] {
		return MediaGeneratorConfig{}, fmt.Errorf("generator %q declares kind %q, which a draft cannot carry (image|video)", name, cfg.Kind)
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.Model == "" {
		return MediaGeneratorConfig{}, fmt.Errorf("generator %q config: model identity required, or its output cannot be attributed", name)
	}
	// One owner for "may this enrolled config invoke this program?" — shared with
	// the act exec transport rather than copied, because a security rule with two
	// copies is a rule that drifts.
	if err := act.ValidateEnrolledArgv("generator", name, cfg.Argv, generatorAllowedBareNames); err != nil {
		return MediaGeneratorConfig{}, err
	}
	if cfg.TimeoutS <= 0 || cfg.TimeoutS > 3600 {
		return MediaGeneratorConfig{}, fmt.Errorf("generator %q config: timeout_s must be between 1 and 3600", name)
	}
	return cfg, nil
}

// readGeneratorAsset parses the control-channel answer and loads the asset the
// generator wrote. Every failure here is loud: a generator that produced nothing
// usable must not become an empty asset that later publishes as a blank image.
//
// Custody is enforced against the root DESCRIPTOR, never against a path string.
// The answer must name the asset RELATIVE to the root handed out in the request,
// and every component of it is resolved beneath the descriptor Compound opened
// before the generator ran. That is what closes MEDIA-P0-001: lexical
// containment plus O_NOFOLLOW on the final component let
// <root>/escape/secret.txt through whenever "escape" was itself a symlink to an
// outside directory, because the intermediate link was followed by the kernel
// and the final component was an innocent regular file. os.Root refuses any
// symlink — intermediate or final — that would leave the root, keeps following
// the ones that stay inside it, and pins the root's identity across renames of
// its path.
//
// The final file is stat'ed AND read through the same descriptor that passed
// containment, and its name must still resolve to the same inode after the
// read, so the asset cannot be swapped between the check and the read.
//
// Threat model, restated where it is enforced: an enrolled generator is a
// trusted local executable running as the user. It can copy any file it can
// read into the output root and custody will ingest the copy — this is path
// custody, NOT a sandbox. What is refused here is the confused-path channel:
// naming an outside path (absolute answers, ..), reaching one through a link at
// any component, aliasing one by hard link, or swapping the root's path.
func readGeneratorAsset(name string, root *os.Root, stdout []byte) ([]byte, string, error) {
	trimmed := strings.TrimSpace(string(stdout))
	if trimmed == "" {
		return nil, "", fmt.Errorf("generator %q produced no output", name)
	}
	var answer generatorAnswer
	if err := json.Unmarshal([]byte(trimmed), &answer); err != nil {
		return nil, "", fmt.Errorf("generator %q output is not a JSON object naming the asset path: %w", name, err)
	}
	assetPath := strings.TrimSpace(answer.Path)
	if assetPath == "" {
		return nil, "", fmt.Errorf("generator %q output names no asset path", name)
	}
	// The contract is a RELATIVE path under the output root. Absolute paths are
	// refused outright: an absolute answer is a claim about the host filesystem,
	// not about the directory Compound owns for this run, and accepting one
	// reopens every lexical-containment question this function exists to close.
	if filepath.IsAbs(assetPath) {
		return nil, "", fmt.Errorf("generator %q asset path %q must be relative to its output root: absolute paths are refused", name, answer.Path)
	}
	if assetPath != filepath.Clean(assetPath) || assetPath == "." || assetPath == ".." || strings.HasPrefix(assetPath, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("generator %q asset path %q must be a clean relative path inside its output root", name, answer.Path)
	}
	// Containment, enforced by the traversal itself: resolving beneath the root
	// descriptor refuses a symlink at ANY component that leaves the root, so
	// "escape/secret.txt" with "escape" linked outside fails here even though
	// "secret.txt" is a real regular file.
	before, err := root.Lstat(assetPath)
	if err != nil {
		return nil, "", fmt.Errorf("generator %q asset %q is not reachable inside its output root: %w", name, assetPath, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, "", fmt.Errorf("generator %q asset %q must be a regular file inside its output root, got %v", name, assetPath, before.Mode().Type())
	}
	f, err := root.Open(assetPath)
	if err != nil {
		return nil, "", fmt.Errorf("generator %q asset %q is not readable inside its output root: %w", name, assetPath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("generator %q asset stat: %w", name, err)
	}
	after, err := root.Lstat(assetPath)
	if err != nil {
		return nil, "", fmt.Errorf("generator %q asset %q vanished from its output root before custody completed: %w", name, assetPath, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(after, info) {
		return nil, "", fmt.Errorf("generator %q asset %q did not remain a stable regular file inside its output root", name, assetPath)
	}
	// Exclusive custody of the name is exclusive custody of the bytes: a link
	// count above one means this inode has at least one other name somewhere
	// Compound does not control — the classic case is a generator hard-linking
	// a host file into the root instead of rendering one. (This closes the
	// alias channel only. It cannot detect a generator that COPIES a readable
	// host file into a fresh one; see the threat model above — that line is
	// trust in the enrolled executable, not path custody.)
	nlink, ok := fileLinkCount(info)
	if !ok {
		return nil, "", fmt.Errorf("generator %q asset %q cannot be verified as exclusively named inside its output root: link count unavailable", name, assetPath)
	}
	if nlink != 1 {
		return nil, "", fmt.Errorf("generator %q asset %q is a hard link (its inode carries %d names): an asset must be exclusively named inside its output root", name, assetPath, nlink)
	}
	if info.Size() == 0 {
		return nil, "", fmt.Errorf("generator %q produced an empty asset", name)
	}
	if info.Size() > MaxGeneratedAssetBytes {
		return nil, "", fmt.Errorf("generator %q asset is %d bytes, which exceeds the %d byte bound", name, info.Size(), MaxGeneratedAssetBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxGeneratedAssetBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("generator %q asset read: %w", name, err)
	}
	final, err := root.Lstat(assetPath)
	if err != nil {
		return nil, "", fmt.Errorf("generator %q asset %q disappeared while it was being read: %w", name, assetPath, err)
	}
	if !os.SameFile(info, final) {
		return nil, "", fmt.Errorf("generator %q asset %q changed while it was being read", name, assetPath)
	}
	if int64(len(data)) > MaxGeneratedAssetBytes {
		return nil, "", fmt.Errorf("generator %q asset exceeds the %d byte bound", name, MaxGeneratedAssetBytes)
	}
	return data, strings.TrimSpace(answer.MIMEType), nil
}

// fileLinkCount reports how many directory names reference the opened file's
// inode. A count above one means another name — one Compound does not control,
// typically outside the output root — refers to the same bytes. The second
// return is false on platforms whose stat cannot answer; callers treat that as
// a refusal, because custody that cannot be verified is not custody.
func fileLinkCount(info os.FileInfo) (uint64, bool) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink), true
	}
	return 0, false
}

// ExecMediaGeneratorRunner runs the enrolled program directly, handing it the
// request on stdin and reading its bounded control-channel answer from stdout. It
// never uses a shell: the argv is executed as-is, already validated by the shared
// enrolled-executable rule.
type ExecMediaGeneratorRunner struct{}

func (ExecMediaGeneratorRunner) Run(cfg MediaGeneratorConfig, stdin []byte) ([]byte, error) {
	cmd := exec.Command(cfg.Argv[0], cfg.Argv[1:]...)
	cmd.Stdin = strings.NewReader(string(stdin))
	// The generator's own environment is inherited deliberately: a local renderer
	// legitimately needs CUDA/model-path variables the operator has configured.
	// Separate buffers, not one shared builder: os/exec writes stdout and stderr
	// from different goroutines, so a single unsynchronized strings.Builder is a
	// data race AND lets a chatty generator's stderr logs corrupt the JSON contract
	// on stdout.
	var stdoutBuf, stderrBuf strings.Builder
	cmd.Stdout = &boundedWriter{w: &stdoutBuf, max: maxGeneratorStdout}
	cmd.Stderr = &boundedWriter{w: &stderrBuf, max: maxGeneratorStdout}
	timer := time.AfterFunc(time.Duration(cfg.TimeoutS)*time.Second, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderrBuf.String()))
	}
	return []byte(stdoutBuf.String()), nil
}

// boundedWriter caps what a generator can push through the control channel.
type boundedWriter struct {
	w   *strings.Builder
	max int
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if remaining := b.max - b.w.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.w.Write(p)
	}
	// Report the full length: a generator must not see a short write and retry.
	return len(p), nil
}
