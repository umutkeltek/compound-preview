package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"compound/internal/envelope"
	homepkg "compound/internal/home"
)

// The shell verb catalog names what a vendor CLI invocation DOES, so a standing
// grant can tell reading mail from sending it.
//
// Without it the routine-read lane sees `gog gmail search` and `gog gmail send`
// as the same unknown verb, and `curl GET api.paddle.com` and `curl POST
// api.paddle.com` as the same network command, each bound to a per-command
// hash destination. No grant can be drawn over a family of such commands, so
// unattended work either holds on every read or gets a grant wide enough to
// send. A catalog row yields a capability (`gmail.message.send`), a mutation
// class, and a semantic destination (`gmail-account:<account>`) a grant can
// scope to. The command hash is kept as evidence.
//
// The catalog only RENAMES what the routine-read lane would call unknown or
// generic network; it never widens what the lane admits around it:
//   - the operator-gated roster runs first, and a gated command is untouched;
//   - a segment that writes through a redirection, splices quotes, changes PATH
//     or the loader, or runs a relative executable never matches a row;
//   - a command with an expansion the shell may split into new words (unquoted
//     `$x`, a glob, "$@") never matches: the visible words are not the argv;
//   - a command matches only when every segment is a catalog row or a local
//     read/governed segment. One unknown, network, install, or destructive
//     neighbour and the whole command keeps exactly today's classification, so
//     a grant on a catalog capability can never carry an uncatalogued action;
//   - two different non-read catalog actions in one command also keep today's
//     classification: one intent carries one capability and one destination.
//
// Matching is an argv prefix over the positional words: `pattern` lists the
// subcommand words in order ("|" separates alternatives, "*" is any one word,
// a trailing "*" is the open tail every pattern already has). Flags may sit
// between subcommand words only when the row knows their arity (value_flags,
// bool_flags, or the `--flag=value` form); an unknown flag before the pattern
// completes means the row does not match, because its value could be the very
// word that picks the subcommand. Rows are tried in order and the first match
// wins: extension rows first, then the built-ins, which list the more severe
// shape of a family before the read.
//
// The operator teaches new CLIs in COMPOUND_HOME/config/shell-verb-catalog.yaml,
// merged over the built-ins (a row with a built-in's id replaces it). A file
// with any invalid row is rejected whole, with an error naming the row; the
// built-ins keep classifying. A half-applied teaching is worse than none,
// because the operator believes it is in force.

const (
	ShellVerbCatalogSchema   = "compound.shell_verb_catalog.v0.1"
	shellVerbCatalogFileName = "shell-verb-catalog.yaml"
	// shellVerbCatalogUnresolved stands in for a destination value the command
	// does not state literally. A grant naming it would be meaningless.
	shellVerbCatalogUnresolved = "unresolved"
	// ShellVerbCatalogFallback is the row id reported when no row matched.
	ShellVerbCatalogFallback = "fallback"
)

// ShellVerbCatalogRow is one taught command shape and what it means.
type ShellVerbCatalogRow struct {
	ID string `yaml:"id"`
	// Binary is the program name, or alternatives joined by "|".
	Binary  string `yaml:"binary"`
	Pattern string `yaml:"pattern,omitempty"`
	// NoSubcommand requires that no positional word follows the pattern, so
	// `asc reviews --app X` (the implicit list) does not also cover a future
	// `asc reviews <new-subcommand>`.
	NoSubcommand bool     `yaml:"no_subcommand,omitempty"`
	ValueFlags   []string `yaml:"value_flags,omitempty"`
	BoolFlags    []string `yaml:"bool_flags,omitempty"`
	// RequireFlags must all be set (bare or `=true`; the last occurrence wins).
	RequireFlags []string `yaml:"require_flags,omitempty"`
	// StrictFlags refuses any flag the row does not list, anywhere in the
	// command and in `--flag=value` form, and any VAR=value prefix.
	StrictFlags bool `yaml:"strict_flags,omitempty"`
	// URLHost and Methods make an HTTP-client row (curl, http, https, xh):
	// every URL in the request must be on URLHost and the method must be in
	// Methods ("*" = any). A read row also refuses a request body or a local
	// output file.
	URLHost string   `yaml:"url_host,omitempty"`
	Methods []string `yaml:"methods,omitempty"`

	Capability    string `yaml:"capability"`
	MutationClass string `yaml:"mutation_class"`
	// ReadOnly, when stated, must agree with MutationClass == "read".
	ReadOnly      *bool  `yaml:"read_only,omitempty"`
	Reversibility string `yaml:"reversibility,omitempty"`
	BlastRadius   string `yaml:"blast_radius,omitempty"`
	// The destination is `<destination_kind>:<value>`. The value is the last
	// literal value of any destination flag; the URL host on an HTTP row; else
	// destination_default ("unresolved" when unset). A flag value the shell
	// computes, or an absent flag under an env-assignment prefix, renders
	// "unresolved". A non-HTTP row with no destination flags is the kind alone.
	DestinationKind    string   `yaml:"destination_kind"`
	DestinationFlags   []string `yaml:"destination_flags,omitempty"`
	DestinationDefault string   `yaml:"destination_default,omitempty"`

	binaries  map[string]bool
	pattern   []string
	flagArity map[string]bool // flag name -> takes a value
	destFlags map[string]bool
}

