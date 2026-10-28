package distiller

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const SessionLensesSourceClass = "session_lenses"

type SessionLensesImport struct {
	ID             string
	SourcePath     string
	SourceDigest   string
	Revision       string
	Decisions      int
	Outcomes       int
	Patterns       int
	Skills         int
	Taste          int
	Contradictions int
	TotalItems     int
	EvidenceRefs   []string
}

func LoadSessionLenses(path string) (SessionLensesImport, error) {
	var out SessionLensesImport
	path = strings.TrimSpace(path)
	if path == "" {
		return out, fmt.Errorf("session lenses path required")
	}
	data, digest, err := readDigest(path)
	if err != nil {
		return out, fmt.Errorf("read session lenses %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return out, fmt.Errorf("parse session lenses %s: %w", path, err)
	}
	out.Decisions, err = countLensItems(raw, "decisions")
	if err != nil {
		return out, err
	}
	out.Outcomes, err = countLensItems(raw, "outcomes")
	if err != nil {
		return out, err
	}
	out.Patterns, err = countLensItems(raw, "patterns")
	if err != nil {
		return out, err
	}
	out.Skills, err = countLensItems(raw, "skills")
	if err != nil {
		return out, err
	}
	out.Taste, err = countLensItems(raw, "taste")
	if err != nil {
		return out, err
	}
	out.Contradictions, err = countLensItems(raw, "contradictions")
	if err != nil {
		return out, err
	}
	out.TotalItems = out.Decisions + out.Outcomes + out.Patterns + out.Skills + out.Taste + out.Contradictions
	out.SourcePath = path
	out.SourceDigest = digest
	out.Revision = digest
	out.ID = "session_lenses:" + digestParts(filepath.Base(path), digest)[:12]
	out.EvidenceRefs = []string{"session_lenses:sha256:" + digest}
	return out, nil
}

func countLensItems(raw map[string]json.RawMessage, key string) (int, error) {
	data, ok := raw[key]
	if !ok || len(data) == 0 || string(data) == "null" {
		return 0, nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return 0, fmt.Errorf("session lens %q must be an array: %w", key, err)
	}
	return len(rows), nil
}
