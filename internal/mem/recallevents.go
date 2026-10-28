package mem

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// RecallEvent represents a single recall event record.
type RecallEvent struct {
	Ts        string   `json:"ts"`
	Session   string   `json:"session"`
	QueryHash string   `json:"query_hash"`
	ClaimIDs  []string `json:"claim_ids"`
	Engine    string   `json:"engine,omitempty"`
}

// AppendRecallEvent validates required fields and appends one JSON line to the recall-events log.
func AppendRecallEvent(home string, ev RecallEvent) error {
	if ev.Ts == "" {
		return fmt.Errorf("ts is required")
	}
	if _, err := time.Parse(time.RFC3339, ev.Ts); err != nil {
		return fmt.Errorf("ts must be RFC3339: %w", err)
	}
	if ev.QueryHash == "" {
		return fmt.Errorf("query_hash is required")
	}

	dir := home + "/ledger"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create ledger directory: %w", err)
	}

	f, err := os.OpenFile(home+"/ledger/recall-events.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open recall events file: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal recall event: %w", err)
	}

	_, err = f.Write(append(data, '\n'))
	return err
}

// ReadRecallEvents reads all valid JSON lines from the recall-events log.
func ReadRecallEvents(home string) ([]RecallEvent, error) {
	f, err := os.Open(home + "/ledger/recall-events.jsonl")
	if err != nil {
		if os.IsNotExist(err) {
			return []RecallEvent{}, nil
		}
		return nil, fmt.Errorf("open recall events file: %w", err)
	}
	defer f.Close()

	var events []RecallEvent
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if line == "" {
			continue
		}
		var ev RecallEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("malformed JSON at line %d: %w", lineNum, err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read recall events file: %w", err)
	}

	return events, nil
}
