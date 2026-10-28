package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	homepkg "compound/internal/home"
)

// Operator presence: the away-mode key.
//
// The operator-state authority may carry one presence window. Its EFFECTIVE
// state is "away" only while now is inside [from, through); at every other
// instant -- before, after, at through, or with no window at all -- it is
// "present". Policy reads the window, never a cached effective value, so a
// long-lived config cannot carry a stale "away".
//
// Presence never widens anything by itself. It does exactly two things:
//
//  1. It is a KEY for standing grants that ask for it (requires_presence:
//     away|present). A grant that names no presence state is unaffected, so
//     no hold clears unless the operator also wrote a grant that says "only
//     while I am away".
//  2. While away, it NARROWS the L3 autonomy envelope: a path scope clears
//     only local_reversible holds (autonomyEnvelopeNarrowedAway). Sending
//     mail, answering reviews, or calling a payment API then needs an exact
//     capability grant carrying requires_presence: away.
const (
	PresencePresent = "present"
	PresenceAway    = "away"
	PresenceAny     = "any"

	operatorPresenceSourceFile = "operator-state-presence-source.json"
)

// OperatorPresence is the presence window on operator state.
type OperatorPresence struct {
	State   string `json:"state"`
	From    string `json:"from,omitempty"`
	Through string `json:"through,omitempty"`
	SetBy   string `json:"set_by,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Effective is the presence state at now. A nil window, a present window, an
// instant outside [from, through), or a window whose times cannot be read are
// all "present" -- the state that unlocks nothing extra.
func (p *OperatorPresence) Effective(now time.Time) string {
	if p == nil || !strings.EqualFold(strings.TrimSpace(p.State), PresenceAway) {
		return PresencePresent
	}
	from, ferr := time.Parse(time.RFC3339, strings.TrimSpace(p.From))
	through, terr := time.Parse(time.RFC3339, strings.TrimSpace(p.Through))
	if ferr != nil || terr != nil {
		return PresencePresent
	}
	if !now.Before(from) && now.Before(through) {
		return PresenceAway
	}
	return PresencePresent
}

// EffectivePresence is the operator's presence state at now.
func (s OperatorState) EffectivePresence(now time.Time) string {
	return s.Presence.Effective(now)
}

// normalizeOperatorPresence validates a window in place. It runs on every
// operator-state parse, so the import door, `presence set`, and the policy
// loader all refuse the same malformed windows.
func normalizeOperatorPresence(p *OperatorPresence) error {
	if p == nil {
		return nil
	}
	p.State = strings.ToLower(strings.TrimSpace(p.State))
	p.From = strings.TrimSpace(p.From)
	p.Through = strings.TrimSpace(p.Through)
	p.SetBy = strings.TrimSpace(p.SetBy)
	p.Reason = strings.TrimSpace(p.Reason)
	switch p.State {
	case PresencePresent:
		if p.From != "" || p.Through != "" {
			return fmt.Errorf("invalid presence: state present carries no from/through window")
		}
		return nil
	case PresenceAway:
	default:
		return fmt.Errorf("invalid presence.state %q: must be present|away", p.State)
	}
	if p.From == "" || p.Through == "" {
		return fmt.Errorf("invalid presence: state away requires from and through (RFC3339)")
	}
	from, err := time.Parse(time.RFC3339, p.From)
	if err != nil {
		return fmt.Errorf("invalid presence.from: %w", err)
	}
	through, err := time.Parse(time.RFC3339, p.Through)
	if err != nil {
		return fmt.Errorf("invalid presence.through: %w", err)
	}
	if !from.Before(through) {
		return fmt.Errorf("invalid presence: from %s must be before through %s", p.From, p.Through)
	}
	if p.SetBy == "" {
		return fmt.Errorf("invalid presence: an away window requires set_by")
	}
	return nil
}

// PresenceReport is the `compound policy presence show` shape.
type PresenceReport struct {
	AuthorityRef       string            `json:"authority_ref"`
	AuthorityPath      string            `json:"authority_path"`
	Installed          bool              `json:"installed"`
	Now                string            `json:"now"`
	Effective          string            `json:"effective"`
	Presence           *OperatorPresence `json:"presence,omitempty"`
	AwayGatedGrants    int               `json:"away_gated_grants"`
	PresentGatedGrants int               `json:"present_gated_grants"`
}

// PresenceSetReport is the `compound policy presence set` shape: the import
// the set went through, and the window it installed.
type PresenceSetReport struct {
	Import    OperatorStateImportReport `json:"import"`
	Presence  *OperatorPresence         `json:"presence"`
	Effective string                    `json:"effective"`
}

// PresenceCLI is the body of `compound policy presence ...`. cmd/compound
// keeps a one-line delegation so the cmd-compound budget only ratchets down.
func PresenceCLI(args []string, stdout, stderr io.Writer, now time.Time) int {
	homePath, err := homepkg.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return runPresenceCLI(homePath, args, stdout, stderr, now)
}

func presenceUsage() string {
	return "usage: compound policy presence show [--json]\n" +
		"       compound policy presence set --state away --from <rfc3339> --through <rfc3339> --by <actor> [--reason <text>]\n" +
		"       compound policy presence set --state present --by <actor> [--reason <text>]"
}

func runPresenceCLI(homePath string, args []string, stdout, stderr io.Writer, now time.Time) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, presenceUsage())
		return 2
	}
	switch args[0] {
	case "show":
		jsonOut := false
		for _, arg := range args[1:] {
			if arg != "--json" {
				fmt.Fprintln(stderr, presenceUsage())
				return 2
			}
			jsonOut = true
		}
		report, err := buildPresenceReport(homePath, now)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if jsonOut {
			return writePresenceJSON(stdout, stderr, report)
		}
		printPresenceReport(stdout, report)
		return 0
	case "set":
		presence, code := parsePresenceSetArgs(args[1:], stderr)
		if code != 0 {
			return code
		}
		report, err := SetOperatorPresence(homePath, presence, now)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return writePresenceJSON(stdout, stderr, report)
	default:
		fmt.Fprintln(stderr, presenceUsage())
		return 2
	}
}

func parsePresenceSetArgs(args []string, stderr io.Writer) (OperatorPresence, int) {
	var p OperatorPresence
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "--state", "--from", "--through", "--by", "--reason":
		default:
			fmt.Fprintf(stderr, "error: unknown flag %s\n%s\n", flag, presenceUsage())
			return p, 2
		}
		if i+1 >= len(args) {
			fmt.Fprintf(stderr, "error: %s needs a value\n%s\n", flag, presenceUsage())
			return p, 2
		}
		i++
		value := strings.TrimSpace(args[i])
		switch flag {
		case "--state":
			p.State = strings.ToLower(value)
		case "--from":
			p.From = value
		case "--through":
			p.Through = value
		case "--by":
			p.SetBy = value
		case "--reason":
			p.Reason = value
		}
	}
	if p.SetBy == "" {
		fmt.Fprintf(stderr, "error: --by <actor> is required\n%s\n", presenceUsage())
		return p, 2
	}
	switch p.State {
	case PresenceAway:
		if p.From == "" || p.Through == "" {
			fmt.Fprintf(stderr, "error: --state away requires --from and --through\n%s\n", presenceUsage())
			return p, 2
		}
	case PresencePresent:
		if p.From != "" || p.Through != "" {
			fmt.Fprintf(stderr, "error: --state present clears the window; it takes no --from/--through\n%s\n", presenceUsage())
			return p, 2
		}
	default:
		fmt.Fprintf(stderr, "error: --state must be away|present\n%s\n", presenceUsage())
		return p, 2
	}
	return p, 0
}

// SetOperatorPresence installs a presence window through the SAME door
// `policy import-operator-state` uses: it rebuilds the full operator-state
// document from the installed authority (every other key kept verbatim),
// writes it as a source file under registries/, and imports that file. The
// import re-parses and validates the whole document and appends its
// source-authority record, so a set is auditable exactly like an import.
// It refuses when no operator state is installed: creating one here would
// also install a default autonomy ceiling, and a presence change must not
// move any decision by itself.
func SetOperatorPresence(homePath string, presence OperatorPresence, now time.Time) (PresenceSetReport, error) {
	if strings.TrimSpace(homePath) == "" {
		return PresenceSetReport{}, fmt.Errorf("COMPOUND_HOME required")
	}
	if err := normalizePresenceTimes(&presence); err != nil {
		return PresenceSetReport{}, err
	}
	if err := normalizeOperatorPresence(&presence); err != nil {
		return PresenceSetReport{}, err
	}
	var report PresenceSetReport
	err := homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		current, err := os.ReadFile(OperatorStatePath(homePath))
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("no operator state installed at %s; import one first with `compound policy import-operator-state <operator-state.json>`", OperatorStatePath(homePath))
			}
			return err
		}
		doc := map[string]any{}
		dec := json.NewDecoder(bytes.NewReader(current))
		dec.UseNumber() // keep every other key's numbers byte-faithful
		if err := dec.Decode(&doc); err != nil {
			return fmt.Errorf("parse installed operator state: %w", err)
		}
		doc["presence"] = presence
		body, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		sourcePath := filepath.Join(homePath, "registries", operatorPresenceSourceFile)
		if err := os.WriteFile(sourcePath, append(body, '\n'), 0o644); err != nil {
			return err
		}
		imported, err := importOperatorStateLocked(homePath, sourcePath, now)
		if err != nil {
			return err
		}
		installed := presence
		report = PresenceSetReport{Import: imported, Presence: &installed, Effective: installed.Effective(now)}
		return nil
	})
	return report, err
}

// normalizePresenceTimes stores CLI-supplied instants as UTC RFC3339 so the
// window reads the same on every surface regardless of the offset typed.
func normalizePresenceTimes(p *OperatorPresence) error {
	for _, field := range []struct {
		name  string
		value *string
	}{{"from", &p.From}, {"through", &p.Through}} {
		raw := strings.TrimSpace(*field.value)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return fmt.Errorf("invalid presence.%s: %w", field.name, err)
		}
		*field.value = t.UTC().Format(time.RFC3339)
	}
	return nil
}

func buildPresenceReport(homePath string, now time.Time) (PresenceReport, error) {
	report := PresenceReport{
		AuthorityRef:  OperatorStateAuthorityRef,
		AuthorityPath: OperatorStatePath(homePath),
		Now:           now.UTC().Format(time.RFC3339),
		Effective:     PresencePresent,
	}
	state, err := LoadOperatorStateFromHome(homePath)
	if err != nil && !os.IsNotExist(err) {
		return PresenceReport{}, err
	}
	if err == nil {
		report.Installed = true
		report.Presence = state.Presence
		report.Effective = state.EffectivePresence(now)
	}
	report.AwayGatedGrants, report.PresentGatedGrants = countPresenceGatedGrants(homePath)
	return report, nil
}

func countPresenceGatedGrants(homePath string) (away, present int) {
	registry, err := LoadStandingGrantsFromHome(homePath)
	if err != nil {
		return 0, 0
	}
	for _, g := range registry.Grants {
		switch strings.ToLower(strings.TrimSpace(g.RequiresPresence)) {
		case PresenceAway:
			away++
		case PresencePresent:
			present++
		}
	}
	return away, present
}

func printPresenceReport(w io.Writer, r PresenceReport) {
	fmt.Fprintf(w, "operator presence authority: %s\n", r.AuthorityRef)
	fmt.Fprintf(w, "installed: %t\n", r.Installed)
	fmt.Fprintf(w, "effective: %s (at %s)\n", r.Effective, r.Now)
	switch {
	case r.Presence == nil:
		fmt.Fprintln(w, "window: none")
	case r.Presence.State == PresenceAway:
		fmt.Fprintf(w, "window: away from %s through %s (exclusive), set_by %s\n", r.Presence.From, r.Presence.Through, r.Presence.SetBy)
	default:
		fmt.Fprintf(w, "window: none (state %s, set_by %s)\n", r.Presence.State, r.Presence.SetBy)
	}
	if r.Presence != nil && r.Presence.Reason != "" {
		fmt.Fprintf(w, "reason: %s\n", r.Presence.Reason)
	}
	fmt.Fprintf(w, "grants gated: away=%d present=%d\n", r.AwayGatedGrants, r.PresentGatedGrants)
}

func writePresenceJSON(stdout, stderr io.Writer, v any) int {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(body))
	return 0
}

// RenderPresenceDoctorLine prints ONE doctor line while an away window is in
// force -- naming the window and how many standing grants it unlocks -- and
// nothing otherwise. Read-only; an unreadable state prints nothing here
// because the policy loader already fails loudly on it.
func RenderPresenceDoctorLine(w io.Writer, homePath string, now time.Time) {
	state, err := LoadOperatorStateFromHome(homePath)
	if err != nil || state.EffectivePresence(now) != PresenceAway {
		return
	}
	away, _ := countPresenceGatedGrants(homePath)
	p := state.Presence
	fmt.Fprintf(w, "- operator presence: AWAY %s -> %s (set_by %s); %d grant(s) gated on requires_presence=away\n", p.From, p.Through, p.SetBy, away)
}