type shellVerbCatalogFile struct {
	SchemaVersion string                `yaml:"schema_version,omitempty"`
	Rows          []ShellVerbCatalogRow `yaml:"rows"`
}

// ShellVerbCatalog is the compiled, ordered row list.
type ShellVerbCatalog struct {
	rows          []ShellVerbCatalogRow
	extensionRows int
}

// ExtensionRows reports how many rows came from the operator's file.
func (c ShellVerbCatalog) ExtensionRows() int { return c.extensionRows }

// ShellVerbCatalogPath is the operator extension file under COMPOUND_HOME/config.
func ShellVerbCatalogPath(homePath string) string {
	return filepath.Join(homePath, "config", shellVerbCatalogFileName)
}

var (
	gogValueFlags = []string{"--account", "-a", "--client", "--home", "--access-token", "--color",
		"--enable-commands", "--enable-commands-exact", "--disable-commands", "--select"}
	gogBoolFlags = []string{"--gmail-no-send", "--json", "-j", "--plain", "-p", "--results-only",
		"--wrap-untrusted", "--dry-run", "-n", "--force", "-y", "--no-input", "--verbose", "-v"}
	ascValueFlags = []string{"--profile", "--report", "--report-file", "--app", "--output", "--limit",
		"--next", "--stars", "--territory", "--sort", "--response-state", "--response-fields", "--id",
		"--review-id", "--response", "--file", "--platform", "--country", "--workers", "--state",
		"--include", "--version", "--version-id"}
	ascBoolFlags = []string{"--pretty", "--paginate", "--confirm", "--dry-run", "--include-response",
		"--only-unresponded", "--all", "--read-only", "--debug", "--api-debug", "--strict-auth",
		"--retry-log", "--latest", "--include-sensitive"}
	adsValueFlags = []string{"--window", "--product", "--platform", "--as-of", "--report", "--account",
		"--currency", "--before", "--after", "--metric", "--change-event"}
)

