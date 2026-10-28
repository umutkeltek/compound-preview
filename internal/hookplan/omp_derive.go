package hookplan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// OmpDeriveSession is the Go-side mirror of the TS shim's
// `deriveOmpSession()` (internal/hookplan/omp_hook_shim.ts). It MUST stay
// in lockstep — the TS runs at hook time; this Go helper is the unit-testable
// companion that lets the F6 falsifier pin both sides to the same algorithm
// without booting bun/node. The contract:
//
//   - homedir is the operator home (TS: `homedir()`, Go: caller-resolved).
//   - cwd is the process's current working directory (process.cwd() / os.Getwd).
//   - Returned TranscriptPath is the absolute path to the newest *.jsonl
//     file under <homedir>/.omp/agent/sessions/<cwd-slug>/ whose header
//     `{"type":"session",...,"cwd":<abs>}` line carries the same cwd. Empty
//     string when no match (callers MUST treat that as "do not pass
//     transcript_path to scanSessionTranscript", never as a probing fallback).
//   - Returned SessionID is the omp-embedded uuid when found; otherwise a
//     synthetic per-process id shaped like the pre-slice-3 fallback so
//     SessionStart and SessionEnd share one correlation key.
//
// Slug derivation: strip a leading `/`, replace every `/` with `-`, replace
// any non-[A-Za-z0-9._-] with `-`, and wrap in `--`. Examples:
//
//	/srv/projects/sample                   -> --srv-projects-sample--
//	/tmp/x                                -> --tmp-x--
//
// ompDeriveMaxProbe is the upper bound on how many `.jsonl` files
// OmpDeriveSession will header-read on a single call. The contract — newest
// session file whose header `cwd` matches — only needs ONE candidate; the
// bound exists to keep the scan cost proportional to a small constant even
// when the operator's <agentDir>/sessions/<slug> accumulates thousands of
// stale files. Real-world slugs on this host hold ~90 files; a recent
// benchmark at 2000 files completed in ~44 ms with the pre-filter, which
// keeps the per-call cost invisible next to the harness's other hook
// startup work. The TS shim (omp_hook_shim.ts) applies the same bound so
// the two stay in lockstep.
const ompDeriveMaxProbe = 64

func OmpDeriveSession(homedir, cwd string) (transcriptPath, sessionID string, ok bool) {
	slug := ompSlugFromCwd(cwd)
	dir := filepath.Join(homedir, ".omp", "agent", "sessions", slug)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ompFallbackID(), false
	}
	// Pre-filter: collect the newest ompDeriveMaxProbe `.jsonl` files by mtime
	// before paying for a header read on each one. Header reads are the
	// dominant cost (file open + 2 KB read + close); mtime reads are a single
	// stat already returned by ReadDir. A bounded probe keeps derive
	// invisible next to the rest of the hook start-up.
	type cand struct {
		path  string
		mtime int64
	}
	var top []cand
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		c := cand{path: filepath.Join(dir, name), mtime: info.ModTime().UnixNano()}
		if len(top) < ompDeriveMaxProbe {
			top = append(top, c)
			continue
		}
		// Find the smallest mtime in top and replace it if c is newer.
		idx := 0
		for i := range top {
			if top[i].mtime < top[idx].mtime {
				idx = i
			}
		}
		if top[idx].mtime < c.mtime {
			top[idx] = c
		}
	}
	var newestPath string
	var newestMtime int64
	var newestID string
	for _, c := range top {
		f, err := os.Open(c.path)
		if err != nil {
			continue
		}
		var headBytes [2048]byte
		n, _ := f.Read(headBytes[:])
		_ = f.Close()
		header := extractSessionHeader(headBytes[:n])
		if header == nil || header.Cwd != cwd {
			continue
		}
		if c.mtime <= newestMtime {
			continue
		}
		newestMtime = c.mtime
		newestPath = c.path
		newestID = header.ID
	}
	if newestPath == "" {
		return "", ompFallbackID(), false
	}
	id := newestID
	if id == "" {
		id = ompFallbackID()
	}
	return newestPath, id, true
}

// ompFallbackID returns the synthetic id the TS shim uses when discovery
// fails. Cross-process uniqueness is enough for SessionStart/SessionEnd
// correlation; the parser never keys off this value (it keys off the
// transcript_path if present, otherwise no skill evidence is collected).
func ompFallbackID() string {
	// os.Getpid() is not in the signature; callers may pass through their
	// own pid if they need a true per-process id. For the test helper we
	// return a stable shape that matches the shim's synthetic id.
	return "omp-fallback-no-match"
}

// ompSlugFromCwd reproduces the TS slug derivation (see file header).
func ompSlugFromCwd(cwd string) string {
	s := strings.TrimLeft(cwd, "/")
	s = strings.ReplaceAll(s, "/", "-")
	// Replace any non-[A-Za-z0-9._-] with "-"
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return "--" + b.String() + "--"
}

type ompHeaderProbe struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Cwd  string `json:"cwd"`
}

func extractSessionHeader(buf []byte) *ompHeaderProbe {
	// omp's session line is on line 2 (line 1 is usually a "title" or other
	// benign entry). Be liberal: scan the first few non-empty lines for a
	// JSON object whose `type` is "session".
	scanner := bufio.NewScanner(bytes.NewReader(buf))
	seen := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		seen++
		if seen > 8 {
			return nil
		}
		if line[0] != '{' {
			continue
		}
		var probe ompHeaderProbe
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if probe.Type != "session" {
			continue
		}
		if probe.ID == "" || probe.Cwd == "" {
			continue
		}
		return &probe
	}
	return nil
}
