package envelope

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MarshalJSON keeps the legacy known-effect bytes exactly. A failed remote
// operation with unknown effects must not emit either boolean assertion.
func (d StateDelta) MarshalJSON() ([]byte, error) {
	type legacy StateDelta
	if d.ChangeKnowledge != "unknown" {
		return json.Marshal(legacy(d))
	}
	if d.Changed {
		return nil, fmt.Errorf("unknown effects cannot assert changed")
	}
	return json.Marshal(struct {
		ChangeKnowledge string   `json:"change_knowledge"`
		Summary         string   `json:"summary"`
		FilesChanged    []string `json:"files_changed"`
		Artifacts       []string `json:"artifacts"`
		Verification    []string `json:"verification"`
	}{d.ChangeKnowledge, d.Summary, d.FilesChanged, d.Artifacts, d.Verification})
}

func (d *StateDelta) UnmarshalJSON(body []byte) error {
	type legacy StateDelta
	var decoded legacy
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	if decoded.ChangeKnowledge == "unknown" {
		if _, exists := fields["changed"]; exists {
			return fmt.Errorf("unknown effects must omit changed")
		}
	}
	*d = StateDelta(decoded)
	return nil
}
