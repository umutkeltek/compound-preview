package harness

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/envelope"
)

// RenderInstructionPackWithContractRef is the explicit binding seam. Callers
// resolve an immutable ContextPack/contract pair through the contract owner first.
func RenderInstructionPackWithContractRef(homePath, harnessName, preset, ref string, now time.Time) (InstructionPack, error) {
	registered, ok := DefaultRegistry().Get(harnessName)
	if !ok {
		return InstructionPack{}, fmt.Errorf("unknown instruction-pack harness %q", harnessName)
	}
	harnessName = registered.Name
	if strings.TrimSpace(ref) == "" {
		return InstructionPack{}, fmt.Errorf("instruction-pack contract ref required")
	}
	pack, err := renderInstructionPack(homePath, harnessName, preset, now, ref)
	if err != nil {
		return InstructionPack{}, err
	}
	if err := writeInstructionPackAtomic(InstructionPackPath(homePath, pack.Harness, pack.Preset), pack); err != nil {
		return InstructionPack{}, err
	}
	return pack, nil
}

// ReadInstructionPackContract resolves only a validated, context-scoped projection.
func ReadInstructionPackContract(homePath, harnessName, ref string) (InstructionPackRosterRef, error) {
	// This is a mechanical mirror of contract.ScopedEffectiveContractPath to avoid
	// the existing contract -> harness import cycle. Never fall back to global latest.
	parts := strings.Split(ref, "/")
	if len(parts) != 5 || parts[0] != "contracts" || parts[1] != "effective" ||
		parts[2] != "scopes" || parts[4] != "latest.json" {
		return InstructionPackRosterRef{}, fmt.Errorf("instruction-pack contract is not context-bound; render with --context-pack")
	}
	key, err := hex.DecodeString(parts[3])
	if err != nil || len(key) != 32 || parts[3] != strings.ToLower(parts[3]) {
		return InstructionPackRosterRef{}, fmt.Errorf("invalid instruction-pack contract context key")
	}
	body, err := os.ReadFile(filepath.Join(homePath, filepath.FromSlash(ref)))
	if err != nil {
		return InstructionPackRosterRef{}, fmt.Errorf("read instruction-pack contract: %w", err)
	}
	var effective envelope.EffectiveContract
	if err := json.Unmarshal(body, &effective); err != nil {
		return InstructionPackRosterRef{}, fmt.Errorf("decode instruction-pack contract: %w", err)
	}
	if err := effective.Validate(); err != nil {
		return InstructionPackRosterRef{}, err
	}
	if effective.Harness != harnessName || effective.ContextKey != parts[3] {
		return InstructionPackRosterRef{}, fmt.Errorf("instruction-pack contract context identity mismatch")
	}
	// A scoped latest file is only a projection. It cannot mint contract authority
	// by being schema-valid or by being hashed into a freshly rendered pack.
	unsealed := effective
	unsealed.ID = ""
	canonical, err := json.Marshal(unsealed)
	if err != nil {
		return InstructionPackRosterRef{}, err
	}
	if effective.ID != "contract_"+digest(canonical)[:16] {
		return InstructionPackRosterRef{}, fmt.Errorf("instruction-pack contract sealed identity mismatch")
	}
	createdAt, err := time.Parse(time.RFC3339, effective.CreatedAt)
	if err != nil {
		return InstructionPackRosterRef{}, fmt.Errorf("instruction-pack contract creation time: %w", err)
	}
	immutablePath := filepath.Join(homePath, "contracts", "effective", createdAt.UTC().Format("2006-01-02"), effective.ID+".json")
	immutable, err := os.ReadFile(immutablePath)
	if err != nil {
		return InstructionPackRosterRef{}, fmt.Errorf("read immutable instruction-pack contract: %w", err)
	}
	if !bytes.Equal(body, immutable) {
		return InstructionPackRosterRef{}, fmt.Errorf("instruction-pack contract projection differs from immutable artifact")
	}
	// Generation identity remains fully verified above. The installed consumer
	// tracks authority semantics, not the time an identical snapshot was sealed.
	unsealed.CreatedAt = ""
	semantic, err := json.Marshal(unsealed)
	if err != nil {
		return InstructionPackRosterRef{}, err
	}
	return InstructionPackRosterRef{Ref: ref, Revision: "sha256:" + digest(semantic)}, nil
}
