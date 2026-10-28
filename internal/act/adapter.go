// Package act is compound's ONE generic governed outbound-write seam: the single
// execute-and-receipt wrapper that makes "no external mutation without a valid
// PolicyDecision" true BY CONSTRUCTION. An Adapter is the only thing that touches the
// outside world, and its mutating methods are UNEXPORTED — unexported interface methods
// are package-scoped, so only types declared in package act can satisfy Adapter and only
// package act can invoke forward writes or pre-image restores. No external package can
// reach either mutation path: Execute and ExecuteRevert are the gated entrypoints. The
// gate is therefore the ONLY reachable path to a mutation, not merely the intended one.
// Every future outbound system (a config repoint, a DNS record, an API toggle) is a new
// Adapter plus a different ActionIntent.Target — never a new governance path.
// See docs/architecture/{runtime,transition}.md and internal/policy.
package act

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"compound/internal/envelope"
)

// Adapter is a generic outbound target. ReadCurrent returns the current value at the
// target (the pre-image); write replaces it with opaque forward data. The mutating
// methods are UNEXPORTED on purpose: only package act can call write or an optional
// restorePreImage implementation. That is the structural seal — external packages
// hand adapters to Execute or ExecuteRevert, both of which refuse without an allowing
// PolicyDecision. Concrete adapters stay tiny and side-effect-only: all governance
// lives above them.
type Adapter interface {
	ReadCurrent(target envelope.ActionTarget) ([]byte, error)
	write(target envelope.ActionTarget, newValue []byte) error
}

type preImageRestorer interface {
	restorePreImage(target envelope.ActionTarget, preImage []byte) error
}

func restoreAdapterPreImage(adapter Adapter, target envelope.ActionTarget, preImage []byte) error {
	if restorer, ok := adapter.(preImageRestorer); ok {
		return restorer.restorePreImage(target, preImage)
	}
	return adapter.write(target, preImage)
}

// ConfigKeyAdapter reads and writes a single dotted key (Target.ID) inside a YAML
// or JSON config file (Target.Path). It is NOT specific to any system: a switchback
// egress alias and a headroom config key differ only by Target.Path / Target.ID. It
// is reversible by nature — the pre-image is the current value, and a rollback is
// just another Write of that captured value.
type ConfigKeyAdapter struct{}

var _ Adapter = ConfigKeyAdapter{}

// NewConfigKeyAdapter returns a ConfigKeyAdapter as an Adapter. External callers use
// this to OBTAIN an adapter to hand to Execute; they cannot invoke its write directly,
// because write is unexported (package-scoped). The only path from an Adapter to a
// mutation is Execute's PolicyDecision gate.
func NewConfigKeyAdapter() Adapter { return ConfigKeyAdapter{} }

// ReadCurrent returns the current scalar value at Target.ID as its serialized bytes
// (e.g. "providerA", "true"). A missing file, missing key, or non-mapping node on
// the path is an explicit error, never a silent empty read.
func (ConfigKeyAdapter) ReadCurrent(target envelope.ActionTarget) ([]byte, error) {
	root, _, err := loadConfig(target.Path)
	if err != nil {
		return nil, err
	}
	leaf, err := navigate(root, target.ID)
	if err != nil {
		return nil, err
	}
	return marshalScalar(leaf)
}

// write replaces the existing value at Target.ID with newValue (parsed as a YAML/JSON
// scalar, so "providerB" stays a string and "false" becomes a bool) and re-serializes
// the whole file in its original format. The key must already exist — this repoints a
// key, it does not create one. It is UNEXPORTED so it is unreachable outside package
// act: Execute is the only caller, and only after an allowing PolicyDecision.
func (ConfigKeyAdapter) write(target envelope.ActionTarget, newValue []byte) error {
	root, isJSON, err := loadConfig(target.Path)
	if err != nil {
		return err
	}
	var parsed any
	if err := yaml.Unmarshal(newValue, &parsed); err != nil {
		return fmt.Errorf("config-key write: parse new value: %w", err)
	}
	if err := setLeaf(root, target.ID, parsed); err != nil {
		return err
	}
	return writeConfig(target.Path, root, isJSON)
}

func loadConfig(path string) (map[string]any, bool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, false, fmt.Errorf("config-key adapter requires target.path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	root := map[string]any{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, false, fmt.Errorf("config-key: parse %s: %w", path, err)
	}
	return root, strings.EqualFold(filepath.Ext(path), ".json"), nil
}

func writeConfig(path string, root map[string]any, isJSON bool) error {
	var data []byte
	var err error
	if isJSON {
		if data, err = json.MarshalIndent(root, "", "  "); err == nil {
			data = append(data, '\n')
		}
	} else {
		data, err = yaml.Marshal(root)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// navigate walks the dotted key through nested mappings and returns the leaf value.
func navigate(root map[string]any, dotted string) (any, error) {
	parts := splitKey(dotted)
	if len(parts) == 0 {
		return nil, fmt.Errorf("config-key: empty key")
	}
	var cur any = root
	for i, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("config-key %q: %q is not a mapping", dotted, strings.Join(parts[:i], "."))
		}
		v, ok := m[p]
		if !ok {
			return nil, fmt.Errorf("config-key %q: key %q not found", dotted, p)
		}
		cur = v
	}
	return cur, nil
}

