package routeev

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// evcardDir is the rebuildable, content-addressed home for composed EV cards:
// COMPOUND_HOME/observed/route-evcards. It mirrors internal/mem's tool-output/capture custody
// posture (observed/tool-output, observed/capture): a card is EXHAUST, never a ledger byte, so
// writing one here can never perturb EngineDoctor's ledger-immutability invariant. This file is
// deliberately the ONLY I/O in this package (card.go stays a pure function of its argument, per
// its own package doc) — Store/Read never call Compose and never resolve a real-door input;
// they only persist and retrieve whatever Card the caller (S2, `compound route ev`) already
// composed.
const evcardDir = "observed/route-evcards"

// CardRef is the content id for a stored Card: "sha256:" + hex(sha256(body)). The same body
// always yields the same ref, so composing the identical card twice (same inputs, by Compose's
// own determinism) resolves to the same ref and the same idempotent write.
func CardRef(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Store marshals card and writes it content-addressed under
// COMPOUND_HOME/observed/route-evcards, returning its ref and the file path written. Rewriting
// an identical card (same bytes) is an idempotent no-op write to the same path. It never mints a
// memory candidate and never appends a ledger byte.
func Store(homePath string, card Card) (ref, path string, err error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return "", "", fmt.Errorf("ev card store: COMPOUND_HOME required")
	}
	body, err := json.Marshal(card)
	if err != nil {
		return "", "", fmt.Errorf("marshal ev card: %w", err)
	}
	ref = CardRef(body)
	dir := filepath.Join(homePath, evcardDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, ref)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", "", err
	}
	return ref, path, nil
}

// ReadBytes returns the exact bytes stored for ref. A missing ref is an EXPLICIT error (never a
// silent nil): the content-addressed blob either round-trips byte-identically or the caller is
// told it is gone. ref is validated to the strict "sha256:<64 hex>" shape before any read, so a
// path-shaped value can never escape the blob dir.
func ReadBytes(homePath, ref string) ([]byte, error) {
	if err := validCardRef(ref); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(homePath, evcardDir, ref))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ev card %s not found (rebuildable blob missing)", ref)
		}
		return nil, err
	}
	return body, nil
}

// Read reads a stored card back and decodes it.
func Read(homePath, ref string) (Card, error) {
	body, err := ReadBytes(homePath, ref)
	if err != nil {
		return Card{}, err
	}
	var card Card
	if err := json.Unmarshal(body, &card); err != nil {
		return Card{}, fmt.Errorf("decode ev card %s: %w", ref, err)
	}
	return card, nil
}

// validCardRef enforces the exact content-id shape ("sha256:" + 64 lowercase hex chars) shared
// by every content-addressed blob door in this codebase (mem's tool-output, capture, …). It is
// both a correctness guard and a path-traversal guard: ref keys a filename under a blob dir, so
// a crafted value ("../…", a separator) must never reach os.ReadFile.
func validCardRef(ref string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(ref, prefix) {
		return fmt.Errorf("ev card ref must be %q-prefixed, got %q", prefix, ref)
	}
	hexPart := ref[len(prefix):]
	if len(hexPart) != 64 {
		return fmt.Errorf("ev card ref must be sha256:<64 hex>, got %q", ref)
	}
	for _, r := range hexPart {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return fmt.Errorf("ev card ref has a non-hex character: %q", ref)
		}
	}
	return nil
}
