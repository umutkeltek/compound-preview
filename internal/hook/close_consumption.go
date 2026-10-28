package hook

import (
	"strings"

	"compound/internal/work"
)

// ConsumptionForClose scans the closing work's transcript for injected claim references.
// The boolean reports readability, not a declaration of application or non-use.
// Missing or unreadable transcripts remain unknown and never block close.
// Both close and SessionEnd use the same matcher so reference observations agree.
func ConsumptionForClose(store *work.Store, homePath, workID, transcriptPath string) (consumed []string, declared bool) {
	if store == nil {
		return nil, false
	}
	if strings.TrimSpace(workID) == "" || strings.TrimSpace(transcriptPath) == "" {
		return nil, false
	}
	return detectSessionConsumption(
		Deps{Store: store, HomePath: homePath},
		Fixture{WorkID: workID, TranscriptPath: transcriptPath},
	)
}
