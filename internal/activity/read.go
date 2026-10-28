package activity

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ReadSpanFile loads the persisted span reduction for one UTC day. ok=false (no error) when the day
// has no span file — an absent family is the normal state (sensor off / never drained), not a
// failure. A present-but-unparseable file IS an error: a consumer must never silently read corrupt
// evidence as an empty day.
func ReadSpanFile(home, day string) (SpanFile, bool, error) {
	path := SpanPath(home, day)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SpanFile{}, false, nil
		}
		return SpanFile{}, false, err
	}
	var sf SpanFile
	if err := json.Unmarshal(body, &sf); err != nil {
		return SpanFile{}, false, fmt.Errorf("parse span file %s: %w", path, err)
	}
	return sf, true, nil
}

// LatestSpanDay returns the most recent UTC day with a persisted span file. Day-rotated YYYY-MM-DD
// naming makes byte order and time order agree, so this is the lexicographic max. ok=false when the
// spans dir is absent or holds no day files.
func LatestSpanDay(home string) (string, bool, error) {
	entries, err := os.ReadDir(SpansRoot(home))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	latest := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		day := strings.TrimSuffix(e.Name(), ".json")
		if day == e.Name() || len(day) != len("2006-01-02") {
			continue
		}
		if day > latest {
			latest = day
		}
	}
	if latest == "" {
		return "", false, nil
	}
	return latest, true, nil
}