// setLeaf replaces the value at the dotted key. The leaf key must already exist so
// a repoint never silently invents a key on a typo'd path.
func setLeaf(root map[string]any, dotted string, val any) error {
	parts := splitKey(dotted)
	if len(parts) == 0 {
		return fmt.Errorf("config-key: empty key")
	}
	cur := root
	for i := 0; i < len(parts)-1; i++ {
		next, ok := cur[parts[i]].(map[string]any)
		if !ok {
			return fmt.Errorf("config-key %q: %q is not a mapping", dotted, strings.Join(parts[:i+1], "."))
		}
		cur = next
	}
	leaf := parts[len(parts)-1]
	if _, ok := cur[leaf]; !ok {
		return fmt.Errorf("config-key %q: key %q not found", dotted, leaf)
	}
	cur[leaf] = val
	return nil
}

func splitKey(dotted string) []string {
	dotted = strings.TrimSpace(dotted)
	if dotted == "" {
		return nil
	}
	return strings.Split(dotted, ".")
}

// marshalScalar renders a leaf value as its bare serialized bytes (no trailing
// newline), so "providerA" round-trips to "providerA" and true to "true".
func marshalScalar(v any) ([]byte, error) {
	out, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(string(out), "\n")), nil
}

// FileArtifactAdapter reads and writes a whole file (Target.Path) as opaque
// bytes. It is the staging seam for outbound content that is local-first and
// reversible — e.g. a distribution post written to an outbox, or any artifact
// a later step ships externally. It is NOT specific to any channel: a post
// outbox file and an exported report differ only by Target.Path. Like
// ConfigKeyAdapter it is reversible by nature — the pre-image records whether
// the file existed plus its prior bytes, and the distinct restore path deletes
// a newly created file or restores an existing file byte-for-byte.
//
// The mutating method is UNEXPORTED on purpose: only package act can call it,
// so Execute's PolicyDecision gate is the sole reachable path to a write — no
// external package can stage an artifact without an allowing PolicyDecision.
type FileArtifactAdapter struct{}

var _ Adapter = FileArtifactAdapter{}

// NewFileArtifactAdapter returns a FileArtifactAdapter as an Adapter. External
// callers use this to OBTAIN an adapter to hand to Execute; they cannot invoke
// its write directly, because write is unexported (package-scoped).
func NewFileArtifactAdapter() Adapter { return FileArtifactAdapter{} }

// filePreImageSchema tags the encoded pre-image so a rollback can tell a file that
// DID NOT EXIST apart from an empty file. Any bytes without this exact marker are a
// legacy byte-only pre-image and are read back as exists=true (old receipts remain
// readable).
const filePreImageSchema = "compound.file_preimage.v0.1"

// filePreImage is the FileArtifactAdapter's first-class pre-image: a captured file
// state carrying whether the target existed plus its content bytes. It travels
// through the content-addressed artifact store as JSON so the exists-bit survives a
// round-trip (afterWriteFailure rollback AND cross-process RevertIntent both restore
// from these bytes). Content marshals as base64, so arbitrary binary round-trips.
type filePreImage struct {
	Schema  string `json:"schema"`
	Exists  bool   `json:"exists"`
	Content []byte `json:"content,omitempty"`
}

func encodeFilePreImage(exists bool, content []byte) []byte {
	b, _ := json.Marshal(filePreImage{Schema: filePreImageSchema, Exists: exists, Content: content})
	return b
}

// decodeFilePreImage interprets bytes as a file pre-image. A marker-tagged value
// yields its exists-bit + content; anything else (a legacy byte-only pre-image, or a
// raw forward-write body) is treated as exists=true with the bytes as content, so
// old receipts and plain content writes both keep working.
func decodeFilePreImage(b []byte) (exists bool, content []byte) {
	var p filePreImage
	if err := json.Unmarshal(b, &p); err == nil && p.Schema == filePreImageSchema {
		return p.Exists, p.Content
	}
	return true, b
}

// ReadCurrent returns the file's pre-image with a first-class "did not exist" bit. A
// missing file is a real absent pre-image (exists=false), not empty bytes — so a
// rollback of a creation can DELETE the target rather than leave an empty file. An
// existing file is captured as exists=true plus its bytes. It is never an error for
// the target to be missing: staging a brand-new artifact is the normal case.
func (FileArtifactAdapter) ReadCurrent(target envelope.ActionTarget) ([]byte, error) {
	if strings.TrimSpace(target.Path) == "" {
		return nil, fmt.Errorf("file-artifact adapter requires target.path")
	}
	data, err := os.ReadFile(target.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return encodeFilePreImage(false, nil), nil
		}
		return nil, err
	}
	return encodeFilePreImage(true, data), nil
}

// write materializes newValue at Target.Path as opaque bytes. Adapter control
// data is never decoded on the forward path, so a legitimate file whose exact
// content matches filePreImageSchema is still written byte-for-byte.
func (FileArtifactAdapter) write(target envelope.ActionTarget, newValue []byte) error {
	if strings.TrimSpace(target.Path) == "" {
		return fmt.Errorf("file-artifact adapter requires target.path")
	}
	return writeFileArtifact(target.Path, newValue)
}

// restorePreImage is the distinct rollback path used by ExecuteRevert and
// Execute's post-write failure cleanup.
func (FileArtifactAdapter) restorePreImage(target envelope.ActionTarget, preImage []byte) error {
	if strings.TrimSpace(target.Path) == "" {
		return fmt.Errorf("file-artifact adapter requires target.path")
	}
	exists, content := decodeFilePreImage(preImage)
	if !exists {
		// Restoring an absent pre-image: remove the target. A missing target is
		// already the desired state, so a not-exist removal is not an error.
		if err := os.Remove(target.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeFileArtifact(target.Path, content)
}

func writeFileArtifact(path string, content []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, content, 0o644)
}