// builtinShellVerbCatalogRows lists the more severe shape of each family first.
func builtinShellVerbCatalogRows() []ShellVerbCatalogRow {
	const (
		gmail     = "gmail|mail|email"
		sendVerbs = "send|forward|fwd|autoreply"
		drafts    = "drafts|draft"
	)
	gog := func(id, pattern, capability, class string) ShellVerbCatalogRow {
		return ShellVerbCatalogRow{ID: id, Binary: "gog", Pattern: pattern, ValueFlags: gogValueFlags,
			BoolFlags: gogBoolFlags, Capability: capability, MutationClass: class,
			DestinationKind: "gmail-account", DestinationFlags: []string{"--account", "-a"}}
	}
	noSend := func(row ShellVerbCatalogRow) ShellVerbCatalogRow {
		row.RequireFlags = []string{"--gmail-no-send"}
		return row
	}
	asc := func(id, pattern, capability, class string) ShellVerbCatalogRow {
		return ShellVerbCatalogRow{ID: id, Binary: "asc", Pattern: pattern, ValueFlags: ascValueFlags,
			BoolFlags: ascBoolFlags, Capability: capability, MutationClass: class,
			DestinationKind: "asc-app", DestinationFlags: []string{"--app"}}
	}
	withShape := func(row ShellVerbCatalogRow, reversibility, blast string) ShellVerbCatalogRow {
		row.Reversibility, row.BlastRadius = reversibility, blast
		return row
	}
	ads := func(id, pattern, capability, class string) ShellVerbCatalogRow {
		return ShellVerbCatalogRow{ID: id, Binary: "compound", Pattern: pattern, ValueFlags: adsValueFlags,
			BoolFlags: []string{"--json"}, Capability: capability, MutationClass: class,
			DestinationKind: "ads-product", DestinationFlags: []string{"--product"}, DestinationDefault: "all"}
	}
	paddle := func(id string, methods []string, capability, class string) ShellVerbCatalogRow {
		return ShellVerbCatalogRow{ID: id, Binary: "curl|http|https|xh", URLHost: "api.paddle.com", Methods: methods,
			Capability: capability, MutationClass: class, DestinationKind: "paddle-api"}
	}
	reviewsList := asc("asc-reviews-list-default", "reviews", "app_store.customer_reviews.read", "read")
	reviewsList.NoSubcommand = true
	adsRead := ads("compound-product-ads-read", "product ads", "ads.report.read", "read")
	adsRead.NoSubcommand = true
	return []ShellVerbCatalogRow{
		// gog refuses every send under --gmail-no-send ("Gmail sending is
		// blocked"), so a send-family command carrying it keeps the read class.
		noSend(gog("gog-gmail-no-send", gmail+" "+sendVerbs, "gmail.message.read", "read")),
		noSend(gog("gog-gmail-drafts-no-send", gmail+" "+drafts+" send|post", "gmail.message.read", "read")),
		gog("gog-gmail-send", gmail+" "+sendVerbs, "gmail.message.send", "public"),
		gog("gog-gmail-drafts-send", gmail+" "+drafts+" send|post", "gmail.message.send", "public"),
		// `read` and `threads` are gog's aliases of the `thread` group, whose
		// `modify` changes labels: only its get is a read.
		gog("gog-gmail-thread-read", gmail+" thread|threads|read get|info|show", "gmail.message.read", "read"),
		gog("gog-gmail-read", gmail+" search|find|query|ls|list|get|info|show", "gmail.message.read", "read"),
		gog("gog-gmail-messages-read", gmail+" messages|message|msg|msgs search|find|query|ls|list", "gmail.message.read", "read"),

		asc("asc-delete", "* delete|remove", "app_store.delete", "destructive"),
		asc("asc-delete-nested", "* * delete|remove", "app_store.delete", "destructive"),
		asc("asc-delete-nested-deep", "* * * delete|remove", "app_store.delete", "destructive"),
		// A response can be edited or deleted through the same API, and one
		// response touches one review; a batch is wider than that.
		withShape(asc("asc-reviews-respond", "reviews respond", "app_store.review_response.write", "external_side_effect"), "easy", "one_review_response"),
		withShape(asc("asc-reviews-respond-batch", "reviews respond-batch", "app_store.review_response.write", "external_side_effect"), "easy", "app_review_responses"),
		reviewsList,
		asc("asc-reviews-read", "reviews list|get|view|ratings|summarizations", "app_store.customer_reviews.read", "read"),
		asc("asc-reviews-response-read", "reviews response view|for-review", "app_store.customer_reviews.read", "read"),
		asc("asc-versions-read", "versions list", "app_store.read", "read"),
		asc("asc-apps-read", "apps list", "app_store.read", "read"),

		paddle("paddle-api-read", []string{"GET", "HEAD"}, "paddle.api.read", "read"),
		paddle("paddle-api-write", []string{"*"}, "paddle.api.write", "external_side_effect"),

		ads("compound-product-ads-write", "product ads import|signal", "ads.report.write", "local_reversible"),
		ads("compound-product-ads-subcommand-read", "product ads platforms|help", "ads.report.read", "read"),
		adsRead,
	}
}

// shellVerbCatalogReservedBinaries cannot be taught by the extension file. Each
// runs another command (a row would classify the wrapper, not the work),
// escalates privilege, or has its own classifier a row must not reach past.
var shellVerbCatalogReservedBinaries = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "csh": true, "tcsh": true,
	"eval": true, "exec": true, "source": true, "env": true, "sudo": true, "doas": true, "su": true,
	"xargs": true, "nohup": true, "timeout": true, "gtimeout": true, "nice": true, "stdbuf": true, "time": true,
	"command": true, "builtin": true, "osascript": true, "python": true, "python3": true, "node": true,
	"ruby": true, "perl": true, "php": true, "deno": true, "bun": true, "git": true, "cp": true, "mkdir": true,
	"find": true, "sed": true, "awk": true, "make": true, "ssh": true,
	// compound's admission is owned by its executable registry.
	"compound": true,
}

var (
	shellVerbCatalogIDPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	shellVerbCatalogBinaryPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]*$`)
	shellVerbCatalogCapabilityPattern = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)+$`)
	shellVerbCatalogWordPattern       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]*$`)
	shellVerbCatalogFlagPattern       = regexp.MustCompile(`^--?[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	shellVerbCatalogMethodPattern     = regexp.MustCompile(`^([A-Z]+|\*)$`)
)

