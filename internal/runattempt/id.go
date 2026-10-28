// Package runattempt is the primitive the WorkEnvelope RunAttemptIDs slot was reserved for.
// A RunAttempt is the concrete record of one harness attempt against one work_id -- distinct
// from the legacy `AdvanceInput.RunID` chain (panel prun_*, observation run_*) which recorded
// what happened in prose alongside ActionIntent ids. RunAttempts are first-class objects:
// they carry their own id (`ratt_<UTC>_<hex>`), store, status, and lifecycle. S1 deliberately
// omits context_pack_ref / workspace / command / heartbeat / transcript_ref / diff_ref /
// test_ref / proof_refs -- those land when a real harness adapter produces them, not before.
// See docs/architecture/runtime.md for the WorkEnvelope schema and the v0.1 mix this slice
// retires (the v0.1 comment on ActionIntentIDs).
package runattempt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"
)

const (
	// SchemaVersion is the current on-disk writer schema for RunAttempt
	// artifacts. v0.2 introduces the load-bearing terminalization reservation
	// and exact completion proof fields. Launch authority remains in its
	// independently versioned immutable sidecars.
	SchemaVersion = "compound.runattempt.v0.2"
	// LegacySchemaVersionV01 remains readable during the explicit compatibility
	// window. Ordinary mutations preserve it; preparing an exact terminalization
	// reservation performs the narrow v0.1 -> v0.2 migration.
	LegacySchemaVersionV01 = "compound.runattempt.v0.1"
)

var acceptedSchemaVersions = map[string]bool{
	SchemaVersion:          true,
	LegacySchemaVersionV01: true,
}

// AcceptsSchemaVersion reports whether this binary can safely decode a
// RunAttempt writer generation. Unknown future versions fail closed.
func AcceptsSchemaVersion(version string) bool {
	return acceptedSchemaVersions[version]
}

// Prefix is the only accepted RunAttempt id prefix. Tightened in envelope.validateRunAttemptIDs.
const Prefix = "ratt_"

// idPattern locks the shape so legacy ids (prun_*, run_*, free strings) cannot masquerade as
// RunAttempts -- this is the same trap the v0.1 ActionIntentIDs mix fell into and the one
// envelope-side rule that prevents a repeat.
var idPattern = regexp.MustCompile(`^ratt_\d{8}T\d{6}Z_[0-9a-f]{8}$`)

// Valid reports whether s is a syntactically well-formed RunAttempt id.
func Valid(s string) bool {
	return idPattern.MatchString(s)
}

// Mint returns a fresh RunAttempt id of the form `ratt_<UTC>_<8 hex>` derived from now. The
// hex suffix is sha256(rand.Reader output)[0:4] so concurrent mints never collide and the id
// is reproducible from its prefix alone (the timestamp is the only time input).
func Mint(now time.Time) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("runattempt mint: read random: %w", err)
	}
	sum := sha256.Sum256(raw[:])
	suffix := hex.EncodeToString(sum[:])[:8]
	return fmt.Sprintf("ratt_%s_%s", now.UTC().Format("20060102T150405Z"), suffix), nil
}

// MintDeterministic returns a RunAttempt id derived deterministically from a
// seed string (e.g. a codex job id). The same seed always produces the same
// id, giving callers idempotency without a separate dedup index: observing
// the same job twice produces the same ratt_ id, and the store's write is a
// byte-identical overwrite. The timestamp component comes from now (UTC),
// matching Mint's layout; the hex suffix is sha256(seed)[:8] instead of
// sha256(rand)[:8].
//
// Pinned by TestMintDeterministicSameSeedSameID and
// TestMintDeterministicDifferentSeedDifferentID.
func MintDeterministic(seed string, now time.Time) string {
	sum := sha256.Sum256([]byte(seed))
	suffix := hex.EncodeToString(sum[:])[:8]
	return fmt.Sprintf("ratt_%s_%s", now.UTC().Format("20060102T150405Z"), suffix)
}
