package game

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// GameID derives a stable, deterministic ID for a catalog row from the
// tuple (source, sourceKey, kind).
func GameID(source, sourceKey, kind string) string {
	parts := []string{
		strings.TrimSpace(source),
		strings.TrimSpace(sourceKey),
		strings.TrimSpace(kind),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "gmid_" + hex.EncodeToString(sum[:6])
}