// shellMutationSeverity orders mutation classes when segments compose.
var shellMutationSeverity = map[string]int{
	"read": 0, "local_reversible": 1, "external_side_effect": 2, "public": 3,
	"financial": 4, "destructive": 5, "security": 6,
}

// shellVerbCatalogShapeDefaults fills the reversibility and blast radius a row
// omits, per mutation class.
var shellVerbCatalogShapeDefaults = map[string][2]string{
	"read":                 {"none", "account"},
	"local_reversible":     {"easy", "local"},
	"external_side_effect": {"hard", "account"},
	"public":               {"hard", "public"},
	"financial":            {"irreversible", "account"},
	"destructive":          {"irreversible", "account"},
	"security":             {"hard", "host"},
}

func (row *ShellVerbCatalogRow) compile() error {
	row.ID = strings.TrimSpace(row.ID)
	row.Binary = strings.ToLower(strings.TrimSpace(row.Binary))
	row.Capability = strings.TrimSpace(row.Capability)
	row.MutationClass = strings.TrimSpace(row.MutationClass)
	row.DestinationKind = strings.TrimSpace(row.DestinationKind)
	if !shellVerbCatalogIDPattern.MatchString(row.ID) {
		return fmt.Errorf("id %q must match %s", row.ID, shellVerbCatalogIDPattern)
	}
	if row.Binary == "" {
		return errors.New("binary is required")
	}
	row.binaries = map[string]bool{}
	for _, binary := range strings.Split(row.Binary, "|") {
		if !shellVerbCatalogBinaryPattern.MatchString(binary) {
			return fmt.Errorf("binary %q must be a bare program name", binary)
		}
		row.binaries[binary] = true
	}
	if !shellVerbCatalogCapabilityPattern.MatchString(row.Capability) {
		return fmt.Errorf("capability %q must be a dotted lowercase name like gmail.message.read", row.Capability)
	}
	defaults, ok := shellVerbCatalogShapeDefaults[row.MutationClass]
	if !ok || !contains(envelope.MutationClasses, row.MutationClass) {
		return fmt.Errorf("mutation_class %q is not one of %s", row.MutationClass, strings.Join(envelope.MutationClasses, ", "))
	}
	if row.ReadOnly != nil && *row.ReadOnly != (row.MutationClass == "read") {
		return fmt.Errorf("read_only %t contradicts mutation_class %q", *row.ReadOnly, row.MutationClass)
	}
	if row.Reversibility = strings.TrimSpace(row.Reversibility); row.Reversibility == "" {
		row.Reversibility = defaults[0]
	} else if !contains(envelope.Reversibilities, row.Reversibility) {
		return fmt.Errorf("reversibility %q is not one of %s", row.Reversibility, strings.Join(envelope.Reversibilities, ", "))
	}
	if row.BlastRadius = strings.TrimSpace(row.BlastRadius); row.BlastRadius == "" {
		row.BlastRadius = defaults[1]
	} else if !shellVerbCatalogWordPattern.MatchString(row.BlastRadius) {
		return fmt.Errorf("blast_radius %q must be a single word", row.BlastRadius)
	}
	row.pattern = strings.Fields(row.Pattern)
	for len(row.pattern) > 0 && row.pattern[len(row.pattern)-1] == "*" {
		row.pattern = row.pattern[:len(row.pattern)-1]
	}
	for _, element := range row.pattern {
		for _, alternative := range strings.Split(element, "|") {
			if alternative != "*" && !shellVerbCatalogWordPattern.MatchString(alternative) {
				return fmt.Errorf("pattern word %q must be words joined by | or *", element)
			}
		}
	}
	row.flagArity = map[string]bool{}
	for _, group := range []struct {
		field string
		flags []string
		value bool
	}{
		{"value_flags", row.ValueFlags, true},
		{"destination_flags", row.DestinationFlags, true},
		{"bool_flags", row.BoolFlags, false},
		{"require_flags", row.RequireFlags, false},
	} {
		for _, flag := range group.flags {
			if !shellVerbCatalogFlagPattern.MatchString(flag) {
				return fmt.Errorf("%s entry %q must be a flag like --name or -n", group.field, flag)
			}
			name := shellCatalogFlagName(flag)
			if takesValue, seen := row.flagArity[name]; seen && takesValue != group.value {
				return fmt.Errorf("flag %q is listed both as taking a value and as boolean", flag)
			}
			row.flagArity[name] = group.value
		}
	}
	row.destFlags = map[string]bool{}
	for _, flag := range row.DestinationFlags {
		row.destFlags[shellCatalogFlagName(flag)] = true
	}
	if row.URLHost != "" || len(row.Methods) > 0 {
		row.URLHost = strings.ToLower(strings.TrimSpace(row.URLHost))
		if row.URLHost == "" || !shellVerbCatalogWordPattern.MatchString(row.URLHost) || strings.Contains(row.URLHost, ":") {
			return fmt.Errorf("url_host %q must be a bare host name", row.URLHost)
		}
		if len(row.Methods) == 0 {
			return errors.New("methods is required with url_host")
		}
		for binary := range row.binaries {
			if !shellHTTPClientBinaries[binary] {
				return fmt.Errorf("url_host needs an HTTP client binary (curl, http, https, xh), not %q", binary)
			}
		}
		if len(row.pattern) > 0 || row.NoSubcommand || len(row.RequireFlags) > 0 || len(row.DestinationFlags) > 0 {
			return errors.New("url_host rows match by URL and method: pattern, no_subcommand, require_flags, and destination_flags do not apply")
		}
		for i, method := range row.Methods {
			row.Methods[i] = strings.ToUpper(strings.TrimSpace(method))
			if !shellVerbCatalogMethodPattern.MatchString(row.Methods[i]) {
				return fmt.Errorf("method %q must be an HTTP method or *", method)
			}
		}
	}
	if !shellVerbCatalogWordPattern.MatchString(row.DestinationKind) || strings.Contains(row.DestinationKind, ":") {
		return fmt.Errorf("destination_kind %q is required and must be one word without ':' (a grant scopes to it)", row.DestinationKind)
	}
	if row.DestinationDefault = strings.TrimSpace(row.DestinationDefault); row.DestinationDefault == "" {
		row.DestinationDefault = shellVerbCatalogUnresolved
	} else if strings.ContainsAny(row.DestinationDefault, " \t$`") {
		return fmt.Errorf("destination_default %q must be one literal word", row.DestinationDefault)
	}
	return nil
}

