package harness

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
)

// readOpenCodeAPIKey reads only the selected native API entry. Errors never
// include decoded values or filesystem paths. Call only after attempt mint.
func readOpenCodeAPIKey(path, provider string) (string, error) {
	invalid := errors.New("OPENCODE_AUTH_INVALID")
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || before.Size() > 1<<20 {
		return "", invalid
	}
	owner, ok := before.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Getuid() {
		return "", invalid
	}
	f, err := os.Open(path)
	if err != nil {
		return "", invalid
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() {
		return "", invalid
	}
	body, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return "", invalid
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(body, &entries) != nil {
		return "", invalid
	}
	var selected struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if json.Unmarshal(entries[provider], &selected) != nil || selected.Type != "api" || strings.TrimSpace(selected.Key) == "" || strings.ContainsAny(selected.Key, "\r\n\x00") {
		return "", invalid
	}
	return selected.Key, nil
}
