package capgraph

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// LoadManifestByFlag dispatches to the right loader based on which flag is
// set, mirroring the original cmd/compound switch. Lives in capgraph so
// every loader it can reach is already a capgraph type. Returns a usage
// error (returned, not os.Exit'd) when no flag matches.
func LoadManifestByFlag(osSkillPath, osPluginBoardPath, brainRecipeDir string, positional []string, now time.Time) (SkillManifest, error) {
	switch {
	case brainRecipeDir != "" && osSkillPath == "" && osPluginBoardPath == "" && len(positional) == 0:
		return LoadBrainRecipeTree(brainRecipeDir, now)
	case osSkillPath != "" && brainRecipeDir == "" && osPluginBoardPath == "" && len(positional) == 0:
		return LoadOSSkillManifest(osSkillPath, now)
	case osPluginBoardPath != "" && brainRecipeDir == "" && osSkillPath == "" && len(positional) == 0:
		return LoadOSPluginBoard(osPluginBoardPath, now)
	case osSkillPath == "" && osPluginBoardPath == "" && brainRecipeDir == "" && len(positional) == 1:
		return LoadSkillManifestForPath(positional[0], now)
	}
	return SkillManifest{}, fmt.Errorf("usage: compound capability import-skills <skill-manifest.yaml> | --os-skill-json <skill-manifest.json> | --os-plugin-board-json <skills-selection-board.json> | --brain-recipe-dir <dir> [--dry-run]")
}

// RunImportSkillsDry executes the dry-run branch of `compound capability
// import-skills --brain-recipe-dir <dir> --dry-run`: load prior source
// authority, find the latest record for the manifest's source path, and
// print the drift probe. Returns 0 on success, 1 on lookup errors.
//
// Slice 7 note: dry-run does NOT need sourceobs — only capgraph types — so
// it can live in capgraph cleanly. The non-dry-run path still relies on
// sourceobs.Apply + sourceobs.FromSkillManifest, which cmd/compound wires
// (cmd already imports both). Format-only `sourceobs` references live in
// RunImportSkillsFinalize which takes string/slice params, not types.
func RunImportSkillsDry(homePath string, manifest SkillManifest, stdout, stderr io.Writer) int {
	if strings.TrimSpace(manifest.SourceRef.Path) == "" {
		fmt.Fprintln(stderr, "error: dry-run requires --brain-recipe-dir (a path SourceRef can carry)")
		return 2
	}
	records, err := LoadSourceAuthority(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	previous, found := LatestSourceAuthorityForPath(records, manifest.SourceRef.Path)
	previousRevision := ""
	previousID := ""
	if found {
		previousRevision = previous.SourceRevision
		previousID = previous.ID
	}
	drift := !found || previousRevision != manifest.SourceRef.Revision
	fmt.Fprintf(stdout, "# capability import-skills brain-recipe-tree dry-run %s\n", manifest.ID)
	fmt.Fprintf(stdout, "- source_path: %s\n", manifest.SourceRef.Path)
	fmt.Fprintf(stdout, "- source_revision: %s\n", manifest.SourceRef.Revision)
	fmt.Fprintf(stdout, "- previous_source_authority: %s\n", previousID)
	fmt.Fprintf(stdout, "- previous_revision: %s\n", previousRevision)
	fmt.Fprintf(stdout, "- drift: %t\n", drift)
	fmt.Fprintf(stdout, "- nodes: %d\n", len(manifest.Skills))
	return 0
}

// RunImportSkillsFinalize prints the post-apply report — status, checks,
// node ids, edge ids, source-observation id, candidate ids, no-promote
// ids, authority id. Takes the sourceobs-derived IDs as primitives so
// capgraph does not depend on internal/sourceobs (which depends on capgraph —
// cycle). The caller (cmd/compound) is the only place that imports both.
//
// Pass src as a value type — slice fields are referenced by the caller
// (no defensive copy needed since cmd/compound owns them).
func RunImportSkillsFinalize(manifest SkillManifest, report SkillManifestReport, sourceObsID string, candidateIDs, noPromoteIDs []string, authID string, stdout io.Writer) {
	fmt.Fprintf(stdout, "# capability import-skills %s\n", report.ManifestID)
	fmt.Fprintf(stdout, "- status: %s\n", report.Status)
	for _, check := range report.Checks {
		fmt.Fprintf(stdout, "- %s: %s", check.Name, check.Status)
		if check.Detail != "" {
			fmt.Fprintf(stdout, " (%s)", check.Detail)
		}
		fmt.Fprintln(stdout)
	}
	for _, id := range report.NodeIDs {
		fmt.Fprintf(stdout, "- node: %s\n", id)
	}
	for _, id := range report.EdgeIDs {
		fmt.Fprintf(stdout, "- edge: %s\n", id)
	}
	fmt.Fprintf(stdout, "- source_observation: %s\n", sourceObsID)
	for _, id := range candidateIDs {
		fmt.Fprintf(stdout, "- source_candidate: %s\n", id)
	}
	for _, id := range noPromoteIDs {
		fmt.Fprintf(stdout, "- source_no_promote: %s\n", id)
	}
	fmt.Fprintf(stdout, "- source_authority: %s\n", authID)
}

// LatestSourceAuthorityForPath is the dry-run probe helper. Returns the
// newest SourceAuthorityRecord matching sourcePath; found=false on no match.
func LatestSourceAuthorityForPath(records []SourceAuthorityRecord, sourcePath string) (SourceAuthorityRecord, bool) {
	sourcePath = strings.TrimSpace(sourcePath)
	var latest SourceAuthorityRecord
	var latestCreated time.Time
	found := false
	for _, rec := range records {
		if strings.TrimSpace(rec.SourcePath) != sourcePath {
			continue
		}
		created, perr := time.Parse(time.RFC3339, rec.CreatedAt)
		if perr != nil {
			created = time.Time{}
		}
		if !found || created.After(latestCreated) || created.Equal(latestCreated) {
			latest = rec
			latestCreated = created
			found = true
		}
	}
	return latest, found
}