func compileShellVerbCatalog(extension []ShellVerbCatalogRow, source string) (ShellVerbCatalog, error) {
	catalog := ShellVerbCatalog{}
	seen := map[string]bool{}
	for i := range extension {
		row := extension[i]
		label := fmt.Sprintf("%s: row %d (id %q)", source, i+1, strings.TrimSpace(row.ID))
		if err := row.compile(); err != nil {
			return ShellVerbCatalog{}, fmt.Errorf("%s: %w", label, err)
		}
		for binary := range row.binaries {
			if shellVerbCatalogReservedBinaries[binary] || shellDestructiveVerbs[binary] || strings.HasPrefix(binary, "mkfs") {
				return ShellVerbCatalog{}, fmt.Errorf("%s: binary %q is reserved: it runs other commands, destroys data, or has its own classifier", label, binary)
			}
		}
		if seen[row.ID] {
			return ShellVerbCatalog{}, fmt.Errorf("%s: duplicate id", label)
		}
		seen[row.ID] = true
		catalog.rows = append(catalog.rows, row)
	}
	catalog.extensionRows = len(catalog.rows)
	for _, row := range builtinShellVerbCatalogRows() {
		if seen[row.ID] {
			continue
		}
		if err := row.compile(); err != nil {
			return ShellVerbCatalog{}, fmt.Errorf("built-in row %q: %w", row.ID, err)
		}
		catalog.rows = append(catalog.rows, row)
	}
	return catalog, nil
}

// LoadShellVerbCatalog returns the built-ins merged with the operator extension
// file and the file it read ("built-in" when there is none). An invalid file
// returns the built-ins alone together with an error naming the bad row.
func LoadShellVerbCatalog(homePath string) (ShellVerbCatalog, string, error) {
	builtins, err := compileShellVerbCatalog(nil, "built-in")
	if err != nil {
		return ShellVerbCatalog{}, "built-in", err
	}
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return builtins, "built-in", nil
	}
	path := ShellVerbCatalogPath(homePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return builtins, "built-in", nil
		}
		return builtins, path, fmt.Errorf("read %s: %w", path, err)
	}
	var file shellVerbCatalogFile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return builtins, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.SchemaVersion != "" && file.SchemaVersion != ShellVerbCatalogSchema {
		return builtins, path, fmt.Errorf("%s: schema_version %q, want %q", path, file.SchemaVersion, ShellVerbCatalogSchema)
	}
	catalog, err := compileShellVerbCatalog(file.Rows, path)
	if err != nil {
		return builtins, path, err
	}
	return catalog, path, nil
}

