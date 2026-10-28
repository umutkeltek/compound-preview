package gamesource

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

func sha256Hex12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

func trimSpace(s string) string { return strings.TrimSpace(s) }

func joinWithUS(parts []string) string { return strings.Join(parts, "\x1f") }

func uniqueObservationRevision(bodyRev string, now time.Time, index int) string {
	if bodyRev == "" {
		bodyRev = "no-body"
	}
	return bodyRev + ".obs" + strconv.FormatInt(now.UnixNano(), 10) + "-" + strconv.Itoa(index)
}
