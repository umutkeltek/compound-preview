package eventstore

import (
	"fmt"
	"path/filepath"
	"strings"
)

const logicalReferencePrefix = "compound://events/"

// Reference is the stable logical identity of one event record. It deliberately
// carries no physical path: loose files are an implementation detail of Store.
type Reference struct {
	id string
}

// ParseReference accepts the three durable compatibility shapes: a bare event
// id, its canonical compound URI, or the historical loose-file reference.
func ParseReference(raw string) (Reference, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return Reference{}, fmt.Errorf("invalid event reference %q", raw)
	}

	id := raw
	switch {
	case strings.HasPrefix(raw, logicalReferencePrefix):
		id = strings.TrimPrefix(raw, logicalReferencePrefix)
	case strings.HasPrefix(raw, "events/"):
		if !strings.HasSuffix(raw, ".json") {
			return Reference{}, fmt.Errorf("historical event reference must end in .json: %q", raw)
		}
		id = strings.TrimSuffix(strings.TrimPrefix(raw, "events/"), ".json")
	}
	return ReferenceForID(id)
}

// ReferenceForID validates an event id and returns its logical reference.
func ReferenceForID(id string) (Reference, error) {
	if id == "" || id != strings.TrimSpace(id) || id == "." || id == ".." ||
		filepath.IsAbs(id) || strings.ContainsAny(id, `/\?#`) || strings.ContainsRune(id, 0) {
		return Reference{}, fmt.Errorf("invalid event id %q", id)
	}
	return Reference{id: id}, nil
}

// ID returns the stable event id without exposing its physical representation.
func (r Reference) ID() string { return r.id }

// String returns the canonical durable reference for this event.
func (r Reference) String() string {
	if r.id == "" {
		return ""
	}
	return logicalReferencePrefix + r.id
}