// shellVerbCatalogCache is the process's catalog. It reloads only when the
// extension file's identity (path, size, mtime) changes, so a long-lived
// process sees an edit and the per-call hook pays one stat.
var shellVerbCatalogCache struct {
	sync.Mutex
	key     string
	catalog ShellVerbCatalog
	source  string
	err     error
}

// ActiveShellVerbCatalog is the catalog the classifier uses: the built-ins
// merged with COMPOUND_HOME/config/shell-verb-catalog.yaml.
func ActiveShellVerbCatalog() (ShellVerbCatalog, string, error) {
	homePath, err := homepkg.Resolve("")
	if err != nil {
		homePath = ""
	}
	key := "built-in"
	if homePath != "" {
		key = ShellVerbCatalogPath(homePath)
		if info, statErr := os.Stat(key); statErr == nil {
			key += fmt.Sprintf("|%d|%s", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
		}
	}
	shellVerbCatalogCache.Lock()
	defer shellVerbCatalogCache.Unlock()
	if shellVerbCatalogCache.key != key {
		catalog, source, loadErr := LoadShellVerbCatalog(homePath)
		shellVerbCatalogCache.key = key
		shellVerbCatalogCache.catalog, shellVerbCatalogCache.source, shellVerbCatalogCache.err = catalog, source, loadErr
	}
	return shellVerbCatalogCache.catalog, shellVerbCatalogCache.source, shellVerbCatalogCache.err
}

type shellVerbCatalogMatch struct {
	row         ShellVerbCatalogRow
	destination string
}

// applyShellVerbCatalog overlays the catalog on a shell classification, or
// returns it unchanged when the catalog does not own the whole command.
func applyShellVerbCatalog(
	classification ActionClassification,
	command string,
	segments []shellSegment,
	firstPartyCommands FirstPartyCommandCatalog,
) ActionClassification {
	if classification.OperatorGated || len(segments) == 0 || shellCatalogCommandMaySplit(command) {
		return classification
	}
	catalog, _, _ := ActiveShellVerbCatalog()
	var chosen *shellVerbCatalogMatch
	writeKey := ""
	legacy, hasLegacy := shellClassRead, false
	for _, segment := range segments {
		match, matched := catalog.matchSegment(segment)
		if !matched {
			class := shellSegmentClass(segment, firstPartyCommands)
			if class != shellClassRead && class != shellClassGoverned {
				return classification
			}
			if shellClassSeverity[class] > shellClassSeverity[legacy] {
				legacy = class
			}
			hasLegacy = true
			continue
		}
		if match.row.MutationClass != "read" {
			key := match.row.Capability + "\x00" + match.destination
			if writeKey != "" && writeKey != key {
				return classification
			}
			writeKey = key
		}
		if chosen == nil || shellMutationSeverity[match.row.MutationClass] > shellMutationSeverity[chosen.row.MutationClass] {
			chosen = &match
		}
	}
	if chosen == nil {
		return classification
	}
	if hasLegacy {
		classification = shellClassificationFor(classification, legacy)
	}
	if !hasLegacy || shellMutationSeverity[chosen.row.MutationClass] >= shellMutationSeverity[classification.MutationClass] {
		classification.MutationClass = chosen.row.MutationClass
		classification.Reversibility = chosen.row.Reversibility
		classification.BlastRadius = chosen.row.BlastRadius
	}
	classification.Known = true
	classification.Operation = chosen.row.Capability
	classification.Capability = chosen.row.Capability
	classification.Destination = chosen.destination
	classification.CatalogRow = chosen.row.ID
	classification.EvidenceRefs = []string{"command:" + actionHash([]byte(command))}
	return classification
}

// matchSegment applies the routine-read lane's prefix, wrapper, and executable
// rules, then returns the first matching row.
func (c ShellVerbCatalog) matchSegment(segment shellSegment) (shellVerbCatalogMatch, bool) {
	if segment.writes || segment.obfuscated {
		return shellVerbCatalogMatch{}, false
	}
	words, ok := shellStripControlKeywords(segment.words)
	if !ok || len(words) == 0 {
		return shellVerbCatalogMatch{}, false
	}
	fields, ok := shellStripCommandPrefixes(words)
	if !ok || len(fields) == 0 {
		return shellVerbCatalogMatch{}, false
	}
	prefix := words[:len(words)-len(fields)]
	if shellPrefixChangesExecution(prefix) || !shellCatalogExecutableTrusted(fields[0]) {
		return shellVerbCatalogMatch{}, false
	}
	binary, args := shellVerbName(fields[0]), fields[1:]
	for _, row := range c.rows {
		if !row.binaries[binary] {
			continue
		}
		if destination, ok := row.match(binary, args, shellPrefixHasAssignment(prefix)); ok {
			return shellVerbCatalogMatch{row: row, destination: destination}, true
		}
	}
	return shellVerbCatalogMatch{}, false
}

// shellCatalogExecutableTrusted admits a bare program name (resolved through
// PATH) or a system bin directory. A relative path is a program the agent may
// have just written, so it never inherits a vendor CLI's classification.
func shellCatalogExecutableTrusted(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, `"'$`) || strings.Contains(token, shellSubstitutionPlaceholder) {
		return false
	}
	if !strings.Contains(token, "/") {
		return true
	}
	switch filepath.Dir(token) {
	case "/usr/bin", "/bin", "/usr/local/bin", "/opt/homebrew/bin":
		return true
	default:
		return false
	}
}

// shellCatalogCommandMaySplit reports an expansion the shell may turn into
// several words -- new flags or a new subcommand -- at run time: an unquoted
// parameter, command, or arithmetic expansion, an unquoted glob or brace, or a
// quoted "$@" / "${a[@]}". Quoted expansions stay one word and are allowed, so
// `-H "Authorization: Bearer $KEY"` still matches.
func shellCatalogCommandMaySplit(command string) bool {
	inSingle, inDouble := false, false
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			}
		case r == '\\':
			i++
		case inDouble:
			switch r {
			case '"':
				inDouble = false
			case '$':
				rest := string(runes[i:])
				if strings.HasPrefix(rest, "$@") || (strings.HasPrefix(rest, "${") && strings.Contains(strings.SplitN(rest, "}", 2)[0], "[@]")) {
					return true
				}
			}
		case r == '\'':
			inSingle = true
		case r == '"':
			inDouble = true
		case r == '$' || r == '`' || r == '*' || r == '?' || r == '[' || r == '{':
			return true
		}
	}
	return false
}

func (row ShellVerbCatalogRow) match(binary string, args []string, assignmentPrefix bool) (string, bool) {
	if row.URLHost != "" {
		return row.matchHTTP(binary, args)
	}
	if row.StrictFlags && assignmentPrefix {
		return "", false
	}
	matched := 0
	set := map[string]bool{}
	flagsDone := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !flagsDone && arg == "--" {
			flagsDone = true
			continue
		}
		if !flagsDone && strings.HasPrefix(arg, "-") && arg != "-" {
			name, value, attached := strings.Cut(arg, "=")
			name = shellCatalogFlagName(name)
			takesValue, known := row.flagArity[name]
			if !known && (row.StrictFlags || !attached && matched < len(row.pattern)) {
				return "", false
			}
			if takesValue && !attached {
				i++
			}
			if !takesValue {
				set[name] = !attached || value == "true"
				if negated, ok := strings.CutPrefix(name, "no-"); ok && !attached {
					set[negated] = false
				}
			}
			continue
		}
		if matched < len(row.pattern) {
			if !shellCatalogPatternWordMatches(row.pattern[matched], arg) {
				return "", false
			}
			matched++
			continue
		}
		if row.NoSubcommand {
			return "", false
		}
	}
	if matched < len(row.pattern) {
		return "", false
	}
	for _, flag := range row.RequireFlags {
		if !set[shellCatalogFlagName(flag)] {
			return "", false
		}
	}
	return row.destination(args, "", assignmentPrefix), true
}

func shellCatalogPatternWordMatches(element, word string) bool {
	for _, alternative := range strings.Split(element, "|") {
		if alternative == "*" || alternative == word {
			return true
		}
	}
	return false
}

// shellCatalogFlagName folds `-app`, `--app`, and `--app=` to one name, the way
// Go's flag package accepts either dash count.
func shellCatalogFlagName(flag string) string {
	return strings.TrimLeft(strings.TrimSpace(flag), "-")
}

func (row ShellVerbCatalogRow) destination(args []string, host string, assignmentPrefix bool) string {
	value := ""
	switch {
	case row.URLHost != "":
		value = host
	case len(row.destFlags) == 0:
		return row.DestinationKind
	default:
		value = row.DestinationDefault
		if assignmentPrefix {
			// An env prefix can change what the tool's default resolves to.
			value = shellVerbCatalogUnresolved
		}
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if arg == "--" {
				break
			}
			if !strings.HasPrefix(arg, "-") || arg == "-" {
				continue
			}
			name, flagValue, attached := strings.Cut(arg, "=")
			name = shellCatalogFlagName(name)
			if !attached && row.flagArity[name] {
				if i+1 >= len(args) {
					break
				}
				i++
				flagValue, attached = args[i], true
			}
			if attached && row.destFlags[name] {
				value = shellCatalogLiteral(flagValue) // the last occurrence wins
			}
		}
	}
	return row.DestinationKind + ":" + value
}

func shellCatalogLiteral(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "$` \t\n") || strings.Contains(value, shellSubstitutionPlaceholder) {
		return shellVerbCatalogUnresolved
	}
	return value
}

// actionCapability is the capability an intent carries: the catalog's name
// when a row matched, else the domain-qualified operation.
func actionCapability(classification ActionClassification) string {
	if classification.Capability != "" {
		return classification.Capability
	}
	return classification.Domain + "." + classification.Operation
}

// ShellClassifyReport is `compound policy classify` output.
type ShellClassifyReport struct {
	Command       string `json:"command"`
	Capability    string `json:"capability"`
	MutationClass string `json:"mutation_class"`
	Reversibility string `json:"reversibility"`
	BlastRadius   string `json:"blast_radius"`
	Destination   string `json:"destination"`
	CatalogRow    string `json:"catalog_row"`
	CommandHash   string `json:"command_hash"`
	Known         bool   `json:"known"`
	CatalogSource string `json:"catalog_source"`
	CatalogError  string `json:"catalog_error,omitempty"`
}

const shellClassifyUsage = "usage: compound policy classify --command <text> [--json]"

// RunClassifyCLI is `compound policy classify`: it classifies one shell command
// exactly as the live hook would and names the catalog row that decided it.
// A rejected extension file still prints the built-in classification, then
// exits 1 with the error naming the row.
func RunClassifyCLI(args []string, firstPartyCommands FirstPartyCommandCatalog, stdout, stderr io.Writer) int {
	command, haveCommand, jsonOut := "", false, false
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--json":
			jsonOut = true
		case arg == "--command" && i+1 < len(args):
			i++
			command, haveCommand = args[i], true
		case strings.HasPrefix(arg, "--command="):
			command, haveCommand = strings.TrimPrefix(arg, "--command="), true
		default:
			fmt.Fprintln(stderr, shellClassifyUsage)
			return 2
		}
	}
	if !haveCommand || strings.TrimSpace(command) == "" {
		fmt.Fprintln(stderr, shellClassifyUsage)
		return 2
	}
	_, source, catalogErr := ActiveShellVerbCatalog()
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "policy classify: resolve cwd:", err)
		return 1
	}
	homePath, err := homepkg.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "policy classify: resolve home:", err)
		return 1
	}
	cfg, _, err := LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "policy classify: load policy:", err)
		return 1
	}
	classification := classifyToolAction(ToolActionRequest{
		Tool:               "Bash",
		Arguments:          map[string]any{"command": command},
		FirstPartyCommands: firstPartyCommands,
		Cwd:                cwd,
	}, cfg)
	report := ShellClassifyReport{
		Command:       command,
		Capability:    actionCapability(classification),
		MutationClass: classification.MutationClass,
		Reversibility: classification.Reversibility,
		BlastRadius:   classification.BlastRadius,
		Destination:   classification.Destination,
		CatalogRow:    classification.CatalogRow,
		CommandHash:   "command:" + actionHash([]byte(command)),
		Known:         classification.Known,
		CatalogSource: source,
	}
	if report.CatalogRow == "" {
		report.CatalogRow = ShellVerbCatalogFallback
	}
	if catalogErr != nil {
		report.CatalogError = catalogErr.Error()
	}
	if jsonOut {
		body, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(stdout, string(body))
	} else {
		fmt.Fprintf(stdout, "capability: %s\nmutation_class: %s\nreversibility: %s\nblast_radius: %s\ndestination: %s\ncatalog_row: %s\ncommand_hash: %s\nknown: %t\ncatalog_source: %s\n",
			report.Capability, report.MutationClass, report.Reversibility, report.BlastRadius, report.Destination,
			report.CatalogRow, report.CommandHash, report.Known, report.CatalogSource)
	}
	if catalogErr != nil {
		fmt.Fprintln(stderr, "policy classify: shell verb catalog extension rejected (built-ins still apply):", catalogErr)
		return 1
	}
	return 0
}
